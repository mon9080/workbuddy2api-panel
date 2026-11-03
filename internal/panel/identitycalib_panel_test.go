package panel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fakeCalibTarball 构造官方形态 tarball（dist 含全部核对字面量 + product.json）。
func fakeCalibTarball(t *testing.T) []byte {
	t.Helper()
	dist := `"X-Stainless-Package-Version":"7.40.0"
"X-Conversation-Request-ID"
"X-CodeBuddy-Request"
"codebuddy.session_id"
"resolveCacheRetention"
"cache_retention"
"injectReasoningSummaryIfNeeded"
"X-Private-Data"
"RequestBodyGzip"`
	product := `{"productName":"CodeBuddy","platform":"CLI","productFeatures":{"RequestBodyGzip":true}}`
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, content := range map[string]string{
		"package/dist/codebuddy.js": dist,
		"package/product.json":      product,
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeNPM 启动伪 npm 元数据 + tarball 端点：/latest 返回 version=9.9.9，
// 其余路径返回构造的 tarball，并以原子计数记录真实下载次数（证明走了完整
// 校准链路；HTTP handler 与断言方非同一 goroutine，共享计数必须原子）。
func fakeNPM(t *testing.T) (base string, downloads *atomic.Int64) {
	t.Helper()
	tgz := fakeCalibTarball(t)
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version": "9.9.9",
				"dist":    map[string]string{"tarball": "http://" + r.Host + "/pkg.tgz"},
			})
			return
		}
		n.Add(1)
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/latest", &n
}

// calibrateOnce 驱动面板端点一次，返回解码后的校准结果。
func calibrateOnce(t *testing.T, p *Panel) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/identity/calibrate", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("calibrate HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应非 JSON: %v (%s)", err, rec.Body.String())
	}
	return out
}

// TestIdentityCalibratePersistOrdering 一键适配「先持久化、后热生效」的次序
// （2026-11-03 审查发现 #7 的面板层收口）。
//
// 落盘失败：status=persist_failed、持久化回调被调用、**运行时保持旧版**（门禁
// 不解除）；落盘成功：status=adapted、persisted=true、运行时由回调热切换
// （生产 SaveConfig 链路即回调本体——面板层不得二次发布，codex 二轮 P1-1）。
func TestIdentityCalibratePersistOrdering(t *testing.T) {
	endpoint, downloads := fakeNPM(t)
	old := upstream.SetNPMLatestEndpointForTest(endpoint)
	t.Cleanup(func() { old() })

	newPanel := func(persistErr error) (*Panel, *upstream.Client, *atomic.Int64, *[2]string) {
		up := upstream.New()
		up.ChatIdentity = "cli"
		up.IdentityStrictGate.Store(true)
		up.IdentityVersionCheck.Store(true) // 门禁断言需「检查开」态（P1-3）
		up.ObserveLatest("9.9.9")           // 模拟后台已探测到新版：门禁拦截中
		var calls atomic.Int64
		args := [2]string{"", ""}
		p := New(Config{
			Version:  "test",
			APIKey:   "k",
			Upstream: up,
			PersistIdentityVersions: func(cli, sdk string) error {
				calls.Add(1)
				args[0], args[1] = cli, sdk
				if persistErr != nil {
					return persistErr
				}
				// 模拟生产回调契约（main.go PersistIdentityVersions → SaveConfig）：
				// 落盘成功**包含**热应用，成功返回前运行时已切换。
				up.SetIdentityVerified(cli, sdk)
				return nil
			},
		})
		return p, up, &calls, &args
	}

	t.Run("落盘失败-运行时保持旧版", func(t *testing.T) {
		p, up, calls, args := newPanel(errors.New("disk full"))
		out := calibrateOnce(t, p)
		if calls.Load() != 1 {
			t.Fatalf("持久化回调调用次数=%d want 1", calls.Load())
		}
		// 回调收到的是校准候选对（P2-5：断言参数而非只数次数）。
		if args[0] != "9.9.9" || args[1] != "7.40.0" {
			t.Fatalf("回调参数 = %s/%s want 9.9.9/7.40.0", args[0], args[1])
		}
		if downloads.Load() == 0 {
			t.Fatal("未真正下载官方包（测试桩未被命中）")
		}
		if out["status"] != "persist_failed" || out["persisted"] != false {
			t.Fatalf("status=%v persisted=%v want persist_failed/false", out["status"], out["persisted"])
		}
		// P2-8：失败态报告真实生效对（CLI 旧版 + 真实 SDK），而非空 SDK。
		if out["new_cli"] != upstream.VerifiedCLIVersion || out["new_sdk"] != upstream.VerifiedSDKVersion {
			t.Fatalf("失败态生效对 = %v/%v want %s/%s", out["new_cli"], out["new_sdk"],
				upstream.VerifiedCLIVersion, upstream.VerifiedSDKVersion)
		}
		if got := up.EffectiveCLIVersion(); got != upstream.VerifiedCLIVersion {
			t.Fatalf("落盘失败后运行时版本被切换：%s", got)
		}
		if blocked, _ := up.IdentityGateBlocked(); !blocked {
			t.Fatal("落盘失败后门禁必须保持拦截")
		}
	})

	t.Run("落盘成功-热生效由回调完成", func(t *testing.T) {
		p, up, calls, args := newPanel(nil)
		out := calibrateOnce(t, p)
		if calls.Load() != 1 {
			t.Fatalf("持久化回调调用次数=%d want 1", calls.Load())
		}
		if args[0] != "9.9.9" || args[1] != "7.40.0" {
			t.Fatalf("回调参数 = %s/%s want 9.9.9/7.40.0", args[0], args[1])
		}
		if out["status"] != "adapted" || out["persisted"] != true {
			t.Fatalf("status=%v persisted=%v want adapted/true", out["status"], out["persisted"])
		}
		// 运行时切换来自回调（生产 SaveConfig），面板层无二次发布。
		if got := up.EffectiveCLIVersion(); got != "9.9.9" {
			t.Fatalf("落盘成功后运行时版本=%s want 9.9.9（回调热应用）", got)
		}
		if blocked, _ := up.IdentityGateBlocked(); blocked {
			t.Fatal("落盘成功后门禁应解除")
		}
	})
}

// TestIdentityCalibrateVersionCheckDisabled 关闭版本检查时校准不得武装门禁
// （codex 二轮 P1-3 面板层验证）：ObserveLatest 不写入快照，门禁评估放行。
func TestIdentityCalibrateVersionCheckDisabled(t *testing.T) {
	endpoint, downloads := fakeNPM(t)
	old := upstream.SetNPMLatestEndpointForTest(endpoint)
	t.Cleanup(func() { old() })

	up := upstream.New()
	up.ChatIdentity = "cli"
	up.IdentityStrictGate.Store(true)
	up.IdentityVersionCheck.Store(false) // 用户显式关闭版本检查
	p := New(Config{
		Version: "test",
		APIKey:  "k",
		Upstream: up,
		PersistIdentityVersions: func(cli, sdk string) error {
			up.SetIdentityVerified(cli, sdk) // 生产契约：成功即热应用
			return nil
		},
	})
	out := calibrateOnce(t, p)
	if out["status"] != "adapted" {
		t.Fatalf("status=%v want adapted", out["status"])
	}
	if downloads.Load() == 0 {
		t.Fatal("未真正下载官方包（测试桩未被命中）")
	}
	if latest, _, blocked := up.IdentityStatus(); blocked || latest != "" {
		t.Fatalf("关闭检查时校准不得写入快照/武装门禁: latest=%q blocked=%v", latest, blocked)
	}
}

// TestIdentityResetLatestEndpoint 人工重置快照端点（codex 二轮 P1-2 恢复出口）：
// npm yank 高版回退 dist-tag 后，单调快照锁死门禁；一键清空立即放行。
func TestIdentityResetLatestEndpoint(t *testing.T) {
	up := upstream.New()
	up.ChatIdentity = "cli"
	up.IdentityStrictGate.Store(true)
	up.IdentityVersionCheck.Store(true)
	up.ObserveLatest("9.9.9") // 模拟已被 yank 的更高版：门禁锁死中
	if blocked, _ := up.IdentityGateBlocked(); !blocked {
		t.Fatal("前置失败：应处于拦截态")
	}
	p := New(Config{Version: "test", APIKey: "k", Upstream: up})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/identity/reset-latest", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset-latest HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if latest, _, blocked := up.IdentityStatus(); blocked || latest != "" {
		t.Fatalf("重置后 latest=%q blocked=%v want 空/false", latest, blocked)
	}
}
