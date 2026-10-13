// Package cli 是命令层：只做参数解析、调用应用层、渲染输出。
//
// 约束（AGENTS.md）：命令层不得包含业务逻辑，也不得直接读写配置文件——
// 需要新能力时，先在 internal/app 里加方法。
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/app"
	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/pkg/hostinfo"
)

// AnnotationTolerateConfigErrors 标记"必须能在配置损坏时仍然工作"的命令。
//
// `bs shell init` / `bs env path` / `bs proxy env` 会在每次启动 shell 时被调用，
// 若因为配置文件写错就让用户的 shell 报错甚至起不来，是不可接受的。
const AnnotationTolerateConfigErrors = "bs.tolerate-config-errors"

// Options 是全局命令行选项。
type Options struct {
	ConfigFile string
	RepoRoot   string
	DryRun     bool
	Yes        bool
	JSON       bool
	NoColor    bool
	Verbose    int
	LogLevel   string

	// NonInteractive 由 `--non-interactive`、`CI=1` 或"输出不是终端"置位：
	// 此时绝不阻塞等待输入，破坏性操作必须由 `--yes` 明确授权（ADR-0003）。
	NonInteractive bool

	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Runtime 持有一次进程运行的依赖。
type Runtime struct {
	Opts *Options
	App  *app.App
	Log  *slog.Logger

	// loadErr 记录配置装载失败的原因（容忍模式下用于提示）。
	loadErr error

	// interactiveOverride 只在测试里注入：真实运行时按终端判定（interactive()）。
	// 用 pty 造终端在单测里既重又不可靠，直接注入判定结果更诚实。
	interactiveOverride *bool
}

// Execute 是 cmd/bs 的唯一入口，返回进程退出码。
func Execute() int {
	rt := &Runtime{Opts: &Options{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}}
	return rt.Run(os.Args[1:])
}

// Run 解析并执行一次命令行调用，返回进程退出码。
//
// 错误报告也放在这里（而不是 main）：这样"命令 → 退出码 → 用户看到什么"
// 这条链路可以在进程内被完整测试，不必 fork 子进程。
func (rt *Runtime) Run(args []string) int {
	root := rt.newRootCmd()
	root.SetOut(rt.Opts.Out)
	root.SetErr(rt.Opts.Err)
	root.SetArgs(args)

	if err := root.Execute(); err != nil {
		code := ExitCode(err)
		if msg := strings.TrimSpace(err.Error()); msg != "" {
			fmt.Fprintf(rt.Opts.Err, "bs: %s\n", msg)
		}
		switch code {
		case ExitUsage:
			fmt.Fprint(rt.Opts.Err, "提示：运行 `bs --help` 查看用法。\n")
		case ExitPrecondition:
			fmt.Fprint(rt.Opts.Err, "提示：运行 `bs doctor` 检查环境。\n")
		}
		return code
	}
	return ExitOK
}

func (rt *Runtime) newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bs",
		Short: "个人开发环境 bootstrap / 运维工具箱",
		Long: `bs 用于在新机器上快速重建开发环境。

它做四件事：
  1. 把仓库克隆/更新到统一目录布局（bs repo）
  2. 按平台安装/更新工具链（bs tool，M2 起）
  3. 管理代理 profile 并让它在当前 shell 立即生效（bs proxy）
  4. 用受管块的方式接管 shell 片段，不污染你的 rc（bs shell）

所有会修改文件系统或外部状态的命令都支持 --dry-run 先看要做的事。`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return rt.init(cmd)
		},
	}

	pf := cmd.PersistentFlags()
	pf.StringVar(&rt.Opts.ConfigFile, "config", "", "指定配置文件（跳过分层搜索）")
	pf.StringVar(&rt.Opts.RepoRoot, "repo-root", "", "覆盖配置中的 repo.root")
	pf.BoolVar(&rt.Opts.DryRun, "dry-run", false, "只展示将要执行的操作，不产生任何修改")
	pf.BoolVarP(&rt.Opts.Yes, "yes", "y", false, "对所有确认提示自动回答 yes")
	pf.BoolVar(&rt.Opts.JSON, "json", false, "以 JSON 输出（便于脚本消费）")
	pf.BoolVar(&rt.Opts.NoColor, "no-color", false, "禁用彩色输出")
	pf.BoolVar(&rt.Opts.NonInteractive, "non-interactive", false, "绝不等待输入：破坏性操作需要 --yes，否则以退出码 5 结束")
	pf.CountVarP(&rt.Opts.Verbose, "verbose", "v", "输出详细日志（可重复：-v 调试，-vv 追踪）")
	pf.StringVar(&rt.Opts.LogLevel, "log-level", "", "日志级别 debug|info|warn|error（默认取配置）")

	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageErr(err) })

	cmd.AddCommand(
		rt.versionCmd(),
		rt.configCmd(),
		rt.proxyCmd(),
		rt.shellCmd(),
		rt.envCmd(),
		rt.repoCmd(),
		rt.toolCmd(),
		rt.doctorCmd(),
	)
	cmd.InitDefaultCompletionCmd()
	cmd.InitDefaultHelpCmd()
	return cmd
}

// init 在每个命令执行前装配 App。
func (rt *Runtime) init(cmd *cobra.Command) error {
	logger, err := rt.newLogger()
	if err != nil {
		return usageErr(err)
	}
	rt.Log = logger

	tolerate := cmd.Annotations[AnnotationTolerateConfigErrors] == "true"

	paths, err := config.ResolvePaths()
	if err != nil {
		if !tolerate {
			return preconditionErr(err)
		}
		fmt.Fprintf(rt.Opts.Err, "bs: 警告：%v（已退回当前目录下的 .bootstrap）\n", err)
		paths = fallbackPaths()
	}

	cwd, _ := os.Getwd()
	res, err := config.Load(config.LoadOptions{
		Paths:        paths,
		ExplicitFile: rt.Opts.ConfigFile,
		ProjectDir:   cwd,
	})
	if err != nil {
		if tolerate {
			rt.loadErr = err
			fmt.Fprintf(rt.Opts.Err, "bs: 警告：%v（已退回内置默认值）\n", err)
			cfg := config.Defaults()
			res = &config.LoadResult{Config: &cfg, Paths: paths}
		} else {
			return preconditionErr(err)
		}
	}
	if rt.Opts.RepoRoot != "" {
		res.Config.Repo.Root = rt.Opts.RepoRoot
	}

	platform := hostinfo.Detect()
	if platform.HomeDir == "" {
		platform.HomeDir = paths.HomeDir
	}

	rt.App = app.New(paths, res.Config, res.Sources, platform, logger, rt.Opts.Out, rt.Opts.Err)
	rt.App.DryRun = rt.Opts.DryRun
	rt.App.Yes = rt.Opts.Yes
	rt.App.JSON = rt.Opts.JSON
	rt.App.NoColor = rt.Opts.NoColor
	rt.App.Verbose = rt.Opts.Verbose
	// dry-run 必须一路传导到命令执行器，否则"预览"会真的动手。
	rt.App.Runner.DryRun = rt.Opts.DryRun
	return nil
}

func (rt *Runtime) newLogger() (*slog.Logger, error) {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(rt.Opts.LogLevel)) {
	case "":
		// 留待 App 装配后按配置决定；这里只处理 -v。
		switch {
		case rt.Opts.Verbose >= 2:
			level = slog.LevelDebug
		case rt.Opts.Verbose == 1:
			level = slog.LevelDebug
		}
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid --log-level %q (want debug|info|warn|error)", rt.Opts.LogLevel)
	}
	handler := slog.NewTextHandler(rt.Opts.Err, &slog.HandlerOptions{Level: level})
	return slog.New(handler), nil
}

// emit 以 JSON 输出结构化结果（未开启 --json 时不做任何事）。
func (rt *Runtime) emit(v any) error {
	if !rt.Opts.JSON {
		return nil
	}
	enc := json.NewEncoder(rt.Opts.Out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// newTable 返回一个按列对齐的 writer。
func (rt *Runtime) newTable() *tabwriter.Writer {
	return tabwriter.NewWriter(rt.Opts.Out, 0, 0, 2, ' ', 0)
}

// out 返回用户可见输出的 writer（stdout）。
func (rt *Runtime) out() io.Writer { return rt.Opts.Out }

// errOut 返回日志/警告的 writer（stderr）。
func (rt *Runtime) errOut() io.Writer { return rt.Opts.Err }

// appOrNil 在容忍模式下 App 可能尚未装配。
func (rt *Runtime) app() *app.App { return rt.App }

// fallbackPaths 在无法确定家目录时给出的兜底路径，保证 shell 启动脚本不崩。
func fallbackPaths() config.Paths {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	base := filepath.Join(cwd, ".bootstrap")
	return config.Paths{
		ConfigDir: base,
		StateDir:  filepath.Join(base, "state"),
		CacheDir:  filepath.Join(base, "cache"),
		HomeDir:   cwd,
	}
}
