// cliidentity_test.go CLI 身份族单测：头族逐头对齐官方 golden（2026-11-02，
// 官方 CLI 2.179.2 mock 捕获）、body 字段注入规则、gzip、desktop 零回归。
package upstream

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// newCLIClient 构造 CLI 身份的 Client（显式 ChatIdentity=cli）。
func newCLIClient() *Client {
	c := New()
	c.ChatIdentity = identityCLI
	return c
}

// newDesktopClient 构造桌面端身份的 Client（零值，改造前行为）。
func newDesktopClient() *Client {
	return New()
}

func TestChatIdentityDefault(t *testing.T) {
	// Go 零值 = desktop（既有测试/直接构造调用方零回归）；显式 cli 才走 CLI 族。
	if got := (&Client{}).chatIdentityOf(); got != identityDesktop {
		t.Fatalf("zero-value identity = %q, want desktop", got)
	}
	if got := newCLIClient().chatIdentityOf(); got != identityCLI {
		t.Fatalf("explicit cli identity = %q, want cli", got)
	}
	// 非法值回落 desktop（宁保守不误切身份）。
	c := New()
	c.ChatIdentity = "CLL"
	if got := c.chatIdentityOf(); got != identityDesktop {
		t.Fatalf("invalid identity = %q, want desktop", got)
	}
}

func TestCLIUserAgentForm(t *testing.T) {
	want := "CLI/" + VerifiedCLIVersion + " CodeBuddy/" + VerifiedCLIVersion
	if got := newCLIClient().cliUserAgent(); got != want {
		t.Fatalf("cli UA = %q, want %q", got, want)
	}
}

func TestMapStainlessOS(t *testing.T) {
	cases := map[string]string{
		"linux": "Linux", "darwin": "MacOS", "windows": "Windows",
		"freebsd": "FreeBSD", "openbsd": "OpenBSD", "sunos": "Unknown", "": "Unknown",
	}
	for in, want := range cases {
		if got := mapStainlessOS(in); got != want {
			t.Errorf("mapStainlessOS(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapStainlessArch(t *testing.T) {
	cases := map[string]string{
		"amd64": "x64", "arm64": "arm64", "386": "ia32", "arm": "arm",
		"wasm": "unknown", "": "unknown",
	}
	for in, want := range cases {
		if got := mapStainlessArch(in); got != want {
			t.Errorf("mapStainlessArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStainlessOverrides(t *testing.T) {
	c := newCLIClient()
	c.IdentityOS = "Linux"
	c.IdentityArch = "x64"
	c.IdentityNode = "v22.11.0"
	req := httptest.NewRequest(http.MethodPost, "http://x/v2/chat/completions", nil)
	c.injectStainlessHeaders(req)
	checks := map[string]string{
		"X-Stainless-OS":              "Linux",
		"X-Stainless-Arch":            "x64",
		"X-Stainless-Runtime-Version": "v22.11.0",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Package-Version": VerifiedSDKVersion,
		"X-Stainless-Retry-Count":     "0",
	}
	for h, want := range checks {
		if got := req.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	// 官方 chat 主路径不带 timeout 头（按需字段）。
	if got := req.Header.Get("X-Stainless-Timeout"); got != "" {
		t.Errorf("X-Stainless-Timeout unexpectedly set: %q", got)
	}
}

// goldenChatHeaders 用统一入口（ChatHeaders）构造请求头，供逐头断言复用。
func goldenChatHeaders(t *testing.T, c *Client, a *auth.Auth) http.Header {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://x/v2/chat/completions", nil)
	meta := ChatMeta{
		ConversationID:        "01a19c44-90ad-7bc5-9891-a80d2fe34e7a",
		ConversationRequestID: "01a19c44941a7caf85060b18d8a4048e",
	}
	c.ChatHeaders(req, a, "", meta)
	return req.Header
}

func TestCLIChatHeadersGoldenAlignment(t *testing.T) {
	c := newCLIClient()
	a := &auth.Auth{UID: "100000000000000001", AccessToken: "tok-cli", Domain: "www.workbuddy.cn"}
	h := goldenChatHeaders(t, c, a)

	exact := map[string]string{
		"User-Agent":                  "CLI/" + VerifiedCLIVersion + " CodeBuddy/" + VerifiedCLIVersion,
		"Accept":                      "application/json",
		"Content-Type":                "application/json",
		"X-Requested-With":            "XMLHttpRequest",
		"X-CodeBuddy-Request":         "1",
		"Authorization":               "Bearer tok-cli",
		"X-User-Id":                   "100000000000000001",
		"X-IDE-Type":                  "CLI",
		"X-IDE-Name":                  "CLI",
		"X-IDE-Version":               VerifiedCLIVersion,
		"X-Product":                   "SaaS",
		"X-Agent-Intent":              "craft",
		"X-Agent-Purpose":             "conversation",
		"X-Agent-Type":                "main",
		"X-Agent-Runtime":             "other",
		"X-Private-Data":              "false",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Package-Version": VerifiedSDKVersion,
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Runtime-Version": VerifiedNodeVersion,
		"X-Stainless-Retry-Count":     "0",
		"X-Stainless-OS":              mapStainlessOS(runtime.GOOS),
		"X-Stainless-Arch":            mapStainlessArch(runtime.GOARCH),
		"X-Conversation-Request-ID":   "01a19c44941a7caf85060b18d8a4048e",
		"X-Root-Request-ID":           "01a19c44941a7caf85060b18d8a4048e",
		"X-Conversation-ID":           "01a19c44-90ad-7bc5-9891-a80d2fe34e7a",
	}
	for name, want := range exact {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// 桌面端/浏览器头族：官方 CLI 从不发，CLI 身份下必须不存在。
	for _, banned := range []string{
		"Origin", "Referer", "Accept-Language", "X-Machine-ID", "X-Session-ID",
		"X-Device-Token", "X-Stainless-Timeout",
	} {
		if got := h.Get(banned); got != "" {
			t.Errorf("CLI 身份不应携带 %s（官方 golden 无此头），实际 %q", banned, got)
		}
	}

	// 会话/链路头族必须齐全（缺会触发上游 11-128 通道校验）。
	for _, required := range []string{
		"X-Conversation-Message-ID", "X-Request-ID", "X-Trace-ID",
		"X-B3-TraceId", "X-B3-SpanId", "X-B3-Sampled", "X-B3-ParentSpanId",
		"B3", "Traceparent", "Baggage",
	} {
		if h.Get(required) == "" {
			t.Errorf("CLI 身份缺少链路头 %s", required)
		}
	}
}

func TestCLIChatHeadersEnterprise(t *testing.T) {
	c := newCLIClient()
	a := &auth.Auth{UID: "u-ent", AccessToken: "tok", EnterpriseID: "ent-1", Domain: "www.workbuddy.cn"}
	h := goldenChatHeaders(t, c, a)
	if got := h.Get("X-Private-Data"); got != "true" {
		t.Errorf("enterprise X-Private-Data = %q, want true", got)
	}
	if got := h.Get("X-Enterprise-Id"); got != "ent-1" {
		t.Errorf("X-Enterprise-Id = %q, want ent-1", got)
	}
	if got := h.Get("X-Domain"); got != "www.workbuddy.cn" {
		t.Errorf("X-Domain = %q, want www.workbuddy.cn", got)
	}
}

var (
	reB3          = regexp.MustCompile(`^[0-9a-f]{32}-[0-9a-f]{16}-1-[0-9a-f]{16}$`)
	reTraceparent = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
)

func TestCLITraceHeaderFormats(t *testing.T) {
	c := newCLIClient()
	h := goldenChatHeaders(t, c, &auth.Auth{UID: "u", AccessToken: "t"})
	if got := h.Get("B3"); !reB3.MatchString(got) {
		t.Errorf("B3 = %q, want <trace32>-<span16>-1-<parent16>", got)
	}
	if got := h.Get("Traceparent"); !reTraceparent.MatchString(got) {
		t.Errorf("Traceparent = %q, want 00-<trace32>-<span16>-01", got)
	}
	baggage := h.Get("Baggage")
	if !strings.Contains(baggage, "codebuddy.session_id=01a19c44-90ad-7bc5-9891-a80d2fe34e7a") {
		t.Errorf("Baggage 缺 session_id: %q", baggage)
	}
	if !strings.Contains(baggage, "codebuddy.conversation_request_id=01a19c44941a7caf85060b18d8a4048e") {
		t.Errorf("Baggage 缺 conversation_request_id: %q", baggage)
	}
	// B3 单头的 trace 段必须与 X-B3-TraceId 一致（同一 trace 的两种编码形态）。
	b3Trace := strings.SplitN(h.Get("B3"), "-", 2)[0]
	if got := h.Get("X-B3-TraceId"); got != b3Trace {
		t.Errorf("B3 trace 段 %q 与 X-B3-TraceId %q 不一致", b3Trace, got)
	}
}

// TestDesktopChatHeadersUnchanged 桌面端身份零回归：零值 Client 的 chat 头族
// 保持改造前形态（三段式 UA + Origin/Referer + X-IDE-Name=WorkBuddy，无 stainless）。
func TestDesktopChatHeadersUnchanged(t *testing.T) {
	c := newDesktopClient()
	a := &auth.Auth{UID: "u", AccessToken: "t", Domain: "www.workbuddy.cn"}
	h := goldenChatHeaders(t, c, a)
	if got := h.Get("User-Agent"); !strings.HasPrefix(got, "WorkBuddy/") {
		t.Errorf("desktop UA = %q, want WorkBuddy/ 三段式", got)
	}
	if h.Get("Origin") == "" || h.Get("Referer") == "" {
		t.Errorf("desktop 身份应保留 Origin/Referer")
	}
	if got := h.Get("X-IDE-Name"); got != "WorkBuddy" {
		t.Errorf("desktop X-IDE-Name = %q, want WorkBuddy", got)
	}
	if h.Get("X-Stainless-Lang") != "" {
		t.Errorf("desktop 身份不应携带 stainless 头")
	}
}

func TestApplyCLIBodyFields(t *testing.T) {
	convID := "01a19c44-90ad-7bc5-9891-a80d2fe34e7a"
	t.Run("补默认字段并剔除Responses专属字段", func(t *testing.T) {
		in := `{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":{"a":1},"conversation_id":"c1","previous_response_id":"p1"}`
		out := applyCLIBodyFields([]byte(in), convID, "u-test")
		for _, want := range []string{`"cache_retention":"long"`, `"verbosity":"high"`} {
			if !strings.Contains(string(out), want) {
				t.Errorf("body 缺 %s: %s", want, out)
			}
		}
		for _, banned := range []string{"metadata", "conversation_id", "previous_response_id"} {
			if strings.Contains(string(out), banned) {
				t.Errorf("body 应剔除 %s: %s", banned, out)
			}
		}
	})
	t.Run("extra_vars自洽派生", func(t *testing.T) {
		// 首轮（1 条 user 消息）：stepIndex=0，对齐官方 golden。
		in := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
		out := applyCLIBodyFields([]byte(in), convID, "u-test")
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		ev, ok := obj["extra_vars"].(map[string]any)
		if !ok {
			t.Fatalf("extra_vars 缺失: %s", out)
		}
		if ev["initiator"] != "user" || ev["interactionType"] != "prompt" {
			t.Errorf("extra_vars 静态字段错误: %v", ev)
		}
		hm, ok := ev["harnessMetadata"].(string)
		if !ok {
			t.Fatalf("harnessMetadata 应为字符串化 JSON: %v", ev["harnessMetadata"])
		}
		var meta struct {
			SchemaVersion   int    `json:"schemaVersion"`
			HarnessVersion  string `json:"harnessVersion"`
			StepIndex       int    `json:"stepIndex"`
			ContextWindowID string `json:"contextWindowId"`
		}
		if err := json.Unmarshal([]byte(hm), &meta); err != nil {
			t.Fatalf("harnessMetadata 解析失败: %v (%s)", err, hm)
		}
		if meta.SchemaVersion != 1 || meta.HarnessVersion != VerifiedCLIVersion || meta.StepIndex != 0 {
			t.Errorf("harnessMetadata 首轮值错误: %+v", meta)
		}
		// contextWindowId 带横线 UUID 形态，且同会话稳定派生。
		if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(meta.ContextWindowID) {
			t.Errorf("contextWindowId 非 UUID 形态: %q", meta.ContextWindowID)
		}
		out2 := applyCLIBodyFields([]byte(in), convID, "u-test")
		if !strings.Contains(string(out2), meta.ContextWindowID) {
			t.Errorf("同会话 contextWindowId 应稳定: %s", out2)
		}
	})
	t.Run("stepIndex按轮内调用数推导", func(t *testing.T) {
		// 官方 allocateHarnessStep：同 requestId 内每次模型调用递增。网关按
		// 「最后一条 user 之后的 assistant 消息数」推导：tool 循环回放历史
		// [user, assistant(tc), tool] → 下一次调用 stepIndex=1（官方同语义）。
		in := `{"model":"m","messages":[{"role":"user","content":"1"},{"role":"assistant","content":"a","tool_calls":[]},{"role":"tool","content":"r"}]}`
		out := string(applyCLIBodyFields([]byte(in), convID, "u-test"))
		if !strings.Contains(out, `\"stepIndex\":1`) {
			t.Errorf("tool 循环续发 stepIndex 应为 1: %s", out)
		}
		// 新一轮（末尾是 user）：归零。
		in2 := `{"model":"m","messages":[{"role":"user","content":"1"},{"role":"assistant","content":"a"},{"role":"user","content":"2"}]}`
		out2 := string(applyCLIBodyFields([]byte(in2), convID, "u-test"))
		if !strings.Contains(out2, `\"stepIndex\":0`) {
			t.Errorf("新轮首呼 stepIndex 应为 0: %s", out2)
		}
	})
	t.Run("effort已表达时补reasoning_summary", func(t *testing.T) {
		for _, body := range []string{
			`{"model":"m","reasoning_effort":"high"}`,
			`{"model":"m","reasoningEffort":"low"}`,
			`{"model":"m","reasoning":{"effort":"low"}}`,
		} {
			out := string(applyCLIBodyFields([]byte(body), convID, "u-test"))
			if !strings.Contains(out, `"reasoning_summary":"auto"`) {
				t.Errorf("effort 表达后应补 reasoning_summary: %s → %s", body, out)
			}
		}
	})
	t.Run("effort未表达不补summary", func(t *testing.T) {
		out := string(applyCLIBodyFields([]byte(`{"model":"m","messages":[]}`), convID, "u-test"))
		if strings.Contains(out, "reasoning_summary") {
			t.Errorf("effort 缺失不应补 reasoning_summary: %s", out)
		}
		// 非字符串形态（数字/null）不算表达（官方 typeof === "string" 判据）。
		out = string(applyCLIBodyFields([]byte(`{"model":"m","reasoning_effort":3,"reasoning":{"effort":null}}`), convID, "u-test"))
		if strings.Contains(out, "reasoning_summary") {
			t.Errorf("非字符串 effort 不应补 reasoning_summary: %s", out)
		}
	})
	t.Run("显式值不覆盖", func(t *testing.T) {
		out := string(applyCLIBodyFields([]byte(`{"model":"m","cache_retention":"short","verbosity":"low","reasoning_summary":"off","reasoning_effort":"high","extra_vars":{"x":1}}`), convID, "u-test"))
		for _, keep := range []string{`"cache_retention":"short"`, `"verbosity":"low"`, `"reasoning_summary":"off"`, `"x":1`} {
			if !strings.Contains(out, keep) {
				t.Errorf("显式值被覆盖（期望保留 %s）: %s", keep, out)
			}
		}
	})
	t.Run("坏body原样返回", func(t *testing.T) {
		in := []byte("{not json")
		if out := applyCLIBodyFields(in, convID, "u-test"); string(out) != string(in) {
			t.Errorf("坏 body 应原样返回: %s", out)
		}
	})
}

func TestGzipCLIBody(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"说 OK"}]}`)
	gz, ok := gzipCLIBody(body)
	if !ok {
		t.Fatal("gzip 失败")
	}
	zr, err := gzip.NewReader(strings.NewReader(string(gz)))
	if err != nil {
		t.Fatalf("产物不是合法 gzip: %v", err)
	}
	round, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	if string(round) != string(body) {
		t.Fatalf("gzip 往返不一致: %s", round)
	}
}

// TestChatStreamCLIPathGzip ChatStreamContext 在 CLI 身份下实际 gzip 出站
// （Content-Encoding 头 + mock 上游解压校验）。
func TestChatStreamCLIPathGzip(t *testing.T) {
	var gotEncoding string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Content-Encoding")
		raw, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(strings.NewReader(string(raw)))
			if err == nil {
				decoded, _ := io.ReadAll(zr)
				raw = decoded
			}
		}
		gotBody = raw
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := newCLIClient()
	c.ChatBaseCN = srv.URL
	a := &auth.Auth{UID: "u", AccessToken: "t", Domain: "www.workbuddy.cn"}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStream(a, body, "", ChatMeta{ConversationID: "conv-1", ConversationRequestID: "01a19c44941a7caf85060b18d8a4048e"})
	if err != nil || status != 200 {
		t.Fatalf("ChatStream 失败: status=%d err=%v", status, err)
	}
	rc.Close()
	if gotEncoding != "gzip" {
		t.Errorf("CLI 身份 Content-Encoding = %q, want gzip", gotEncoding)
	}
	if !strings.Contains(string(gotBody), `"cache_retention":"long"`) {
		t.Errorf("CLI body 缺 cache_retention: %s", gotBody)
	}
}
