// cliversion_test.go 版本指纹常量、npm 拉取校验与适配门禁矩阵的单测。
package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.179.2", "2.179.2", 0},
		{"2.180.0", "2.179.2", 1},
		{"2.179.2", "2.180.0", -1},
		{"2.179.10", "2.179.9", 1},
		{"3.0.0", "2.999.999", 1},
		{"2.179.2", "2.179.3", -1},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Errorf("compareVersion(%s,%s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFetchNPMLatest(t *testing.T) {
	// 端点可注入：全部用例指向本地 httptest，不打真实 npm。
	withServer := func(t *testing.T, handler http.HandlerFunc, fn func()) {
		t.Helper()
		srv := httptest.NewServer(handler)
		defer srv.Close()
		old := npmLatestEndpoint
		npmLatestEndpoint = srv.URL
		t.Cleanup(func() { npmLatestEndpoint = old })
		fn()
	}
	t.Run("合法版本", func(t *testing.T) {
		withServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"version":"2.180.0"}`))
		}, func() {
			v, err := fetchNPMLatest(context.Background(), http.DefaultClient)
			if err != nil || v != "2.180.0" {
				t.Fatalf("got %q err=%v, want 2.180.0", v, err)
			}
		})
	})
	t.Run("预发布串拒绝", func(t *testing.T) {
		withServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"version":"2.179.2-dev.05e2519.202611021616"}`))
		}, func() {
			if _, err := fetchNPMLatest(context.Background(), http.DefaultClient); err == nil {
				t.Fatal("预发布串应被拒绝（只认 latest dist-tag 的纯 semver）")
			}
		})
	})
	t.Run("next通道拒绝", func(t *testing.T) {
		withServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"version":"2.166.0-next.7626a4c.202610141224"}`))
		}, func() {
			if _, err := fetchNPMLatest(context.Background(), http.DefaultClient); err == nil {
				t.Fatal("next 预发布串应被拒绝")
			}
		})
	})
	t.Run("非200报错", func(t *testing.T) {
		withServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}, func() {
			if _, err := fetchNPMLatest(context.Background(), http.DefaultClient); err == nil {
				t.Fatal("非 200 应返回错误")
			}
		})
	})
}

// setLatestForTest 直接注入 latest 快照（同包测试访问内部状态）。
func setLatestForTest(c *Client, latest string) {
	c.identityVersion.mu.Lock()
	c.identityVersion.latest = latest
	c.identityVersion.mu.Unlock()
}

func TestIdentityGateBlockedMatrix(t *testing.T) {
	newGate := func(identity string, strict bool, latest string) *Client {
		c := New()
		c.ChatIdentity = identity
		c.IdentityStrictGate.Store(strict)
		setLatestForTest(c, latest)
		return c
	}
	cases := []struct {
		name     string
		identity string
		strict   bool
		latest   string
		blocked  bool
	}{
		{"cli+新版+strict=拦截", identityCLI, true, "2.180.0", true},
		{"cli+同版=放行", identityCLI, true, VerifiedCLIVersion, false},
		{"cli+旧版=放行(npm回退)", identityCLI, true, "2.179.1", false},
		{"cli+latest未知=放行(不因网络故障停服)", identityCLI, true, "", false},
		{"cli+新版+strict关=放行", identityCLI, false, "2.180.0", false},
		{"desktop=不适用门禁", identityDesktop, true, "2.180.0", false},
		{"零值身份(desktop)=不适用", "", true, "2.180.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked, detail := newGate(tc.identity, tc.strict, tc.latest).IdentityGateBlocked()
			if blocked != tc.blocked {
				t.Fatalf("blocked = %v, want %v (detail=%q)", blocked, tc.blocked, detail)
			}
			if blocked {
				// 拦截说明必须含两个版本号（日志/客户端可据此定位差异）。
				if !strings.Contains(detail, tc.latest) || !strings.Contains(detail, VerifiedCLIVersion) {
					t.Errorf("detail 缺版本号: %q", detail)
				}
			}
		})
	}
}
