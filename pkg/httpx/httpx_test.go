package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
		"NO_PROXY":    "127.0.0.1,localhost,10.0.0.0/8,192.168.0.0/16,corp.example.com,corp.example.cn,.corp.example.com,.example.org,::1,[::1]",
	}
	tests := []struct {
		name   string
		env    map[string]string
		target string
		want   string
	}{
		{"https goes through https proxy", full, "https://github.com/x", "http://127.0.0.1:7891"},
		{"http goes through http proxy", full, "http://example.com/x", "http://127.0.0.1:7890"},
		{"no_proxy plain host", full, "https://corp.example.com/x", ""},
		{"no_proxy dot suffix", full, "https://git.corp.example.com/x", ""},
		{"no_proxy bare suffix", full, "https://git.corp.example.cn/x", ""},
		{"no_proxy other domain", full, "https://other.example.org/x", ""},
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
	// Windows 没有 POSIX 权限位（读出来恒为 0666/0777），这条断言只在 POSIX 上有意义；
	// 内容/路径/校验和等断言在 Windows 上照旧执行。
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(dest)
		if info.Mode().Perm() != 0o755 {
			t.Errorf("mode = %v, want 0755", info.Mode().Perm())
		}
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

// rangeServer 是支持 Range 的测试服务器：记录收到的 Range 头，按需切断首个响应。
type rangeServer struct {
	payload   []byte
	requests  []string
	cutAfter  int  // >0 时第一次请求只写这么多字节就断开
	ignoreRng bool // 模拟"不支持 Range"的服务端（永远回 200 全量）
	srv       *httptest.Server
}

func newRangeServer(t *testing.T, payload []byte) *rangeServer {
	t.Helper()
	rs := &rangeServer{payload: payload}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.requests = append(rs.requests, r.Header.Get("Range"))
		if rs.ignoreRng {
			rs.requests[len(rs.requests)-1] = "IGNORED:" + r.Header.Get("Range")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
		if start, ok := parseRangeHeader(r.Header.Get("Range")); ok {
			if start >= int64(len(payload)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[start:])
			return
		}
		if rs.cutAfter > 0 && len(rs.requests) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload[:rs.cutAfter])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// 直接掐断连接：模拟下载中途断网
			hj, _ := w.(http.Hijacker)
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

func parseRangeHeader(v string) (int64, bool) {
	if !strings.HasPrefix(v, "bytes=") {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(v, "bytes="), "-"), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// TestDownloadResumesPartialFile 是本项目"慢网络下别白下一遍"的核心断言：
// 第一次下载被掐断后，半成品要留着；第二次必须带 Range 从断点续，并且校验和照旧通过。
func TestDownloadResumesPartialFile(t *testing.T) {
	payload := bytes.Repeat([]byte("bootstrap-resume-"), 4096) // ~72KB
	sum := sha256.Sum256(payload)
	rs := newRangeServer(t, payload)
	rs.cutAfter = 20000

	dir := t.TempDir()
	dest := filepath.Join(dir, "bs")
	c := New(5*time.Second, nil)

	if _, err := c.Download(context.Background(), rs.srv.URL+"/bs", dest, hex.EncodeToString(sum[:]), 0o755); err == nil {
		t.Fatal("被掐断的下载应当报错")
	}
	part := partPath(dest, rs.srv.URL+"/bs")
	info, err := os.Stat(part)
	if err != nil {
		t.Fatalf("半成品应当保留下来供续传：%v", err)
	}
	if info.Size() == 0 || info.Size() >= int64(len(payload)) {
		t.Fatalf("半成品大小 = %d，期望介于 0 与 %d 之间", info.Size(), len(payload))
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("没下完不该出现目标文件")
	}

	// 第二次：必须带 Range，且最终落在 dest
	path, err := c.Download(context.Background(), rs.srv.URL+"/bs", dest, hex.EncodeToString(sum[:]), 0o755)
	if err != nil {
		t.Fatalf("续传下载失败：%v", err)
	}
	if path != dest {
		t.Errorf("path = %q", path)
	}
	last := rs.requests[len(rs.requests)-1]
	if !strings.HasPrefix(last, "bytes=") {
		t.Errorf("第二次请求没有带 Range，实际 %q（全部：%v）", last, rs.requests)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("续传后内容不一致：err=%v len=%d want=%d", err, len(got), len(payload))
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Error("成功后应当清掉半成品")
	}
}

// TestDownloadRestartsWhenServerIgnoresRange：服务端不支持 Range（回 200）时
// 必须整体重写——否则文件里会前一段旧数据 + 后一段新数据，校验和永远对不上。
func TestDownloadRestartsWhenServerIgnoresRange(t *testing.T) {
	payload := []byte("0123456789abcdefghij")
	sum := sha256.Sum256(payload)
	rs := newRangeServer(t, payload)
	rs.cutAfter = 8

	dir := t.TempDir()
	dest := filepath.Join(dir, "bs")
	c := New(5*time.Second, nil)
	if _, err := c.Download(context.Background(), rs.srv.URL+"/bs", dest, hex.EncodeToString(sum[:]), 0o644); err == nil {
		t.Fatal("被掐断的下载应当报错")
	}

	rs.ignoreRng = true
	rs.cutAfter = 0
	if _, err := c.Download(context.Background(), rs.srv.URL+"/bs", dest, hex.EncodeToString(sum[:]), 0o644); err != nil {
		t.Fatalf("服务端忽略 Range 时应当能整体重下：%v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, payload) {
		t.Errorf("内容 = %q，期望 %q", got, payload)
	}
}

// TestDownloadStaleRangeRestarts：本地半成品比资源还长（416）时要从零重来，
// 不能一直卡在"续传出错"上。
func TestDownloadStaleRangeRestarts(t *testing.T) {
	payload := []byte("short payload")
	sum := sha256.Sum256(payload)
	rs := newRangeServer(t, payload)

	dir := t.TempDir()
	dest := filepath.Join(dir, "bs")
	part := partPath(dest, rs.srv.URL+"/bs")
	if err := os.WriteFile(part, bytes.Repeat([]byte("x"), len(payload)+100), 0o644); err != nil {
		t.Fatalf("write stale part: %v", err)
	}
	c := New(5*time.Second, nil)
	if _, err := c.Download(context.Background(), rs.srv.URL+"/bs", dest, hex.EncodeToString(sum[:]), 0o644); err != nil {
		t.Fatalf("416 后应当整体重下：%v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, payload) {
		t.Errorf("内容 = %q，期望 %q", got, payload)
	}
}

// TestDownloadChecksumFailureDropsPartial：校验失败说明半成品是坏数据，
// 必须删掉——留着只会让下一次续传出"永远对不上"的结果。
func TestDownloadChecksumFailureDropsPartial(t *testing.T) {
	payload := []byte("payload")
	rs := newRangeServer(t, payload)
	dir := t.TempDir()
	dest := filepath.Join(dir, "bs")
	c := New(5*time.Second, nil)
	if _, err := c.Download(context.Background(), rs.srv.URL+"/bs", dest, strings.Repeat("0", 64), 0o644); err == nil {
		t.Fatal("校验和不匹配应当失败")
	}
	if _, err := os.Stat(partPath(dest, rs.srv.URL+"/bs")); !os.IsNotExist(err) {
		t.Error("校验失败后不该留下半成品")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("校验失败后不该留下目标文件")
	}
}

// TestPartPathIsPerURL：半成品文件名要带 URL 指纹，避免不同版本互相续传。
func TestPartPathIsPerURL(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "bs")
	a := partPath(dest, "https://example.com/v1.0/bs")
	b := partPath(dest, "https://example.com/v2.0/bs")
	if a == b {
		t.Fatalf("不同 URL 的半成品路径不该相同：%s", a)
	}
	for _, p := range []string{a, b} {
		if filepath.Dir(p) != dir {
			t.Errorf("半成品应当放在目标同目录：%s", p)
		}
		if !strings.HasPrefix(filepath.Base(p), ".bs.") || !strings.HasSuffix(p, ".part") {
			t.Errorf("半成品命名不符合约定：%s", p)
		}
	}
}

// TestTLSHint：缺系统 CA 证书是"新机器第一公里"最常见的失败，
// 原样抛出 `x509: certificate signed by unknown authority` 等于没说，
// 必须补一句"去装系统 CA 包"的提示；其它错误不许被改写（免得掩盖真实原因）。
func TestTLSHint(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // 期望出现（空 = 原样返回）
	}{
		{"未知签发者", x509.UnknownAuthorityError{}, "no usable system CA certificates"},
		{"系统根证书缺失", x509.SystemRootsError{}, "no usable system CA certificates"},
		{"证书无效", x509.CertificateInvalidError{Reason: x509.Expired}, "no usable system CA certificates"},
		{"包在 url.Error 里也要认出来", &url.Error{Op: "Get", URL: "https://x/", Err: x509.UnknownAuthorityError{}}, "no usable system CA certificates"},
		{"连接被拒不改写", errors.New("connection refused"), ""},
		{"超时不改写", &url.Error{Op: "Get", URL: "https://x/", Err: errors.New("timeout")}, ""},
	}
	for _, tc := range cases {
		got := TLSHint(tc.err).Error()
		if tc.want == "" {
			if got != tc.err.Error() {
				t.Errorf("%s：不该改写，得到 %q", tc.name, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s：期望包含 %q，实际 %q", tc.name, tc.want, got)
		}
		// 原始错误必须保留，便于 -v 日志里查真正的原因
		if !errors.Is(TLSHint(tc.err), tc.err) && !strings.Contains(got, tc.err.Error()) {
			t.Errorf("%s：原始错误被丢了：%q", tc.name, got)
		}
	}
}
