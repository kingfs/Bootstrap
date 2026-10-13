// Package config 负责配置目录解析、分层装载与校验。
//
// 目录策略（docs/design.md §5.1，ADR-0001）：
//
//	Linux / macOS : $BS_CONFIG_HOME > $BOOTSTRAP_CONFIG_HOME > $XDG_CONFIG_HOME/bootstrap > ~/.config/bootstrap
//	Windows       : $BS_CONFIG_HOME > $BOOTSTRAP_CONFIG_HOME > %AppData%\bootstrap
//
// state 与 cache 目录同理，遵循 XDG 规范。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func realGOOS() string { return runtime.GOOS }

func envOrEmpty(key string) string { return os.Getenv(key) }

func defaultHome(getenv EnvFunc) string {
	if h := getenv("HOME"); h != "" {
		return h
	}
	if h := getenv("USERPROFILE"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// Paths 是本工具会用到的全部目录。
type Paths struct {
	ConfigDir string
	StateDir  string
	CacheDir  string
	// HomeDir 是解析 ~ 的基准目录。
	HomeDir string
}

// EnvFunc 是读取环境变量的函数（便于测试注入）。
type EnvFunc func(string) string

// ResolvePaths 使用真实环境解析目录。
//
// 必须把 os.Getenv 传下去：用户依赖 BS_CONFIG_HOME 等变量做重定向
// （测试与多环境隔离都会用到）。
func ResolvePaths() (Paths, error) {
	return ResolvePathsWith(realGOOS(), os.Getenv, "")
}

// ResolvePathsWith 使用指定 goos / 环境变量 / home 解析目录，便于跨平台单测。
func ResolvePathsWith(goos string, getenv EnvFunc, home string) (Paths, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if home == "" {
		home = defaultHome(getenv)
	}
	if home == "" {
		return Paths{}, errors.New("cannot determine home directory (set HOME or BS_CONFIG_HOME)")
	}

	var configDir, stateDir, cacheDir string
	if goos == "windows" {
		appData := firstNonEmpty(getenv("AppData"), filepath.Join(home, "AppData", "Roaming"))
		localAppData := firstNonEmpty(getenv("LocalAppData"), filepath.Join(home, "AppData", "Local"))
		configDir = firstNonEmpty(getenv("BS_CONFIG_HOME"), getenv("BOOTSTRAP_CONFIG_HOME"), filepath.Join(appData, "bootstrap"))
		stateDir = firstNonEmpty(getenv("BS_STATE_HOME"), filepath.Join(localAppData, "Bootstrap", "state"))
		cacheDir = firstNonEmpty(getenv("BS_CACHE_HOME"), filepath.Join(localAppData, "Bootstrap", "cache"))
	} else {
		xdgConfig := firstNonEmpty(getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config"))
		xdgState := firstNonEmpty(getenv("XDG_STATE_HOME"), filepath.Join(home, ".local", "state"))
		xdgCache := firstNonEmpty(getenv("XDG_CACHE_HOME"), filepath.Join(home, ".cache"))
		configDir = firstNonEmpty(getenv("BS_CONFIG_HOME"), getenv("BOOTSTRAP_CONFIG_HOME"), filepath.Join(xdgConfig, "bootstrap"))
		stateDir = firstNonEmpty(getenv("BS_STATE_HOME"), filepath.Join(xdgState, "bootstrap"))
		cacheDir = firstNonEmpty(getenv("BS_CACHE_HOME"), filepath.Join(xdgCache, "bootstrap"))
	}

	return Paths{
		ConfigDir: filepath.Clean(configDir),
		StateDir:  filepath.Clean(stateDir),
		CacheDir:  filepath.Clean(cacheDir),
		HomeDir:   home,
	}, nil
}

// ConfigFile 返回用户级配置文件路径。
func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "config.yaml") }

// SystemConfigFile 返回系统级配置文件路径（只读层）。
func (p Paths) SystemConfigFile(goos string) string {
	switch goos {
	case "windows":
		return filepath.Join(firstNonEmpty(envOrEmpty("ProgramData"), `C:\ProgramData`), "Bootstrap", "config.yaml")
	case "darwin":
		return "/Library/Application Support/Bootstrap/config.yaml"
	default:
		return "/etc/bootstrap/config.yaml"
	}
}

// ProjectConfigFile 返回项目级配置文件路径（当前目录下，存在才生效）。
func (p Paths) ProjectConfigFile(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ".bootstrap.yaml")
}

// ToolsDir 是用户自定义工具清单目录。
func (p Paths) ToolsDir() string { return filepath.Join(p.ConfigDir, "tools") }

// ShellSnippetsDir 是用户 shell 片段目录。
func (p Paths) ShellSnippetsDir() string { return filepath.Join(p.ConfigDir, "shell") }

// StateFile 是活跃 profile / 安装记录等状态文件。
func (p Paths) StateFile() string { return filepath.Join(p.StateDir, "state.yaml") }

// LogsDir 是执行日志目录。
func (p Paths) LogsDir() string { return filepath.Join(p.StateDir, "logs") }

// Describe 返回可打印的路径清单，用于 `bs config path`。
func (p Paths) Describe(goos string) [][2]string {
	return [][2]string{
		{"config", p.ConfigDir},
		{"config file", p.ConfigFile()},
		{"system config", p.SystemConfigFile(goos)},
		{"tools dir", p.ToolsDir()},
		{"shell snippets", p.ShellSnippetsDir()},
		{"state", p.StateDir},
		{"state file", p.StateFile()},
		{"cache", p.CacheDir},
		{"home", p.HomeDir},
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ErrNoHome 表示无法确定家目录。
var ErrNoHome = errors.New("cannot determine home directory")

// Validate 检查路径是否可以正常使用（用于 bs doctor / bs config validate）。
func (p Paths) Validate() error {
	if p.ConfigDir == "" || p.StateDir == "" || p.CacheDir == "" {
		return fmt.Errorf("incomplete paths: %+v", p)
	}
	if !filepath.IsAbs(p.ConfigDir) {
		return fmt.Errorf("config dir %q is not absolute", p.ConfigDir)
	}
	return nil
}
