// Package ghrelease 封装 GitHub Release 的少量约定：
// 最新 tag、资产下载地址、checksums.txt 解析。
//
// 与业务无关（可独立复用于任何"从 GitHub Release 装二进制"的场景），因此放在 pkg/。
package ghrelease

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/kingfs/Bootstrap/pkg/httpx"
)

// 默认前缀。调用方（含测试）想还原时用这两个常量，别自己拼字符串。
const (
	// DefaultAssetBaseURL 是资产下载地址的默认前缀。
	DefaultAssetBaseURL = "https://github.com"
	// DefaultAPIBaseURL 是 GitHub API 的默认前缀。
	DefaultAPIBaseURL = "https://api.github.com"
)

// AssetBaseURL 是资产下载地址的前缀（便于测试与镜像替换）。
var AssetBaseURL = DefaultAssetBaseURL

// APIBaseURL 是 GitHub API 前缀。
var APIBaseURL = DefaultAPIBaseURL

// SetBaseURLs 覆盖两个前缀；空串（或纯空白）表示**用回默认前缀**。
//
// "空串回到默认"而不是"空串保持不动"：这个函数是幂等的，反复调用（比如配置重载）
// 不会把上一次的镜像残留下来——少了状态，也就少了"为什么这次还在走镜像"的排查。
//
// 存在的理由：GitHub 在很多网络环境下不可用或很慢，而"从 Release 装东西"这件事只认
// GitHub。调用方可以把前缀指向镜像/加速站（本仓库是从 BS_GITHUB_API /
// BS_GITHUB_ASSET_BASE 环境变量读进来，见 internal/app/applyGitHubMirror）。
//
// 注意信任边界：**校验和与产物都来自同一个前缀**，所以镜像的信任级别等于 GitHub 本身
// （它完全可以同时伪造产物和 checksums.txt）。这一点写在 docs/design.md §16。
// 本包不读环境变量——环境变量属于项目约定，不该出现在可复用的 pkg 里。
func SetBaseURLs(api, assets string) {
	APIBaseURL = normalizeBase(api, DefaultAPIBaseURL)
	AssetBaseURL = normalizeBase(assets, DefaultAssetBaseURL)
}

func normalizeBase(v, def string) string {
	if s := strings.TrimRight(strings.TrimSpace(v), "/"); s != "" {
		return s
	}
	return def
}

// LatestTag 查询仓库最新 release 的 tag（如 `v0.3.0`）。
//
// 先用 API；API 被限流（403/429）时退化为跟随 /releases/latest 的重定向，
// 这样在没配 token 的机器上也能工作。
func LatestTag(ctx context.Context, c *httpx.Client, repo string) (string, error) {
	repo = strings.Trim(repo, "/")
	if repo == "" {
		return "", fmt.Errorf("empty repo")
	}
	data, err := c.GetBytes(ctx, APIBaseURL+"/repos/"+repo+"/releases/latest")
	if err == nil {
		var payload struct {
			TagName string `json:"tag_name"`
		}
		if jerr := json.Unmarshal(data, &payload); jerr == nil && payload.TagName != "" {
			return payload.TagName, nil
		}
	}
	// API 失败（限流 / 未认证配额用尽）时退化为跟随重定向。
	if tag, terr := latestTagViaRedirect(ctx, c, repo); terr == nil {
		return tag, nil
	}
	if err == nil {
		err = fmt.Errorf("response did not contain tag_name")
	}
	return "", fmt.Errorf("resolve latest release of %s: %w", repo, err)
}

func latestTagViaRedirect(ctx context.Context, c *httpx.Client, repo string) (string, error) {
	// 不依赖 API 的兜底：/releases/latest 会 302 到 /releases/tag/<tag>。
	// 用 GetCtx 而不是 http.Client.Get：后者不带 context，请求无法被取消。
	resp, err := c.GetCtx(ctx, AssetBaseURL+"/"+repo+"/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	final := resp.Request.URL.String()
	if i := strings.Index(final, "/tag/"); i >= 0 {
		return final[i+len("/tag/"):], nil
	}
	if resp.StatusCode/100 != 3 && resp.StatusCode != 200 {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return "", fmt.Errorf("could not determine latest tag from %s", final)
}

// ReleaseAPIURL 拼出某个 tag 的 Release API 地址。
//
// tag 里的 `/`（kustomize 那种 `kustomize/v5.8.3`）必须转义成 %2F：
// 不转义会被当成路径分隔符，请求打到不存在的路由上。
func ReleaseAPIURL(repo, tag string) string {
	return fmt.Sprintf("%s/repos/%s/releases/tags/%s",
		strings.TrimRight(APIBaseURL, "/"), strings.Trim(repo, "/"), url.PathEscape(tag))
}

// AssetDigest 查询某个 release 资产的 sha256（GitHub API 的 digest 字段）。
//
// 2025 年起 GitHub 给**不可变 release**（immutable release）的每个资产附带
// `digest: sha256:<hex>`。像 herdr 这种只发裸二进制、不发任何校验和文件的仓库，
// 这是唯一可信的校验和来源。ok=false 表示"这份响应里没有可用的 sha256"
// （该资产不存在 / release 不是不可变的 / 算法不是 sha256），它**不是**错误——
// 调用方应当把它当成"这个来源没有"，继续按"拿不到校验和就中止"处理。
func AssetDigest(ctx context.Context, c *httpx.Client, repo, tag, asset string) (string, bool, error) {
	data, err := c.GetBytes(ctx, ReleaseAPIURL(repo, tag))
	if err != nil {
		return "", false, err
	}
	sha, ok := ParseAssetDigest(string(data), asset)
	return sha, ok, nil
}

// ParseAssetDigest 从 Release API 的 JSON 里取出某个资产的 sha256。
//
// 资产名必须**逐字相等**：这里不做模糊匹配——取错资产的哈希去校验另一个文件，
// 只会得到一次莫名其妙的 checksum mismatch。
func ParseAssetDigest(releaseJSON, asset string) (string, bool) {
	var payload struct {
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.Unmarshal([]byte(releaseJSON), &payload); err != nil {
		return "", false
	}
	want := strings.TrimPrefix(strings.TrimSpace(asset), "./")
	for _, a := range payload.Assets {
		if a.Name != want {
			continue
		}
		algo, hex, found := strings.Cut(a.Digest, ":")
		if !found || !strings.EqualFold(strings.TrimSpace(algo), "sha256") {
			return "", false
		}
		hex = strings.ToLower(strings.TrimSpace(hex))
		if !bareSHA256.MatchString(hex) {
			return "", false
		}
		return hex, true
	}
	return "", false
}

// AssetURL 拼出某个 tag 下资产的下载地址。
func AssetURL(repo, tag, asset string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", strings.TrimRight(AssetBaseURL, "/"), strings.Trim(repo, "/"), tag, asset)
}

// ChecksumsURL 拼出 checksums.txt 的地址。
func ChecksumsURL(repo, tag string) string {
	return AssetURL(repo, tag, "checksums.txt")
}

var checksumLine = regexp.MustCompile(`^([0-9a-fA-F]{64})\s+\*?(.+)$`)

// ChecksumFileNames 是 GitHub Release 里校验和文件最常见的几种叫法。
//
// 实测：goreleaser 默认写 checksums.txt；voidint/g 用 sha256sum.txt；
// k9s 用 checksums.sha256；Rust 生态（uv/ruff/ripgrep）干脆每个产物配一个
// `<asset>.sha256`。逐个试比强制清单作者记住某个项目的习惯更可靠。
var ChecksumFileNames = []string{"checksums.txt", "sha256sum.txt", "checksums.sha256", "SHA256SUMS"}

// BareChecksum 从"只有一个哈希"的文件里取 sha256（`<asset>.sha256` 的格式）。
func BareChecksum(text string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := checksumLine.FindStringSubmatch(line); m != nil {
			return strings.ToLower(m[1]), true
		}
		if bareSHA256.MatchString(line) {
			return strings.ToLower(line), true
		}
	}
	return "", false
}

var bareSHA256 = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// ChecksumFor 从 checksums.txt 内容里取出某个文件的 sha256。
//
// 兼容两种格式：`<hash>  <name>`（sha256sum）与 `<hash> *<name>`（二进制模式）。
func ChecksumFor(text, asset string) (string, error) {
	want := strings.TrimPrefix(strings.TrimSpace(asset), "./")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := checksumLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := strings.TrimPrefix(strings.TrimSpace(m[2]), "./")
		if name == want || strings.HasSuffix(name, "/"+want) {
			return strings.ToLower(m[1]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", asset)
}
