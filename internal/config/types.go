package config

import (
	"time"

	"github.com/kingfs/Bootstrap/pkg/proxyenv"
)

// Kind 是工具的功能类型（与 docs/design.md §6.1 的枚举一致）。
type Kind string

// 支持的工具功能类型。
const (
	KindRuntime    Kind = "runtime"
	KindVersionMgr Kind = "versionmgr"
	KindPkgMgr     Kind = "pkgmgr"
	KindCLI        Kind = "cli"
	KindDevDep     Kind = "devdep"
	KindService    Kind = "service"
	KindGUI        Kind = "gui"
	KindAgent      Kind = "agent"
)

// AllKinds 是全部合法的 kind，用于校验配置与清单。
var AllKinds = []Kind{KindRuntime, KindVersionMgr, KindPkgMgr, KindCLI, KindDevDep, KindService, KindGUI, KindAgent}

// Config 是强类型的整体配置。
//
// 业务代码只读这个结构体，不直接调用 viper.Get —— 见 docs/design.md §5.2。
type Config struct {
	Version int         `mapstructure:"version" yaml:"version"`
	Log     LogConfig   `mapstructure:"log" yaml:"log"`
	Repo    RepoConfig  `mapstructure:"repo" yaml:"repo"`
	Proxy   ProxyConfig `mapstructure:"proxy" yaml:"proxy"`
	Tool    ToolConfig  `mapstructure:"tool" yaml:"tool"`
	Shell   ShellConfig `mapstructure:"shell" yaml:"shell"`
	UI      UIConfig    `mapstructure:"ui" yaml:"ui"`
}

// LogConfig 控制日志输出。
type LogConfig struct {
	Level string `mapstructure:"level" yaml:"level"`
	Color string `mapstructure:"color" yaml:"color"`
}

// RepoConfig 控制仓库拉取行为（需求 1）。
type RepoConfig struct {
	Root        string      `mapstructure:"root" yaml:"root"`
	Layout      string      `mapstructure:"layout" yaml:"layout"`
	DefaultHost string      `mapstructure:"default_host" yaml:"default_host"`
	Hosts       []string    `mapstructure:"hosts" yaml:"hosts"`
	Clone       CloneConfig `mapstructure:"clone" yaml:"clone"`
	Pull        PullConfig  `mapstructure:"pull" yaml:"pull"`
	Sync        SyncConfig  `mapstructure:"sync" yaml:"sync"`
}

// CloneConfig 控制 git clone。
type CloneConfig struct {
	Protocol  string   `mapstructure:"protocol" yaml:"protocol"`
	Depth     int      `mapstructure:"depth" yaml:"depth"`
	ExtraArgs []string `mapstructure:"extra_args" yaml:"extra_args"`
}

// PullConfig 控制 git pull。
type PullConfig struct {
	Strategy  string `mapstructure:"strategy" yaml:"strategy"` // ff-only|rebase|merge
	Autostash bool   `mapstructure:"autostash" yaml:"autostash"`
	Prune     bool   `mapstructure:"prune" yaml:"prune"`
}

// SyncConfig 控制批量同步。
type SyncConfig struct {
	Jobs int `mapstructure:"jobs" yaml:"jobs"`
}

// ProxyConfig 是代理 profile 集合（需求 5、6）。
type ProxyConfig struct {
	DefaultProfile   string                      `mapstructure:"default_profile" yaml:"default_profile"`
	AutoApply        bool                        `mapstructure:"auto_apply" yaml:"auto_apply"`
	OverrideExisting bool                        `mapstructure:"override_existing" yaml:"override_existing"`
	Profiles         map[string]proxyenv.Profile `mapstructure:"profiles" yaml:"profiles"`
}

// ToolConfig 控制工具清单与安装行为（需求 3、7、8、9）。
type ToolConfig struct {
	Manifest       ManifestConfig    `mapstructure:"manifest" yaml:"manifest"`
	DefaultProfile string            `mapstructure:"default_profile" yaml:"default_profile"`
	DefaultVia     []string          `mapstructure:"default_via" yaml:"default_via"`
	AllowSystemFor []string          `mapstructure:"allow_system_for" yaml:"allow_system_for"`
	Managers       map[string]string `mapstructure:"managers" yaml:"managers"`
	Versions       map[string]string `mapstructure:"versions" yaml:"versions"`
	ExtraPath      []string          `mapstructure:"extra_path" yaml:"extra_path"`
}

// ManifestConfig 控制清单来源（ADR-0002：内置 + 本地覆盖）。
type ManifestConfig struct {
	Builtin  bool     `mapstructure:"builtin" yaml:"builtin"`
	UserDirs []string `mapstructure:"user_dirs" yaml:"user_dirs"`
}

// ShellConfig 控制 shell 受管块与片段（ADR-0007/0012）。
type ShellConfig struct {
	SnippetsDir string            `mapstructure:"snippets_dir" yaml:"snippets_dir"`
	RCFiles     map[string]string `mapstructure:"rc_files" yaml:"rc_files"`
	AutoPath    bool              `mapstructure:"auto_path" yaml:"auto_path"`
	AutoProxy   bool              `mapstructure:"auto_proxy" yaml:"auto_proxy"`
}

// UIConfig 控制交互行为。
type UIConfig struct {
	Confirm  bool   `mapstructure:"confirm" yaml:"confirm"`
	Progress string `mapstructure:"progress" yaml:"progress"`
}

// Defaults 返回内置默认配置。
//
// 注意：不在这里设置 proxy.default_profile —— 默认"不使用代理"更安全，
// 由用户或 `bs proxy on <name>` 显式激活。
func Defaults() Config {
	return Config{
		Version: 1,
		Log:     LogConfig{Level: "info", Color: "auto"},
		Repo: RepoConfig{
			Layout:      "{{.Host}}/{{.Owner}}/{{.Repo}}",
			DefaultHost: "github.com",
			Hosts:       []string{"github.com", "gitlab.com", "gitee.com", "codeberg.org", "bitbucket.org"},
			Clone:       CloneConfig{Protocol: "https", Depth: 0},
			Pull:        PullConfig{Strategy: "ff-only", Prune: true},
			Sync:        SyncConfig{Jobs: 4},
		},
		Proxy: ProxyConfig{
			AutoApply:        true,
			OverrideExisting: false,
			Profiles:         map[string]proxyenv.Profile{},
		},
		Tool: ToolConfig{
			Manifest:       ManifestConfig{Builtin: true},
			DefaultProfile: "base",
			AllowSystemFor: []string{string(KindDevDep), string(KindService), string(KindCLI)},
			Managers: map[string]string{
				"golang":    "g",
				"nodejs":    "fnm",
				"python":    "uv",
				"rust":      "rustup",
				"universal": "mise",
			},
			Versions:  map[string]string{},
			ExtraPath: []string{"~/.local/bin", "~/go/bin"},
		},
		Shell: ShellConfig{
			RCFiles:  map[string]string{},
			AutoPath: true,
		},
		UI: UIConfig{Confirm: true, Progress: "auto"},
	}
}

// CloneTimeout 等常量集中在这里，避免散落在业务代码中。
const (
	// DefaultProbeTimeout 是单次版本探测的超时。
	DefaultProbeTimeout = 5 * time.Second
	// DefaultSyncJobs 是并发同步的默认并行度。
	DefaultSyncJobs = 4
)
