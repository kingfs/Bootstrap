package config

import (
	"fmt"
	"strings"
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

// String 实现 fmt.Stringer，便于把 kind 当作字符串比较与展示。
func (k Kind) String() string { return string(k) }

// AllKinds 是全部合法的 kind，用于校验配置与清单。
var AllKinds = []Kind{KindRuntime, KindVersionMgr, KindPkgMgr, KindCLI, KindDevDep, KindService, KindGUI, KindAgent}

// Config 是强类型的整体配置。
//
// 业务代码只读这个结构体，不直接调用 viper.Get —— 见 docs/design.md §5.2。
type Config struct {
	Version int           `mapstructure:"version" yaml:"version"`
	Log     LogConfig     `mapstructure:"log" yaml:"log"`
	Repo    RepoConfig    `mapstructure:"repo" yaml:"repo"`
	Proxy   ProxyConfig   `mapstructure:"proxy" yaml:"proxy"`
	Tool    ToolConfig    `mapstructure:"tool" yaml:"tool"`
	Shell   ShellConfig   `mapstructure:"shell" yaml:"shell"`
	UI      UIConfig      `mapstructure:"ui" yaml:"ui"`
	Time    TimeoutConfig `mapstructure:"timeouts" yaml:"timeouts"`
}

// TimeoutConfig 是各环节的时限，写成 Go 时长字符串（"30s"、"10m"）。
//
// 留空表示"用该环节的内置默认值"（外部命令 10min、版本探测 5s、元数据查询 60s、
// 下载 15min）——默认值散落在各自包里，这里只负责覆盖，不重复定义一遍。
type TimeoutConfig struct {
	// Global 是整个 bs 命令的总时限：到点后所有子进程一起被停掉。
	Global string `mapstructure:"global" yaml:"global"`
	// Command 是单个外部命令（git/go/apt/...）的时限。
	Command string `mapstructure:"command" yaml:"command"`
	// Probe 是单次版本探测的时限。
	Probe string `mapstructure:"probe" yaml:"probe"`
	// Lookup 是"查最新 tag / 校验和文件"这类元数据请求的时限。
	Lookup string `mapstructure:"lookup" yaml:"lookup"`
	// Download 是单个归档下载的时限。
	Download string `mapstructure:"download" yaml:"download"`
}

// Timeouts 是 TimeoutConfig 解析后的形式；0 表示"用内置默认值"。
type Timeouts struct {
	Global   time.Duration
	Command  time.Duration
	Probe    time.Duration
	Lookup   time.Duration
	Download time.Duration
}

// ResolvedTimeouts 解析时限配置。字段留空即为 0（用内置默认值）。
func (c *Config) ResolvedTimeouts() (Timeouts, error) {
	var out Timeouts
	for _, f := range []struct {
		key string
		raw string
		dst *time.Duration
	}{
		{"timeouts.global", c.Time.Global, &out.Global},
		{"timeouts.command", c.Time.Command, &out.Command},
		{"timeouts.probe", c.Time.Probe, &out.Probe},
		{"timeouts.lookup", c.Time.Lookup, &out.Lookup},
		{"timeouts.download", c.Time.Download, &out.Download},
	} {
		d, err := ParseTimeout(f.key, f.raw)
		if err != nil {
			return Timeouts{}, err
		}
		*f.dst = d
	}
	return out, nil
}

// ParseTimeout 解析一个时限字符串：空串合法（返回 0＝用默认值），否则必须是正数时长。
//
// 只接受带单位的写法（"30s"/"10m"/"1h"）：裸数字在不同环节含义不同（秒？分？），
// 与其猜，不如要求写清楚。
func ParseTimeout(key, raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a valid duration (try 30s, 10m, 1h)", key, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be > 0", key)
	}
	return d, nil
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
	// Completion 为真时，`bs shell init` 会在受管内容末尾追加"加载 bs 补全"的片段。
	// 默认关：理由与 AutoProxy 相同——不在用户没要求时改变每个新 shell 的启动行为。
	// 打开它的正规入口是 `bs completion install [shell]`（内部就是把这个键置真）。
	Completion bool `mapstructure:"completion" yaml:"completion"`
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
			// 清单模板里的 {{versions.<stack>}} 用这里的值。默认给"最新/稳定"，
			// 由各自的版本管理器在安装时解析；想可复现就写死具体版本。
			Versions: map[string]string{
				"golang": "latest",
				"nodejs": "latest",
				"python": "3.12",
				"rust":   "stable",
			},
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
