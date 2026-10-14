package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/viper"

	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/proxyenv"
)

// LoadOptions 是装载配置所需的全部输入。
type LoadOptions struct {
	Paths Paths
	// GOOS 为空时使用 runtime.GOOS。
	GOOS string
	// ExplicitFile 对应 --config；非空时只读该文件（缺失即报错）。
	ExplicitFile string
	// ProjectDir 用于发现项目级 .bootstrap.yaml；为空表示不启用项目层。
	ProjectDir string
	Getenv     EnvFunc
}

// LoadResult 是装载结果。
type LoadResult struct {
	Config  *Config
	Sources []string // 实际读取到的配置文件（按覆盖顺序）
	Paths   Paths
}

// envBindings 是显式支持的环境变量覆盖。
//
// viper 的 AutomaticEnv 对 Unmarshal 不可靠，因此这里逐个 BindEnv，
// 保证"配置 → 环境变量 → 命令行 flag"这条优先级链真的可预期。
var envBindings = map[string][]string{
	"log.level":             {"BS_LOG_LEVEL"},
	"log.color":             {"BS_LOG_COLOR", "NO_COLOR"},
	"repo.root":             {"BS_REPO_ROOT"},
	"repo.default_host":     {"BS_REPO_DEFAULT_HOST"},
	"repo.clone.protocol":   {"BS_REPO_CLONE_PROTOCOL"},
	"repo.pull.strategy":    {"BS_REPO_PULL_STRATEGY"},
	"proxy.default_profile": {"BS_PROXY_PROFILE", "BS_PROXY_DEFAULT_PROFILE"},
	"proxy.auto_apply":      {"BS_PROXY_AUTO_APPLY"},
	"tool.default_profile":  {"BS_TOOL_PROFILE"},
	"tool.manifest.builtin": {"BS_MANIFEST_BUILTIN"},
	"shell.snippets_dir":    {"BS_SHELL_SNIPPETS_DIR"},
	"timeouts.global":       {"BS_TIMEOUT"},
	"timeouts.command":      {"BS_TIMEOUT_COMMAND"},
	"timeouts.probe":        {"BS_TIMEOUT_PROBE"},
	"timeouts.lookup":       {"BS_TIMEOUT_LOOKUP"},
	"timeouts.download":     {"BS_TIMEOUT_DOWNLOAD"},
}

// Load 按"默认值 → 系统级 → 用户级 → 项目级 → 环境变量"的顺序合并配置。
func Load(opts LoadOptions) (*LoadResult, error) {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if err := opts.Paths.Validate(); err != nil {
		return nil, err
	}

	v := viper.New()
	v.SetConfigType("yaml")

	var sources []string
	if opts.ExplicitFile != "" {
		path := fsx.ExpandPath(opts.ExplicitFile, opts.Paths.HomeDir)
		if !fsx.IsFile(path) {
			return nil, fmt.Errorf("config file not found: %s", path)
		}
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		sources = append(sources, path)
	} else {
		layers := []string{opts.Paths.SystemConfigFile(opts.GOOS), opts.Paths.ConfigFile()}
		if p := opts.Paths.ProjectConfigFile(opts.ProjectDir); p != "" {
			layers = append(layers, p)
		}
		for _, f := range layers {
			if !fsx.IsFile(f) {
				continue
			}
			v.SetConfigFile(f)
			if err := v.MergeInConfig(); err != nil {
				return nil, fmt.Errorf("read config %s: %w", f, err)
			}
			sources = append(sources, f)
		}
	}

	bindEnv(v, opts.Getenv)

	cfg := Defaults()
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	ApplyDerived(&cfg, opts.Paths)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &LoadResult{Config: &cfg, Sources: sources, Paths: opts.Paths}, nil
}

// bindEnv 把环境变量注入配置。
//
// 用 Set 而不是仅 BindEnv 的原因：Set 的优先级稳定高于配置文件，且可以配合
// 注入的 getenv 使用（viper.BindEnv 在取值时只认进程环境，测试无法注入）。
// 命令行 flag 由 internal/cli 在装载之后覆盖，因此仍然保持"flag > env"。
func bindEnv(v *viper.Viper, getenv EnvFunc) {
	for key, names := range envBindings {
		for _, name := range names {
			if strings.TrimSpace(getenv(name)) == "" {
				continue
			}
			v.Set(key, getenv(name))
			_ = v.BindEnv(key, name)
		}
	}
}

// ApplyDerived 填充依赖其它字段才能确定的默认值，并展开 ~。
//
// 导出给 app 用：`bs config set` 需要"内置默认值 + 派生字段"作为类型模板与
// "这个值其实是默认值"的比较基准，自己抄一遍必然走样。
func ApplyDerived(cfg *Config, paths Paths) {
	home := paths.HomeDir

	if len(cfg.Tool.Manifest.UserDirs) == 0 {
		cfg.Tool.Manifest.UserDirs = []string{paths.ToolsDir()}
	}
	for i, d := range cfg.Tool.Manifest.UserDirs {
		cfg.Tool.Manifest.UserDirs[i] = fsx.ExpandPath(d, home)
	}
	if cfg.Shell.SnippetsDir == "" {
		cfg.Shell.SnippetsDir = paths.ShellSnippetsDir()
	}
	cfg.Shell.SnippetsDir = fsx.ExpandPath(cfg.Shell.SnippetsDir, home)

	cfg.Repo.Root = fsx.ExpandPath(cfg.Repo.Root, home)
	for i, p := range cfg.Tool.ExtraPath {
		cfg.Tool.ExtraPath[i] = fsx.ExpandPath(p, home)
	}
	if len(cfg.Shell.RCFiles) > 0 {
		expanded := make(map[string]string, len(cfg.Shell.RCFiles))
		for shell, file := range cfg.Shell.RCFiles {
			if strings.TrimSpace(file) == "" {
				continue
			}
			expanded[shell] = fsx.ExpandPath(file, home)
		}
		cfg.Shell.RCFiles = expanded
	}
	if cfg.Repo.Sync.Jobs <= 0 {
		cfg.Repo.Sync.Jobs = DefaultSyncJobs
	}
	if strings.TrimSpace(cfg.Log.Level) == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Proxy.Profiles == nil {
		cfg.Proxy.Profiles = map[string]proxyenv.Profile{}
	}
	// NO_COLOR 是跨工具通用约定：只要存在就关闭颜色，且优先于配置文件。
	if v := strings.TrimSpace(os.Getenv("NO_COLOR")); v != "" || envExists("NO_COLOR") {
		cfg.Log.Color = "never"
	}
}

func envExists(key string) bool {
	_, ok := os.LookupEnv(key)
	return ok
}

// DirExists 报告目录是否存在（供 doctor 使用）。
func DirExists(p string) bool {
	st, err := os.Stat(filepath.Clean(p))
	return err == nil && st.IsDir()
}
