// calibration_proxy_test.go 「一键适配」下载代理的接线测试。
//
// 背景（2026-11-06）：国内服务器直连 registry.npmjs.org 实测约 107KB/s，67MB 的
// 官方包需要 10 分钟以上，而校准预算只有 5 分钟——历史每次「更新官方最新版本」
// 都以 context deadline exceeded 收场。修复方式是给校准下载一个独立代理开关，
// 业务出站不受影响。这里验证三态：未配置（原样返回）、合法（真的走代理）、
// 非法（告警回退、不 panic）。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// 未配置代理时必须原样返回调用方的 client：任何包装都会改变连接复用行为。
func TestCalibProxyClientEmptyReturnsSame(t *testing.T) {
	c := &Client{HTTP: &http.Client{}}
	c.SetCalibrationProxy("")
	if got := c.calibProxyClient(c.HTTP); got != c.HTTP {
		t.Fatal("未配置代理时应原样返回传入的 client")
	}
}

// 配置合法代理后，请求必须真的经由代理发出（用 httptest 充当 HTTP 代理）。
func TestCalibProxyClientRoutesThroughProxy(t *testing.T) {
	var hits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Host == "" {
			t.Errorf("代理收到非绝对 URI 请求: %s", r.URL.String())
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	c := &Client{HTTP: &http.Client{}}
	c.SetCalibrationProxy(proxy.URL)
	hc := c.calibProxyClient(c.HTTP)
	if hc == c.HTTP {
		t.Fatal("配置代理后应返回新的 client")
	}
	req, err := http.NewRequest(http.MethodGet, "http://example.invalid/pkg.tgz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("经代理请求失败: %v", err)
	}
	resp.Body.Close()
	if hits != 1 {
		t.Fatalf("代理命中 %d 次，want 1", hits)
	}
	// 传入的 client 不能被就地改写：业务出站仍然直连。
	if c.HTTP.Transport != nil {
		if tr, ok := c.HTTP.Transport.(*http.Transport); ok && tr.Proxy != nil {
			if u, err := tr.Proxy(req); err == nil && u != nil {
				t.Fatalf("传入 client 的 Proxy 被改写为 %v", u)
			}
		}
	}
}

// 非法代理地址只告警并回退直连，不能 panic，也不能把请求发去奇怪的地方。
func TestCalibProxyClientInvalidFallsBack(t *testing.T) {
	for _, bad := range []string{"not-a-url", "ftp://127.0.0.1:1", "://bad"} {
		c := &Client{HTTP: &http.Client{}}
		c.SetCalibrationProxy(bad)
		if got := c.calibProxyClient(c.HTTP); got != c.HTTP {
			t.Fatalf("非法代理 %q 应回退到传入的 client", bad)
		}
	}
}

// 空 client（nil HTTP）不应炸：校准器在测试里可能没有装配 HTTP。
func TestCalibProxyClientNilBase(t *testing.T) {
	c := &Client{}
	c.SetCalibrationProxy("http://127.0.0.1:7890")
	hc := c.calibProxyClient(nil)
	if hc == nil {
		t.Fatal("nil base 应回退到 http.DefaultClient 的副本")
	}
	u, err := url.Parse("http://example.invalid/x")
	if err != nil {
		t.Fatal(err)
	}
	if tr, ok := hc.Transport.(*http.Transport); !ok || tr.Proxy == nil {
		t.Fatal("nil base 时也应设置代理")
	} else if got, err := tr.Proxy(&http.Request{URL: u}); err != nil || got == nil ||
		got.String() != "http://127.0.0.1:7890" {
		t.Fatalf("代理解析 = %v, %v; want http://127.0.0.1:7890", got, err)
	}
}
