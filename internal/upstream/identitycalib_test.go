// identitycalib_test.go 静态校准器单测：伪 npm 元数据 + 构造 tarball，
// 覆盖 已最新 / 适配成功（热覆盖 + 门禁解除）/ 漂移拒绝 三态。
package upstream

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
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

// tarballIntegrity 计算 npm 形态的 dist.integrity（sha512-<base64>）。
func tarballIntegrity(b []byte) string {
	sum := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// startFakeNPM 启动伪 npm：/latest 返回元数据（含 dist.tarball 与 dist.integrity），
// /pkg.tgz 返回构造包。
func startFakeNPM(t *testing.T, version string, tarball []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			json.NewEncoder(w).Encode(map[string]any{
				"version": version,
				"dist": map[string]string{
					"tarball":   srvBase(r) + "/pkg.tgz",
					"integrity": tarballIntegrity(tarball),
				},
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
	if _, _, err := downloadAndExtract(nil, srv.Client(), srv.URL, "", dir); err == nil {
		t.Fatal("缺少目标文件应报错")
	}
}

// extractCtx 模拟「HTTP 请求已完成、解包阶段才被取消」的场景：Done() 永不
// 触发（transport 不受影响），但 Err() 恒返回 Canceled——只有解包期间的读取
// 路径（budgetReader.Read）会看到取消，从而钉死「大条目跳过期间响应取消」。
type extractCtx struct{ context.Context }

func (extractCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (extractCtx) Done() <-chan struct{}       { c := make(chan struct{}); return c }
func (extractCtx) Err() error                  { return context.Canceled }
func (extractCtx) Value(any) any               { return nil }

// TestDownloadAndExtractRespectsContextCancel 解包期间必须响应 context 取消
// （codex 四审 P2）：大条目的跳过/读取都经由 budgetReader，取消后不得继续把
// 归档读到底——否则恶意/异常 tarball 可让校准长时间占用容器 CPU。
func TestDownloadAndExtractRespectsContextCancel(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	// 大型非目标条目：只有读穿它才会轮到目标文件
	big := strings.Repeat("0123456789", 200000) // ~2MB
	if err := tw.WriteHeader(&tar.Header{Name: "package/other.bin", Size: int64(len(big)), Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"package/dist/codebuddy.js": calibDistOK,
		"package/product.json":      calibProductOK,
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	if _, _, err := downloadAndExtract(extractCtx{context.Background()}, srv.Client(), srv.URL, "", dir); err == nil {
		t.Fatal("解包期间 context 已取消必须报错，而不是把大条目读到底")
	}
}

// TestIdentityCalibDownloadSecurity 下载安全（codex 二轮 #5/#9）：
// 源/重定向校验、大小上限、integrity 强制。任一失效都可能被异常或恶意
// npm 元数据利用（写满磁盘 / 指向内网 / 替换包内容）。
func TestIdentityCalibDownloadSecurity(t *testing.T) {
	tarball := buildFakeTarball(t, calibDistOK, calibProductOK)

	t.Run("integrity 不匹配-拒绝", func(t *testing.T) {
		wrong := "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, sha512.Size))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/latest":
				json.NewEncoder(w).Encode(map[string]any{
					"version": "9.9.9",
					"dist": map[string]string{
						"tarball":   srvBase(r) + "/pkg.tgz",
						"integrity": wrong,
					},
				})
			case "/pkg.tgz":
				w.Write(tarball)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()
		withFakeNPM(t, srv)
		if _, err := newCLIClient().RunIdentityStaticCalib(); err == nil || !strings.Contains(err.Error(), "integrity") {
			t.Fatalf("integrity 不匹配必须拒绝, err=%v", err)
		}
	})

	t.Run("tarball 源不合法-不发下载请求", func(t *testing.T) {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			json.NewEncoder(w).Encode(map[string]any{
				"version": "9.9.9",
				"dist":    map[string]string{"tarball": "https://evil.example/pkg.tgz"},
			})
		}))
		defer srv.Close()
		withFakeNPM(t, srv)
		if _, err := newCLIClient().RunIdentityStaticCalib(); err == nil || !strings.Contains(err.Error(), "源不合法") {
			t.Fatalf("非白名单源必须拒绝, err=%v", err)
		}
		if hits != 1 {
			t.Fatalf("只应请求元数据一次（不下载），实际 %d 次", hits)
		}
	})

	t.Run("重定向到异源-拒绝", func(t *testing.T) {
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(tarball)
		}))
		defer other.Close()
		// 元数据 host=127.0.0.1；重定向目标改成 localhost（host 名不同）→ 逐跳校验拒绝。
		otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/latest":
				json.NewEncoder(w).Encode(map[string]any{
					"version": "9.9.9",
					"dist":    map[string]string{"tarball": srvBase(r) + "/pkg.tgz"},
				})
			case "/pkg.tgz":
				http.Redirect(w, r, otherURL+"/pkg.tgz", http.StatusFound)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()
		withFakeNPM(t, srv)
		if _, err := newCLIClient().RunIdentityStaticCalib(); err == nil || !strings.Contains(err.Error(), "源不合法") {
			t.Fatalf("重定向到异源必须拒绝, err=%v", err)
		}
	})

	t.Run("超过大小上限-拒绝", func(t *testing.T) {
		old := calibMaxTarballBytes
		calibMaxTarballBytes = 64 // 构造包（gzip 后数百字节）必然超限
		t.Cleanup(func() { calibMaxTarballBytes = old })
		srv := startFakeNPM(t, "9.9.9", tarball)
		defer srv.Close()
		withFakeNPM(t, srv)
		if _, err := newCLIClient().RunIdentityStaticCalib(); err == nil || !strings.Contains(err.Error(), "大小上限") {
			t.Fatalf("超过大小上限必须拒绝, err=%v", err)
		}
	})

	// 三审 P2：元数据请求自身的重定向也必须受源策略约束（此前只校验 dist.tarball）。
	t.Run("元数据重定向到异源-拒绝", func(t *testing.T) {
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"version": "9.9.9"})
		}))
		defer other.Close()
		otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, otherURL+"/latest", http.StatusFound)
		}))
		defer srv.Close()
		withFakeNPM(t, srv)
		if _, err := newCLIClient().RunIdentityStaticCalib(); err == nil || !strings.Contains(err.Error(), "源不合法") {
			t.Fatalf("元数据重定向到异源必须拒绝, err=%v", err)
		}
	})

	// 三审 P2：预算耗尽必须显式报错，不能被 tar 当成正常结束（io.LimitReader 的 EOF 陷阱）。
	t.Run("解压预算耗尽-拒绝", func(t *testing.T) {
		old := calibMaxDecompressedBytes
		calibMaxDecompressedBytes = 32
		t.Cleanup(func() { calibMaxDecompressedBytes = old })
		srv := startFakeNPM(t, "9.9.9", tarball)
		defer srv.Close()
		withFakeNPM(t, srv)
		if _, err := newCLIClient().RunIdentityStaticCalib(); err == nil || !strings.Contains(err.Error(), "解压流") {
			t.Fatalf("解压预算耗尽必须拒绝, err=%v", err)
		}
	})

	// 三审 P2：integrity 可含多个摘要，任一 sha512 匹配即通过（不能只看首个）。
	t.Run("多摘要 integrity-任一 sha512 匹配即通过", func(t *testing.T) {
		correct := tarballIntegrity(tarball)
		cases := []string{
			"sha1-AAAAAAAA " + correct,                          // sha1 在前
			"sha512-" + base64.StdEncoding.EncodeToString(make([]byte, sha512.Size)) + " " + correct, // 错误 sha512 在前
		}
		for _, integrity := range cases {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/latest":
					json.NewEncoder(w).Encode(map[string]any{
						"version": "9.9.9",
						"dist": map[string]string{
							"tarball":   srvBase(r) + "/pkg.tgz",
							"integrity": integrity,
						},
					})
				case "/pkg.tgz":
					w.Write(tarball)
				default:
					http.NotFound(w, r)
				}
			}))
			withFakeNPM(t, srv)
			res, err := newCLIClient().RunIdentityStaticCalib()
			srv.Close()
			if err != nil || res.Status != "adapted" {
				t.Fatalf("integrity=%q 应通过, status=%v err=%v", integrity, res, err)
			}
		}
	})

	// 三审 P2：调用方已有的 CheckRedirect 必须被保留（组合而不是覆盖）。
	t.Run("保留调用方 CheckRedirect", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/latest":
				json.NewEncoder(w).Encode(map[string]any{
					"version": "9.9.9",
					"dist": map[string]string{
						"tarball":   srvBase(r) + "/pkg.tgz",
						"integrity": tarballIntegrity(tarball),
					},
				})
			case "/pkg.tgz":
				http.Redirect(w, r, srvBase(r)+"/pkg-real.tgz", http.StatusFound)
			default:
				w.Write(tarball)
			}
		}))
		defer srv.Close()
		withFakeNPM(t, srv)
		c := newCLIClient()
		c.HTTP = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errors.New("blocked by caller policy")
		}}
		if _, err := c.RunIdentityStaticCalib(); err == nil || !strings.Contains(err.Error(), "blocked by caller policy") {
			t.Fatalf("调用方 CheckRedirect 必须生效, err=%v", err)
		}
	})
}
