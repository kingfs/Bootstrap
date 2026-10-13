// Package proxy 解析"当前应该用哪个代理"，并把结果提供给网络相关操作。
//
// 三条来源（优先级从高到低）：
//  1. 配置里的 profile + state 里记录的活跃项（由 `bs proxy on <name>` 设置）
//  2. 进程环境里已有的 HTTP_PROXY / HTTPS_PROXY（用户在别处设置过）
//  3. 没有任何代理
//
// 本包只做解析与计算，不写任何文件；状态写入由 internal/state 负责。
package proxy

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/internal/state"
	"github.com/kingfs/Bootstrap/pkg/proxyenv"
)

// Source 标识代理设置的来源。
type Source string

// 代理来源枚举。
const (
	SourceNone   Source = "none"
	SourceConfig Source = "config"
	SourceEnv    Source = "env"
)

// Resolution 是一次"当前代理是什么"的完整回答。
type Resolution struct {
	Name    string
	Profile proxyenv.Profile
	Source  Source
	// Detail 说明具体来自哪个 profile 或哪些环境变量，用于 status/doctor 展示。
	Detail string
}

// Enabled 报告当前是否真的启用了代理。
func (r Resolution) Enabled() bool { return !r.Profile.IsZero() }

// Service 解析代理配置。
type Service struct {
	cfg   *config.Config
	store *state.Store
	// Getenv 便于测试注入；为空时使用 os.Getenv。
	Getenv func(string) string
	// Environ 便于测试注入；为空时使用 os.Environ。
	Environ func() []string
}

// NewService 构造代理服务。
func NewService(cfg *config.Config, store *state.Store) *Service {
	return &Service{cfg: cfg, store: store, Getenv: os.Getenv, Environ: os.Environ}
}

func (s *Service) getenv(key string) string {
	if s.Getenv == nil {
		return os.Getenv(key)
	}
	return s.Getenv(key)
}

func (s *Service) environ() []string {
	if s.Environ == nil {
		return os.Environ()
	}
	return s.Environ()
}

// Names 返回全部已配置的 profile 名（排序后）。
func (s *Service) Names() []string {
	names := make([]string, 0, len(s.cfg.Proxy.Profiles))
	for name := range s.cfg.Proxy.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Lookup 按名字取 profile。
func (s *Service) Lookup(name string) (proxyenv.Profile, error) {
	p, ok := s.cfg.Proxy.Profiles[name]
	if !ok {
		return proxyenv.Profile{}, fmt.Errorf("proxy profile %q not found (available: %s)",
			name, strings.Join(s.Names(), ", "))
	}
	p.Name = name
	return p, nil
}

// Mode 返回显式代理模式：state 里的 on/off 优先，否则 auto。
func (s *Service) Mode() string {
	if s.store == nil {
		return state.ProxyModeAuto
	}
	mode, err := s.store.ProxyMode()
	if err != nil || mode == "" {
		return state.ProxyModeAuto
	}
	return mode
}

// ActiveName 返回活跃 profile 名。
//
// 优先级：state 里 mode=on 的 Active > 配置的 proxy.default_profile。
// mode=off 时直接返回空——此时不再看配置默认值，也不回退到环境变量，
// 这样 `bs proxy off` 的语义才是"真的关掉"。
func (s *Service) ActiveName() (string, Source, error) {
	switch s.Mode() {
	case state.ProxyModeOff:
		return "", SourceNone, nil
	case state.ProxyModeOn:
		if s.store != nil {
			if name, err := s.store.ActiveProfile(); err == nil && name != "" {
				return name, SourceConfig, nil
			}
		}
	}
	if name := s.cfg.Proxy.DefaultProfile; name != "" {
		return name, SourceConfig, nil
	}
	return "", SourceNone, nil
}

// Active 解析当前生效的代理。
func (s *Service) Active() (Resolution, error) {
	if s.Mode() == state.ProxyModeOff {
		return Resolution{Source: SourceNone, Detail: "proxy disabled by `bs proxy off`"}, nil
	}
	name, _, err := s.ActiveName()
	if err != nil {
		return Resolution{}, err
	}
	if name != "" {
		p, err := s.Lookup(name)
		if err != nil {
			return Resolution{}, err
		}
		return Resolution{Name: name, Profile: p, Source: SourceConfig, Detail: "config profile " + name}, nil
	}

	// 退化到进程环境。
	p := proxyenv.Profile{
		HTTPProxy:  firstEnv(s.getenv, "HTTP_PROXY", "http_proxy"),
		HTTPSProxy: firstEnv(s.getenv, "HTTPS_PROXY", "https_proxy"),
		NoProxy:    firstEnv(s.getenv, "NO_PROXY", "no_proxy"),
		AllProxy:   firstEnv(s.getenv, "ALL_PROXY", "all_proxy"),
	}
	if p.IsZero() {
		return Resolution{Source: SourceNone, Detail: "no proxy configured"}, nil
	}
	return Resolution{Name: "env", Profile: p, Source: SourceEnv, Detail: "inherited from environment"}, nil
}

// NetworkEnv 返回应该注入给网络类子进程的环境变量。
//
// cfg.Proxy.AutoApply 为 false 时返回空；OverrideExisting 为 false 时
// 不覆盖进程里已经存在的变量（尊重用户当前 shell 的设置）。
func (s *Service) NetworkEnv() (map[string]string, error) {
	if !s.cfg.Proxy.AutoApply {
		return nil, nil
	}
	res, err := s.Active()
	if err != nil {
		return nil, err
	}
	if !res.Enabled() {
		return nil, nil
	}
	env := res.Profile.Env()
	if s.cfg.Proxy.OverrideExisting {
		return env, nil
	}
	existing := map[string]bool{}
	for _, kv := range s.environ() {
		if k, _, ok := strings.Cut(kv, "="); ok {
			existing[k] = true
		}
	}
	for k := range env {
		if existing[k] {
			delete(env, k)
		}
	}
	return env, nil
}

// Masked 返回用于展示的脱敏 profile（保留 host:port，隐去凭据）。
func Masked(p proxyenv.Profile) proxyenv.Profile {
	return proxyenv.Profile{
		Name:       p.Name,
		HTTPProxy:  proxyenv.MaskProxyURL(p.HTTPProxy),
		HTTPSProxy: proxyenv.MaskProxyURL(p.HTTPSProxy),
		AllProxy:   proxyenv.MaskProxyURL(p.AllProxy),
		NoProxy:    p.NoProxy,
	}
}

func firstEnv(getenv func(string) string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
