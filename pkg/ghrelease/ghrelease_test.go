package ghrelease

import (
	"context"
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
