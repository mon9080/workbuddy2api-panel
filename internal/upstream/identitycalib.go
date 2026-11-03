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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// calibTimeout 下载 65MB tarball + 解包核对的总额上限。
const calibTimeout = 5 * time.Minute

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

// npmDist URL 与版本一起取（/latest 端点带 dist.tarball）。
func fetchNPMDist(ctx context.Context, hc *http.Client) (version, tarball string, err error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, npmLatestEndpoint, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("npm latest status %d", resp.StatusCode)
	}
	var payload struct {
		Version string `json:"version"`
		Dist    struct {
			Tarball string `json:"tarball"`
		} `json:"dist"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", "", err
	}
	v := strings.TrimSpace(payload.Version)
	if !plainVersionRe.MatchString(v) || payload.Dist.Tarball == "" {
		return "", "", fmt.Errorf("npm latest 元数据非法: version=%q", payload.Version)
	}
	return v, payload.Dist.Tarball, nil
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

	latest, tarball, err := fetchNPMDist(ctx, c.HTTP)
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

	distJS, productJSON, err := downloadAndExtract(ctx, c.HTTP, tarball, tmp)
	if err != nil {
		return nil, fmt.Errorf("下载/解包官方包失败: %w", err)
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
func downloadAndExtract(ctx context.Context, hc *http.Client, url, destDir string) (distJS, productJSON []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("tarball status %d", resp.StatusCode)
	}
	f, err := os.Create(filepath.Join(destDir, "pkg.tgz"))
	if err != nil {
		return nil, nil, err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
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
	tr := tar.NewReader(zr)
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
			distJS, err = io.ReadAll(tr)
		case "package/product.json":
			productJSON, err = io.ReadAll(tr)
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
