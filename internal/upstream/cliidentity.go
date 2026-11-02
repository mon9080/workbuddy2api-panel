// cliidentity.go CodeBuddy CLI 出站身份族（chat 路径专用）。
//
// 证据：官方 CLI 2.179.2 对 mock 上游实测捕获 golden（2026-11-02，
// _codebuddy_oracle/captures/requests.jsonl）+ 官方包 product.json /
// dist/codebuddy.js（buildUserAgent / getPlatformHeaders / GzipRequestProcessor）。
// 与桌面端身份（headers.go 三段式 UA + Origin/Referer/X-Machine-ID 头族）的关键差异：
//   - UA 两段式 `CLI/<ver> CodeBuddy/<ver>`，无平台段
//   - 不发 Origin / Referer / Accept-Language / X-Machine-ID / X-Session-ID /
//     X-Device-Token（浏览器/桌面端头族，官方 CLI 从不发）
//   - Accept 恒 application/json（流式也不带 text/event-stream）
//   - 归因头族换 CLI 形态（x-ide-name/type=CLI + x-agent-* 四头 + x-private-data）
//   - 追加 x-stainless-* SDK 指纹七头（无 timeout 头——官方按需，默认不发）
//   - 追加链路头族 b3 / traceparent / baggage（codebuddy.session_id +
//     conversation_request_id）/ x-b3-parentspanid
//   - chat 请求体 gzip（官方 productFeatures.RequestBodyGzip=true，生产恒压；
//     oracle 抓包未压是因为 CODEBUDDY_BASE_URL 覆盖会显式跳过）
package upstream

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// identityCLI / identityDesktop chat_identity 配置值。
const (
	identityCLI     = "cli"
	identityDesktop = "desktop"
)

// chatIdentityOf 返回生效的 chat 出站身份。**只有显式 "cli" 才走 CLI 身份族**；
// 空/desktop/其他值回落 desktop——Go 零值保持改造前行为（既有测试与直接构造
// Client 的调用方零回归），生产默认由 config 层填 "cli"（config.go 默认值）。
func (c *Client) chatIdentityOf() string {
	if c != nil && c.ChatIdentity == identityCLI {
		return identityCLI
	}
	return identityDesktop
}

// cliIdentity 报告当前 chat 出站是否走 CLI 身份族。
func (c *Client) cliIdentity() bool {
	return c.chatIdentityOf() == identityCLI
}

// cliUserAgent CLI 身份 UA：`CLI/<ver> CodeBuddy/<ver>`（官方 buildUserAgent：
// platform 段 + productName 段；SDK 自带 UA 被 adapter 删除，无第三段）。
// 版本恒取 VerifiedCLIVersion——config 的 cli_version 是桌面端 UA 第三段的
// 语义，不得混用到 CLI 身份（两族版本独立演进）。
func (c *Client) cliUserAgent() string {
	return "CLI/" + VerifiedCLIVersion + " CodeBuddy/" + VerifiedCLIVersion
}

// stainlessOS X-Stainless-OS：runtime.GOOS 映射官方 normalizePlatform 值域。
// config upstream.identity_os 可显式覆盖（逃生舱，默认空 = 真实容器值，不伪装）。
func (c *Client) stainlessOS() string {
	if c != nil && c.IdentityOS != "" {
		return c.IdentityOS
	}
	return mapStainlessOS(runtime.GOOS)
}

// mapStainlessOS 官方 normalizePlatform 的 Go 侧等价映射（纯函数，便于逐值测试）。
func mapStainlessOS(goos string) string {
	switch goos {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "freebsd":
		return "FreeBSD"
	case "openbsd":
		return "OpenBSD"
	case "linux":
		return "Linux"
	default:
		return "Unknown"
	}
}

// stainlessArch X-Stainless-Arch：runtime.GOARCH 映射官方 normalizeArch 值域。
func (c *Client) stainlessArch() string {
	if c != nil && c.IdentityArch != "" {
		return c.IdentityArch
	}
	return mapStainlessArch(runtime.GOARCH)
}

// mapStainlessArch 官方 normalizeArch 的 Go 侧等价映射（纯函数，便于逐值测试）。
func mapStainlessArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "ia32"
	case "arm":
		return "arm"
	default:
		return "unknown"
	}
}

// stainlessNodeVersion X-Stainless-Runtime-Version：官方取 process.version
// （带 v 前缀）。钉扎 oracle 校准运行值，config upstream.identity_node_version
// 可覆盖。
func (c *Client) stainlessNodeVersion() string {
	if c != nil && c.IdentityNode != "" {
		return c.IdentityNode
	}
	return VerifiedNodeVersion
}

// injectStainlessHeaders 注入官方 OpenAI Node SDK（stainless 生成）指纹七头。
// 不发 X-Stainless-Timeout（官方仅显式设置超时的请求携带，chat 主路径实测无此头）；
// 不发 X-Stainless-Helper-Method（仅 helper 调用携带）。
func (c *Client) injectStainlessHeaders(req *http.Request) {
	h := req.Header
	h.Set("X-Stainless-Lang", "js")
	h.Set("X-Stainless-Package-Version", VerifiedSDKVersion)
	h.Set("X-Stainless-OS", c.stainlessOS())
	h.Set("X-Stainless-Arch", c.stainlessArch())
	h.Set("X-Stainless-Runtime", "node")
	h.Set("X-Stainless-Runtime-Version", c.stainlessNodeVersion())
	h.Set("X-Stainless-Retry-Count", "0")
}

// injectCLIAttribution CLI 身份的用量归因头族。对照桌面端 injectAttribution
// （X-IDE-Name/Type/Product=WorkBuddy + X-Agent-Purpose）：CLI 形态为
// x-ide-type/name=CLI、x-product=SaaS、x-agent-intent=craft、x-agent-purpose=
// conversation、x-agent-type=main、x-agent-runtime=other、x-private-data
// （企业号 true / 个人号 false，官方 getCustomHeaders 默认值）。
func (c *Client) injectCLIAttribution(req *http.Request, a *auth.Auth) {
	req.Header.Set("X-IDE-Type", "CLI")
	req.Header.Set("X-IDE-Name", "CLI")
	req.Header.Set("X-IDE-Version", VerifiedCLIVersion)
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-Agent-Runtime", "other")
	if a != nil && a.IsEnterprise() {
		req.Header.Set("X-Private-Data", "true")
	} else {
		req.Header.Set("X-Private-Data", "false")
	}
}

// injectCLITraceHeaders CLI 链路头族：b3（单头形式）+ traceparent + baggage
// + x-b3-parentspanid，并**统一覆写** injectConversationHeaders 落的初值。
//
// 官方源码证据（指纹审查，dist/codebuddy.js @9641614）：trace/span 由同一个
// `eu` 对象派生——`X-B3-TraceId == b3.trace 段 == traceparent.trace 段`、
// `X-B3-SpanId == b3.span 段 == traceparent.span 段`，同请求内三处一致；
// traceId 为**均匀随机 32hex**（与 conversationRequestId 解耦——后者是 UUIDv7，
// 时间戳前缀批量可辨，不得复用作 trace）。本实现全部覆写为同源派生值，
// 保证五处（X-B3-TraceId/SpanId、B3、Traceparent、X-Trace-ID）一致。
// parentspanID 由会话 ID 稳定派生（上游轮次的稳定父跨度）。
// Connection: keep-alive 对齐官方 Node keepAlive agent（Go 持久连接默认不发此头）。
func (c *Client) injectCLITraceHeaders(req *http.Request) {
	convReqID := req.Header.Get("X-Conversation-Request-ID")
	traceID := deriveTraceID(convReqID)
	spanID := deriveSpanID(convReqID, "span")
	parentID := deriveSpanID(req.Header.Get("X-Conversation-ID"), "parent")
	req.Header.Set("X-B3-TraceId", traceID)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-ParentSpanId", parentID)
	req.Header.Set("X-Trace-ID", traceID)
	req.Header.Set("B3", traceID+"-"+spanID+"-1-"+parentID)
	req.Header.Set("Traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("Baggage", "codebuddy.session_id="+req.Header.Get("X-Conversation-ID")+
		",codebuddy.conversation_request_id="+convReqID)
	req.Header.Set("Connection", "keep-alive")
}

// deriveTraceID 从会话请求 ID 稳定派生 32hex 均匀随机形态的 traceId
// （官方 traceId 与请求 ID 解耦、分布均匀；派生值同时保证同轮重试/换号时
// trace 稳定，聚合语义与既有 ChatMeta 一致）。
func deriveTraceID(convReqID string) string {
	sum := sha256.Sum256([]byte("cli-trace:" + convReqID))
	return hex.EncodeToString(sum[:16])
}

// deriveSpanID 从种子串稳定派生 16 hex（B3 SpanId 合法长度）。
func deriveSpanID(seed, purpose string) string {
	sum := sha256.Sum256([]byte("cli-trace:" + purpose + ":" + seed))
	return hex.EncodeToString(sum[:8])
}

// cliChatHeaders CLI 身份的 chat 出站请求头（替代桌面端 CommonHeaders+ChatHeaders
// 组合）。与官方 golden 逐头对齐；桌面端独有的头（Origin/Referer/Accept-Language/
// X-Machine-ID/X-Session-ID/X-Device-Token）一律不发。
//
// 保留项的取舍说明：X-Enterprise-Id / X-Domain 按账号透传（golden 为 custom-token
// 匿名形态无此两头，但 cn 端点历史实测需要账号域声明过 11-128 通道校验——保守保留，
// 真实登录态 CLI 的确切形态待部署期真实流量核对，golden 无法覆盖登录场景）。
// **global 账号例外**：X-Domain 强制 www.workbuddy.ai（与出站端点同域），个人号
// 加 X-No-Enterprise-Id: 1 显式声明——对齐桌面端 injectGlobalChatHeaders 的
// 实测防 11140 逻辑（headers.go 注释：global 账号送错平台段/域头会触发风控；
// 交叉审查发现 #3：CLI 分支不得无声丢弃这层防护）。
func (c *Client) cliChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("User-Agent", c.cliUserAgent())
	h.Set("X-CodeBuddy-Request", "1")
	if at := a.AccessTokenValue(); at != "" {
		h.Set("Authorization", "Bearer "+at)
	}
	if a.UID != "" {
		h.Set("X-User-Id", a.UID)
	}
	if a != nil {
		if a.IsGlobal() {
			h.Set("X-Domain", "www.workbuddy.ai")
			if a.EnterpriseID == "" {
				h.Set("X-No-Enterprise-Id", "1")
			}
		} else {
			if a.EnterpriseID != "" {
				h.Set("X-Enterprise-Id", a.EnterpriseID)
			}
			if d := a.DomainValue(); d != "" {
				h.Set("X-Domain", d)
			}
		}
	}
	c.injectCLIAttribution(req, a)
	c.injectStainlessHeaders(req)
	// 不透传客户端 IP 头（X-Forwarded-For/X-Real-IP/X-Client-IP）：官方 CLI 是
	// 端客户端，永不发代理头族——PassthroughIP 开启时这三头是网关特征
	//（指纹审查 H-2）。需要 IP 透传的部署请用 desktop 身份。
	c.injectConversationHeaders(req, meta)
	c.injectCLITraceHeaders(req)
}

// gzipCLIBody gzip 压缩 chat 请求体（官方 CLI 生产形态恒压缩，无体积阈值——
// axios gzip 拦截器对 JSON POST 全量压缩）。返回 (压缩体, 是否压缩成功)；
// 失败时调用方退回原 body（优雅降级：不压缩也能被上游接受，只是形态偏差）。
func gzipCLIBody(body []byte) ([]byte, bool) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return body, false
	}
	if err := zw.Close(); err != nil {
		return body, false
	}
	return buf.Bytes(), true
}

// applyCLIBodyFields CLI 身份的请求体字段对齐（在既有 prepareBody 管线之后执行）：
//   - cache_retention：缺省补 "long"（官方 resolveCacheRetention 默认档，
//     仅 CODEBUDDY_DISABLE_PROMPT_CACHING_1H 时 "short"——网关无此开关，恒 long）
//   - verbosity：缺省补 "high"（官方 CLI 默认档，客户端偏好类字段，无会话状态）
//   - reasoning_summary：思考已表达（reasoning_effort / reasoning.effort 为字符串）
//     且未显式携带时补 "auto"（官方 injectReasoningSummaryIfNeeded）
//   - extra_vars：按官方 agent 会话语义**自洽派生**（不静态伪造）——
//     harnessMetadata（字符串化 JSON）取真实 CLI 首轮/多轮形态：
//     schemaVersion=1、harnessVersion=已适配 CLI 版本、stepIndex=会话内已完成的
//     用户轮次数（user 消息数-1，CLI 的 step 语义）、contextWindowId=按会话 ID
//     稳定派生的带横线 UUID（同会话同窗口，跨会话互异）、agent.depth=0；
//     initiator="user"、interactionType="prompt"（官方主对话恒定值）
//   - 剔除 metadata / conversation_id / previous_response_id / prompt_cache_key
//     （Responses API 与客户端自定义字段，官方 CLI chat 路径不发；prompt_cache_key
//     是本网关桌面身份时代的自有字段，官方 2.179.2 全量源码中不存在——CLI 身份
//     下它是最直接的 body 层网关指纹，指纹审查 B-1）
//
// 所有字段均为缺省注入（客户端已写不覆盖）。conversationID 为空时（客户端
// 未带会话标识）以 uid 参与派生——否则全部空会话请求会共享同一个
// contextWindowId，构成跨账号可关联的常量指纹（交叉审查发现 #4）。
func applyCLIBodyFields(body []byte, conversationID, uid string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body
	}
	changed := false
	if _, has := obj["cache_retention"]; !has {
		obj["cache_retention"] = "long"
		changed = true
	}
	if _, has := obj["verbosity"]; !has {
		obj["verbosity"] = "high"
		changed = true
	}
	if _, has := obj["reasoning_summary"]; !has && cliEffortExpressed(obj) {
		obj["reasoning_summary"] = "auto"
		changed = true
	}
	if _, has := obj["extra_vars"]; !has {
		if ev := cliHarnessExtraVars(obj, conversationID, uid); ev != nil {
			obj["extra_vars"] = ev
			changed = true
		}
	}
	for _, drop := range [...]string{"metadata", "conversation_id", "previous_response_id", "prompt_cache_key"} {
		if _, has := obj[drop]; has {
			delete(obj, drop)
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// cliHarnessMetadata 官方 extra_vars.harnessMetadata 的结构（序列化为字符串嵌入）。
// StepIndex 语义（官方 allocateHarnessStep @10356944 实证）：同一 conversationRequestId
// 内每次模型调用递增，新 requestId 归零。网关的 X-Conversation-Request-ID 按「最后
// 一条 user 消息」派生（handler #170 轮级键），tool 循环续发复用同一 requestId——
// 官方计数器为 1、2…，网关按「最后一条 user 消息之后的 assistant 消息数」推导同值
// （codex 交叉审查 #3）。官方 stepIndex≥1 还附 previous.conversationMessageId——上游
// 消息 ID 不随流返回、网关无从得知，省略（已知残差，见 oracle/README）。
type cliHarnessMetadata struct {
	SchemaVersion   int    `json:"schemaVersion"`
	HarnessVersion  string `json:"harnessVersion"`
	StepIndex       int    `json:"stepIndex"`
	ContextWindowID string `json:"contextWindowId"`
	Agent           struct {
		Depth int `json:"depth"`
	} `json:"agent"`
}

// cliHarnessExtraVars 派生官方 CLI 形态的 extra_vars（自洽：同会话同窗口、
// stepIndex 恒 0、版本随指纹常量）。
func cliHarnessExtraVars(obj map[string]any, conversationID, uid string) map[string]any {
	meta := cliHarnessMetadata{
		SchemaVersion:   1,
		HarnessVersion:  VerifiedCLIVersion,
		StepIndex:       assistantCallsSinceLastUser(obj),
		ContextWindowID: deriveContextWindowID(conversationID, uid),
	}
	meta.Agent.Depth = 0
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	return map[string]any{
		"harnessMetadata": string(metaJSON),
		"initiator":       "user",
		"interactionType": "prompt",
	}
}

// deriveContextWindowID 按会话 ID 稳定派生 UUIDv7 **形态**（版本 nibble 钉 7、
// variant 位钉 10xx——官方 contextWindowId 实测为 v7 形态，指纹审查 B-3；完整
// v7 的时间戳前缀与「同会话稳定」不可兼得，取舍为形态对齐 + 会话稳定）。
// conversationID 为空时掺入 uid 隔离（跨账号绝不共享窗口——共享即跨账号关联
// 指纹，与 X-Machine-ID 按 uid 派生的防关联哲学同向）。
// assistantCallsSinceLastUser 官方 harness step 计数器的历史推导：最后一条 user
// 消息之后的 assistant 消息数（每条 = 同 requestId 内一次更早的模型调用；tool 结果
// 消息不计）。无 user 消息时按 0（无从定位轮边界，宁可低报）。
func assistantCallsSinceLastUser(obj map[string]any) int {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return 0
	}
	count := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		switch role, _ := m["role"].(string); role {
		case "user":
			return count
		case "assistant":
			count++
		}
	}
	return 0
}

func deriveContextWindowID(conversationID, uid string) string {
	seed := conversationID
	if strings.TrimSpace(seed) == "" {
		seed = "uid:" + uid
	}
	sum := sha256.Sum256([]byte("cli-harness-window:" + seed))
	b := []byte(hex.EncodeToString(sum[:16]))
	b[12] = '7' // UUID 版本位（v7）
	if b[16] < '8' {
		b[16] = 'a' // variant 位（10xx）
	}
	return string(b[0:8]) + "-" + string(b[8:12]) + "-" + string(b[12:16]) + "-" + string(b[16:20]) + "-" + string(b[20:32])
}

// cliEffortExpressed 官方判据：reasoning_effort 或 reasoning.effort 为字符串
// （typeof === "string"，数字/null/对象不算表达）。camelCase reasoningEffort
// 与 snake_case 在本仓库别处同等对待，这里保持一致。
func cliEffortExpressed(obj map[string]any) bool {
	for _, key := range [...]string{"reasoning_effort", "reasoningEffort"} {
		if _, ok := obj[key].(string); ok {
			return true
		}
	}
	if reasoning, ok := obj["reasoning"].(map[string]any); ok {
		if _, ok := reasoning["effort"].(string); ok {
			return true
		}
	}
	return false
}
