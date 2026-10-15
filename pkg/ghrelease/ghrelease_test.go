package ghrelease

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kingfs/Bootstrap/pkg/httpx"
)

func TestAssetAndChecksumURL(t *testing.T) {
	if got := AssetURL("voidint/g", "v1.5.0", "g1.5.0-linux.tar.gz"); got != "https://github.com/voidint/g/releases/download/v1.5.0/g1.5.0-linux.tar.gz" {
		t.Errorf("AssetURL = %q", got)
	}
	if got := ChecksumsURL("voidint/g", "v1.5.0"); !strings.HasSuffix(got, "/checksums.txt") {
		t.Errorf("ChecksumsURL = %q", got)
	}
}

// TestSetBaseURLs：空值保持不动、末尾斜杠去掉、地址真的被拼出来。
func TestSetBaseURLs(t *testing.T) {
	oldAPI, oldAsset := APIBaseURL, AssetBaseURL
	t.Cleanup(func() { APIBaseURL, AssetBaseURL = oldAPI, oldAsset })

	APIBaseURL, AssetBaseURL = "https://stale.example.com", "https://stale.example.com"
	SetBaseURLs("", "")
	if APIBaseURL != DefaultAPIBaseURL || AssetBaseURL != DefaultAssetBaseURL {
		t.Fatalf("空值应当用回默认前缀（不能留上次的镜像）：%q / %q", APIBaseURL, AssetBaseURL)
	}

	SetBaseURLs("https://mirror.example.com/api/", "  https://mirror.example.com/gh/  ")
	if APIBaseURL != "https://mirror.example.com/api" {
		t.Errorf("API 前缀 = %q", APIBaseURL)
	}
	if AssetBaseURL != "https://mirror.example.com/gh" {
		t.Errorf("资产前缀 = %q", AssetBaseURL)
	}
	if got, want := AssetURL("o/r", "v1", "a.tar.gz"), "https://mirror.example.com/gh/o/r/releases/download/v1/a.tar.gz"; got != want {
		t.Errorf("AssetURL = %q，期望 %q", got, want)
	}
	if got, want := ChecksumsURL("o/r", "v1"), "https://mirror.example.com/gh/o/r/releases/download/v1/checksums.txt"; got != want {
		t.Errorf("ChecksumsURL = %q，期望 %q", got, want)
	}
}

func TestChecksumFor(t *testing.T) {
	text := `
# comment
d1b2c3d4e5f60718293a4b5c6d7e8f9012345678abcdef0123456789abcdef01  g1.5.0-linux_amd64.tar.gz
0000000000000000000000000000000000000000000000000000000000000000 *bs_windows_amd64.zip
`
	got, err := ChecksumFor(text, "g1.5.0-linux_amd64.tar.gz")
	if err != nil || got != "d1b2c3d4e5f60718293a4b5c6d7e8f9012345678abcdef0123456789abcdef01" {
		t.Fatalf("ChecksumFor = %q, %v", got, err)
	}
	got, err = ChecksumFor(text, "./bs_windows_amd64.zip")
	if err != nil || !strings.HasPrefix(got, "0000") {
		t.Fatalf("binary-mode line = %q, %v", got, err)
	}
	if _, err := ChecksumFor(text, "missing.tgz"); err == nil {
		t.Error("missing entry must fail")
	}
	if _, err := ChecksumFor("garbage\n", "x"); err == nil {
		t.Error("garbage must fail")
	}
}

func TestLatestTagFromAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/voidint/g/releases/latest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"tag_name":"v1.5.0"}`))
	}))
	defer srv.Close()

	oldAPI := APIBaseURL
	APIBaseURL = srv.URL
	defer func() { APIBaseURL = oldAPI }()

	c := httpx.New(5*time.Second, nil)
	tag, err := LatestTag(context.Background(), c, "voidint/g")
	if err != nil || tag != "v1.5.0" {
		t.Fatalf("LatestTag = %q, %v", tag, err)
	}
	if _, err := LatestTag(context.Background(), c, ""); err == nil {
		t.Error("empty repo must fail")
	}
}

// TestLatestTagFallsBackToRedirect 覆盖 API 被限流时走 /releases/latest 重定向。
func TestLatestTagFallsBackToRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/voidint/g/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/voidint/g/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/voidint/g/releases/tag/v9.9.9", http.StatusFound)
	})
	mux.HandleFunc("/voidint/g/releases/tag/v9.9.9", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("release page"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldAPI, oldAsset := APIBaseURL, AssetBaseURL
	APIBaseURL, AssetBaseURL = srv.URL, srv.URL
	defer func() { APIBaseURL, AssetBaseURL = oldAPI, oldAsset }()

	c := httpx.New(5*time.Second, nil)
	c.Retries = 0
	tag, err := LatestTag(context.Background(), c, "voidint/g")
	if err != nil || tag != "v9.9.9" {
		t.Fatalf("LatestTag = %q, %v", tag, err)
	}
}

// TestReleaseAPIURL：tag 里的 `/`（kustomize 那种 `kustomize/v5.8.3`）必须转义成 %2F，
// 否则会被当成路径分隔符、请求打到不存在的路由上。
func TestReleaseAPIURL(t *testing.T) {
	old := APIBaseURL
	APIBaseURL = "https://api.github.com/"
	defer func() { APIBaseURL = old }()

	if got, want := ReleaseAPIURL("herdrdev/herdr", "v0.9.5"), "https://api.github.com/repos/herdrdev/herdr/releases/tags/v0.9.5"; got != want {
		t.Errorf("ReleaseAPIURL = %q，期望 %q", got, want)
	}
	if got, want := ReleaseAPIURL("/o/r/", "kustomize/v5.8.3"), "https://api.github.com/repos/o/r/releases/tags/kustomize%2Fv5.8.3"; got != want {
		t.Errorf("带斜杠的 tag = %q，期望 %q", got, want)
	}
}

// TestParseAssetDigest：不可变 release 的 digest 解析，以及各种"没有"的形态。
//
// 关键约定：**资产名逐字相等**，哈希小写归一；算法不对/长度不对一律当作"没有"，
// 宁可让上层报"找不到校验和"，也不能拿一个来路不明的哈希去校验。
func TestParseAssetDigest(t *testing.T) {
	const want = "17d7536f2da50859523680ef279dffcac8e4a235f4e46fe8da6c2ec44141897c"
	good := `{"tag_name":"v0.9.5","assets":[` +
		`{"name":"other","digest":"sha256:1111"},` +
		`{"name":"herdr-linux-aarch64","digest":"sha256:` + strings.ToUpper(want) + `"}]}`

	if sha, ok := ParseAssetDigest(good, "herdr-linux-aarch64"); !ok || sha != want {
		t.Errorf("ParseAssetDigest = %q, %v，期望 %q, true", sha, ok, want)
	}
	tests := []struct{ name, body, asset string }{
		{"资产不存在", good, "missing"},
		{"没有 digest 字段（非不可变 release）", `{"assets":[{"name":"a"}]}`, "a"},
		{"算法不是 sha256", `{"assets":[{"name":"a","digest":"sha512:abcd"}]}`, "a"},
		{"哈希长度不对", `{"assets":[{"name":"a","digest":"sha256:abcd"}]}`, "a"},
		{"不是 JSON", `<!doctype html>`, "a"},
	}
	for _, tc := range tests {
		if sha, ok := ParseAssetDigest(tc.body, tc.asset); ok {
			t.Errorf("%s：不该解析出校验和，得到 %q", tc.name, sha)
		}
	}
}

// TestAssetDigestViaAPI：URL 拼装 + 真实 HTTP + 解析接得上（httptest 冒充 API）。
func TestAssetDigestViaAPI(t *testing.T) {
	const want = "17d7536f2da50859523680ef279dffcac8e4a235f4e46fe8da6c2ec44141897c"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/herdrdev/herdr/releases/tags/v0.9.5" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"assets":[{"name":"herdr-linux-aarch64","digest":"sha256:%s"}]}`, want)
	}))
	defer srv.Close()

	oldAPI := APIBaseURL
	APIBaseURL = srv.URL
	defer func() { APIBaseURL = oldAPI }()

	c := httpx.New(5*time.Second, nil)
	sha, ok, err := AssetDigest(context.Background(), c, "herdrdev/herdr", "v0.9.5", "herdr-linux-aarch64")
	if err != nil || !ok || sha != want {
		t.Fatalf("AssetDigest = %q, %v, %v", sha, ok, err)
	}
	// 资产名对不上时是"没有"，不是错误（上层据此继续走"拿不到校验和就中止"）。
	if _, ok, err := AssetDigest(context.Background(), c, "herdrdev/herdr", "v0.9.5", "nope"); err != nil || ok {
		t.Fatalf("未知资产应当返回 ok=false / err=nil，得到 ok=%v err=%v", ok, err)
	}
	// API 不通时必须是错误：不能把"取不到"说成"上游没发校验和"。
	if _, _, err := AssetDigest(context.Background(), c, "herdrdev/herdr", "v0.0.1", "x"); err == nil {
		t.Error("404 应当是错误")
	}
}
