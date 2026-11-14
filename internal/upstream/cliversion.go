// cliversion.go CLI 身份的版本指纹与适配门禁（fail-closed）。
//
// 版本指纹三常量是 CLI 身份出站的唯一版本来源（UA / x-ide-version /
// x-stainless-package-version / x-stainless-runtime-version），随代码发布钉扎。
// 升级它们的唯一途径是 oracle 重校准（mock 上游驱动官方 CLI → 与 golden diff
// 为空 → 提交 bump），生产进程不做运行时版本替换。
//
// 门禁语义：npm latest 出现比 VerifiedCLIVersion 更新的版本而本地未完成适配时，
// CLI 身份的 chat 出站自动拒绝（handler 层，不选号不罚号），日志给出操作指引。
// npm 拉取失败只告警不拦截（fail-closed 只针对「确认存在未适配新版」，不针对
// 网关自身的网络故障）。
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// VerifiedCLIVersion 已适配（oracle 校准通过）的官方 CodeBuddy CLI 版本。
	// 证据：2026-11-02 对 2.179.2（npm latest）mock 驱动捕获 golden
	// （_codebuddy_oracle/captures/requests.jsonl），UA `CLI/2.179.2 CodeBuddy/2.179.2`。
	VerifiedCLIVersion = "2.184.0"

	// VerifiedSDKVersion 2.179.2 内嵌 stainless SDK 的 X-Stainless-Package-Version。
	// 证据：官方包 dist/codebuddy.js getPlatformProperties 硬编码 "7.39.0"。
	// 该值随 CLI 版本联动（hub 时代 2.168.2 为 6.25.0），只能随 oracle 校准一起升。
	VerifiedSDKVersion = "7.39.0"

	// VerifiedNodeVersion oracle 校准运行所用的 node 版本（X-Stainless-Runtime-Version，
	// 官方取 process.version，带 v 前缀）。钉扎与校准运行一致的值保证指纹自洽，
	// 不在生产容器里装 node 探测。
	VerifiedNodeVersion = "v24.22.2"

	// npmLatestURL 官方 CLI 的 npm latest 元数据端点（只认 latest dist-tag，
	// next/dev 预发布通道一律不采）。
	npmLatestURL = "https://registry.npmjs.org/@tencent-ai%2Fcodebuddy-code/latest"
)

// npmLatestEndpoint 实际请求端点（测试可替换指向 httptest 服务器；
// 生产恒为 npmLatestURL）。
var npmLatestEndpoint = npmLatestURL

// SetNPMLatestEndpointForTest 替换 npm latest 端点（测试指向 httptest 服务器），
// 返回原值供 defer 还原。
func SetNPMLatestEndpointForTest(url string) func() {
	old := npmLatestEndpoint
	npmLatestEndpoint = url
	return func() { npmLatestEndpoint = old }
}

const (
	// identityCheckInterval 后台版本检查周期。官方节奏约日更，1 小时粒度足够；
	// TTL 缓存（3 天）不适用于该端点——元数据本身只有几 KB，直接查。
	identityCheckInterval = time.Hour

	// identityCheckTimeout npm 元数据拉取超时。
	identityCheckTimeout = 10 * time.Second
)

// plainVersionRe 纯 semver 校验：拒绝 npm 上的预发布串
// （如 2.179.2-dev.05e2519.202611021616 / 2.166.0-next.7626a4c）。
var plainVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// identityVersionState CLI 身份版本门禁的共享状态（挂在 Client 上）。
type identityVersionState struct {
	mu        sync.RWMutex
	latest    string    // npm latest 已知值（空 = 未知，不拦截）
	checkedAt time.Time // 最近一次成功检查时刻（日志/诊断用）

	// verifiedCLI/verifiedSDK 运行时版本覆盖（面板「一键适配升级」写入；空 =
	// 编译期常量 VerifiedCLIVersion/VerifiedSDKVersion）。持久化由 config 的
	// identity_verified_cli/identity_verified_sdk 承载，重启后仍在。
	verifiedCLI string
	verifiedSDK string
}

// SetIdentityVerified 热覆盖生效的已适配版本（配置落盘成功后调用；空串还原
// 编译常量）。一键适配走「先落盘、后热生效」：落盘失败时不调用，运行时保持
// 旧版本、门禁继续拦截（审查发现 #7）。
func (c *Client) SetIdentityVerified(cli, sdk string) {
	c.identityVersion.mu.Lock()
	c.identityVersion.verifiedCLI = strings.TrimSpace(cli)
	c.identityVersion.verifiedSDK = strings.TrimSpace(sdk)
	c.identityVersion.mu.Unlock()
}

// EffectiveCLIVersion 当前生效的已适配 CLI 版本（运行时覆盖 > 编译常量）。
// 出站 UA / x-ide-version / harnessVersion 统一经此取值。
func (c *Client) EffectiveCLIVersion() string {
	c.identityVersion.mu.RLock()
	defer c.identityVersion.mu.RUnlock()
	if c.identityVersion.verifiedCLI != "" {
		return c.identityVersion.verifiedCLI
	}
	return VerifiedCLIVersion
}

// EffectiveSDKVersion 当前生效的 x-stainless-package-version（覆盖 > 常量）。
func (c *Client) EffectiveSDKVersion() string {
	c.identityVersion.mu.RLock()
	defer c.identityVersion.mu.RUnlock()
	if c.identityVersion.verifiedSDK != "" {
		return c.identityVersion.verifiedSDK
	}
	return VerifiedSDKVersion
}

// IdentityStatus 面板状态接口：npm latest / 生效版本 / 门禁态。
func (c *Client) IdentityStatus() (latest, verified string, blocked bool) {
	c.identityVersion.mu.RLock()
	latest = c.identityVersion.latest
	c.identityVersion.mu.RUnlock()
	blocked, _ = c.IdentityGateBlocked()
	return latest, c.EffectiveCLIVersion(), blocked
}

// StartIdentityVersionLoop 启动后台版本检查循环：**启动即首查**（冷启动不让
// 门禁空转一小时）+ 每小时巡检。IdentityVersionCheck 关闭时不查（latest 恒空
// → 门禁永不触发，语义 = 「不检查就不拦截」）。每轮结果只更新 latest 快照；
// 拒绝决策在 IdentityGateBlocked 里按需求值。
func (c *Client) StartIdentityVersionLoop() {
	go func() {
		c.checkIdentityVersionSafe() // 启动即首查（异步：不让 npm 抖动阻塞进程启动）
		ticker := time.NewTicker(identityCheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			c.checkIdentityVersionSafe()
		}
	}()
}

// checkIdentityVersionSafe 带超时与 panic 防护的检查入口（后台 goroutine 里
// 未恢复的 panic 会带走整个进程）。拉取超时走 identityCheckTimeout，不复用
// 短 RPC 的 120s（版本元数据只有几 KB，10s 足够，快速失败快速重试）。
//
// 关闭开关时**清空已快照的 latest**：与「重启后从未检查」语义对齐（关闭 =
// 版本未知 → 门禁永不拦截）。不清空会出现「热关继续拦截、重启后放行」的
// 行为分叉（交叉审查发现 #2）。
func (c *Client) checkIdentityVersionSafe() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("WARN: [upstream] identity version check panic: %v", r)
		}
	}()
	if !c.IdentityVersionCheck.Load() {
		c.identityVersion.mu.Lock()
		c.identityVersion.latest = ""
		c.identityVersion.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), identityCheckTimeout)
	defer cancel()
	c.checkIdentityVersion(ctx)
}

// checkIdentityVersion 拉一次 npm latest 并更新快照。失败只记日志不更新
// （保留上一次的已知值——若上次已知有新版，门禁继续拦截，正确；
// 若从未成功过，latest 恒空不拦截，也正确：网关网络故障不该停服）。
func (c *Client) checkIdentityVersion(ctx context.Context) {
	latest, err := fetchNPMLatest(ctx, c.HTTP)
	if err != nil {
		log.Printf("WARN: [upstream] identity version check failed (gate unaffected): %v", err)
		return
	}
	c.identityVersion.mu.Lock()
	// 只进不退：npm 回退 dist-tag 时不得把已知的更新版抹掉（否则门禁窗口被
	// 悄悄打开）；同版或更新才覆盖（审查发现 #3）。
	if c.identityVersion.latest == "" || compareVersion(latest, c.identityVersion.latest) >= 0 {
		c.identityVersion.latest = latest
		c.identityVersion.checkedAt = time.Now()
	}
	c.identityVersion.mu.Unlock()
	if latest != VerifiedCLIVersion {
		log.Printf("WARN: [upstream] 官方 CLI 已发布 %s（已适配 %s）：%s",
			latest, VerifiedCLIVersion, identityGateHint)
	}
}

// identityGateHint 门禁触发时的操作指引（日志与错误响应共用，避免两处文案漂移）。
const identityGateHint = "等待每日 oracle 校准 Action 自动 bump verified_cli_version；或手动跑 _codebuddy_oracle 校准（diff 非空需先按漂移字段适配代码）"

// IdentityGateBlocked 报告 CLI 身份 chat 出站是否应被版本适配门禁拒绝。
// 返回 (false, "") = 放行；(true, detail) = 拒绝，detail 为中文说明（含两版本号）。
//
// 拦截条件（全部满足）：身份为 cli；strict gate 开；版本检查开；latest 已知
// 且比 verified 新。任一不满足即放行：desktop 身份无公开版本源不适用门禁；
// strict 关 = 用户显式选择了「告警放行」；版本检查关 = 版本未知（P1-3，
// 与关闭即清空快照同一契约）；latest 空 = 从未成功检查（不因自身网络故障
// 停服）；latest 旧于等于 verified = 已适配（npm 回退/同版不拦截）。

// ClearIdentityLatestSnapshot 清空已快照的 npm latest（热关闭 identity_version_check
// 时由面板保存路径调用，codex 交叉审查 #2：不等下一小时巡检——否则已触发的门禁
// 在热关后最长继续拦截约一小时，违背「关闭 = 版本未知、门禁永不触发」的配置承诺）。
func (c *Client) ClearIdentityLatestSnapshot() {
	c.identityVersion.mu.Lock()
	c.identityVersion.latest = ""
	c.identityVersion.mu.Unlock()
}

// ObserveLatest 把一次**成功获取的** npm latest 发布到门禁共享快照。
//
// 校准路径用（审查发现 #3）：一键适配拿到元数据后就立刻发布，之后无论
// drift、解包失败还是落盘失败都保留该观测——否则「校准明明发现新版却
// 因失败没更新快照」会让门禁在下一小时巡检前继续放行未适配版本。
// 同样只进不退（与后台轮询口径一致）。
//
// 版本检查关闭时不发布（codex 二轮 P1-3）：关闭 = 版本未知，校准观测不得
// 把快照重新武装起来；与 checkIdentityVersionSafe 关闭即清空同一口径。
func (c *Client) ObserveLatest(v string) {
	if !c.IdentityVersionCheck.Load() {
		return
	}
	v = strings.TrimSpace(v)
	if !plainVersionRe.MatchString(v) {
		return
	}
	c.identityVersion.mu.Lock()
	if c.identityVersion.latest == "" || compareVersion(v, c.identityVersion.latest) >= 0 {
		c.identityVersion.latest = v
		c.identityVersion.checkedAt = time.Now()
	}
	c.identityVersion.mu.Unlock()
}

func (c *Client) IdentityGateBlocked() (bool, string) {
	if c == nil || c.chatIdentityOf() != identityCLI {
		return false, ""
	}
	if !c.IdentityStrictGate.Load() {
		return false, ""
	}
	// 版本检查关闭 = 版本未知 = 门禁永不拦截（codex 二轮 P1-3）。在评估期
	// 强制该不变量，而不是只靠写入侧：否则关闭后一次在途校准的 ObserveLatest
	// 仍会把快照重新武装，门禁违背「关闭 = 永不触发」的配置承诺。
	if !c.IdentityVersionCheck.Load() {
		return false, ""
	}
	c.identityVersion.mu.RLock()
	latest := c.identityVersion.latest
	c.identityVersion.mu.RUnlock()
	verified := c.EffectiveCLIVersion()
	if latest == "" || latest == verified {
		return false, ""
	}
	if compareVersion(latest, verified) <= 0 {
		return false, ""
	}
	return true, fmt.Sprintf("官方 CLI 已发布 %s，本地已适配版本 %s，未适配新版前拒绝出站。%s",
		latest, verified, identityGateHint)
}

// fetchNPMLatest 拉取 npm latest dist-tag 的版本号并做纯 semver 校验。
// 解析出的版本非法（预发布串/空）返回错误——宁可不拦截也不被脏数据触发。
func fetchNPMLatest(ctx context.Context, hc *http.Client) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, npmLatestEndpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm latest status %d", resp.StatusCode)
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	v := strings.TrimSpace(payload.Version)
	if !plainVersionRe.MatchString(v) {
		return "", fmt.Errorf("npm latest version not plain semver: %q", payload.Version)
	}
	return v, nil
}

// compareVersion 比较两个纯 semver（a>b 返回 1，a<b 返回 -1，相等 0）。
// 防御：段数不足 3 的畸形输入按相等处理（不拦截），避免 index panic
// （当前调用面入参均过 plainVersionRe，纯防御缺口）。
func compareVersion(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		if i >= len(pa) || i >= len(pb) {
			return 0
		}
		na, _ := strconv.Atoi(pa[i])
		nb, _ := strconv.Atoi(pb[i])
		if na != nb {
			if na > nb {
				return 1
			}
			return -1
		}
	}
	return 0
}

// ErrIdentityGateBlocked 版本适配门禁拒绝的哨兵错误（CLI 身份 chat 出站收口，
// 覆盖 handler / autotask / blackcat 等全部 ChatStream 调用点——交叉审查发现：
// 只在 handler 挡会让合成任务流量在拦截期继续以未适配指纹出站）。
// 调用方按请求级错误处理：不罚号、不轮转、不重试。
var ErrIdentityGateBlocked = errors.New("identity gate blocked: official CLI released an unadapted version")
