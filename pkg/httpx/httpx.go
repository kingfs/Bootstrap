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
	"crypto/x509"
	"encoding/hex"
	"errors"
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

// DefaultTimeout 是通用 GET（元数据、checksums 文件）的默认超时。
const DefaultTimeout = 60 * time.Second

// DefaultDownloadTimeout 是下载归档的默认超时。
//
// 刻意与 GET 分开：元数据慢说明网络有问题，应当快速失败；
// 而下载慢是正常的（几十 MB 的 Go 工具链、限速的代理），
// 一个全局 60s 会把"能下完"变成"永远装不上"。
const DefaultDownloadTimeout = 15 * time.Minute

// maxBody 是 GetBytes 的默认上限（防止把大文件读进内存）。
const maxBody = 8 << 20

// dialTimeout 是建立单条 TCP 连接（含连代理）的预算。
//
// 必须明显小于上层给整次请求的预算，否则"第一次尝试卡到超时"会把预算吃光，
// `Retries` 就永远只是重放一次立即失败——这正是"元数据偶尔查不到"的根因。
const dialTimeout = 10 * time.Second

// Client 是一个带代理感知的 HTTP 客户端。
type Client struct {
	HTTP *http.Client
	// Env 是代理相关环境变量；为空时回退到进程环境。
	Env map[string]string
	// UserAgent 会附加到每个请求上（GitHub API 要求）。
	UserAgent string
	// Retries 是失败重试次数（默认 1 次重试）。
	Retries int
	// Timeout 是通用 GET 的单次预算。
	Timeout time.Duration
	// DownloadTimeout 是下载单个文件的预算。
	DownloadTimeout time.Duration
}

// New 构造客户端。env 为 nil 时使用进程环境。
func New(timeout time.Duration, env map[string]string) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	c := &Client{Env: env, UserAgent: "bootstrap", Retries: 1, Timeout: timeout, DownloadTimeout: DefaultDownloadTimeout}
	transport := &http.Transport{
		Proxy: c.proxyFunc,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	// 不设 http.Client.Timeout：超时由每个操作各自的预算（Timeout/DownloadTimeout）决定，
	// 否则一个全局上限会把"下载 100MB 工具链"和"读一个 1KB 的 checksums 文件"
	// 绑成同一个约束，二者只能取其一。
	c.HTTP = &http.Client{Transport: transport}
	return c
}

// GetCtx 发一个带预算的 GET，返回未读取的响应体（调用方负责关闭）。
//
// 给"需要看响应头/最终跳转地址"的调用方使用（例如跟随 /releases/latest 的重定向）。
func (c *Client) GetCtx(ctx context.Context, rawURL string) (*http.Response, error) {
	ctx, cancel := withBudget(ctx, c.Timeout)
	defer cancel()
	return c.HTTP.Do(mustRequest(ctx, rawURL, c.UserAgent))
}

// withBudget 给 ctx 套上单次操作的预算（已有更短 deadline 时以更短者为准）。
func withBudget(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		budget = DefaultTimeout
	}
	return context.WithTimeout(ctx, budget)
}

func mustRequest(ctx context.Context, rawURL, userAgent string) *http.Request {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		// 只在 URL 非法时发生；把错误留到 Do 里报，调用方已有 URL 校验。
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "http://invalid.invalid/", nil)
		return req
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	return req
}

func (c *Client) getenv(key string) string {
	if c.Env != nil {
		return c.Env[key]
	}
	return os.Getenv(key)
}

func (c *Client) proxyFunc(req *http.Request) (*url.URL, error) {
	u, err := ProxyFor(func(k string) string { return c.getenv(k) }, req.URL)
	// 把"这次请求走不走代理、走哪个"记进调试日志：网络问题排查时这是第一个要看的事实
	// （曾经出现"日志说代理没问题，实际请求却在直连"的情况，靠肉眼是看不出来的）。
	if c.Env == nil {
		c.logf("proxy for %s: %v (env from process)", req.URL.Host, u)
	} else {
		c.logf("proxy for %s: %v (env injected)", req.URL.Host, u)
	}
	return u, err
}

// GetBytes 取回一个较小的响应体（用于 API 与 checksums 文件）。
func (c *Client) GetBytes(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := withBudget(ctx, c.Timeout)
	defer cancel()
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
// 支持**断点续传**：半成品留在 dest 同目录的 `.<名字>.<url 指纹>.part`（同名 URL 才复用），
// 下次调用带 `Range` 接着下；服务端不支持范围请求（回 200）时自动从头开始。
// 校验失败会把半成品删掉（它已经被证明是坏数据），网络中断则保留下来供下次续传。
//
// 返回实际写入的路径。
func (c *Client) Download(ctx context.Context, rawURL, dest, wantSHA256 string, mode os.FileMode) (string, error) {
	if dest == "" {
		return "", fmt.Errorf("download %s: empty destination", rawURL)
	}
	if mode == 0 {
		mode = 0o644
	}
	if err := fsx.EnsureDir(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	// 下载用独立的（更长的）预算，见 DefaultDownloadTimeout 的说明。
	ctx, cancel := withBudget(ctx, c.DownloadTimeout)
	defer cancel()

	// 已经下载过且校验和一致：直接用，别再把几十 MB 重下一遍。
	// 只在给了校验和时这么做——没有校验和就无法证明缓存里的文件是它对的那一份。
	if wantSHA256 != "" {
		if got, err := hashFile(dest); err == nil && strings.EqualFold(strings.TrimSpace(wantSHA256), got) {
			if err := os.Chmod(dest, mode); err == nil {
				c.logf("download cache hit %s → %s", rawURL, dest)
				return dest, nil
			}
		}
	}

	part := partPath(dest, rawURL)
	offset := partSize(part)

	resp, resumed, err := c.openDownload(ctx, rawURL, part, offset)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if resumed {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(part, flags, mode)
	if err != nil {
		return "", fmt.Errorf("open partial download %s: %w", part, err)
	}
	written, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		// 网络断了不是"数据坏了"：留下半成品，下次从这里接着下。
		c.logf("download %s interrupted after %d bytes; keeping %s for resume: %v",
			rawURL, offset+written, part, copyErr)
		return "", fmt.Errorf("download %s: %w", rawURL, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close %s: %w", part, closeErr)
	}
	if resumed {
		c.logf("resumed %s from byte %d (%d more bytes)", rawURL, offset, written)
	}

	if wantSHA256 != "" {
		got, err := hashFile(part)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(strings.TrimSpace(wantSHA256), got) {
			// 坏数据不能留着：留着只会让下一次续传出"永远对不上"的结果。
			os.Remove(part)
			return "", fmt.Errorf("checksum mismatch for %s: want %s, got %s", rawURL, strings.TrimSpace(wantSHA256), got)
		}
	}
	if err := os.Chmod(part, mode); err != nil {
		os.Remove(part)
		return "", err
	}
	if err := os.Rename(part, dest); err != nil {
		os.Remove(part)
		return "", fmt.Errorf("move into place: %w", err)
	}
	c.logf("downloaded %s (%d bytes) → %s", rawURL, offset+written, dest)
	return dest, nil
}

// openDownload 发起（可能带 Range 的）下载请求。
//
// resumed 为真表示"服务端同意从 offset 接着传"（206）；服务端忽略 Range 回 200 时
// 必须从头写，否则文件里会混进两段内容。
func (c *Client) openDownload(ctx context.Context, rawURL, part string, offset int64) (*http.Response, bool, error) {
	resp, err := c.fetch(ctx, rawURL, offset)
	if err != nil {
		var status *StatusError
		// 416：本地半成品比服务端资源还长（版本换了/文件被改小了）——丢掉它从零重来。
		if offset > 0 && errors.As(err, &status) && status.Status == http.StatusRequestedRangeNotSatisfiable {
			os.Remove(part)
			resp, err = c.fetch(ctx, rawURL, 0)
			if err != nil {
				return nil, false, err
			}
			return resp, false, nil
		}
		return nil, false, err
	}
	if offset == 0 {
		return resp, false, nil
	}
	if resp.StatusCode == http.StatusPartialContent {
		return resp, true, nil
	}
	// 200（服务端不支持/忽略了 Range）：丢掉半成品，整体重下。
	c.logf("server ignored Range for %s; restarting from scratch", rawURL)
	return resp, false, nil
}

// fetch 发一次 GET（带 offset 时加 Range 头），并按 Retries 重试**建立响应**阶段。
//
// 只重试"拿到响应头"这一段：真正的断线发生在读 body 时，那由半成品 + 下次续传来兜。
func (c *Client) fetch(ctx context.Context, rawURL string, offset int64) (*http.Response, error) {
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
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		resp, err := c.doOnce(req, rawURL)
		if err != nil {
			var perm *permanentError
			if errors.As(err, &perm) {
				return nil, perm.err
			}
			lastErr = err
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// partPath 是半成品文件的路径：`.<名字>.<url 指纹8位>.part`。
//
// 名字里带 URL 指纹，保证"同一个目标文件的不同版本/镜像"不会互相续传；
// 放在 dest 同目录，保证跨进程、跨命令都能接着下（放临时目录就没意义了）。
func partPath(dest, rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	name := filepath.Base(dest)
	return filepath.Join(filepath.Dir(dest), "."+name+"."+hex.EncodeToString(sum[:4])+".part")
}

// partSize 返回可续传的半成品大小（不存在或为空都算 0）。
func partSize(part string) int64 {
	info, err := os.Stat(part)
	if err != nil || info.IsDir() {
		return 0
	}
	return info.Size()
}

// hashFile 计算文件内容的 sha256。
//
// 续传场景下哈希必须覆盖"已有前缀 + 新写入部分"，所以这里整文件流式读一遍：
// 比边下边算多一次本地读，但换来"续传/重下走同一条校验路径"的简单性。
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *Client) logf(format string, args ...any) {
	if Logger != nil {
		Logger(format, args...)
	}
}

// Logger 是可选的下载日志钩子（由上层注入 slog）。
var Logger func(format string, args ...any)

// do 是带重试的 GET（元数据、checksums 这类整体请求走这里）。
//
// 4xx（429 除外）重试没有意义，直接返回；5xx 与网络错误按 Retries 重放。
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
		resp, err := c.doOnce(req, rawURL)
		if err != nil {
			var perm *permanentError
			if errors.As(err, &perm) {
				return nil, perm.err
			}
			lastErr = err
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// tlsHint 给"系统缺少 CA 证书"这类 TLS 错误补一句可行动的提示。
//
// 为什么必须补：全新最小镜像（ubuntu:24.04 docker 镜像就是这样）里没有系统证书文件，
// 第一次 HTTPS 请求只会得到 `x509: certificate signed by unknown authority`——
// 看到这句话的人无从下手。提示用英文（与包内其它错误一致），"该执行什么命令"交给
// 上层（`bs doctor` 会按发行版给出具体命令）。
func tlsHint(err error) error {
	var (
		unknownAuthority x509.UnknownAuthorityError
		systemRoots      x509.SystemRootsError
		certInvalid      x509.CertificateInvalidError
	)
	if errors.As(err, &unknownAuthority) || errors.As(err, &systemRoots) || errors.As(err, &certInvalid) {
		return fmt.Errorf("%w (hint: no usable system CA certificates; install your distro's CA package, e.g. ca-certificates)", err)
	}
	return err
}

// doRequest 发一次请求（不重试），把非 2xx 变成 *StatusError。
//
// 供带自定义头（Range / If-Range）的下载复用；重试策略留在调用方，
// 因为"断点续传的 206"与"整体 GET 的 200"对状态码的期待不同。
func (c *Client) doOnce(req *http.Request, rawURL string) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", rawURL, tlsHint(err))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	statusErr := &StatusError{
		URL:    rawURL,
		Status: resp.StatusCode,
		Body:   strings.TrimSpace(string(body)),
	}
	if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		// 4xx（除 429）重试没有意义：包一层让调用方一眼看出是"不再重试"。
		return nil, &permanentError{statusErr}
	}
	return nil, statusErr
}

// permanentError 标记"重试也没用"的错误（4xx）。
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

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
					return nil, fmt.Errorf("invalid proxy %s=%q: %w", k, v, err)
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
