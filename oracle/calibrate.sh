#!/usr/bin/env bash
# oracle/calibrate.sh — CLI 身份版本校准闭环（设计见 docs/oracle 或 vault 设计文档 §3.1/§5）。
#
# 流程：npm latest 有新版 → 下载解包 → mock 驱动官方 CLI 捕获 golden →
#       Go oracle 比对（internal/upstream/oracle_diff_test.go）→
#       diff 空 → bump VerifiedCLIVersion/VerifiedSDKVersion 并（--ci 时）自动提交；
#       diff 非空 → 退出非零（CI 侧开 issue，生产门禁继续拦截）。
# 收尾：自动清理下载物（tarball + 解包目录，保留最近 2 版供回滚 diff）。
#
# 用法: bash oracle/calibrate.sh [--ci]
#   --ci  校准通过后自动 git commit + push（GitHub Actions 用；本地默认只打印指引）
set -euo pipefail
CI_MODE="${1:-}"

ORACLE_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(dirname "$ORACLE_DIR")"
WORK="$ORACLE_DIR/.work"          # 下载/解包工作区（收尾清理）
ARCHIVE="$ORACLE_DIR/.archive"    # tarball 归档（保留最近 2）
# 每次运行独立捕获目录（codex 交叉审查 #4：并发校准时第二个 mock 会先清空
# 第一个的 golden 再撞端口——按 PID 隔离目录 + 随机端口探测空闲后使用）。
CAPTURES="$WORK/captures-$$"
mkdir -p "$WORK" "$ARCHIVE" "$CAPTURES"

log() { echo "[calibrate] $*"; }

# ---------- 1. npm latest ----------
LATEST=$(node -e "
fetch('https://registry.npmjs.org/@tencent-ai%2Fcodebuddy-code/latest')
  .then(r=>r.json()).then(d=>process.stdout.write(d.version||''))
  .catch(()=>process.exit(1));
")
if ! [[ "$LATEST" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  log "npm latest 非纯 semver（预发布通道？）：$LATEST —— 拒绝校准，退出"
  exit 1
fi
CURRENT=$(grep -oP 'VerifiedCLIVersion = "\K[^"]+' "$REPO_DIR/internal/upstream/cliversion.go")
log "npm latest=$LATEST 已适配=$CURRENT"
if [ "$LATEST" = "$CURRENT" ]; then
  log "已是最新适配版本，无需校准"
  exit 0
fi

# ---------- 2. 下载解包 ----------
log "下载 @tencent-ai/codebuddy-code@$LATEST ..."
cd "$WORK"
npm pack "@tencent-ai/codebuddy-code@$LATEST" --pack-destination "$WORK" >/dev/null
TGZ="$WORK/tencent-ai-codebuddy-code-$LATEST.tgz"
PKG="$WORK/pkg-$LATEST"
mkdir -p "$PKG"
tar -xzf "$TGZ" -C "$PKG"
log "解包完成: $PKG/package"

# ---------- 3. 提取内嵌 SDK 版本 ----------
# 注：node 内用相对路径（cd 进解包目录）——Git Bash 的 /d/... 风格路径传入
# node 的 readFileSync 会被解析成 D:\d\...（POSIX 绝对路径在 Windows node 下
# 按当前盘符根重写），两处文件读取都必须走 cd + 相对路径。
SDK=$(cd "$PKG/package" && node -e "
const fs=require('fs');
const src=fs.readFileSync('dist/codebuddy.js','utf8');
const m=src.match(/\"X-Stainless-Package-Version\":\"([^\"]+)\"/);
process.stdout.write(m?m[1]:'');
")
if [ -z "$SDK" ]; then
  log "无法从 dist 提取 X-Stainless-Package-Version —— 上游结构变化，需人工适配"
  exit 2
fi
log "内嵌 SDK 版本=$SDK"

# ---------- 4. mock 驱动官方 CLI ----------
# 随机端口 + 就绪探测（就绪失败换口重试三次；避免固定端口的并发冲突）。
PORT=""
for _ in $(seq 1 3); do
  CAND=$((18800 + RANDOM % 1000))
  if node -e "const n=require('net');const s=n.connect($CAND,'127.0.0.1');s.on('error',()=>process.exit(0));s.on('connect',()=>{s.destroy();process.exit(1)})"; then
    PORT=$CAND; break
  fi
done
if [ -z "$PORT" ]; then PORT=18799; fi
node "$ORACLE_DIR/mock-upstream.js" "$PORT" "$CAPTURES" &
MOCK_PID=$!
trap 'kill $MOCK_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 20); do
  if node -e "fetch('http://127.0.0.1:$PORT/v1/models').then(()=>0).catch(()=>process.exit(1))" 2>/dev/null; then break; fi
  sleep 0.5
done

CFG="$WORK/codebuddy-home"
mkdir -p "$CFG"
CODEBUDDY_CONFIG_DIR="$CFG" \
CODEBUDDY_BASE_URL="http://127.0.0.1:$PORT" \
CODEBUDDY_AUTH_TOKEN="oracle-mock-token" \
CODEBUDDY_USER_ID="100000000000000000" \
CODEBUDDY_DISABLE_IDE=1 CODEBUDDY_DISABLE_AUTO_MEMORY=1 CODEBUDDY_SKIP_GIT_BASH_CHECK=1 \
  node "$PKG/package/bin/codebuddy" -p "say OK" --model deepseek-v4.1-flash >/dev/null 2>&1 || true
kill $MOCK_PID 2>/dev/null || true

if ! grep -q "chat/completions" "$CAPTURES/requests.jsonl" 2>/dev/null; then
  log "驱动失败：捕获里没有 chat/completions 请求"
  exit 3
fi
log "golden 捕获完成: $CAPTURES/requests.jsonl"

# 从 golden 提取官方实际运行的 node 版本（X-Stainless-Runtime-Version）：
# 钉扎值必须等于校准运行所用 node——CI runner 的 node 小版本与本仓库常量
# 漂移时，oracle diff 的全等断言会假失败，这里在 bump 时一并对齐。
NODE_VER=$(cd "$CAPTURES" && node -e "
const fs=require('fs');
const lines=fs.readFileSync('requests.jsonl','utf8').trim().split('\n');
for (const l of lines) {
  try { const e=JSON.parse(l);
    if (e.url && e.url.includes('chat/completions') && e.headers && e.headers['x-stainless-runtime-version']) {
      process.stdout.write(e.headers['x-stainless-runtime-version']); break; } } catch {}
}
")
if ! [[ "$NODE_VER" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  log "无法从 golden 提取 x-stainless-runtime-version（$NODE_VER）—— 需人工核查"
  exit 3
fi
log "校准运行 node 版本=$NODE_VER"

# ---------- 5. 先 bump 候选常量，再跑比对（次序关键！）----------
# oracle diff 对版本承载头（UA/X-IDE-Version/SDK/Node）做全等比对：测试跑在
# 旧常量上必然失败（golden=新版，实现=旧版）→ bump 永远执行不到（交叉审查 P0）。
# 正确次序：先把三个常量 bump 到候选值 → 用新常量编译的实现对新版 golden 比对
# → 失败回滚常量并报警，成功则常量已是目标值、直接提交。
restore_constants() {
  git checkout -- internal/upstream/cliversion.go
  log "已回滚版本常量（保持旧适配版本）"
}
# 失败路径统一兜底：回滚常量 + 杀 mock（两个 trap 合一，后设的 trap 会覆盖
# 先前的 mock-kill）。必须先 cd 回仓库根——此前的 cwd 是 $WORK（npm pack 目录），
# 相对路径的 sed/checkout 会静默找错文件（2026-11-03 实案：bump 段静默死亡）。
cd "$REPO_DIR"
trap 'restore_constants; kill $MOCK_PID 2>/dev/null || true' EXIT
sed -i "s/VerifiedCLIVersion = \"[^\"]*\"/VerifiedCLIVersion = \"$LATEST\"/" internal/upstream/cliversion.go
sed -i "s/VerifiedSDKVersion = \"[^\"]*\"/VerifiedSDKVersion = \"$SDK\"/" internal/upstream/cliversion.go
sed -i "s/VerifiedNodeVersion = \"[^\"]*\"/VerifiedNodeVersion = \"$NODE_VER\"/" internal/upstream/cliversion.go
log "候选常量已 bump：CLI=$LATEST SDK=$SDK Node=$NODE_VER，开始 oracle 比对"

if ! WB2A_ORACLE_GOLDEN="$CAPTURES/requests.jsonl" go test ./internal/upstream/ -run TestOracleGoldenDiff -v; then
  restore_constants
  trap - EXIT
  log "❌ 新版 $LATEST 出站形态与实现存在漂移（见上方 diff）。已回滚 bump，生产门禁继续拦截。"
  log "   适配路径：按漂移字段修改 internal/upstream/cliidentity.go 后重跑本脚本。"
  exit 4
fi
trap - EXIT
log "✅ 校准通过：VerifiedCLIVersion=$LATEST VerifiedSDKVersion=$SDK VerifiedNodeVersion=$NODE_VER"

if [ "$CI_MODE" = "--ci" ]; then
  git config user.name  "oracle-bot"
  git config user.email "oracle-bot@users.noreply.github.com"
  git add internal/upstream/cliversion.go
  git commit -m "chore(identity): oracle 校准通过，bump verified CLI $LATEST (SDK $SDK)"
  git push
  log "已提交并推送 bump"
else
  log "本地模式不自动提交；请手动提交 internal/upstream/cliversion.go 的版本常量"
fi

# ---------- 7. 清理（成功失败都执行到不了这里时由 trap 之外手动兜底；成功路径在此清理） ----------
cleanup() {
  local freed=0
  # tarball 归档保留最近 2 版
  mv "$TGZ" "$ARCHIVE/" 2>/dev/null || true
  ls -t "$ARCHIVE"/*.tgz 2>/dev/null | tail -n +3 | while read -r f; do
    freed=$((freed + $(du -k "$f" | cut -f1)))
    rm -f "$f"; log "清理旧归档: $f"
  done
  # 解包目录全清（需要时可从归档 tarball 秒级恢复）；历史 per-run 捕获目录一并清
  #（当前 run 的留在原处供排查，下次校准收尾时被清）。
  rm -rf "$PKG"
  find "$WORK" -maxdepth 1 -type d -name 'captures-*' ! -path "$CAPTURES" -exec rm -rf {} + 2>/dev/null || true
  log "清理完成（解包目录已删，归档保留最近 2 版）"
}
cleanup
