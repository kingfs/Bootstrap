// Package app 是应用层：编排用例，不依赖 cobra，也不直接向终端打印。
//
// 依赖方向：internal/cli → internal/app → internal/domain(接口) ← 基础设施实现。
package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kingfs/Bootstrap/internal/buildinfo"
	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/internal/install"
	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/internal/proxy"
	"github.com/kingfs/Bootstrap/internal/state"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
	"github.com/kingfs/Bootstrap/pkg/hostinfo"
)

// App 是一次进程运行期间共享的依赖容器。
type App struct {
	Paths         config.Paths
	Cfg           *config.Config
	ConfigSources []string
	Platform      hostinfo.Platform
	Store         *state.Store
	Runner        *cmdx.Runner
	Log           *slog.Logger

	Out io.Writer
	Err io.Writer

	// 全局 flag 的解析结果。
	DryRun  bool
	Yes     bool
	JSON    bool
	NoColor bool
	Verbose int

	// Fallback 控制"安装失败后是否允许换一种方式"（ADR-0013）。
	// nil 表示按是否交互自动决定（见 fallbackEnabled）。
	Fallback *bool
	// NonInteractive 表示当前不是交互式会话（管道 / CI / --non-interactive）。
	NonInteractive bool
	// DefaultViaOverride 是 --via 指定的安装方式（覆盖配置里的 tool.default_via）。
	DefaultViaOverride string
	// Progress 在每个工具开始处理前被调用（由 cli 按 ui.progress 决定是否设置）。
	Progress func(install.Step)
	// GOOSOverride 让清单按"另一个系统"加载与校验（`bs tool manifest --os darwin`），
	// 用于在 Linux/CI 上检查 macOS/Windows 的清单是否写错。
	GOOSOverride string

	// CurrentVersion 是当前二进制的版本（自更新时用来判断"要不要换"）。
	CurrentVersion string
	// ExePath 是自更新要替换的二进制路径（空 = os.Executable()；测试注入用）。
	ExePath string
	// TrustStore 覆盖"系统 CA 证书"探测（nil = hostinfo.DetectTrustStore；测试注入用）。
	TrustStore func() hostinfo.TrustStore

	// loaded 缓存已装载的清单，避免一次命令里重复解析 YAML。
	loaded *manifest.Loaded
}

// New 构造 App。
func New(paths config.Paths, cfg *config.Config, sources []string, platform hostinfo.Platform, log *slog.Logger, out, errw io.Writer) *App {
	// 每个进程进来先应用一次 GitHub 镜像前缀（BS_GITHUB_API / BS_GITHUB_ASSET_BASE）：
	// 命令层不该各自记得这件事，漏一个就会"有的命令走镜像、有的直连 GitHub"。
	applyGitHubMirror()
	store := state.NewStore(paths.StateFile())
	a := &App{
		Paths:          paths,
		Cfg:            cfg,
		ConfigSources:  sources,
		Platform:       platform,
		Store:          store,
		Log:            log,
		Out:            out,
		Err:            errw,
		CurrentVersion: buildinfo.Get().Version,
	}
	a.Runner = &cmdx.Runner{
		Log:            log,
		DefaultTimeout: cmdx.DefaultCommandTimeout,
		Stdout:         nil,
		Secrets:        a.secrets(),
	}
	return a
}

// secrets 返回需要在日志中脱敏的字符串（代理 URL 里的凭据）。
func (a *App) secrets() []string {
	var out []string
	for _, p := range a.Cfg.Proxy.Profiles {
		for _, v := range []string{p.HTTPProxy, p.HTTPSProxy, p.AllProxy} {
			if strings.Contains(v, "@") {
				out = append(out, v)
			}
		}
	}
	return out
}

// ProxyService 返回代理服务（每次构造，成本可忽略）。
func (a *App) ProxyService() *proxy.Service {
	return proxy.NewService(a.Cfg, a.Store)
}

// NetworkEnv 返回应注入网络类子进程的代理环境变量。
func (a *App) NetworkEnv() map[string]string {
	env, err := a.ProxyService().NetworkEnv()
	if err != nil {
		a.Log.Warn("resolve proxy env", "err", err)
		return nil
	}
	return env
}

// mergeMaps 合并 map，后者覆盖前者（不修改入参）。
func mergeMaps(base, overlay map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// VerboseEnabled 报告是否处于详细模式。
func (a *App) VerboseEnabled() bool { return a.Verbose > 0 }

// Now 便于测试替换时间源。
var Now = func() time.Time { return time.Now().UTC() }

// Stamp 返回用于备份文件的时间戳。
func Stamp() string { return Now().Format("20060102T150405") }

// EnsureRuntimeDirs 创建 state/cache 目录（只读命令不需要调用）。
func (a *App) EnsureRuntimeDirs() error {
	for _, dir := range []string{a.Paths.StateDir, a.Paths.CacheDir, a.Paths.LogsDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// ExpandPath 按当前 home 展开 ~。
func (a *App) ExpandPath(p string) string {
	if strings.HasPrefix(p, "~") {
		return filepath.Join(a.Paths.HomeDir, strings.TrimPrefix(strings.TrimPrefix(p, "~"), string(filepath.Separator)))
	}
	return p
}

// ContextWithTimeout 返回带超时的 ctx，供 CLI 层统一使用。
func ContextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, d)
}
