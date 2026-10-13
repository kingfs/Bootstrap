// Package httpx 是 bs 自己发起网络请求时用的极小 HTTP 客户端封装。
//
// 与业务无关（因此放在 pkg/）：它只做三件事——
//  1. 按注入的环境变量选择代理（含 NO_PROXY 的域名后缀 / IP / CIDR 匹配）；
//  2. 带重试与超时的 GET / 下载；
//  3. 下载后校验 sha256（校验失败就删除临时文件，绝不留半个文件）。
package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// DefaultTimeout 是单次请求的默认超时。
const DefaultTimeout = 60 * time.Second

// maxBody 是 GetBytes 的默认上限（防止把大文件读进内存）。
const maxBody = 8 << 20

// Client 是一个带代理感知的 HTTP 客户端。
type Client struct {
	HTTP *http.Client
	// Env 是代理相关环境变量；为空时回退到进程环境。
	Env map[string]string
	// UserAgent 会附加到每个请求上（GitHub API 要求）。
	UserAgent string
	// Retries 是失败重试次数（默认 1 次重试）。
	Retries int
}

// New 构造客户端。env 为 nil 时使用进程环境。
func New(timeout time.Duration, env map[string]string) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	c := &Client{Env: env, UserAgent: "bootstrap", Retries: 1}
	transport := &http.Transport{
		Proxy:                 c.proxyFunc,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	c.HTTP = &http.Client{Transport: transport, Timeout: timeout}
	return c
}

func (c *Client) getenv(key string) string {
	if c.Env != nil {
		return c.Env[key]
	}
	return os.Getenv(key)
}

func (c *Client) proxyFunc(req *http.Request) (*url.URL, error) {
	return ProxyFor(func(k string) string { return c.getenv(k) }, req.URL)
}

// GetBytes 取回一个较小的响应体（用于 API 与 checksums 文件）。
func (c *Client) GetBytes(ctx context.Context, rawURL string) ([]byte, error) {
	resp, err := c.do(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rawURL, err)
	}
	return data, nil
}

// GetString 是 GetBytes 的字符串版本。
func (c *Client) GetString(ctx context.Context, rawURL string) (string, error) {
	data, err := c.GetBytes(ctx, rawURL)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Download 下载到 dest（父目录自动创建），可选校验 sha256。
//
// 返回实际写入的路径。校验失败时不会留下任何文件。
func (c *Client) Download(ctx context.Context, rawURL, dest, wantSHA256 string, mode os.FileMode) (string, error) {
	if dest == "" {
		return "", fmt.Errorf("download %s: empty destination", rawURL)
	}
	if mode == 0 {
		mode = 0o644
	}
	resp, err := c.do(ctx, rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if err := fsx.EnsureDir(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".bs-download-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	h := sha256.New()
	var written int64
	if wantSHA256 != "" {
		written, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	} else {
		written, err = io.Copy(tmp, resp.Body)
	}
	if err != nil {
		cleanup()
		return "", fmt.Errorf("download %s: %w", rawURL, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("close temp file: %w", err)
	}

	if wantSHA256 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(strings.TrimSpace(wantSHA256), got) {
			os.Remove(tmpName)
			return "", fmt.Errorf("checksum mismatch for %s: want %s, got %s", rawURL, strings.TrimSpace(wantSHA256), got)
		}
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("move into place: %w", err)
	}
	c.logf("downloaded %s (%d bytes) → %s", rawURL, written, dest)
	return dest, nil
}

func (c *Client) logf(format string, args ...any) {
	if Logger != nil {
		Logger(format, args...)
	}
}

// Logger 是可选的下载日志钩子（由上层注入 slog）。
var Logger func(format string, args ...any)

func (c *Client) do(ctx context.Context, rawURL string) (*http.Response, error) {
	attempts := c.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("bad url %s: %w", rawURL, err)
		}
		if c.UserAgent != "" {
			req.Header.Set("User-Agent", c.UserAgent)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("GET %s: %w", rawURL, err)
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		lastErr = &StatusError{
			URL:    rawURL,
			Status: resp.StatusCode,
			Body:   strings.TrimSpace(string(body)),
		}
		// 4xx（除 429）重试没有意义。
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// StatusError 描述非 2xx 响应。
type StatusError struct {
	URL    string
	Status int
	Body   string
}

// Error 实现 error。
func (e *StatusError) Error() string {
	msg := fmt.Sprintf("%s returned HTTP %d", e.URL, e.Status)
	if e.Body != "" {
		if len(e.Body) > 200 {
			e.Body = e.Body[:200]
		}
		msg += ": " + e.Body
	}
	return msg
}

// ProxyFor 根据环境变量选择代理。
//
// 支持 HTTP_PROXY / HTTPS_PROXY / ALL_PROXY（大小写均可）与 NO_PROXY：
// 后者支持 `*`、域名后缀（.example.com 或 example.com）、IP、CIDR、host:port。
// 匹配规则与 curl / Go 标准库的行为一致，因此用 `no_proxy=10.2.0.0/16` 这类
// 内网段写法时，bs 自己的下载不会绕代理。
func ProxyFor(getenv func(string) string, target *url.URL) (*url.URL, error) {
	if target == nil {
		return nil, nil
	}
	host := target.Hostname()
	if host == "" {
		return nil, nil
	}
	if noProxyMatch(getenv("NO_PROXY"), getenv("no_proxy"), host, target.Port()) {
		return nil, nil
	}
	keys := []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"}
	if target.Scheme == "http" {
		keys = []string{"HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"}
	}
	for _, k := range keys {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			u, err := url.Parse(v)
			if err != nil || u.Host == "" {
				// 允许 `127.0.0.1:7890` 这种省略 scheme 的写法
				// （url.Parse 会因为冒号把它当成非法 path 而报错）。
				u, err = url.Parse("http://" + strings.TrimPrefix(v, "//"))
				if err != nil || u.Host == "" {
					return nil, fmt.Errorf("invalid proxy %s=%q: %v", k, v, err)
				}
			}
			if u.Scheme == "https" || u.Scheme == "http" || strings.HasPrefix(u.Scheme, "socks") {
				return u, nil
			}
			return nil, fmt.Errorf("invalid proxy %s=%q: unsupported scheme %q", k, v, u.Scheme)
		}
	}
	return nil, nil
}

func noProxyMatch(upper, lower, host, port string) bool {
	raw := strings.TrimSpace(upper)
	if raw == "" {
		raw = strings.TrimSpace(lower)
	}
	if raw == "" {
		return false
	}
	hostPort := host
	if port != "" {
		hostPort = net.JoinHostPort(host, port)
	}
	ip := net.ParseIP(host)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		if strings.EqualFold(entry, host) || strings.EqualFold(entry, hostPort) {
			return true
		}
		// host:port 形式
		if h, p, err := net.SplitHostPort(entry); err == nil {
			if p == port && strings.EqualFold(strings.TrimPrefix(h, "."), host) {
				return true
			}
			continue
		}
		// CIDR 形式
		if _, cidr, err := net.ParseCIDR(entry); err == nil {
			if ip != nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		// 域名后缀形式：.example.com 或 example.com 都算
		suffix := strings.TrimPrefix(entry, ".")
		if strings.EqualFold(host, suffix) || strings.HasSuffix(strings.ToLower(host), "."+strings.ToLower(suffix)) {
			return true
		}
	}
	return false
}
