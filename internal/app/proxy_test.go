package app

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProxyTestDirectIgnoresEnvProxy：`bs proxy test` 的第一列必须是**真的直连**。
//
// 回归的故障长这样：direct 用 http.Client{}（Transport 为 nil → http.DefaultTransport），
// 而 DefaultTransport 会读 HTTP(S)_PROXY，于是 shell 里一有代理，这一列就跟着那个代理
// 一起成功/失败。用户看到"直连失败"会去查网络，其实是代理的证书/认证问题
// （实测踩过：公司网关 MITM 时这一列报 x509，看着像直连不可用）。
//
// 手法：目标放在本机的**非回环**地址上，代理指到黑洞 127.0.0.1:9。
// 修复后：直连直接打到本地服务 → ok；修复前：请求被交给黑洞 → 失败。
func TestProxyTestDirectIgnoresEnvProxy(t *testing.T) {
	srv := newLocalServer(t)
	defer srv.Close()

	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	// NO_PROXY 的大小写两份都要清掉：Go 只要看到 no_proxy 里有 127.0.0.1 就会绕过代理，
	// 那这条用例就变成"无论修没修都通过"了。
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	a, _ := newTestApp(t)
	res, err := a.ProxyTest(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("ProxyTest: %v", err)
	}
	if !res.Direct.OK {
		t.Fatalf("直连不该受环境代理影响，却失败了：%+v", res.Direct)
	}
	// 反过来：走代理那一列就是要认环境里的代理（这里指向黑洞），必须失败。
	// 两条一起才说明"该直连的直连、该走代理的走代理"。
	if res.Proxied.OK {
		t.Fatalf("代理这一列应走环境里的代理（黑洞），不该成功：%+v", res.Proxied)
	}
}

// newLocalServer 在本机的**非回环 IP** 上起一个 HTTP 服务。
//
// 为什么不用 httptest.NewServer（绑 127.0.0.1）：Go 的 ProxyFromEnvironment 对
// localhost/回环地址有"一律不走代理"的特例，于是在回环目标上，"直连会不会误用环境代理"
// 永远测不出来（这条用例第一版就踩了这个坑：改成错误的实现照样通过）。
// 拿不到非回环地址（例如 --network none 的容器）就跳过——那是环境限制，不是回归。
func newLocalServer(t *testing.T) *httptest.Server {
	t.Helper()
	conn, err := net.Dial("udp", "8.8.8.8:80") // 不发包：只是问内核"出网会用哪张网卡"
	if err != nil {
		t.Skipf("拿不到本机非回环地址，跳过：%v", err)
	}
	ip := conn.LocalAddr().(*net.UDPAddr).IP
	_ = conn.Close()
	if ip.IsLoopback() {
		t.Skip("本机只有回环地址，无法区分「直连」与「走环境代理」")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Skipf("无法在 %s 上监听，跳过：%v", ip, err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	_ = srv.Listener.Close() // 换掉 httptest 默认的回环监听
	srv.Listener = ln
	srv.Start()
	return srv
}

// TestProxyTestTLSFailureCarriesCAHint：证书类失败要带上"去装系统 CA"的提示。
//
// httptest 的 TLS 服务用的是自签证书，系统信任库必然验不过——正好复现最小镜像
// 里第一次 HTTPS 的经典失败，且整个过程不出本机。
func TestProxyTestTLSFailureCarriesCAHint(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a, _ := newTestApp(t)
	res, err := a.ProxyTest(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("ProxyTest: %v", err)
	}
	if res.Direct.OK {
		t.Fatal("自签证书不该被系统信任库接受")
	}
	if !strings.Contains(res.Direct.Error, "no usable system CA certificates") {
		t.Fatalf("x509 失败应带 CA 提示，实际：%q", res.Direct.Error)
	}
}

// TestProxyTestMasksProxyCredentials：上游地址里的凭据在输出里只能是 ***。
//
// 这条路径会把"实际用的代理"打进结果（CLI 用它显示"经 <地址>"），
// 与其它输出一样必须脱敏（AGENTS §7）。
func TestProxyTestMasksProxyCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a, _ := newTestApp(t)
	a.Cfg.Proxy.Profiles["corp"] = proxyProfile{
		HTTPProxy:  "http://user:pw@127.0.0.1:9",
		HTTPSProxy: "http://user:pw@127.0.0.1:9",
	}
	if _, err := a.ProxyUse(context.Background(), "corp"); err != nil {
		t.Fatalf("ProxyUse: %v", err)
	}
	res, err := a.ProxyTest(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("ProxyTest: %v", err)
	}
	if res.Profile != "corp" {
		t.Fatalf("Profile = %q, want corp", res.Profile)
	}
	if !strings.Contains(res.ProxyURL, "***") {
		t.Fatalf("上游地址应显示为脱敏形式，实际 %q", res.ProxyURL)
	}
	if strings.Contains(res.ProxyURL, "user") || strings.Contains(res.ProxyURL, "pw") {
		t.Fatalf("凭据泄漏到输出里了：%q", res.ProxyURL)
	}
}
