package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kingfs/Bootstrap/internal/proxy"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/httpx"
	"github.com/kingfs/Bootstrap/pkg/proxyenv"
)

// ProxySummary 是 `bs proxy status` 的结构化结果。
type ProxySummary struct {
	Active     string            `json:"active,omitempty"`
	Mode       string            `json:"mode"`
	Source     string            `json:"source"`
	Detail     string            `json:"detail"`
	HTTPProxy  string            `json:"http_proxy,omitempty"`
	HTTPSProxy string            `json:"https_proxy,omitempty"`
	AllProxy   string            `json:"all_proxy,omitempty"`
	NoProxy    string            `json:"no_proxy,omitempty"`
	ShellEnv   map[string]string `json:"shell_env,omitempty"`
}

// ProxyProfileInfo 描述一个可用 profile。
type ProxyProfileInfo struct {
	Name       string `json:"name"`
	Active     bool   `json:"active"`
	Default    bool   `json:"default"`
	HTTPProxy  string `json:"http_proxy,omitempty"`
	HTTPSProxy string `json:"https_proxy,omitempty"`
	NoProxy    string `json:"no_proxy,omitempty"`
}

// ProxyStatus 返回当前代理状态（凭据已脱敏）。
func (a *App) ProxyStatus() (ProxySummary, error) {
	res, err := a.ProxyService().Active()
	if err != nil {
		return ProxySummary{}, err
	}
	masked := proxy.Masked(res.Profile)
	summary := ProxySummary{
		Mode:       a.ProxyService().Mode(),
		Source:     string(res.Source),
		Detail:     res.Detail,
		HTTPProxy:  masked.HTTPProxy,
		HTTPSProxy: masked.HTTPSProxy,
		AllProxy:   masked.AllProxy,
		NoProxy:    masked.NoProxy,
	}
	if res.Source == proxy.SourceConfig {
		summary.Active = res.Name
	}
	// 当前 shell 里实际存在的变量（只列出我们关心的键）。
	shellEnv := map[string]string{}
	for _, k := range proxyenv.ProxyKeys {
		if v := os.Getenv(k); v != "" {
			shellEnv[k] = proxyenv.MaskProxyURL(v)
		}
	}
	if len(shellEnv) > 0 {
		summary.ShellEnv = shellEnv
	}
	return summary, nil
}

// ProxyProfiles 列出全部 profile（含活跃标记）。
func (a *App) ProxyProfiles() ([]ProxyProfileInfo, error) {
	svc := a.ProxyService()
	activeName, _, err := svc.ActiveName()
	if err != nil {
		return nil, err
	}
	names := svc.Names()
	out := make([]ProxyProfileInfo, 0, len(names))
	for _, name := range names {
		p, err := svc.Lookup(name)
		if err != nil {
			return nil, err
		}
		masked := proxy.Masked(p)
		out = append(out, ProxyProfileInfo{
			Name:       name,
			Active:     name == activeName,
			Default:    name == a.Cfg.Proxy.DefaultProfile,
			HTTPProxy:  masked.HTTPProxy,
			HTTPSProxy: masked.HTTPSProxy,
			NoProxy:    masked.NoProxy,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ProxyUse 激活一个 profile（写入 state）。
//
// 名称为空时使用配置里的 default_profile；两者都没有则报错。
func (a *App) ProxyUse(ctx context.Context, name string) (proxy.Resolution, error) {
	if name == "" {
		name = a.Cfg.Proxy.DefaultProfile
	}
	if name == "" {
		return proxy.Resolution{}, fmt.Errorf("no proxy profile given and proxy.default_profile is empty")
	}
	p, err := a.ProxyService().Lookup(name)
	if err != nil {
		return proxy.Resolution{}, err
	}
	res := proxy.Resolution{Name: name, Profile: p, Source: proxy.SourceConfig, Detail: "config profile " + name}

	if a.DryRun {
		a.Log.Info("dry-run: would activate proxy profile", "profile", name)
		return res, nil
	}
	if err := a.EnsureRuntimeDirs(); err != nil {
		return proxy.Resolution{}, err
	}
	changed, err := a.Store.SetActiveProfile(name)
	if err != nil {
		return proxy.Resolution{}, err
	}
	a.Log.Debug("proxy profile activated", "profile", name, "changed", changed)
	return res, nil
}

// ProxyOff 关闭代理（清空 state 中的活跃项）。
func (a *App) ProxyOff(ctx context.Context) error {
	if a.DryRun {
		a.Log.Info("dry-run: would clear active proxy profile")
		return nil
	}
	if err := a.EnsureRuntimeDirs(); err != nil {
		return err
	}
	_, err := a.Store.SetActiveProfile("")
	return err
}

// RenderProxyEnv 渲染代理环境变量。
//
// apply=true 表示"把当前 shell 对齐到 bs 的配置状态"：以 state/配置里的活跃
// profile 为准，没有活跃项时输出清除语句（这是 `bs proxy off` 能真正关掉代理的原因），
// 此时忽略从父进程继承来的代理变量。
//
// apply=false 是"展示当前实际生效的代理"，允许回退到环境变量。
func (a *App) RenderProxyEnv(profileName string, apply bool, shell string) (string, error) {
	sh, err := proxyenv.NormalizeShell(shell)
	if err != nil {
		return "", err
	}
	svc := a.ProxyService()

	if profileName != "" {
		p, err := svc.Lookup(profileName)
		if err != nil {
			return "", err
		}
		return proxyenv.RenderProfile(sh, p)
	}

	name, _, err := svc.ActiveName()
	if err != nil {
		return "", err
	}
	if name != "" {
		p, err := svc.Lookup(name)
		if err != nil {
			return "", err
		}
		return proxyenv.RenderProfile(sh, p)
	}

	if apply {
		return proxyenv.RenderOff(sh)
	}
	res, err := svc.Active()
	if err != nil {
		return "", err
	}
	if !res.Enabled() {
		return proxyenv.RenderOff(sh)
	}
	return proxyenv.RenderProfile(sh, res.Profile)
}

// ProxyExec 在一个带代理的子进程里执行命令。
func (a *App) ProxyExec(ctx context.Context, profileName string, argv []string) (int, error) {
	if len(argv) == 0 {
		return 2, fmt.Errorf("no command given")
	}
	env, err := a.ProxyService().NetworkEnv()
	if err != nil {
		return 1, err
	}
	if profileName != "" {
		p, err := a.ProxyService().Lookup(profileName)
		if err != nil {
			return 1, err
		}
		env = p.Env()
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, a.Out, a.Err
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// ProxyTestResult 是连通性测试结果。
type ProxyTestResult struct {
	Target  string      `json:"target"`
	Direct  TestAttempt `json:"direct"`
	Proxied TestAttempt `json:"proxied"`
	Profile string      `json:"profile,omitempty"`
	// ProxyURL 是这次"走代理"实际用的上游地址（已脱敏），
	// 让用户不用回头翻配置就知道这两行分别打到了哪儿。
	ProxyURL string `json:"proxy_url,omitempty"`
}

// TestAttempt 是一次连接尝试的结果。
type TestAttempt struct {
	OK      bool   `json:"ok"`
	Latency string `json:"latency,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ProxyTest 分别测试直连与走代理的连通性。
func (a *App) ProxyTest(ctx context.Context, target string) (ProxyTestResult, error) {
	if target == "" {
		target = "https://github.com"
	}
	res := ProxyTestResult{Target: target}

	// "直连"必须真的直连。这里不能图省事用 http.Client{}（Transport 为 nil 时
	// 落到 http.DefaultTransport），因为 DefaultTransport 会读 HTTP(S)_PROXY：
	// shell 里一旦有代理，这一列就跟着那个代理一起成功/失败，用户看到"直连失败"
	// 却以为是网络到不了，而实际上是代理的证书/认证问题（实测踩过：公司网关
	// MITM 时这一列报 x509，看着像直连不可用）。空 Transport 的 Proxy 字段为 nil，
	// 正好是我们要的语义。
	direct := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{},
	}
	res.Direct = attempt(ctx, direct, target)

	active, err := a.ProxyService().Active()
	if err != nil {
		return res, err
	}
	if !active.Enabled() {
		res.Proxied = TestAttempt{OK: false, Error: "no active proxy profile"}
		return res, nil
	}
	res.Profile = active.Name
	proxyURL, err := url.Parse(firstNonEmpty(active.Profile.HTTPSProxy, active.Profile.HTTPProxy))
	if err != nil {
		return res, fmt.Errorf("invalid proxy URL: %w", err)
	}
	res.ProxyURL = proxyenv.MaskProxyURL(proxyURL.String())
	proxied := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	res.Proxied = attempt(ctx, proxied, target)
	return res, nil
}

func attempt(ctx context.Context, client *http.Client, target string) TestAttempt {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return TestAttempt{Error: err.Error()}
	}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Round(time.Millisecond)
	if err != nil {
		// 证书类错误补一句"去装系统 CA"：这两行是用户排障的第一现场，
		// 不该只有一句 x509 原文（下载路径早就有这句提示，两处要一致）。
		return TestAttempt{Latency: latency.String(), Error: httpx.TLSHint(err).Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	return TestAttempt{OK: resp.StatusCode < 400, Latency: latency.String()}
}

// ProxyAddResult 是 `bs proxy add` 的结果。
type ProxyAddResult struct {
	Name    string `json:"name"`
	Written bool   `json:"written"`
	File    string `json:"file,omitempty"`
	Snippet string `json:"snippet"`
}

// ProxyAdd 新增一个 profile。
//
// write=false 时只返回 YAML 片段（用户自己粘贴）；
// write=true 时合并进用户配置文件（写前备份，注释可能被重排）。
func (a *App) ProxyAdd(name, httpProxy, httpsProxy, noProxy string, write bool) (ProxyAddResult, error) {
	if strings.TrimSpace(name) == "" {
		return ProxyAddResult{}, fmt.Errorf("profile name must not be empty")
	}
	if httpProxy == "" && httpsProxy == "" {
		return ProxyAddResult{}, fmt.Errorf("at least one of --http or --https must be given")
	}
	profile := map[string]string{}
	if httpProxy != "" {
		profile["http_proxy"] = httpProxy
	}
	if httpsProxy != "" {
		profile["https_proxy"] = httpsProxy
	}
	if noProxy != "" {
		profile["no_proxy"] = noProxy
	}
	node := map[string]any{"proxy": map[string]any{"profiles": map[string]any{name: profile}}}
	snippet, err := yaml.Marshal(node)
	if err != nil {
		return ProxyAddResult{}, err
	}
	result := ProxyAddResult{Name: name, Snippet: string(snippet)}
	if !write {
		return result, nil
	}

	path := a.Paths.ConfigFile()
	result.File = path
	if a.DryRun {
		a.Log.Info("dry-run: would update config file", "file", path)
		return result, nil
	}
	if fsx.IsFile(path) {
		backup, err := fsx.BackupFile(path, Stamp())
		if err != nil {
			return result, err
		}
		a.Log.Debug("config backed up", "backup", backup)
	}
	if err := mergeIntoConfigFile(path, name, profile); err != nil {
		return result, err
	}
	result.Written = true
	return result, nil
}

func mergeIntoConfigFile(path, name string, profile map[string]string) error {
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parse existing config %s: %w", path, err)
		}
	}
	proxySection, _ := doc["proxy"].(map[string]any)
	if proxySection == nil {
		proxySection = map[string]any{}
	}
	profiles, _ := proxySection["profiles"].(map[string]any)
	if profiles == nil {
		profiles = map[string]any{}
	}
	profiles[name] = profile
	proxySection["profiles"] = profiles
	doc["proxy"] = proxySection

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	header := "# 本文件由 bs proxy add 修改（注释可能被重排）；请用 bs config validate 校验\n"
	return fsx.AtomicWriteFile(path, append([]byte(header), out...), 0o644)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
