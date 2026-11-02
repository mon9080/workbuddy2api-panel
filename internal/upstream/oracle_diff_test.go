// oracle_diff_test.go 与官方 CLI golden 的逐头比对（oracle 校准，设计 §5）。
//
// 用法：WB2A_ORACLE_GOLDEN=<官方捕获 requests.jsonl 路径> go test -run TestOracleGoldenDiff ./internal/upstream/
// 未设环境变量时跳过（普通 go test 不依赖外部文件）。
//
// golden 来源：官方 CLI（@tencent-ai/codebuddy-code@<VerifiedCLIVersion>，-p 模式
// + CODEBUDDY_BASE_URL 指向 mock）真实出站捕获，见 _codebuddy_oracle 工具目录。
// 比对口径分三层：
//  1. 确定性身份头：逐头全等（UA/归因/stainless/基础头）
//  2. 会话/链路头：存在性 + 形态（regex），值本身按请求随机
//  3. 禁发头：官方不发的一律不得出现
package upstream

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// oracleExactHeaders 第 1 层：与官方 golden 全等比对的头（值随版本/环境确定）。
var oracleExactHeaders = []string{
	"User-Agent", "Accept", "Content-Type", "X-Requested-With",
	"X-CodeBuddy-Request",
	"X-IDE-Type", "X-IDE-Name", "X-IDE-Version", "X-Product",
	"X-Agent-Intent", "X-Agent-Purpose", "X-Agent-Type", "X-Agent-Runtime",
	"X-Stainless-Lang", "X-Stainless-Package-Version", "X-Stainless-OS",
	"X-Stainless-Arch", "X-Stainless-Runtime", "X-Stainless-Runtime-Version",
	"X-Stainless-Retry-Count",
}

// oraclePresenceHeaders 第 2 层：存在即算过（值按请求随机/账号不同）。
var oraclePresenceHeaders = []string{
	"Authorization", "X-User-Id",
	"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID",
	"X-Request-ID", "X-Root-Request-ID", "X-Trace-ID",
	"X-B3-TraceId", "X-B3-SpanId", "X-B3-Sampled", "X-B3-ParentSpanId",
	"B3", "Traceparent", "Baggage",
}

// oracleBannedHeaders 第 3 层：官方 CLI 从不发送，出现即漂移。
var oracleBannedHeaders = []string{
	"Origin", "Referer", "Accept-Language", "X-Machine-ID", "X-Session-ID",
	"X-Device-Token", "X-Stainless-Timeout",
}

func TestOracleGoldenDiff(t *testing.T) {
	goldenPath := os.Getenv("WB2A_ORACLE_GOLDEN")
	if goldenPath == "" {
		t.Skip("WB2A_ORACLE_GOLDEN 未设置：跳过 oracle 比对（设置后对官方捕获逐头 diff）")
	}

	// 读官方 golden，取 chat/completions 那条。
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("读 golden 失败: %v", err)
	}
	var official map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var entry struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if strings.Contains(entry.URL, "chat/completions") {
			official = entry.Headers
			break
		}
	}
	if official == nil {
		t.Fatal("golden 中找不到 chat/completions 捕获")
	}

	// 本实现同样打 mock 上游，捕获出站。
	var oursHeaders http.Header
	var oursBody []byte
	var oursGzipped bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oursHeaders = r.Header.Clone()
		oursGzipped = r.Header.Get("Content-Encoding") == "gzip"
		raw, _ := io.ReadAll(r.Body)
		if oursGzipped {
			if zr, err := gzip.NewReader(strings.NewReader(string(raw))); err == nil {
				raw, _ = io.ReadAll(zr)
			}
		}
		oursBody = raw
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := newCLIClient()
	c.ChatBaseCN = srv.URL
	a := &auth.Auth{UID: "u1", AccessToken: "t", Domain: "www.workbuddy.cn"}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	rc, status, _, err := c.ChatStream(a, body, "", ChatMeta{
		ConversationID:        "01a19c44-90ad-7bc5-9891-a80d2fe34e7a",
		ConversationRequestID: "01a19c44941a7caf85060b18d8a4048e",
	})
	if err != nil || status != 200 {
		t.Fatalf("ChatStream 失败: status=%d err=%v", status, err)
	}
	rc.Close()

	// 第 1 层：确定性身份头全等。
	for _, name := range oracleExactHeaders {
		want := headerGet(official, name)
		got := oursHeaders.Get(name)
		if got != want {
			t.Errorf("身份头漂移 %s:\n  官方: %q\n  本实现: %q", name, want, got)
		}
	}
	// 第 2 层：会话/链路头存在 + 形态。
	for _, name := range oraclePresenceHeaders {
		if oursHeaders.Get(name) == "" {
			t.Errorf("本实现缺少链路头 %s", name)
		}
	}
	if got := oursHeaders.Get("B3"); !reB3.MatchString(got) {
		t.Errorf("B3 形态漂移: %q", got)
	}
	if got := oursHeaders.Get("Traceparent"); !reTraceparent.MatchString(got) {
		t.Errorf("Traceparent 形态漂移: %q", got)
	}
	// 第 3 层：禁发头。
	for _, name := range oracleBannedHeaders {
		if got := oursHeaders.Get(name); got != "" {
			t.Errorf("本实现多发了官方不发的头 %s: %q", name, got)
		}
	}

	// 请求体：gzip + 网关控制字段。
	if !oursGzipped {
		t.Errorf("本实现 chat 请求体未 gzip（官方生产形态恒压缩）")
	}
	var payload map[string]any
	if err := json.Unmarshal(oursBody, &payload); err != nil {
		t.Fatalf("本实现 body 解析失败: %v", err)
	}
	wantFields := map[string]any{
		"cache_retention":   "long",
		"verbosity":         "high",
		"reasoning_summary": "auto",
		"stream":            true,
	}
	for k, want := range wantFields {
		if got := payload[k]; !reflect.DeepEqual(got, want) {
			t.Errorf("body 字段 %s = %v, want %v", k, got, want)
		}
	}
	if so, ok := payload["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
		t.Errorf("body stream_options.include_usage 缺失或非 true: %v", payload["stream_options"])
	}
	for _, banned := range []string{"metadata", "conversation_id", "previous_response_id"} {
		if _, has := payload[banned]; has {
			t.Errorf("body 应剔除 %s", banned)
		}
	}
}

// headerGet 大小写不敏感取头（捕获 JSON 的键为小写，Go http.Header canonical 化）。
func headerGet(h map[string]string, name string) string {
	v, ok := h[strings.ToLower(name)]
	if !ok {
		v = h[name]
	}
	return v
}
