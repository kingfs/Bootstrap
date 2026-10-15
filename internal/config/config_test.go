package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/pkg/proxyenv"
)

func TestResolvePathsPerOS(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	home := "/home/kingfs"

	t.Run("linux defaults", func(t *testing.T) {
		p, err := ResolvePathsWith("linux", getenv, home)
		if err != nil {
			t.Fatalf("ResolvePathsWith: %v", err)
		}
		if want := "/home/kingfs/.config/bootstrap"; p.ConfigDir != want {
			t.Errorf("ConfigDir = %q, want %q", p.ConfigDir, want)
		}
		if want := "/home/kingfs/.local/state/bootstrap"; p.StateDir != want {
			t.Errorf("StateDir = %q, want %q", p.StateDir, want)
		}
		if want := "/home/kingfs/.cache/bootstrap"; p.CacheDir != want {
			t.Errorf("CacheDir = %q, want %q", p.CacheDir, want)
		}
	})

	t.Run("macOS unified to .config", func(t *testing.T) {
		p, err := ResolvePathsWith("darwin", getenv, home)
		if err != nil {
			t.Fatalf("ResolvePathsWith: %v", err)
		}
		if want := "/home/kingfs/.config/bootstrap"; p.ConfigDir != want {
			t.Errorf("ConfigDir = %q, want %q", p.ConfigDir, want)
		}
	})

	t.Run("xdg respected", func(t *testing.T) {
		envLocal := map[string]string{"XDG_CONFIG_HOME": "/x/cfg", "XDG_STATE_HOME": "/x/state", "XDG_CACHE_HOME": "/x/cache"}
		p, err := ResolvePathsWith("linux", func(k string) string { return envLocal[k] }, home)
		if err != nil {
			t.Fatalf("ResolvePathsWith: %v", err)
		}
		if p.ConfigDir != "/x/cfg/bootstrap" || p.StateDir != "/x/state/bootstrap" || p.CacheDir != "/x/cache/bootstrap" {
			t.Errorf("unexpected paths: %+v", p)
		}
	})

	t.Run("BS_CONFIG_HOME wins", func(t *testing.T) {
		envLocal := map[string]string{"XDG_CONFIG_HOME": "/x/cfg", "BS_CONFIG_HOME": "/bs/config"}
		p, err := ResolvePathsWith("linux", func(k string) string { return envLocal[k] }, home)
		if err != nil {
			t.Fatalf("ResolvePathsWith: %v", err)
		}
		if p.ConfigDir != "/bs/config" {
			t.Errorf("ConfigDir = %q, want /bs/config", p.ConfigDir)
		}
	})

	t.Run("windows uses AppData", func(t *testing.T) {
		envWin := map[string]string{"AppData": `C:\Users\k\AppData\Roaming`, "LocalAppData": `C:\Users\k\AppData\Local`}
		p, err := ResolvePathsWith("windows", func(k string) string { return envWin[k] }, `C:\Users\k`)
		if err != nil {
			t.Fatalf("ResolvePathsWith: %v", err)
		}
		if want := `C:\Users\k\AppData\Roaming\bootstrap`; p.ConfigDir != want {
			t.Errorf("ConfigDir = %q, want %q", p.ConfigDir, want)
		}
		if want := `C:\Users\k\AppData\Local\Bootstrap\state`; p.StateDir != want {
			t.Errorf("StateDir = %q, want %q", p.StateDir, want)
		}
	})

	t.Run("home falls back to os.UserHomeDir", func(t *testing.T) {
		// getenv 给不出 HOME 时应回退到 os.UserHomeDir；只有在两者都失败时才报错。
		p, err := ResolvePathsWith("linux", func(string) string { return "" }, "")
		if err != nil {
			if p.ConfigDir != "" {
				t.Errorf("failed resolution must not return paths: %+v", p)
			}
			return
		}
		if !filepath.IsAbs(p.ConfigDir) {
			t.Errorf("ConfigDir = %q, want absolute path", p.ConfigDir)
		}
	})
}

func TestPathsValidate(t *testing.T) {
	p := Paths{ConfigDir: "relative/path", StateDir: "/s", CacheDir: "/c"}
	if err := p.Validate(); err == nil {
		t.Error("relative config dir should fail validation")
	}
}

func TestProjectConfigFile(t *testing.T) {
	p := Paths{ConfigDir: "/cfg"}
	if got := p.ProjectConfigFile("/proj"); got != filepath.Join("/proj", ".bootstrap.yaml") {
		t.Errorf("ProjectConfigFile = %q", got)
	}
	if got := p.ProjectConfigFile(""); got != "" {
		t.Errorf("empty dir should disable project layer, got %q", got)
	}
}

// TestLoadLayering 验证"默认 < 用户 < 项目 < 环境变量"这条优先级链。
func TestLoadLayering(t *testing.T) {
	tmp := t.TempDir()
	paths := Paths{
		ConfigDir: filepath.Join(tmp, "config"),
		StateDir:  filepath.Join(tmp, "state"),
		CacheDir:  filepath.Join(tmp, "cache"),
		HomeDir:   filepath.Join(tmp, "home"),
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	projectDir := filepath.Join(tmp, "proj")
	projectFile := paths.ProjectConfigFile(projectDir)

	// 第一层：只有默认值。
	res, err := Load(LoadOptions{Paths: paths, GOOS: "linux", Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if got := res.Config.Tool.DefaultProfile; got != "base" {
		t.Errorf("default tool profile = %q, want base", got)
	}
	if len(res.Sources) != 0 {
		t.Errorf("expected no config sources, got %v", res.Sources)
	}
	if res.Config.Tool.Manifest.UserDirs[0] != paths.ToolsDir() {
		t.Errorf("tools dir default = %v, want %s", res.Config.Tool.Manifest.UserDirs, paths.ToolsDir())
	}

	// 第二层：用户配置覆盖默认值。
	write(paths.ConfigFile(), "version: 1\nlog:\n  level: warn\nrepo:\n  root: ~/code\n")
	res, err = Load(LoadOptions{Paths: paths, GOOS: "linux", ProjectDir: projectDir, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if res.Config.Log.Level != "warn" {
		t.Errorf("log.level = %q, want warn", res.Config.Log.Level)
	}
	if want := filepath.Join(paths.HomeDir, "code"); res.Config.Repo.Root != want {
		t.Errorf("repo.root = %q, want %q (should expand ~)", res.Config.Repo.Root, want)
	}

	// 第三层：项目配置覆盖用户配置。
	write(projectFile, "log:\n  level: debug\n")
	res, err = Load(LoadOptions{Paths: paths, GOOS: "linux", ProjectDir: projectDir, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	if res.Config.Log.Level != "debug" {
		t.Errorf("log.level = %q, want debug (project layer must win)", res.Config.Log.Level)
	}
	if len(res.Sources) != 2 {
		t.Errorf("sources = %v, want 2 entries", res.Sources)
	}

	// 第四层：环境变量覆盖项目配置。
	env := map[string]string{"BS_LOG_LEVEL": "error"}
	res, err = Load(LoadOptions{Paths: paths, GOOS: "linux", ProjectDir: projectDir, Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatalf("load env: %v", err)
	}
	if res.Config.Log.Level != "error" {
		t.Errorf("log.level = %q, want error (env must win)", res.Config.Log.Level)
	}
}

func TestLoadExplicitFile(t *testing.T) {
	tmp := t.TempDir()
	paths := Paths{
		ConfigDir: filepath.Join(tmp, "config"),
		StateDir:  filepath.Join(tmp, "state"),
		CacheDir:  filepath.Join(tmp, "cache"),
		HomeDir:   filepath.Join(tmp, "home"),
	}
	explicit := filepath.Join(tmp, "custom.yaml")
	if err := os.WriteFile(explicit, []byte("log:\n  level: warn\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := Load(LoadOptions{Paths: paths, GOOS: "linux", ExplicitFile: explicit, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if res.Config.Log.Level != "warn" {
		t.Errorf("log.level = %q, want warn", res.Config.Log.Level)
	}
	if len(res.Sources) != 1 || res.Sources[0] != explicit {
		t.Errorf("sources = %v, want [%s]", res.Sources, explicit)
	}

	// 用户级配置存在也不应被读取（--config 绕过分层搜索）。
	if err := os.MkdirAll(paths.ConfigDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte("log:\n  level: error\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err = Load(LoadOptions{Paths: paths, GOOS: "linux", ExplicitFile: explicit, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if res.Config.Log.Level != "warn" {
		t.Errorf("explicit file must bypass the layered search, got %q", res.Config.Log.Level)
	}

	if _, err := Load(LoadOptions{Paths: paths, GOOS: "linux", ExplicitFile: filepath.Join(tmp, "missing.yaml")}); err == nil {
		t.Error("missing --config file should be an error")
	}
}

// TestLoadPartialNestedBlockKeepsDefaults：配置文件只写嵌套块的某个字段时，
// 同块里没写的字段必须保持内置默认值——否则用户只加一行 subgroup_hosts，
// 就会把 depth 变成 0 并撞上校验错误。
func TestLoadPartialNestedBlockKeepsDefaults(t *testing.T) {
	tmp := t.TempDir()
	paths := Paths{
		ConfigDir: filepath.Join(tmp, "config"),
		StateDir:  filepath.Join(tmp, "state"),
		CacheDir:  filepath.Join(tmp, "cache"),
		HomeDir:   filepath.Join(tmp, "home"),
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "repo:\n  scan:\n    subgroup_hosts: [\"gitlab.com\", \"git.in.chaitin.net\"]\n"
	if err := os.WriteFile(paths.ConfigFile(), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := Load(LoadOptions{Paths: paths, GOOS: "linux", Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	scan := res.Config.Repo.Scan
	if scan.Depth != 2 || scan.SubgroupDepth != 4 {
		t.Errorf("未写的字段应保持默认：%+v", scan)
	}
	if len(scan.SubgroupHosts) != 2 || scan.SubgroupHosts[1] != "git.in.chaitin.net" {
		t.Errorf("subgroup_hosts 未被覆盖：%v", scan.SubgroupHosts)
	}
}

func TestLoadInvalidConfig(t *testing.T) {
	tmp := t.TempDir()
	paths := Paths{
		ConfigDir: filepath.Join(tmp, "config"),
		StateDir:  filepath.Join(tmp, "state"),
		CacheDir:  filepath.Join(tmp, "cache"),
		HomeDir:   filepath.Join(tmp, "home"),
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte("repo:\n  pull:\n    strategy: bogus\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(LoadOptions{Paths: paths, GOOS: "linux", Getenv: func(string) string { return "" }})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "repo.pull.strategy") {
		t.Errorf("error should mention repo.pull.strategy, got %v", err)
	}
}

// TestWriteDefaultIsIdempotent 断言重复生成默认配置不会修改已有文件。
func TestWriteDefaultIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	paths := Paths{
		ConfigDir: filepath.Join(tmp, "config"),
		StateDir:  filepath.Join(tmp, "state"),
		CacheDir:  filepath.Join(tmp, "cache"),
		HomeDir:   filepath.Join(tmp, "home"),
	}
	path, created, err := WriteDefault(paths, false)
	if err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	if !created {
		t.Fatal("first WriteDefault should create the file")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	_, created, err = WriteDefault(paths, false)
	if err != nil {
		t.Fatalf("WriteDefault second: %v", err)
	}
	if created {
		t.Error("second WriteDefault must not modify the existing file")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("file mtime changed on the no-op path")
	}

	// --force 会覆盖，且留下备份。
	if _, created, err = WriteDefault(paths, true); err != nil {
		t.Fatalf("WriteDefault force: %v", err)
	}
	if !created {
		t.Error("force should rewrite the file")
	}
	backups, _ := filepath.Glob(path + ".bs-backup.*")
	if len(backups) != 1 {
		t.Errorf("expected exactly one backup, got %v", backups)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"bad log level", func(c *Config) { c.Log.Level = "loud" }, "log.level"},
		{"bad layout", func(c *Config) { c.Repo.Layout = "no-repo-placeholder" }, "{{.Repo}}"},
		{"layout escapes root", func(c *Config) { c.Repo.Layout = "../../{{.Repo}}" }, "escapes"},
		{"bad protocol", func(c *Config) { c.Repo.Clone.Protocol = "ftp" }, "repo.clone.protocol"},
		{"bad strategy", func(c *Config) { c.Repo.Pull.Strategy = "yolo" }, "repo.pull.strategy"},
		{"missing default profile", func(c *Config) { c.Proxy.DefaultProfile = "nope" }, "proxy.default_profile"},
		{"empty profile", func(c *Config) { c.Proxy.Profiles = map[string]proxyenv.Profile{"x": {}} }, "empty"},
		{"bad proxy scheme", func(c *Config) {
			c.Proxy.Profiles = map[string]proxyenv.Profile{"x": {HTTPProxy: "gopher://h:1"}}
		}, "scheme"},
		{"bad kind", func(c *Config) { c.Tool.AllowSystemFor = []string{"nonsense"} }, "allow_system_for"},
		{"bad progress", func(c *Config) { c.UI.Progress = "fancy" }, "ui.progress"},
		{"bad version", func(c *Config) { c.Version = 99 }, "version"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Proxy.Profiles = map[string]proxyenv.Profile{}
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}
