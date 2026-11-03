// identitycalib_test.go 静态校准器单测：伪 npm 元数据 + 构造 tarball，
// 覆盖 已最新 / 适配成功（热覆盖 + 门禁解除）/ 漂移拒绝 三态。
package upstream

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// buildFakeTarball 构造只含 dist/codebuddy.js 与 product.json 的官方形态 tarball。
func buildFakeTarball(t *testing.T, distJS, productJSON string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, content := range map[string]string{
		"package/dist/codebuddy.js": distJS,
		"package/product.json":      productJSON,
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

const calibDistOK = `var a=1;function resolveCacheRetention(){return "long"}var cache_retention="long";
function injectReasoningSummaryIfNeeded(){}var X="X-Stainless-Package-Version":"7.40.0";
var h={"X-Conversation-Request-ID":1,"X-CodeBuddy-Request":1,"X-Private-Data":1};
var baggage="codebuddy.session_id=x";var feat="RequestBodyGzip";`

const calibProductOK = `{"productName":"CodeBuddy","platform":"CLI","productFeatures":{"RequestBodyGzip":true}}`

// startFakeNPM 启动伪 npm：/latest 返回元数据（含 dist.tarball），/pkg.tgz 返回构造包。
func startFakeNPM(t *testing.T, version string, tarball []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			json.NewEncoder(w).Encode(map[string]any{
				"version": version,
				"dist":    map[string]string{"tarball": srvBase(r) + "/pkg.tgz"},
			})
		case "/pkg.tgz":
			w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

// srvBase 从请求反推服务器基址（测试服务器 URL 在 handler 内可得的简便法）。
func srvBase(r *http.Request) string {
	return "http://" + r.Host
}

func withFakeNPM(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := npmLatestEndpoint
	npmLatestEndpoint = srv.URL + "/latest"
	t.Cleanup(func() { npmLatestEndpoint = old })
}

func TestRunIdentityStaticCalib(t *testing.T) {
	// 官方 dist 关键字面量必须与 identitycalib.go 的核对清单同步（防清单自身烂掉）。
	for _, lit := range distFingerprintLiterals {
		if !strings.Contains(calibDistOK, lit) {
			t.Fatalf("测试夹具 dist 缺少核对清单字面量 %s（夹具与清单需同步）", lit)
		}
	}

	t.Run("已是最新", func(t *testing.T) {
		srv := startFakeNPM(t, VerifiedCLIVersion, buildFakeTarball(t, calibDistOK, calibProductOK))
		defer srv.Close()
		withFakeNPM(t, srv)
		c := newCLIClient()
		res, err := c.RunIdentityStaticCalib()
		if err != nil || res.Status != "current" {
			t.Fatalf("status=%s err=%v", res.Status, err)
		}
	})

	t.Run("适配成功-只返回候选不发布", func(t *testing.T) {
		srv := startFakeNPM(t, "9.9.9", buildFakeTarball(t, calibDistOK, calibProductOK))
		defer srv.Close()
		withFakeNPM(t, srv)
		c := newCLIClient()
		c.IdentityStrictGate.Store(true)
		setLatestForTest(c, "9.9.9") // 模拟后台已探测到新版（门禁拦截中）
		if blocked, _ := c.IdentityGateBlocked(); !blocked {
			t.Fatal("前置失败：门禁应处于拦截态")
		}
		res, err := c.RunIdentityStaticCalib()
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "adapted" || res.NewCLI != "9.9.9" || res.NewSDK != "7.40.0" {
			t.Fatalf("status=%s new=%s/%s", res.Status, res.NewCLI, res.NewSDK)
		}
		// 审查发现 #7：校准器**只核对不发布**——落盘前运行态必须保持旧版。
		if got := c.EffectiveCLIVersion(); got != VerifiedCLIVersion {
			t.Errorf("校准器不得自行热覆盖：EffectiveCLI=%s want %s", got, VerifiedCLIVersion)
		}
		if blocked, _ := c.IdentityGateBlocked(); !blocked {
			t.Error("落盘前门禁必须保持拦截")
		}
		// 调用方（面板层）落盘成功后热生效：UA/SDK 切换、门禁解除。
		c.SetIdentityVerified(res.NewCLI, res.NewSDK)
		if got := c.cliUserAgent(); !strings.Contains(got, "CLI/9.9.9 CodeBuddy/9.9.9") {
			t.Errorf("热生效后 UA 未切换: %s", got)
		}
		if got := c.EffectiveSDKVersion(); got != "7.40.0" {
			t.Errorf("热生效后 SDK 版本未切换: %s", got)
		}
		if blocked, _ := c.IdentityGateBlocked(); blocked {
			t.Error("热生效后门禁应解除")
		}
		// 还原覆盖：回落编译常量。
		c.SetIdentityVerified("", "")
		if c.EffectiveCLIVersion() != VerifiedCLIVersion {
			t.Error("清空覆盖应回落编译常量")
		}
	})

	t.Run("校准发现新版即发布快照（漂移也保留）", func(t *testing.T) {
		// 审查发现 #3：后台轮询尚未看到 9.9.9，校准拿到元数据后即使 drift，
		// 门禁也必须立刻看到新版，而不是等下一小时巡检。
		driftDist := strings.Replace(calibDistOK, "codebuddy.session_id", "other.session_key", 1)
		srv := startFakeNPM(t, "9.9.9", buildFakeTarball(t, driftDist, calibProductOK))
		defer srv.Close()
		withFakeNPM(t, srv)
		c := newCLIClient()
		c.IdentityStrictGate.Store(true)
		setLatestForTest(c, "") // 模拟轮询尚未探测到（latest 未知 → 门禁放行）
		if blocked, _ := c.IdentityGateBlocked(); blocked {
			t.Fatal("前置失败：latest 未知时不应拦截")
		}
		res, err := c.RunIdentityStaticCalib()
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "drift" {
			t.Fatalf("status=%s want drift", res.Status)
		}
		if blocked, _ := c.IdentityGateBlocked(); !blocked {
			t.Error("校准观测到新版后门禁必须立即拦截（无需等轮询）")
		}
	})

	t.Run("校准观测到新版后坏包也保留快照", func(t *testing.T) {
		srv := startFakeNPM(t, "9.9.9", []byte("not a tarball"))
		defer srv.Close()
		withFakeNPM(t, srv)
		c := newCLIClient()
		c.IdentityStrictGate.Store(true)
		setLatestForTest(c, "")
		if _, err := c.RunIdentityStaticCalib(); err == nil {
			t.Fatal("坏 tarball 应报错")
		}
		if blocked, _ := c.IdentityGateBlocked(); !blocked {
			t.Error("下载失败不得丢失已观测到的新版（否则门禁窗口被打开）")
		}
	})

	t.Run("漂移拒绝-不覆盖不解除", func(t *testing.T) {
		driftDist := strings.Replace(calibDistOK, "codebuddy.session_id", "other.session_key", 1)
		srv := startFakeNPM(t, "9.9.9", buildFakeTarball(t, driftDist, calibProductOK))
		defer srv.Close()
		withFakeNPM(t, srv)
		c := newCLIClient()
		c.IdentityStrictGate.Store(true)
		setLatestForTest(c, "9.9.9")
		res, err := c.RunIdentityStaticCalib()
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "drift" || len(res.Drift) == 0 {
			t.Fatalf("status=%s drift=%v", res.Status, res.Drift)
		}
		if c.EffectiveCLIVersion() != VerifiedCLIVersion {
			t.Error("漂移时不得覆盖版本")
		}
		if blocked, _ := c.IdentityGateBlocked(); !blocked {
			t.Error("漂移时门禁必须保持拦截（fail-closed）")
		}
	})

	t.Run("坏包报错", func(t *testing.T) {
		srv := startFakeNPM(t, "9.9.9", []byte("not a tarball"))
		defer srv.Close()
		withFakeNPM(t, srv)
		c := newCLIClient()
		if _, err := c.RunIdentityStaticCalib(); err == nil {
			t.Fatal("坏 tarball 应报错")
		}
	})
}

// TestDownloadAndExtractMissingEntries tarball 缺目标文件时报错（官方结构变化的哨兵）。
func TestDownloadAndExtractMissingEntries(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "package/other.txt", Size: 3, Mode: 0o644})
	tw.Write([]byte("abc"))
	tw.Close()
	zw.Close()
	if err := os.WriteFile(dir+"/x.tgz", buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer srv.Close()
	if _, _, err := downloadAndExtract(nil, srv.Client(), srv.URL, dir); err == nil {
		t.Fatal("缺少目标文件应报错")
	}
}
