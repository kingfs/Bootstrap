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
