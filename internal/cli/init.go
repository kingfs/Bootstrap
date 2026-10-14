package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/app"
	"github.com/kingfs/Bootstrap/internal/install"
	"github.com/kingfs/Bootstrap/internal/ui"
)

// initCmd 是 `bs init`：新机器上的第一条命令。
//
// 它把 M1/M2 的能力串成一条"一把梭"的路径：
// 看环境 → （缺配置就生成）→ 选工具 → 看计划 → 确认 → 安装 → 可选接管 shell。
func (rt *Runtime) initCmd() *cobra.Command {
	var (
		profile     string
		toolList    []string
		missingOnly bool
		withShell   bool
		noShell     bool
		noTools     bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "新机器一把梭：认环境 → 选工具 → 装好（可交互，也可全自动）",
		Long: `把"这台新机器变成熟悉的样子"串成一条命令：

  1. 只读探测并展示环境（系统/架构、配置与目录、仓库根、代理、工具装机情况）；
  2. 配置文件不存在时，问一句后生成默认配置（等价 ` + "`bs config init`" + `）；
  3. 选工具：交互式可选套装 / 只补缺失 / 逐个勾选 / 先不装；非交互式用配置里的默认套装
     （--no-tools 直接跳过这一步：只做准备，一个工具都不装）；
  4. 先算好计划并展示，确认后执行（幂等：已装好的会跳过）；
  5. 可选把受管块写进 shell 启动文件（代理命令即时生效 + 自动装配 PATH）。

全自动（脚本/CI 里的用法）：
  bs init --yes                      # 用配置里的默认套装，不再问
  bs init --profile full --yes       # 指定套装
  bs init --tools golang,ripgrep -y  # 只装指定的几个
  bs init --missing-only --yes       # 只补当前缺的
  bs init --no-tools --yes           # 只做准备（配置/仓库根/受管块），一个工具都不装

安全约定：
  * --dry-run 只展示，不写配置、不装东西、不碰 rc 文件；
  * 非交互环境（管道/CI/--json）绝不阻塞：没有 --yes 就以退出码 5 结束；
  * 用户在确认时输入 n 视为主动取消，退出码 0（什么都没做）。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if withShell && noShell {
				return usageErrf("--with-shell 与 --no-shell 不能同时使用")
			}
			if missingOnly && (profile != "" || len(toolList) > 0) {
				return usageErrf("--missing-only 不能与 --profile/--tools 同时使用（它们已经明确了要装什么）")
			}
			if noTools && (profile != "" || len(toolList) > 0 || missingOnly) {
				return usageErrf("--no-tools 表示不装任何工具，不能与 --profile/--tools/--missing-only 同时使用")
			}
			opt := app.InitOptions{
				Profile:     profile,
				Tools:       splitList(toolList),
				MissingOnly: missingOnly,
				NoTools:     noTools,
				Prompter:    rt.prompter(),
				// ui.confirm=false 只影响"交互会话里还要不要问一句"：
				// 非交互环境依然必须 --yes，否则退出码 5（AGENTS.md §8.6 红线）。
				AssumeYes:  rt.Opts.Yes || (rt.interactive() && !rt.app().Cfg.UI.Confirm),
				OnOverview: rt.renderInitOverview,
				OnPlan:     rt.renderInitPlan,
			}
			switch {
			case withShell:
				v := true
				opt.ShellInstall = &v
			case noShell:
				v := false
				opt.ShellInstall = &v
			}

			res, err := rt.app().Init(cmd.Context(), opt)
			if errors.Is(err, ui.ErrCancelled) {
				fmt.Fprintln(rt.out(), "已取消，未做任何修改。")
				return nil
			}
			if errors.Is(err, ui.ErrNotInteractive) {
				return coded(ExitNeedsInteraction, errors.New(
					"需要确认但当前是非交互模式：请加 --yes 明确授权（或用 --dry-run 先看计划）"))
			}
			if err != nil {
				// 部分成功也可能是"计划里就有失败项"，交给统一的映射。
				if res.Report != nil {
					if rerr := rt.renderInitResult(res); rerr != nil {
						return rerr
					}
					return toolApplyErr(err, *res.Report)
				}
				return toolErr(err)
			}
			if err := rt.renderInitResult(res); err != nil {
				return err
			}
			if res.Report != nil && res.Report.Failed() != nil {
				return toolApplyErr(res.Report.Failed(), *res.Report)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "用哪个套装（默认：配置里的 tool.default_profile，交互时可改选）")
	cmd.Flags().StringSliceVar(&toolList, "tools", nil, "只处理这些工具（逗号分隔，可重复）")
	cmd.Flags().BoolVar(&missingOnly, "missing-only", false, "只处理当前缺失的工具")
	cmd.Flags().BoolVar(&noTools, "no-tools", false, "只做环境准备（配置/仓库根/受管块），不安装任何工具")
	cmd.Flags().BoolVar(&withShell, "with-shell", false, "顺便把 bs 受管块写进 shell 启动文件")
	cmd.Flags().BoolVar(&noShell, "no-shell", false, "明确不碰 shell 启动文件")
	return cmd
}

// renderInitPlan 展示计划：JSON 模式留给最后的统一输出，dry-run 要明确标注"不会执行"。
func (rt *Runtime) renderInitPlan(plan install.Plan) error {
	if rt.Opts.JSON {
		return nil
	}
	banner := ""
	if rt.Opts.DryRun {
		banner = "（dry-run：以下操作都不会被执行）"
	}
	return rt.renderPlan(plan, banner)
}

// renderInitOverview 打印环境概览（只读信息，出错也不影响后续流程）。
func (rt *Runtime) renderInitOverview(ov app.InitOverview) error {
	if rt.Opts.JSON {
		return nil // JSON 模式最后一次性输出结构化结果
	}
	w := rt.newTable()
	fmt.Fprintln(w, "项\t值")
	fmt.Fprintf(w, "系统\t%s/%s", ov.OS, ov.Arch)
	if ov.Distro != "" {
		fmt.Fprintf(w, "（%s）", ov.Distro)
	}
	fmt.Fprintln(w)
	if ov.Shell != "" {
		fmt.Fprintf(w, "shell\t%s\n", ov.Shell)
	}
	cfg := ov.ConfigFile
	if !ov.ConfigExists {
		cfg += "（尚未生成）"
	}
	fmt.Fprintf(w, "配置文件\t%s\n", cfg)
	fmt.Fprintf(w, "仓库根目录\t%s", ov.RepoRoot)
	if ov.RepoRootFrom != "" {
		fmt.Fprintf(w, "（%s）", ov.RepoRootFrom)
	}
	fmt.Fprintln(w)
	switch {
	case ov.Proxy != "":
		fmt.Fprintf(w, "代理\t%s", ov.Proxy)
		if ov.ProxyDetail != "" {
			fmt.Fprintf(w, "（%s）", ov.ProxyDetail)
		}
		fmt.Fprintln(w)
	default:
		fmt.Fprintln(w, "代理\t未启用（bs proxy use <名字> 可启用）")
	}
	fmt.Fprintf(w, "工具\t适用 %d：已装 %d，可升级 %d，缺失 %d\n",
		ov.Tools.Applicable, ov.Tools.Installed, ov.Tools.Outdated, ov.Tools.Missing)
	return w.Flush()
}

// renderInitResult 渲染向导的最终结果（报告 + shell 受管块 + 下一步提示）。
func (rt *Runtime) renderInitResult(res app.InitResult) error {
	if rt.Opts.JSON {
		return rt.emit(res)
	}
	// 顺序按"实际动手的顺序"来：配置 → 工具 → shell 受管块。
	switch {
	case res.DryRunPlannedConfig:
		fmt.Fprintf(rt.out(), "[dry-run] 会生成配置文件 %s\n", res.Overview.ConfigFile)
	case res.ConfigCreated:
		fmt.Fprintf(rt.out(), "已生成配置文件 %s\n", res.Overview.ConfigFile)
	}
	if res.SkipTools {
		fmt.Fprintf(rt.out(), "已跳过工具安装（本机适用 %d：已装 %d，缺失 %d）。\n",
			res.Overview.Tools.Applicable, res.Overview.Tools.Installed, res.Overview.Tools.Missing)
		if p := strings.TrimSpace(rt.app().Cfg.Tool.DefaultProfile); p != "" {
			fmt.Fprintf(rt.out(), "想装的时候：bs tool install --profile %s --yes（或重跑 bs init）\n", p)
		} else {
			fmt.Fprintln(rt.out(), "想装的时候：bs tool install --profile <套装> --yes（或重跑 bs init）")
		}
	}
	// dry-run 下计划表刚刚已经逐行展示过，结果表只会是同一份内容的翻版。
	if res.Report != nil && !rt.Opts.DryRun {
		if err := rt.renderReport(*res.Report, install.OpInstall); err != nil {
			return err
		}
	}
	if res.Shell != nil {
		switch {
		case !res.Shell.Changed:
			fmt.Fprintf(rt.out(), "%s 中的受管块已是最新，未修改\n", res.Shell.RCFile)
		case rt.Opts.DryRun:
			fmt.Fprintf(rt.out(), "[dry-run] 将更新 %s（%s）\n", res.Shell.RCFile, res.Shell.DryRunDiff)
		default:
			fmt.Fprintf(rt.out(), "已更新 %s（shell=%s）\n", res.Shell.RCFile, res.Shell.Shell)
			if res.Shell.Backup != "" {
				fmt.Fprintf(rt.out(), "备份：%s\n", res.Shell.Backup)
			}
		}
	}
	if res.Mode == app.SelectionNone && res.Report == nil {
		fmt.Fprintln(rt.out(), "没有需要处理的工具，环境已经是目标状态。")
	}
	// 收尾提示只在"确实装了东西"时给，避免刷屏（dry-run 下什么都没装，也不该说"新装的工具"）。
	if res.Report != nil && res.Report.Changed() {
		fmt.Fprintln(rt.errOut(), "提示：新装的工具可能需要重开 shell 或执行 `eval \"$(bs env path)\"` 才能进入 PATH。")
	}
	return nil
}

// splitList 把 --tools 的值拆成 id 列表（逗号分隔，忽略空白）。
func splitList(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// prompter 按当前环境挑一个提问实现，优先级：
//
//	非交互            → NonInteractive（一问就退出码 5，绝不阻塞）
//	交互 + --plain-ui → 纯文本
//	交互 + 真终端     → huh（空格勾选 / 方向键选择；huh 只允许出现在 internal/ui）
//	交互但没有终端    → 纯文本（测试注入 reader、被包装器挡住终端等）
//
// 纯文本路径永远存在，所以"没有 huh"只是少一点漂亮，不会少一个功能。
func (rt *Runtime) prompter() ui.Prompter {
	if !rt.interactive() {
		return ui.NonInteractive{}
	}
	in := rt.Opts.In
	if in == nil {
		in = strings.NewReader("")
	}
	if rt.Opts.PlainUI {
		return ui.NewText(in, rt.Opts.Err)
	}
	if p, ok := ui.NewInteractive(in, rt.Opts.Err); ok {
		return p
	}
	return ui.NewText(in, rt.Opts.Err)
}

// progressHook 根据 ui.progress 决定要不要在安装过程中打进度。
//
//	auto（默认）：只在交互终端上打（管道/CI 里静默，日志才不会被进度刷满）；
//	plain/always：总是打；none/off：从不打。
func (rt *Runtime) progressHook() func(install.Step) {
	mode := strings.ToLower(strings.TrimSpace(rt.app().Cfg.UI.Progress))
	switch mode {
	case "none", "off", "false", "quiet":
		return nil
	case "plain", "always", "on", "true":
		return rt.printStep
	case "", "auto":
		if rt.Opts.JSON || !app.IsTerminalWriter(rt.Opts.Err) {
			return nil
		}
		return rt.printStep
	default:
		// 写错了不该让安装失败——按 auto 处理并提醒一句（配置校验另有 warning）。
		rt.Log.Warn("unknown ui.progress value, falling back to auto", "value", rt.app().Cfg.UI.Progress)
		if rt.Opts.JSON || !app.IsTerminalWriter(rt.Opts.Err) {
			return nil
		}
		return rt.printStep
	}
}

// printStep 打一行进度到 stderr：stdout 要留给结果本身（表格/JSON）。
func (rt *Runtime) printStep(s install.Step) {
	label := s.ToolID
	if s.Name != "" && s.Name != s.ToolID {
		label = fmt.Sprintf("%s（%s）", s.ToolID, s.Name)
	}
	via := s.Via
	if s.Manager != "" && !strings.Contains(via, s.Manager) {
		via = via + ":" + s.Manager
	}
	if via != "" {
		fmt.Fprintf(rt.errOut(), "→ %s  %s\n", label, via)
		return
	}
	fmt.Fprintf(rt.errOut(), "→ %s\n", label)
}
