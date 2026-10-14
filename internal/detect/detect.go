// Package detect 回答"某个工具装没装、什么版本"（docs/design.md §7.4）。
//
// 三条设计约束：
//   - **实时优先**：探测结果只是缓存，凡是要据此跳过安装的决定都必须强制重探；
//   - **永不阻塞**：单个工具的探测超时（默认 5s）不会拖慢整体，并发执行；
//   - **三态之外还有 Unknown**：命令在但版本解析不出来时不阻断安装，只提示。
package detect

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/version"
)

// Status 是探测结果的状态。
type Status string

// 探测状态枚举。
const (
	StatusInstalled Status = "installed"
	StatusOutdated  Status = "outdated"
	StatusMissing   Status = "missing"
	StatusUnknown   Status = "unknown"
)

// Result 是单个工具的探测结果。
type Result struct {
	ToolID    string    `json:"tool_id"`
	Status    Status    `json:"status"`
	Command   string    `json:"command,omitempty"`
	Version   string    `json:"version,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	// Cached 表示这条结果来自缓存而非本次真实探测。
	Cached bool `json:"-"`
}

// Present 报告工具是否存在（含"存在但版本未知"）。
func (r Result) Present() bool {
	return r.Status == StatusInstalled || r.Status == StatusOutdated || r.Status == StatusUnknown
}

// Executor 是探测所需的最小执行能力（cmdx.Runner 天然满足）。
type Executor interface {
	Run(ctx context.Context, c cmdx.Command) (cmdx.Result, error)
}

// PathLookup 是探测所需的最小查找能力。
type PathLookup interface {
	LookPath(name string) (string, bool)
}

// Detector 执行探测。
type Detector struct {
	Executor Executor
	LookPath PathLookup
	// HomeDir 用于展开 provides 里的 `~`。
	HomeDir string
	// Timeout 是单次版本探测的超时（默认 5s）。
	Timeout time.Duration
	// Concurrency 是并发探测度（默认 min(8, NumCPU)）。
	Concurrency int
	// Cache 为 nil 时不使用缓存。
	Cache *Cache
	// Now 便于测试注入时间。
	Now func() time.Time
}

// Options 控制一次探测。
type Options struct {
	// Refresh 为 true 时忽略缓存（"要据此跳过安装"时必须为 true）。
	Refresh bool
	// UseCache 为 false 时不读也不写缓存。
	UseCache bool
	// Only 非空时只探测这些 id。
	Only map[string]bool
}

// ProbeTimeout 是默认的单次探测超时。
const ProbeTimeout = 5 * time.Second

// MaxConcurrency 是探测并发上限。
const MaxConcurrency = 8

func (d *Detector) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Detector) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return ProbeTimeout
}

func (d *Detector) concurrency() int {
	if d.Concurrency > 0 {
		return d.Concurrency
	}
	n := runtime.NumCPU()
	if n > MaxConcurrency {
		n = MaxConcurrency
	}
	if n < 1 {
		n = 1
	}
	return n
}

// Detect 并发探测给定的工具集合，返回 id → 结果。
//
// 缓存只加速"显示"：命中缓存的结果会被标记 Cached=true，
// 调用方在据此跳过安装前必须用 Refresh 再探一次。
func (d *Detector) Detect(ctx context.Context, tools []manifest.Tool, opts Options) (map[string]Result, error) {
	if d.Executor == nil && d.LookPath == nil {
		return nil, ErrNoExecutor
	}

	out := make(map[string]Result, len(tools))
	var mu sync.Mutex

	var cached map[string]Result
	if opts.UseCache && !opts.Refresh && d.Cache != nil {
		loaded, err := d.Cache.Load()
		if err == nil {
			cached = loaded
		}
	}

	var pending []manifest.Tool
	for _, t := range tools {
		if opts.Only != nil && !opts.Only[t.ID] {
			continue
		}
		if cached != nil {
			if r, ok := cached[t.ID]; ok {
				r.Cached = true
				out[t.ID] = r
				continue
			}
		}
		pending = append(pending, t)
	}

	if len(pending) > 0 {
		g, gctx := errgroup.WithContext(ctx)
		sem := make(chan struct{}, d.concurrency())
		for _, t := range pending {
			g.Go(func() error {
				sem <- struct{}{}
				defer func() { <-sem }()
				r := d.Probe(gctx, t)
				mu.Lock()
				out[t.ID] = r
				mu.Unlock()
				return nil // 单个工具探测失败不算整体失败：结果里就是 Missing/Unknown
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
	}

	if opts.UseCache && d.Cache != nil && len(pending) > 0 {
		// 缓存写入失败不影响探测结果本身。
		_ = d.Cache.Merge(out)
	}
	return out, nil
}

// Probe 探测单个工具。它**不会**返回错误：拿不到结论时返回 StatusUnknown，
// 因为"探测不清楚"不应该让整个安装流程失败。
func (d *Detector) Probe(ctx context.Context, t manifest.Tool) Result {
	res := Result{ToolID: t.ID, CheckedAt: d.now()}

	if t.Verify.Command != "" {
		path, ok := d.lookPath(t.Verify.Command)
		if ok {
			res.Command = path
			return d.probeVersion(ctx, t, res)
		}
	}

	// 文件型探针：nvm 这类工具是 shell 函数，PATH 里没有可执行文件，
	// 只能看"安装出来的那个文件在不在"。
	if t.Verify.File != "" {
		if full := fsx.ExpandPath(t.Verify.File, d.HomeDir); full != "" && fsx.Exists(full) {
			res.Command = full
			res.Status = StatusInstalled
			res.Detail = "文件存在：" + full
			return res
		}
	}

	// 命令不在 PATH 里：release/source 类工具可能装在 provides 路径上，
	// 这些路径要等 shell 装配 PATH 之后才可见，所以单独检查一次。
	if path, ok := d.providesExecutable(t); ok {
		res.Command = path
		res.Status = StatusUnknown
		res.Detail = "命令不在 PATH，但 provides 路径存在：" + path
		return res
	}

	res.Status = StatusMissing
	switch {
	case t.Verify.Command != "":
		res.Detail = "未找到命令 " + t.Verify.Command
	case t.Verify.File != "":
		res.Detail = "未找到文件 " + t.Verify.File
	default:
		res.Detail = "未声明 verify.command，且 provides 路径不存在"
	}
	return res
}

// probeVersion 在命令存在的前提下解析版本。
func (d *Detector) probeVersion(ctx context.Context, t manifest.Tool, res Result) Result {
	re := t.Verify.Regex()
	if re == nil || len(t.Verify.VersionArgs) == 0 {
		res.Status = StatusInstalled
		res.Detail = "命令存在（未声明版本探测）"
		return res
	}
	if d.Executor == nil {
		res.Status = StatusUnknown
		res.Detail = "无法执行版本探测（未注入 Executor）"
		return res
	}

	runCtx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()

	args := t.Verify.VersionArgs
	// 用 LookPath 解析出的**完整路径**执行：查找规则里包含 extra_path 与
	// `~/.local/bin` 这类"还没进 shell PATH"的目录，若这里退回裸命令名，
	// 就会出现"找得到、却执行不了"的自相矛盾（新机器上刚装好的工具最典型）。
	name := t.Verify.Command
	if res.Command != "" {
		name = res.Command
	}
	cmd := cmdx.Command{Name: name, Args: args, Timeout: d.timeout()}
	out, err := d.Executor.Run(runCtx, cmd)
	text := out.Stdout + "\n" + out.Stderr

	m := re.FindStringSubmatch(text)
	if m == nil {
		res.Status = StatusUnknown
		switch {
		case err != nil:
			res.Detail = fmt.Sprintf("%s %s 失败：%v", t.Verify.Command, strings.Join(args, " "), err)
		case strings.TrimSpace(text) == "":
			res.Detail = "命令没有输出，无法解析版本"
		default:
			res.Detail = "版本输出无法用 version_regex 解析"
		}
		return res
	}
	raw := m[re.SubexpIndex("v")]
	res.Version = raw

	parsed, perr := version.Parse(raw)
	if perr != nil {
		res.Status = StatusUnknown
		res.Detail = "版本号无法解析：" + raw
		return res
	}

	if ex := t.Verify.Exact(); ex != nil && !version.Equal(parsed.Raw, ex.Raw) {
		res.Status = StatusOutdated
		res.Detail = fmt.Sprintf("版本 %s 与要求的 %s 不一致", raw, ex.Raw)
		return res
	}
	if min := t.Verify.Min(); min != nil && version.Less(parsed.Raw, min.Raw) {
		res.Status = StatusOutdated
		res.Detail = fmt.Sprintf("版本 %s 低于最低要求 %s", raw, min.Raw)
		return res
	}
	res.Status = StatusInstalled
	return res
}

func (d *Detector) lookPath(name string) (string, bool) {
	if d.LookPath == nil {
		return "", false
	}
	return d.LookPath.LookPath(name)
}

// providesExecutable 在 provides 声明的路径里找一个可执行文件。
func (d *Detector) providesExecutable(t manifest.Tool) (string, bool) {
	for _, p := range t.Provides {
		full := fsx.ExpandPath(p, d.HomeDir)
		if full == "" {
			continue
		}
		if fsx.IsExecutable(full) {
			return full, true
		}
		// provides 通常是 bin 目录：退一步看目录本身是否可执行文件所在。
		if fsx.IsDir(full) && t.Verify.Command != "" {
			candidate := filepath.Join(full, executableName(t.Verify.Command))
			if fsx.IsExecutable(candidate) {
				return candidate, true
			}
		}
	}
	return "", false
}

// executableName 在 Windows 上补 .exe 后缀。
func executableName(name string) string {
	if runtime.GOOS != "windows" {
		return name
	}
	if filepath.Ext(name) != "" {
		return name
	}
	return name + ".exe"
}

// Installed 是便捷判断：状态为 installed。
func (r Result) Installed() bool { return r.Status == StatusInstalled }

// Outdated 是便捷判断：状态为 outdated。
func (r Result) Outdated() bool { return r.Status == StatusOutdated }

// Label 返回给终端显示用的短标签。
func (r Result) Label() string {
	switch r.Status {
	case StatusInstalled:
		if r.Version != "" {
			return "已安装 " + r.Version
		}
		return "已安装"
	case StatusOutdated:
		if r.Version != "" {
			return "版本过旧 " + r.Version
		}
		return "版本过旧"
	case StatusMissing:
		return "未安装"
	default:
		if r.Version != "" {
			return "版本未知 " + r.Version
		}
		return "版本未知"
	}
}

// ErrNoExecutor 在既没有 Executor 也没有 LookPath 时返回，避免静默给出全是 Missing 的结果。
var ErrNoExecutor = errors.New("detector has neither executor nor path lookup")
