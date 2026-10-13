package proxy

import (
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/internal/state"
	"github.com/kingfs/Bootstrap/pkg/proxyenv"
)

func testConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Proxy.DefaultProfile = "local"
	cfg.Proxy.Profiles = map[string]proxyenv.Profile{
		"local": {
			HTTPProxy:  "http://127.0.0.1:7890",
			HTTPSProxy: "http://127.0.0.1:7890",
			NoProxy:    "127.0.0.1,localhost",
		},
		"corp": {
			HTTPProxy:  "http://user:pw@proxy.corp:8123",
			HTTPSProxy: "http://user:pw@proxy.corp:8123",
		},
	}
	return &cfg
}

func TestActiveFallsBackToDefaultProfile(t *testing.T) {
	svc := NewService(testConfig(), state.NewStore(""))
	res, err := svc.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if res.Name != "local" || res.Source != SourceConfig {
		t.Errorf("Active = %+v, want local/config", res)
	}
	if !res.Enabled() {
		t.Error("profile with proxies should be Enabled")
	}
}

func TestOffModeIgnoresDefaultProfileAndEnv(t *testing.T) {
	store := state.NewStore("")
	if _, err := store.SetActiveProfile(""); err != nil {
		t.Fatalf("off: %v", err)
	}
	svc := NewService(testConfig(), store)
	svc.Getenv = func(k string) string {
		if strings.EqualFold(k, "HTTPS_PROXY") {
			return "http://from-env:8080"
		}
		return ""
	}
	res, err := svc.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if res.Enabled() {
		t.Errorf("explicit off must disable everything, got %+v", res)
	}
	if res.Source != SourceNone {
		t.Errorf("Source = %q, want none", res.Source)
	}
	name, _, err := svc.ActiveName()
	if err != nil {
		t.Fatalf("ActiveName: %v", err)
	}
	if name != "" {
		t.Errorf("ActiveName = %q, want empty after off", name)
	}
}

func TestOnModeUsesStateOverConfig(t *testing.T) {
	store := state.NewStore("")
	if _, err := store.SetActiveProfile("corp"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	svc := NewService(testConfig(), store)
	name, _, err := svc.ActiveName()
	if err != nil {
		t.Fatalf("ActiveName: %v", err)
	}
	if name != "corp" {
		t.Errorf("ActiveName = %q, want corp (state must win over default_profile)", name)
	}
}

func TestActiveFallsBackToEnvironment(t *testing.T) {
	cfg := testConfig()
	cfg.Proxy.DefaultProfile = ""
	svc := NewService(cfg, state.NewStore(""))
	svc.Getenv = func(k string) string {
		switch k {
		case "HTTPS_PROXY":
			return "http://env-proxy:8080"
		case "NO_PROXY":
			return "localhost"
		}
		return ""
	}
	res, err := svc.Active()
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if res.Source != SourceEnv || res.Profile.HTTPSProxy != "http://env-proxy:8080" {
		t.Errorf("Active = %+v, want env fallback", res)
	}
}

func TestNetworkEnvRespectsOverrideExisting(t *testing.T) {
	store := state.NewStore("")
	if _, err := store.SetActiveProfile("local"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	svc := NewService(testConfig(), store)
	svc.Environ = func() []string {
		return []string{"HTTPS_PROXY=http://user-set:1", "PATH=/usr/bin"}
	}
	env, err := svc.NetworkEnv()
	if err != nil {
		t.Fatalf("NetworkEnv: %v", err)
	}
	// override_existing=false：用 shell 里的值，但**不能缺席**——
	// 缺了就等于"用户明明配了代理，bs 自己的请求却全直连"。
	if env["HTTPS_PROXY"] != "http://user-set:1" {
		t.Errorf("HTTPS_PROXY = %q, want the shell value (not dropped)", env["HTTPS_PROXY"])
	}
	if env["https_proxy"] != "http://user-set:1" {
		t.Errorf("https_proxy = %q, want the same value as HTTPS_PROXY（大小写不能分裂）", env["https_proxy"])
	}
	if env["HTTP_PROXY"] == "" {
		t.Error("HTTP_PROXY should still be injected")
	}

	svc.cfg.Proxy.OverrideExisting = true
	env, err = svc.NetworkEnv()
	if err != nil {
		t.Fatalf("NetworkEnv: %v", err)
	}
	if env["HTTPS_PROXY"] != "http://127.0.0.1:7890" {
		t.Errorf("HTTPS_PROXY = %q, want the profile value", env["HTTPS_PROXY"])
	}
}

func TestNetworkEnvDisabledByAutoApply(t *testing.T) {
	cfg := testConfig()
	cfg.Proxy.AutoApply = false
	svc := NewService(cfg, state.NewStore(""))
	env, err := svc.NetworkEnv()
	if err != nil {
		t.Fatalf("NetworkEnv: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("auto_apply=false must inject nothing, got %v", env)
	}
}

func TestLookupAndNames(t *testing.T) {
	svc := NewService(testConfig(), state.NewStore(""))
	names := svc.Names()
	if len(names) != 2 || names[0] != "corp" || names[1] != "local" {
		t.Errorf("Names = %v, want sorted [corp local]", names)
	}
	p, err := svc.Lookup("corp")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if p.Name != "corp" {
		t.Errorf("Lookup should fill Name, got %q", p.Name)
	}
	if _, err := svc.Lookup("nope"); err == nil {
		t.Error("Lookup of unknown profile should fail")
	} else if !strings.Contains(err.Error(), "available") {
		t.Errorf("error should list available profiles: %v", err)
	}
}

func TestMaskedHidesCredentials(t *testing.T) {
	p, err := NewService(testConfig(), state.NewStore("")).Lookup("corp")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	masked := Masked(p)
	if strings.Contains(masked.HTTPProxy, "user:pw") {
		t.Errorf("credentials leaked: %s", masked.HTTPProxy)
	}
	if !strings.Contains(masked.HTTPProxy, "***@proxy.corp:8123") {
		t.Errorf("masked value = %q, want host preserved", masked.HTTPProxy)
	}
}
