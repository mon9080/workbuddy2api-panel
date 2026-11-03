// identitycalib.go 面板「一键适配升级」的静态校准器。
//
// 背景：生产容器是 alpine 精简镜像（无 node），跑不了官方 CLI 的运行时 oracle。
// 本校准器在容器内做**静态指纹核对**：下载官方 npm tarball（仅解出
// dist/codebuddy.js 与 product.json 两个文件），把版本承载常量与关键指纹字面量
// 与实现期望逐一比对——全过则把 verified 版本热覆盖为新版（门禁即解），任一
// 不符拒绝并报漂移字段（fail-closed 不破坏）。运行时形态的深度验证仍由每日
// oracle Action 兜底。
package upstream

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// calibTimeout 下载 65MB tarball + 解包核对的总额上限。
const calibTimeout = 5 * time.Minute

// 下载安全上限（codex 二轮 P2：#5 无大小上限、#9 无源校验）。官方包约 65MB，
// 上限留足余量；异常/恶意元数据不得把容器磁盘写满。
// 用 var 而非 const：单测需缩小上限验证超限路径。
var (
	calibMaxTarballBytes      int64 = 200 << 20 // tarball 下载硬上限
	calibMaxEntryBytes        int64 = 64 << 20  // 解出的单文件上限（codebuddy.js 约 30MB）
	calibMaxDecompressedBytes int64 = 512 << 20 // 解压流总量上限（防 gzip 炸弹空耗 CPU）
)

// calibTarballHosts 官方 registry 域白名单（tarball 与元数据同源，且必须 https）。
// 自定义镜像（含测试 httptest）按「与 npm 元数据端点同源」放行，见 validateTarballURL。
var calibTarballHosts = map[string]bool{
	"registry.npmjs.org": true,
	"registry.npmjs.com": true,
}

// IdentityCalibResult 一次静态校准的结果（面板展示用）。
type IdentityCalibResult struct {
	Status string   `json:"status"`          // "current" | "adapted" | "drift"
	Latest string   `json:"latest"`          // npm latest
	OldCLI string   `json:"old_cli"`         // 校准前生效的 CLI 版本
	NewCLI string   `json:"new_cli"`         // 校准后生效的 CLI 版本
	NewSDK string   `json:"new_sdk"`         // 校准后生效的 SDK 版本
	Checks []string `json:"checks"`          // 通过的核对项
	Drift  []string `json:"drift,omitempty"` // 漂移项（Status=drift 时非空）

	// Persisted / PersistError 由面板层填写（校准器只核对、不发布）：
	// 落盘成功后才热生效；失败时 Status=persist_failed、运行时保持旧版。
	Persisted    bool   `json:"persisted"`
	PersistError string `json:"persist_error,omitempty"`
}

// validateTarballURL tarball 源校验（codex 二轮 #9）：仅允许官方 registry 域
// （必须 https），或与 npm 元数据端点**同源**的 host（自定义镜像 / 测试 httptest）。
// 校验发生在下载前与每次重定向处——元数据把 tarball 指向任意内网/外部地址时
// 直接拒绝，不发请求。
func validateTarballURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("tarball URL 非法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("tarball URL scheme 不合法: %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("tarball URL 缺少 host: %q", raw)
	}
	if calibTarballHosts[host] {
		if u.Scheme != "https" {
			return fmt.Errorf("官方 registry tarball 必须 https: %q", raw)
		}
		return nil
	}
	meta, err := url.Parse(npmLatestEndpoint)
	if err == nil && strings.EqualFold(meta.Hostname(), host) {
		return nil // 与元数据同源：元数据源可信则 tarball 源可信（含测试与自定义镜像）
	}
	return fmt.Errorf("tarball 源不合法（host=%s）：仅允许官方 registry 或与 npm 元数据同源", host)
}

// verifyIntegrity 校验 tarball 与 npm 元数据 dist.integrity（sha512-<base64>）。
// 元数据提供 integrity 时强制校验，不匹配即拒绝（fail-closed）。
func verifyIntegrity(sum []byte, integrity string) error {
	const prefix = "sha512-"
	integrity = strings.TrimSpace(integrity)
	if integrity == "" {
		return nil // 元数据未提供 integrity：源校验仍生效（见 validateTarballURL）
	}
	// npm 可能给空格分隔的多个摘要（如 "sha512-… sha1-…"）：取首个 sha512 条目；
	// 没有 sha512 时拒绝（不支持其它算法，不做静默跳过）。
	digest := ""
	for _, f := range strings.Fields(integrity) {
		if strings.HasPrefix(f, prefix) {
			digest = f
			break
		}
	}
	if digest == "" {
		return fmt.Errorf("integrity 不含 sha512 摘要: %q", integrity)
	}
	want := strings.TrimPrefix(digest, prefix)
	got := base64.StdEncoding.EncodeToString(sum)
	if got != want {
		return fmt.Errorf("tarball integrity 校验失败（期望 %s…，实际 %s…）", want[:min(12, len(want))], got[:12])
	}
	return nil
}

// readLimited 读取解包条目并强制大小上限（tar 头里的 Size 不可信，按实际读取量判）。
func readLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("解包文件超过大小上限 %d MiB", max>>20)
	}
	return b, nil
}

// fetchNPMDist URL 与版本一起取（/latest 端点带 dist.tarball 与 dist.integrity）。
func fetchNPMDist(ctx context.Context, hc *http.Client) (version, tarball, integrity string, err error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, npmLatestEndpoint, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("npm latest status %d", resp.StatusCode)
	}
	var payload struct {
		Version string `json:"version"`
		Dist    struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", "", "", err
	}
	v := strings.TrimSpace(payload.Version)
	if !plainVersionRe.MatchString(v) || payload.Dist.Tarball == "" {
		return "", "", "", fmt.Errorf("npm latest 元数据非法: version=%q", payload.Version)
	}
	if err := validateTarballURL(payload.Dist.Tarball); err != nil {
		return "", "", "", err
	}
	return v, payload.Dist.Tarball, strings.TrimSpace(payload.Dist.Integrity), nil
}

// distFingerprintLiterals 静态核对的指纹字面量清单——这些常量任何一个从官方
// dist 消失都意味着形态漂移（我们的实现按它们构造出站）。
var distFingerprintLiterals = []string{
	"X-Stainless-Package-Version",
	"X-Conversation-Request-ID",
	"X-CodeBuddy-Request",
	"codebuddy.session_id", // baggage 键
	"resolveCacheRetention",
	"cache_retention",
	"injectReasoningSummaryIfNeeded",
	"X-Private-Data",
	"RequestBodyGzip",
}

var reSDKVersion = regexp.MustCompile(`"X-Stainless-Package-Version":"([^"]+)"`)

// RunIdentityStaticCalib 执行静态校准。
//
// 本函数**只核对、不发布**：成功时把候选版本放进返回值（Status=adapted），
// 由调用方先持久化、再调 SetIdentityVerified 热生效（审查发现 #7：此前的
// 「先热生效后落盘」会让落盘失败时运行态与磁盘态分叉）。
func (c *Client) RunIdentityStaticCalib() (*IdentityCalibResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), calibTimeout)
	defer cancel()

	latest, tarball, integrity, err := fetchNPMDist(ctx, c.HTTP)
	if err != nil {
		return nil, fmt.Errorf("查询 npm latest 失败: %w", err)
	}
	// 拿到元数据即发布观测（审查发现 #3）：下面无论 drift / 解包失败 / 调用方
	// 落盘失败，门禁都能立即看到「官方已有新版」，不必等下一小时巡检。
	c.ObserveLatest(latest)
	old := c.EffectiveCLIVersion()
	res := &IdentityCalibResult{Latest: latest, OldCLI: old, NewCLI: old}
	if latest == old {
		res.Status = "current"
		return res, nil
	}

	tmp, err := os.MkdirTemp("", "identity-calib-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	distJS, productJSON, err := downloadAndExtract(ctx, c.HTTP, tarball, integrity, tmp)
	if err != nil {
		return nil, fmt.Errorf("下载/解包官方包失败: %w", err)
	}
	res.Checks = append(res.Checks, fmt.Sprintf("tarball 源校验通过（官方 registry/同源）+ 大小上限 %d MiB", calibMaxTarballBytes>>20))
	if integrity != "" {
		res.Checks = append(res.Checks, "tarball sha512 与 npm 元数据一致")
	} else {
		res.Checks = append(res.Checks, "上游未提供 dist.integrity（跳过哈希校验）")
	}

	// 核对 product.json 产品常量。
	var prod struct {
		ProductName     string `json:"productName"`
		Platform        string `json:"platform"`
		ProductFeatures struct {
			RequestBodyGzip bool `json:"RequestBodyGzip"`
		} `json:"productFeatures"`
	}
	if err := json.Unmarshal(productJSON, &prod); err != nil {
		return nil, fmt.Errorf("product.json 解析失败: %w", err)
	}
	if prod.ProductName != "CodeBuddy" {
		res.Drift = append(res.Drift, "product.json productName="+prod.ProductName+"（期望 CodeBuddy）")
	}
	if prod.Platform != "CLI" {
		res.Drift = append(res.Drift, "product.json platform="+prod.Platform+"（期望 CLI）")
	}
	if !prod.ProductFeatures.RequestBodyGzip {
		res.Drift = append(res.Drift, "productFeatures.RequestBodyGzip=false（实现按恒压缩出站）")
	}
	res.Checks = append(res.Checks, "product.json 产品常量（productName/platform/gzip）")

	// 核对 dist 指纹字面量与 SDK 版本。
	dist := string(distJS)
	for _, lit := range distFingerprintLiterals {
		if !strings.Contains(dist, lit) {
			res.Drift = append(res.Drift, "dist 缺少指纹常量 "+lit)
		}
	}
	res.Checks = append(res.Checks, fmt.Sprintf("dist 指纹字面量 ×%d", len(distFingerprintLiterals)))

	m := reSDKVersion.FindStringSubmatch(dist)
	if len(m) < 2 || !plainVersionRe.MatchString(m[1]) {
		if len(res.Drift) == 0 {
			res.Drift = append(res.Drift, "无法从 dist 提取合法 SDK 版本")
		}
	} else if len(res.Drift) == 0 {
		// 全部核对通过：只返回候选版本，**不写运行时**——调用方落盘成功后才调用
		// SetIdentityVerified 热生效（审查发现 #7）。
		res.NewCLI = latest
		res.NewSDK = m[1]
		res.Status = "adapted"
		res.Checks = append(res.Checks, "SDK 版本 "+m[1])
		return res, nil
	}
	res.Status = "drift"
	return res, nil
}

// downloadAndExtract 下载 tarball 并只解出两个目标文件（流式跳过其余条目）。
func downloadAndExtract(ctx context.Context, hc *http.Client, url, integrity, destDir string) (distJS, productJSON []byte, err error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	// 重定向逐跳校验（#9）：每一跳都必须通过源校验，防止元数据借 302 指向内网/外部。
	client := *hc
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("tarball 重定向超过 5 次")
		}
		return validateTarballURL(req.URL.String())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("tarball status %d", resp.StatusCode)
	}
	// 大小上限（#5）：Content-Length 先行拒绝；无 Content-Length 时按实际写入量兜底。
	if resp.ContentLength > calibMaxTarballBytes {
		return nil, nil, fmt.Errorf("tarball 超过大小上限 %d MiB（Content-Length=%d）", calibMaxTarballBytes>>20, resp.ContentLength)
	}
	f, err := os.Create(filepath.Join(destDir, "pkg.tgz"))
	if err != nil {
		return nil, nil, err
	}
	sum := sha512.New()
	written, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(resp.Body, calibMaxTarballBytes+1))
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if written > calibMaxTarballBytes {
		f.Close()
		return nil, nil, fmt.Errorf("tarball 超过大小上限 %d MiB", calibMaxTarballBytes>>20)
	}
	if err := verifyIntegrity(sum.Sum(nil), integrity); err != nil {
		f.Close()
		return nil, nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, err
	}
	defer zr.Close()
	// 解压总量上限：tar 对非目标条目会整段读掉（不落盘但耗 CPU），gzip 炸弹可借
	// 高分压缩比空耗解压；超限截断后 tar 必然报错 → 校准 fail-closed 失败。
	tr := tar.NewReader(io.LimitReader(zr, calibMaxDecompressedBytes))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		switch hdr.Name {
		case "package/dist/codebuddy.js":
			distJS, err = readLimited(tr, calibMaxEntryBytes)
		case "package/product.json":
			productJSON, err = readLimited(tr, calibMaxEntryBytes)
		default:
			continue
		}
		if err != nil {
			return nil, nil, err
		}
	}
	if distJS == nil || productJSON == nil {
		return nil, nil, fmt.Errorf("tarball 缺少 dist/codebuddy.js 或 product.json")
	}
	return distJS, productJSON, nil
}
