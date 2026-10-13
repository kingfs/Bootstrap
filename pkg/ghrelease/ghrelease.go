// Package ghrelease 封装 GitHub Release 的少量约定：
// 最新 tag、资产下载地址、checksums.txt 解析。
//
// 与业务无关（可独立复用于任何"从 GitHub Release 装二进制"的场景），因此放在 pkg/。
package ghrelease

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/kingfs/Bootstrap/pkg/httpx"
)

// AssetBaseURL 是资产下载地址的公开前缀（便于测试替换）。
var AssetBaseURL = "https://github.com"

// APIBaseURL 是 GitHub API 前缀。
var APIBaseURL = "https://api.github.com"

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
	resp, err := c.HTTP.Get(AssetBaseURL + "/" + repo + "/releases/latest")
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
