package install

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kingfs/Bootstrap/pkg/cmdx"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/ghrelease"
	"github.com/kingfs/Bootstrap/pkg/httpx"
)

// OSEnv 是 ExecEnv 的真实实现：唯一真正接触系统的地方。
//
// 它不决定"装什么"，只负责把命令发出去、把文件落下来。
// 测试里一律用假实现替换它，因此单元测试不会真的安装任何东西。
type OSEnv struct {
	runner   *cmdx.Runner
	client   *httpx.Client
	env      map[string]string
	home     string
	goos     string
	arch     string
	distro   string
	dryRun   bool
	workDir  string
	log      *slog.Logger
	extraBin []string
}

// OSEnvOptions 是 OSEnv 的构造参数。
type OSEnvOptions struct {
	// Env 会作为子进程环境（已含代理）；同时用于 bs 自身的下载代理选择。
	Env map[string]string
	// ExtraPath 是额外参与 LookPath 的目录（配置的 extra_path + bs 管理的目录）。
	ExtraPath []string
	HomeDir   string
	GOOS      string
	Arch      string
	Distro    string
	DryRun    bool
	WorkDir   string
	Log       *slog.Logger
	// Runner 允许注入自定义执行器（默认是 cmdx.OSRunner）。
	Runner *cmdx.Runner
	// Client 允许注入自定义 HTTP 客户端（默认按 Env 构造）。
	Client *httpx.Client
}

// NewOSEnv 构造真实执行环境。
func NewOSEnv(opt OSEnvOptions) *OSEnv {
	log := opt.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	client := opt.Client
	if client == nil {
		client = httpx.New(httpx.DefaultTimeout, opt.Env)
	}
	if httpx.Logger == nil {
		httpx.Logger = func(format string, args ...any) { log.Debug("http", "msg", fmt.Sprintf(format, args...)) }
	}
	return &OSEnv{
		runner:   opt.Runner,
		client:   client,
		env:      opt.Env,
		home:     opt.HomeDir,
		goos:     opt.GOOS,
		arch:     opt.Arch,
		distro:   opt.Distro,
		dryRun:   opt.DryRun,
		workDir:  opt.WorkDir,
		log:      log,
		extraBin: opt.ExtraPath,
	}
}

// Run 实现 ExecEnv。
func (e *OSEnv) Run(ctx context.Context, c Command) (Result, error) {
	if e.dryRun {
		e.log.Info("dry-run", "command", c.String())
		return Result{}, ErrDryRun
	}
	if e.runner == nil {
		return Result{}, errors.New("install: no command runner configured")
	}
	return e.runner.Run(ctx, c)
}

// Download 实现 ExecEnv。
func (e *OSEnv) Download(ctx context.Context, d Download) (string, error) {
	if e.dryRun {
		return "", ErrDryRun
	}
	if d.Dest == "" {
		d.Dest = filepath.Join(e.workDir, filepath.Base(d.URL))
	}
	return e.client.Download(ctx, d.URL, d.Dest, d.SHA256, d.Mode)
}

// FetchText 实现 ExecEnv。
//
// 这是只读请求，干跑时也照常进行：plan 需要真实的校验和才能在预览里
// 显示"将下载什么、校验什么"，而读取本身不会改变系统状态。
func (e *OSEnv) FetchText(ctx context.Context, url string) (string, error) {
	return e.client.GetString(ctx, url)
}

// LatestRelease 实现 ExecEnv。
func (e *OSEnv) LatestRelease(ctx context.Context, repo string) (string, error) {
	if e.dryRun {
		return "", ErrDryRun
	}
	return ghrelease.LatestTag(ctx, e.client, repo)
}

// LookPath 实现 ExecEnv：先在注入的 PATH 里找，再看 extra_path。
func (e *OSEnv) LookPath(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if p, err := exec.LookPath(name); err == nil {
		if pathIsUsable(p) {
			return p, true
		}
	}
	for _, dir := range e.searchDirs() {
		candidate := filepath.Join(dir, executableName(name, e.goos))
		if fsx.IsExecutable(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func (e *OSEnv) searchDirs() []string {
	dirs := append([]string{}, e.extraBin...)
	for _, dir := range filepath.SplitList(e.env["PATH"]) {
		dirs = append(dirs, dir)
	}
	if e.home != "" {
		dirs = append(dirs,
			filepath.Join(e.home, ".local", "bin"),
			filepath.Join(e.home, "bin"),
			filepath.Join(e.home, ".g", "versions", "current", "bin"),
		)
	}
	return dirs
}

func pathIsUsable(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// Env 实现 ExecEnv。
func (e *OSEnv) Env() map[string]string { return e.env }

// DryRun 实现 ExecEnv。
func (e *OSEnv) DryRun() bool { return e.dryRun }

// IsRoot 实现 ExecEnv。
func (e *OSEnv) IsRoot() bool { return os.Geteuid() == 0 }

// OS 实现 ExecEnv。
func (e *OSEnv) OS() string { return e.goos }

// Arch 实现 ExecEnv。
func (e *OSEnv) Arch() string { return e.arch }

// Distro 实现 ExecEnv。
func (e *OSEnv) Distro() string { return e.distro }

// HomeDir 实现 ExecEnv。
func (e *OSEnv) HomeDir() string { return e.home }

// WorkDir 实现 ExecEnv。
func (e *OSEnv) WorkDir() string { return e.workDir }

// Log 实现 ExecEnv。
func (e *OSEnv) Log() *slog.Logger { return e.log }
