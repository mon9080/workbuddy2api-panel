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

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
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
// + x-b3-parentspanid。既有 injectConversationHeaders 已覆盖 X-Conversation-* /
// X-Request-* / X-B3-TraceId/SpanId/Sampled，此处只补官方多出的四头。
//
// 形态（golden 实测）：traceID=32hex（=会话请求 ID 同源）、spanID=16hex、
// parentspanID=16hex；b3 = `<trace>-<span>-1-<parent>`；traceparent =
// `00-<trace>-<span>-01`；baggage = `codebuddy.session_id=<conversationID>,
// codebuddy.conversation_request_id=<requestID>`。
func (c *Client) injectCLITraceHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		convReqID = session.NewMessageID()
	}
	traceID := convReqID
	if !validTraceID(traceID) {
		traceID = session.NewMessageID()
	}
	// span 用 traceID 派生（同一轮对话内稳定，与官方「一次 user send 一个 span」
	// 语义一致）；parent 用 conversationID 派生（上游轮次的稳定父跨度）。
	spanID := deriveSpanID(traceID, "span")
	parentID := deriveSpanID(meta.ConversationID, "parent")
	req.Header.Set("X-B3-ParentSpanId", parentID)
	req.Header.Set("B3", traceID+"-"+spanID+"-1-"+parentID)
	req.Header.Set("Traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("Baggage", "codebuddy.session_id="+meta.ConversationID+
		",codebuddy.conversation_request_id="+convReqID)
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
		if a.EnterpriseID != "" {
			h.Set("X-Enterprise-Id", a.EnterpriseID)
		}
		if d := a.DomainValue(); d != "" {
			h.Set("X-Domain", d)
		}
	}
	c.injectCLIAttribution(req, a)
	c.injectStainlessHeaders(req)
	c.injectClientIP(req, clientIP)
	c.injectConversationHeaders(req, meta)
	c.injectCLITraceHeaders(req, meta)
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
//   - 剔除 metadata / conversation_id / previous_response_id（Responses API 与
//     客户端自定义字段，官方 CLI chat 路径不发）
//
// 不做的事：extra_vars / messages[].agent / messages[].conversationRequestId 是
// 官方 CLI 的 agent 运行时元数据（harnessMetadata/contextWindowId/stepIndex 等
// 会话状态），网关无从得知也无从伪造——伪造含虚构会话状态的字段比缺省更易被
// 识别，故不注入。
func applyCLIBodyFields(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
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
	for _, drop := range [...]string{"metadata", "conversation_id", "previous_response_id"} {
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
