package install

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	workRoot string
	cacheDir string
	workDir  string
	workOnce sync.Once
	log      *slog.Logger
	extraBin []string
	versions map[string]string
	// lookup 是元数据查询（最新 tag / 校验和文件）的时限，0 时用 LookupTimeout。
	lookup time.Duration
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
	// WorkRoot 是临时工作目录的父目录（默认 cache 目录）；真正用到时才创建。
	WorkRoot string
	// CacheDir 是 bs 的缓存目录；下载缓存放在它的 downloads/ 子目录里。
	// 留空则不跨次复用下载文件（只影响"能不能续传/命中缓存"，不影响正确性）。
	CacheDir string
	// Versions 来自配置 tool.versions，供清单模板使用。
	Versions map[string]string
	Log      *slog.Logger
	// Runner 允许注入自定义执行器（默认是 cmdx.OSRunner）。
	Runner *cmdx.Runner
	// Client 允许注入自定义 HTTP 客户端（默认按 Env 构造）。
	Client *httpx.Client
	// LookupTimeout 覆盖元数据查询时限（0 = LookupTimeout 常量）。
	LookupTimeout time.Duration
	// DownloadTimeout 覆盖单个归档的下载时限（0 = httpx.DefaultDownloadTimeout）。
	DownloadTimeout time.Duration
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
	if opt.DownloadTimeout > 0 {
		client.DownloadTimeout = opt.DownloadTimeout
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
		log:      log,
		extraBin: opt.ExtraPath,
		versions: opt.Versions,
		lookup:   opt.LookupTimeout,
		cacheDir: opt.CacheDir,
	}
}

// lookupTimeout 返回元数据查询时限（配置留空时用内置默认值）。
func (e *OSEnv) lookupTimeout() time.Duration {
	if e.lookup > 0 {
		return e.lookup
	}
	return LookupTimeout
}

// Run 实现 ExecEnv。
func (e *OSEnv) Run(ctx context.Context, c Command) (Result, error) {
	if e.dryRun {
		e.log.Debug("dry-run", "command", c.String())
		return Result{}, ErrDryRun
	}
	if e.runner == nil {
		return Result{}, errors.New("install: no command runner configured")
	}
	// 命令名的解析必须和探测（LookPath）用同一套规则：新机器上刚装进 ~/.local/bin
	// 的管理器，本次运行就要拿它去装 runtime（g → golang、uv → python），而子进程
	// 的 PATH 继承自宿主 shell，未必包含这些目录，`exec` 于是报"找不到可执行文件"。
	// 找不到时保留原名，让报错仍然是用户认识的那个命令。
	if !strings.ContainsAny(c.Name, `/\`) {
		if p, ok := e.LookPath(c.Name); ok {
			c.Name = p
		}
	}
	return e.runner.Run(ctx, c)
}

// Download 实现 ExecEnv。
func (e *OSEnv) Download(ctx context.Context, d Download) (string, error) {
	if e.dryRun {
		return "", ErrDryRun
	}
	if d.Dest == "" {
		d.Dest = filepath.Join(e.DownloadDir(), filepath.Base(d.URL))
	}
	if strings.HasPrefix(d.Dest, e.DownloadDir()) {
		// 下载缓存是有上限的：每次真正下载前顺手清掉长期没用过的条目，
		// 否则"每个工具的每个版本一个归档"会无声地把缓存撑大。
		e.pruneDownloadCache()
	}
	return e.client.Download(ctx, d.URL, d.Dest, d.SHA256, d.Mode)
}

// downloadCacheMaxAge 是下载缓存的保留期。
//
// 取舍：太短会让"昨天断了的下载"今天还得从头来；太长则每个工具的每个版本
// 都留一个归档（Go 工具链一个就上百 MB）。30 天足够覆盖"重装一遍环境"的节奏。
const downloadCacheMaxAge = 30 * 24 * time.Hour

// pruneDownloadCache 清掉久未使用的下载缓存与残留半成品。
//
// 只在"真的要下载"时调用（`--dry-run` 不碰任何文件），失败只记日志：
// 清理是打扫卫生，不该让安装失败。
func (e *OSEnv) pruneDownloadCache() {
	if e.dryRun {
		return
	}
	dir := e.DownloadDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-downloadCacheMaxAge)
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		info, err := ent.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		if err := os.Remove(path); err == nil {
			e.log.Debug("pruned stale download", "file", path)
		}
	}
}

// LookupTimeout 是"元数据查询"（最新 tag、校验和文件、版本文本）的超时。
//
// 比下载超时短得多（下载动辄几十 MB，见 httpx.DefaultDownloadTimeout），
// 但要**装得下 httpx 的两次尝试**：单次最长 30s（响应头超时），两次正好 60s。
// 实测在限速代理下，一个几百字节的校验和文件也可能要十几秒才有响应——
// 元数据查询只在真正安装时发生（`bs tool plan` 是离线的），所以宁可多等两次，
// 也不要让"网慢一点就装不上"。
const LookupTimeout = 60 * time.Second

// FetchText 实现 ExecEnv。
//
// 这是只读请求，干跑时也照常进行：plan 需要真实的校验和才能在预览里
// 显示"将下载什么、校验什么"，而读取本身不会改变系统状态。
func (e *OSEnv) FetchText(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, e.lookupTimeout())
	defer cancel()
	start := time.Now()
	text, err := e.client.GetString(ctx, url)
	// 记一条耗时日志：网络问题排查时，"哪个请求慢/失败、花了多久"是最关键的信息，
	// 而下载之外（tag、校验和）原本没有任何痕迹。
	e.log.Debug("http", "get", url, "elapsed", time.Since(start).Round(time.Millisecond), "err", err)
	return text, err
}

// LatestRelease 实现 ExecEnv。
func (e *OSEnv) LatestRelease(ctx context.Context, repo string) (string, error) {
	if e.dryRun {
		return "", ErrDryRun
	}
	ctx, cancel := context.WithTimeout(ctx, e.lookupTimeout())
	defer cancel()
	start := time.Now()
	tag, err := ghrelease.LatestTag(ctx, e.client, repo)
	e.log.Debug("http", "latest", repo, "tag", tag, "elapsed", time.Since(start).Round(time.Millisecond), "err", err)
	return tag, err
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
	dirs = append(dirs, filepath.SplitList(e.env["PATH"])...)
	if e.home != "" {
		dirs = append(dirs,
			filepath.Join(e.home, ".local", "bin"),
			filepath.Join(e.home, "bin"),
			// g（Go 版本管理器）的布局：`~/.g/go` 是指向当前版本的软链，
			// `go` 在 `~/.g/go/bin`；g 自己由官方脚本装时在 `~/.g/bin`。
			filepath.Join(e.home, ".g", "bin"),
			filepath.Join(e.home, ".g", "go", "bin"),
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

// Versions 实现 ExecEnv。
func (e *OSEnv) Versions() map[string]string { return e.versions }

// DownloadDir 实现 ExecEnv：下载缓存放在 `<cache>/downloads`。
//
// 下载缓存必须跨次运行稳定（否则断点续传/缓存命中都无从谈起），
// 同时它是**可随时删除**的：删掉只会让下次重新下载，不影响任何状态。
func (e *OSEnv) DownloadDir() string {
	if e.cacheDir == "" {
		// 没给缓存目录时退回临时工作目录：此时只是"能下完"，不跨次复用。
		return e.WorkDir()
	}
	return filepath.Join(e.cacheDir, "downloads")
}

// WorkDir 实现 ExecEnv。
//
// 只是"算出一个路径"，并不创建目录：`bs tool plan` / `--dry-run` 不该因为
// 需要展示一个临时路径就在磁盘上留下东西。真正落盘的一方自己按需建目录。
func (e *OSEnv) WorkDir() string {
	e.workOnce.Do(func() {
		base := e.workRoot
		if base == "" {
			base = os.TempDir()
		}
		e.workDir = filepath.Join(base, "work", fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano()))
	})
	return e.workDir
}

// Log 实现 ExecEnv。
func (e *OSEnv) Log() *slog.Logger { return e.log }
