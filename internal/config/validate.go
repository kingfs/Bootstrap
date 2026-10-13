package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/kingfs/Bootstrap/pkg/giturl"
)

// Validate 校验配置的合法性与内部一致性。
//
// 所有问题一次性汇总返回，而不是遇到第一个就退出——这在使用 `bs config validate` 时体验更好。
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.Version == 0 {
		c.Version = 1
	}
	if c.Version != 1 {
		fail("unsupported config version %d (want 1)", c.Version)
	}

	if !oneOf(c.Log.Level, "debug", "info", "warn", "error") {
		fail("log.level %q must be one of debug|info|warn|error", c.Log.Level)
	}
	if !oneOf(c.Log.Color, "auto", "always", "never") {
		fail("log.color %q must be one of auto|always|never", c.Log.Color)
	}

	// repo
	if strings.TrimSpace(c.Repo.Layout) == "" {
		fail("repo.layout must not be empty")
	} else {
		if !strings.Contains(c.Repo.Layout, "{{.Repo}}") {
			fail("repo.layout %q must reference {{.Repo}}", c.Repo.Layout)
		}
		if _, err := giturl.RenderLayoutData(c.Repo.Layout, giturl.LayoutData{
			Host: "github.com", Owner: "owner", Repo: "repo",
		}); err != nil {
			fail("%v", err)
		}
	}
	if !oneOf(c.Repo.Clone.Protocol, "https", "ssh") {
		fail("repo.clone.protocol %q must be https or ssh", c.Repo.Clone.Protocol)
	}
	if !oneOf(c.Repo.Pull.Strategy, "ff-only", "rebase", "merge") {
		fail("repo.pull.strategy %q must be ff-only|rebase|merge", c.Repo.Pull.Strategy)
	}
	if c.Repo.Clone.Depth < 0 {
		fail("repo.clone.depth must be >= 0")
	}
	if c.Repo.Sync.Jobs < 1 {
		fail("repo.sync.jobs must be >= 1")
	}
	for i, h := range c.Repo.Hosts {
		if strings.TrimSpace(h) == "" {
			fail("repo.hosts[%d] must not be empty", i)
		}
	}

	// proxy
	if c.Proxy.DefaultProfile != "" {
		if _, ok := c.Proxy.Profiles[c.Proxy.DefaultProfile]; !ok {
			fail("proxy.default_profile %q is not defined in proxy.profiles", c.Proxy.DefaultProfile)
		}
	}
	for name, p := range c.Proxy.Profiles {
		if strings.TrimSpace(name) == "" {
			fail("proxy.profiles contains an empty profile name")
		}
		if p.IsZero() {
			fail("proxy.profiles.%s is empty (need at least one of http_proxy/https_proxy/all_proxy)", name)
		}
		for field, raw := range map[string]string{
			"http_proxy":  p.HTTPProxy,
			"https_proxy": p.HTTPSProxy,
			"all_proxy":   p.AllProxy,
		} {
			if raw == "" {
				continue
			}
			if err := validateProxyURL(raw); err != nil {
				fail("proxy.profiles.%s.%s: %v", name, field, err)
			}
		}
	}

	// tool
	for _, k := range c.Tool.AllowSystemFor {
		if !IsKind(k) {
			fail("tool.allow_system_for contains unknown kind %q", k)
		}
	}
	for stack, mgr := range c.Tool.Managers {
		if strings.TrimSpace(stack) == "" || strings.TrimSpace(mgr) == "" {
			fail("tool.managers has an empty entry (%q -> %q)", stack, mgr)
		}
	}

	// ui
	if !oneOf(c.UI.Progress, "auto", "plain", "off") {
		fail("ui.progress %q must be auto|plain|off", c.UI.Progress)
	}

	return errors.Join(errs...)
}

func validateProxyURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h", "socks4":
	default:
		return fmt.Errorf("unsupported proxy scheme %q in %q", u.Scheme, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host in %q", raw)
	}
	return nil
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// IsKind 报告字符串是否是合法的工具类型。
func IsKind(v string) bool {
	for _, k := range AllKinds {
		if string(k) == v {
			return true
		}
	}
	return false
}
