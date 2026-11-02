# oracle — CLI 身份校准闭环

对官方 `@tencent-ai/codebuddy-code`（npm latest）做**出站形态校准**：mock 上游驱动官方 CLI 捕获真实请求（golden），与本网关实现逐头/逐字段比对。

## 组件

| 文件 | 作用 |
|---|---|
| `mock-upstream.js` | mock 上游：记录官方 CLI 全部出站请求（含 gzip 解码），对 models/chat 给最小可用响应 |
| `calibrate.sh` | 校准主脚本：npm latest → 下载解包 → 驱动捕获 → Go 比对 → 通过则 bump 版本常量 → 清理（tarball 保留最近 2 版） |
| `.github/workflows/oracle-daily.yml` | 每日定时跑校准：diff 空自动提交 bump；diff 非空作业失败并开 issue |

## 比对实现

`internal/upstream/oracle_diff_test.go`（`WB2A_ORACLE_GOLDEN=<捕获文件> go test -run TestOracleGoldenDiff ./internal/upstream/`）：

1. **确定性身份头全等**：UA / x-ide-\* / x-agent-\* / x-product / x-private-data / x-stainless-\* / 基础头
2. **会话/链路头**：存在性 + 形态（b3 / traceparent / baggage 等，值按请求随机）
3. **禁发头零出现**：Origin / Referer / Accept-Language / X-Machine-ID / X-Session-ID / X-Device-Token / X-Stainless-Timeout
4. **请求体**：gzip + cache_retention / verbosity / reasoning_summary / stream_options / 剔除字段

## 本地使用

```bash
bash oracle/calibrate.sh          # 有新版时校准；通过则改版本常量（不自动提交）
bash oracle/calibrate.sh --ci     # CI 模式：通过后自动 commit + push
```

已知偏差（非 bug）：官方 CLI 在 `CODEBUDDY_BASE_URL` 覆盖时跳过 gzip（生产恒压），故捕获中的 gzip 状态不参与比对，gzip 行为由 Go 单测独立覆盖。

## 清理策略

- 解包目录每次校准后全删；tarball 归档保留最近 2 版（回滚 diff 用）
- GitHub Actions runner 为临时环境，零堆积
- 生产服务器不下载 CLI 包（只查 npm 元数据，几 KB）
