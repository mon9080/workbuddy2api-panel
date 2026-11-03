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
		c.IdentityVersionCheck.Store(true) // 门禁矩阵在「检查开」态下断言（P1-3）
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

// TestCheckIdentityVersionNoDowngrade 后台轮询「只进不退」（2026-11-03 审查发现 #3）：
// npm 回退 dist-tag 时不得把已知的更新版抹掉，否则门禁窗口被悄悄打开。
// latestForTest 读取门禁快照中的 npm latest（IdentityStatus 返回的是生效版本）。
func latestForTest(c *Client) string {
	c.identityVersion.mu.RLock()
	defer c.identityVersion.mu.RUnlock()
	return c.identityVersion.latest
}

func TestCheckIdentityVersionNoDowngrade(t *testing.T) {
	serve := func(t *testing.T, version string) func() {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"version":"` + version + `"}`))
		}))
		old := npmLatestEndpoint
		npmLatestEndpoint = srv.URL
		return func() { npmLatestEndpoint = old; srv.Close() }
	}

	c := newCLIClient()
	c.IdentityStrictGate.Store(true)

	restore := serve(t, "9.9.9")
	c.checkIdentityVersion(context.Background())
	restore()
	if blocked, _ := c.IdentityGateBlocked(); !blocked {
		t.Fatal("已知新版应触发门禁")
	}

	// npm 回退到旧版本：快照不得回退。
	restore = serve(t, "1.0.0")
	c.checkIdentityVersion(context.Background())
	restore()
	if blocked, _ := c.IdentityGateBlocked(); !blocked {
		t.Fatal("轮询看到更旧版本不得抹掉已观测的新版（只进不退）")
	}

	// 同一快照口径也适用于校准观测：更旧值不得覆盖。
	c.ObserveLatest("0.0.1")
	if got := latestForTest(c); got != "9.9.9" {
		t.Fatalf("ObserveLatest 收到旧值后 latest=%s want 9.9.9", got)
	}
}

// TestObserveLatestValidates 非法版本串不进入快照（防脏数据触发/掩盖门禁）。
func TestObserveLatestValidates(t *testing.T) {
	c := newCLIClient()
	c.ObserveLatest("2.180.0-dev.1")
	if got := latestForTest(c); got != "" {
		t.Fatalf("预发布串不应写入快照: %q", got)
	}
	c.ObserveLatest(" 2.180.0 ")
	if got := latestForTest(c); got != "2.180.0" {
		t.Fatalf("合法版本应写入（去空白）: %q", got)
	}
}

// TestIdentityGateDisabledNeverBlocks 版本检查关闭时门禁永不拦截（codex 二轮
// P1-3）：即使快照仍留着更新版（热关与在途校准 ObserveLatest 的竞态），评估期
// 的开关检查也必须放行；且关闭期间校准观测不得写入快照（口径 = 版本未知）。
func TestIdentityGateDisabledNeverBlocks(t *testing.T) {
	c := newCLIClient()
	c.IdentityStrictGate.Store(true)
	setLatestForTest(c, "9.9.9")
	if blocked, _ := c.IdentityGateBlocked(); !blocked {
		t.Fatal("前置失败：检查开 + 已知新版应拦截")
	}
	// 热关检查（快照尚未来得及清空——模拟在途校准刚发布的竞态窗口）：
	// 门禁在评估期放行，不等写入侧清理。
	c.IdentityVersionCheck.Store(false)
	if blocked, detail := c.IdentityGateBlocked(); blocked {
		t.Fatalf("检查关后门禁不得拦截（detail=%q）", detail)
	}
	// 关闭期间校准观测不得写入：快照口径与门禁一致（版本未知）。
	c.ObserveLatest("8.8.8")
	if got := latestForTest(c); got != "9.9.9" {
		t.Fatalf("关闭检查时 ObserveLatest 不得改写快照: %q", got)
	}
	// 恢复检查 + 面板人工重置快照（P1-2 恢复出口）后回到正常评估。
	c.IdentityVersionCheck.Store(true)
	c.ClearIdentityLatestSnapshot()
	if blocked, _ := c.IdentityGateBlocked(); blocked {
		t.Fatal("快照清空后门禁不得拦截（latest 未知）")
	}
}
