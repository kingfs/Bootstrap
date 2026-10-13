package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestProxyFor(t *testing.T) {
	full := map[string]string{
		"HTTP_PROXY":  "http://127.0.0.1:7890",
		"HTTPS_PROXY": "http://127.0.0.1:7891",
		"ALL_PROXY":   "socks5://127.0.0.1:1080",
		"NO_PROXY":    "127.0.0.1,localhost,10.2.0.0/16,192.168.0.0/16,chaitin.net,chaitin.cn,.chaitin.net,.chaitin.cn,.baizhi.cloud,::1,[::1]",
	}
	tests := []struct {
		name   string
		env    map[string]string
		target string
		want   string
	}{
		{"https goes through https proxy", full, "https://github.com/x", "http://127.0.0.1:7891"},
		{"http goes through http proxy", full, "http://example.com/x", "http://127.0.0.1:7890"},
		{"no_proxy plain host", full, "https://chaitin.net/x", ""},
		{"no_proxy dot suffix", full, "https://git.chaitin.net/x", ""},
		{"no_proxy bare suffix", full, "https://git.chaitin.cn/x", ""},
		{"no_proxy other domain", full, "https://baizhi.cloud/x", ""},
		{"no_proxy ipv4", full, "https://127.0.0.1:8080/x", ""},
		{"no_proxy cidr", full, "https://10.2.3.4/x", ""},
		{"cidr outside range", full, "https://11.2.3.4/x", "http://127.0.0.1:7891"},
		{"all_proxy fallback", map[string]string{"ALL_PROXY": "socks5://127.0.0.1:1080"}, "https://x.com", "socks5://127.0.0.1:1080"},
		{"lowercase vars", map[string]string{"https_proxy": "http://127.0.0.1:7892"}, "https://x.com", "http://127.0.0.1:7892"},
		{"scheme-less proxy value", map[string]string{"HTTPS_PROXY": "127.0.0.1:7890"}, "https://x.com", "http://127.0.0.1:7890"},
		{"none configured", map[string]string{}, "https://x.com", ""},
		{"star means everything", map[string]string{"HTTPS_PROXY": "http://p:1", "NO_PROXY": "*"}, "https://x.com", ""},
		{"host:port entry", map[string]string{"HTTPS_PROXY": "http://p:1", "NO_PROXY": "example.com:8443"}, "https://example.com:8443/x", ""},
		{"host:port mismatch", map[string]string{"HTTPS_PROXY": "http://p:1", "NO_PROXY": "example.com:8443"}, "https://example.com:9443/x", "http://p:1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.target)
			if err != nil {
				t.Fatalf("parse target: %v", err)
			}
			got, err := ProxyFor(env(tc.env), u)
			if err != nil {
				t.Fatalf("ProxyFor: %v", err)
			}
			if tc.want == "" {
				if got != nil {
					t.Fatalf("proxy = %v, want none", got)
				}
				return
			}
			if got == nil || got.String() != tc.want {
				t.Fatalf("proxy = %v, want %s", got, tc.want)
			}
		})
	}

	if _, err := ProxyFor(env(map[string]string{"HTTPS_PROXY": "ftp://x"}), mustURL(t, "https://a.com")); err == nil {
		t.Error("unsupported proxy scheme must fail")
	}
	if got, err := ProxyFor(env(nil), nil); err != nil || got != nil {
		t.Errorf("nil target: %v, %v", got, err)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return u
}

func TestGetBytesAndStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("User-Agent must be set")
		}
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("hello"))
		case "/nope":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("gone"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := New(5*time.Second, nil)
	c.Retries = 0
	got, err := c.GetString(context.Background(), srv.URL+"/ok")
	if err != nil || got != "hello" {
		t.Fatalf("GetString = %q, %v", got, err)
	}
	_, err = c.GetBytes(context.Background(), srv.URL+"/nope")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want StatusError 404", err)
	}
	if !strings.Contains(se.Error(), "404") {
		t.Errorf("error text = %q", se.Error())
	}
	// 5xx 会重试，最终仍失败
	c.Retries = 1
	if _, err := c.GetBytes(context.Background(), srv.URL+"/boom"); err == nil {
		t.Error("5xx must fail")
	}
}

func TestDownloadVerifiesChecksum(t *testing.T) {
	payload := []byte("bs binary payload")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := New(5*time.Second, nil)
	dest := filepath.Join(dir, "sub", "bs")

	// 校验通过
	path, err := c.Download(context.Background(), srv.URL+"/bs", dest, hex.EncodeToString(sum[:]), 0o755)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if path != dest {
		t.Errorf("path = %q", path)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != string(payload) {
		t.Fatalf("content = %q, %v", data, err)
	}
	info, _ := os.Stat(dest)
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	// 大小写不敏感
	if _, err := c.Download(context.Background(), srv.URL+"/bs", dest, strings.ToUpper(hex.EncodeToString(sum[:])), 0o644); err != nil {
		t.Errorf("uppercase checksum should match: %v", err)
	}

	// 校验失败：不留文件、旧文件不被破坏
	wrong := strings.Repeat("0", 64)
	if _, err := c.Download(context.Background(), srv.URL+"/bs", filepath.Join(dir, "bad"), wrong, 0o644); err == nil {
		t.Fatal("checksum mismatch must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "bad")); !os.IsNotExist(err) {
		t.Error("failed download must not leave a file behind")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bs-download-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	if _, err := c.Download(context.Background(), srv.URL+"/bs", "", "", 0o644); err == nil {
		t.Error("empty destination must fail")
	}
}

// TestNewUsesPerConnectionDialTimeout：单条连接的预算必须小于整体预算，
// 否则第一次尝试卡到超时就吃光全部时间，Retries 形同虚设。
func TestNewUsesPerConnectionDialTimeout(t *testing.T) {
	c := New(DefaultTimeout, nil)
	tr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", c.HTTP.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext 未设置：连接阶段会一直等到整体超时")
	}
	if dialTimeout <= 0 || dialTimeout >= DefaultTimeout {
		t.Errorf("dialTimeout = %s，应当 >0 且 < %s", dialTimeout, DefaultTimeout)
	}
	if tr.Proxy == nil {
		t.Error("Proxy 未设置：代理配置会失效")
	}
	// http.Client 不能设全局 Timeout（会把下载和元数据绑成同一个约束）。
	if c.HTTP.Timeout != 0 {
		t.Errorf("http.Client.Timeout = %s，应当为 0（超时由各操作预算控制）", c.HTTP.Timeout)
	}
}
