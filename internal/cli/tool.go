package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/app"
	"github.com/kingfs/Bootstrap/internal/install"
	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/internal/ui"
)

// toolCmd 是 `bs tool` 的根：清单查询 + 安装/升级/卸载。
//
// 命令层只做三件事：解析参数、调用 app、渲染。所有"哪些工具、装没装、
// 用什么方式装"的判断都在 internal/app 与 internal/install 里。
func (rt *Runtime) toolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tool",
		Short: "按技术栈安装/更新工具链",
		Long: `按内置清单（tools/*.yaml）管理工具链。

清单按技术栈与功能分组，每个工具声明"怎么判断装没装"和"按优先级排列的安装方式"；
bs 会严格按清单里的书写顺序选择第一个可用方式，而不是在代码里硬编码偏好。

所有命令都支持 --dry-run：先看要做什么，再决定是否执行。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		rt.toolListCmd(),
		rt.toolInfoCmd(),
		rt.toolSearchCmd(),
		rt.toolPlanCmd(),
		rt.toolInstallCmd(),
		rt.toolUpgradeCmd(),
		rt.toolUninstallCmd(),
		rt.manifestCmd(),
	)
	return cmd
}

// toolFilterFlags 是列表类命令共用的过滤参数。
type toolFilterFlags struct {
	profile   string
	stack     string
	kind      string
	tags      []string
	installed bool
	missing   bool
	outdated  bool
	all       bool
	refresh   bool
	noProbe   bool
}

func (f *toolFilterFlags) bind(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.profile, "profile", "", "只看某个 profile 包含的工具")
	fl.StringVar(&f.stack, "stack", "", "按技术栈过滤（golang/python/nodejs/rust/cpp/base/...）")
	fl.StringVar(&f.kind, "kind", "", "按类型过滤（runtime/versionmgr/pkgmgr/cli/devdep/service/gui/agent）")
	fl.StringSliceVar(&f.tags, "tag", nil, "按标签过滤（可重复，需全部命中）")
	fl.BoolVar(&f.installed, "installed", false, "只看已安装的")
	fl.BoolVar(&f.missing, "missing", false, "只看未安装的")
	fl.BoolVar(&f.outdated, "outdated", false, "只看已安装但落后于清单版本的")
	fl.BoolVar(&f.all, "all", false, "包含 disabled 与本机不适用的条目")
	fl.BoolVar(&f.refresh, "refresh", false, "忽略探测缓存，强制重新探测")
	fl.BoolVar(&f.noProbe, "no-probe", false, "不探测安装状态（只列清单，更快）")
}

func (f *toolFilterFlags) options() app.ToolListOptions {
	return app.ToolListOptions{
		Profile: f.profile, Stack: f.stack, Kind: f.kind, Tags: f.tags,
		Installed: f.installed, Missing: f.missing, Outdated: f.outdated, All: f.all,
		Refresh: f.refresh, NoProbe: f.noProbe,
	}
}

func (rt *Runtime) toolListCmd() *cobra.Command {
	var f toolFilterFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出工具及安装状态",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			rows, err := rt.app().ToolRows(context.Background(), f.options())
			if err != nil {
				return toolErr(err)
			}
			if rt.Opts.JSON {
				return rt.emit(rows)
			}
			return rt.renderToolTable(rows)
		},
	}
	f.bind(cmd)
	return cmd
}

func (rt *Runtime) toolSearchCmd() *cobra.Command {
	var f toolFilterFlags
	cmd := &cobra.Command{
		Use:     "search <关键词>",
		Aliases: []string{"find"},
		Short:   "按名字/描述/标签搜索工具",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			opt := f.options()
			opt.Search = args[0]
			rows, err := rt.app().ToolRows(context.Background(), opt)
			if err != nil {
				return toolErr(err)
			}
			if rt.Opts.JSON {
				return rt.emit(rows)
			}
			if len(rows) == 0 {
				fmt.Fprintf(rt.out(), "没有匹配 %q 的工具\n", args[0])
				return nil
			}
			return rt.renderToolTable(rows)
		},
	}
	f.bind(cmd)
	return cmd
}

func (rt *Runtime) toolInfoCmd() *cobra.Command {
	var refresh bool
	cmd := &cobra.Command{
		Use:   "info <工具>",
		Short: "显示一个工具的详情与安装方式链",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			detail, err := rt.app().ToolInfo(context.Background(), args[0], refresh)
			if err != nil {
				return toolErr(err)
			}
			if rt.Opts.JSON {
				return rt.emit(detail)
			}
			return rt.renderToolDetail(detail)
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "忽略探测缓存，强制重新探测")
	return cmd
}

// toolSelectFlags 是安装类命令共用的选择参数。
type toolSelectFlags struct {
	profile    string
	stack      string
	kind       string
	tags       []string
	missing    bool
	select_    bool
	upgradeAll bool
	via        string
	fallback   bool
	noFallback bool
}

func (f *toolSelectFlags) bind(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.profile, "profile", "", "安装某个 profile 的全部工具")
	fl.StringVar(&f.stack, "stack", "", "安装某个技术栈的全部工具")
	fl.StringVar(&f.kind, "kind", "", "按类型选择（runtime/versionmgr/...）")
	fl.StringSliceVar(&f.tags, "tag", nil, "按标签选择（可重复）")
	fl.BoolVar(&f.missing, "missing", false, "只安装当前缺失的工具")
	fl.BoolVarP(&f.select_, "interactive", "i", false,
		"先探测再用交互界面勾选要处理的工具（需要终端；候选范围由 --profile/--stack/--kind/--tag 决定）")
	fl.StringVar(&f.via, "via", "", "强制使用某种安装方式（默认按清单顺序自动选择）")
	fl.BoolVar(&f.fallback, "fallback", false, "失败后允许自动换用后面的安装方式")
	fl.BoolVar(&f.noFallback, "no-fallback", false, "失败后不换方式，直接报错")
}

// apply 把 flag 写到 App 上（含 ADR-0013 的默认值策略）。
func (f *toolSelectFlags) apply(rt *Runtime) error {
	a := rt.app()
	if f.fallback && f.noFallback {
		return usageErr(errors.New("--fallback 与 --no-fallback 不能同时使用"))
	}
	switch {
	case f.fallback:
		v := true
		a.Fallback = &v
	case f.noFallback:
		v := false
		a.Fallback = &v
	}
	if !app.IsTerminalWriter(rt.Opts.Out) {
		a.NonInteractive = true
	}
	a.DefaultViaOverride = f.via
	return nil
}

func (rt *Runtime) toolPlanCmd() *cobra.Command {
	var f toolSelectFlags
	cmd := &cobra.Command{
		Use:   "plan [工具...]",
		Short: "只展示将要执行的安装步骤，不做任何修改",
		Long: `只展示将要执行的安装步骤，不做任何修改。

plan 是**离线**预览：不联网解析"最新版本"，因此 URL 里显示 {{version}} 的占位符，
并且连探测缓存都不会写。真实的版本号、下载地址与校验和会在执行安装时解析。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.apply(rt); err != nil {
				return err
			}
			// plan 就是"不改任何东西"的预览：让引擎走干跑语义，
			// 从而不联网解析最新版本、也不写探测缓存（新机器上还没配好网络时
			// `bs tool plan` 必须立刻给出结果，而不是每个工具卡 15 秒）。
			rt.app().DryRun = true
			plan, err := rt.planFor(cmd, f, args, install.OpInstall)
			if err != nil {
				return toolErr(err)
			}
			// plan 不安装/不升级任何东西，所以这里永远提示（而不是只在 --dry-run 时）。
			// 措辞刻意不提"不执行任何操作"：探测会跑清单里的 verify.command（如 `go version`），
			// 而工具自身的命令可能有副作用（Go 会往 ~/.config/go/telemetry 写计数文件）。
			return rt.renderPlan(plan, "（只展示计划，不会安装或升级任何工具）")
		},
	}
	f.bind(cmd)
	return cmd
}

func (rt *Runtime) toolInstallCmd() *cobra.Command {
	var f toolSelectFlags
	cmd := &cobra.Command{
		Use:   "install [工具...]",
		Short: "安装工具（幂等：已安装会自动跳过）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return rt.runToolOp(cmd, f, args, install.OpInstall)
		},
	}
	f.bind(cmd)
	return cmd
}

func (rt *Runtime) toolUpgradeCmd() *cobra.Command {
	var f toolSelectFlags
	cmd := &cobra.Command{
		Use:   "upgrade [工具...]",
		Short: "更新工具（交给各自的包管理器/版本管理器）",
		Long: `更新工具：把工作交给工具自己的包管理器/版本管理器。

--all 把清单里**所有已安装**的工具过一遍（能升级的按各自方式升级，已经是新的由管理器
自行跳过），不需要一个个点名。

系统包管理器（apt/dnf/pacman…）按设计**不升级系统包**（设计文档 §7.6），这些工具在
--all 下会被列为"跳过（无可用升级方式）"而不是失败；真正的问题（缺校验和、via 写错）
仍然整体报错。一个都升级不了、也没有已安装工具时，退出码是 0——脚本里可以放心用。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.upgradeAll && len(args) > 0 {
				return usageErrf("--all 不能与显式工具名同时使用：用 --profile/--stack/--kind/--tag 缩小范围")
			}
			return rt.runToolOp(cmd, f, args, install.OpUpgrade)
		},
	}
	f.bind(cmd)
	cmd.Flags().BoolVar(&f.upgradeAll, "all", false,
		"升级所有已安装但落后于清单版本的（没有需要升级的就正常结束）")
	return cmd
}

func (rt *Runtime) toolUninstallCmd() *cobra.Command {
	var f toolSelectFlags
	cmd := &cobra.Command{
		Use:   "uninstall [工具...]",
		Short: "卸载工具（交给各自的包管理器/官方方式执行）",
		Long: "按工具声明的卸载方式链执行卸载。\n\n" +
			"清单可以单独写 `uninstall:` 链；没写就沿用安装链（apt 会变成 apt-get remove）。\n" +
			"系统包管理器需要 root，bs 不会静默提权；缺权限时会明确报错。\n" +
			"只会移除清单里那个安装位置的东西，不会去删用户手动装到别处的副本。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return rt.runToolOp(cmd, f, args, install.OpUninstall)
		},
	}
	f.bind(cmd)
	return cmd
}

// errNothingToUpgrade 表示"该做的都做完了"：候选集合为空，不是错误。
var errNothingToUpgrade = errors.New("nothing to upgrade")

// withoutUnsupported 把"设计上就不做"的步骤（系统包管理器升级）从计划里摘出来。
//
// 只对 `upgrade --all` 用：批量命令必须能跑完能跑的部分，而点名单个工具时
// 应当照旧明确报错——"升级 htop 不支持"本身就是用户要的答案。
func withoutUnsupported(plan install.Plan) (install.Plan, []install.Step) {
	kept := make([]install.Step, 0, len(plan.Steps))
	var skipped []install.Step
	for _, s := range plan.Steps {
		if s.Status == install.StepUnsupported || s.NoMethod {
			skipped = append(skipped, s)
			continue
		}
		kept = append(kept, s)
	}
	if len(skipped) == 0 {
		return plan, nil
	}
	plan.Steps = kept
	order := make([]string, 0, len(plan.Order))
	for _, id := range plan.Order {
		if !hasStep(skipped, id) {
			order = append(order, id)
		}
	}
	plan.Order = order
	return plan, skipped
}

func hasStep(steps []install.Step, id string) bool {
	for _, s := range steps {
		if s.ToolID == id {
			return true
		}
	}
	return false
}

// renderSkipped 打印"被跳过的工具"（--all 专属），最多列 8 个。
//
// JSON 模式下写到 stderr：`--json` 的 stdout 必须是一份完整可解析的文档，
// 而"谁被跳过了"是给人看的提示。
func (rt *Runtime) renderSkipped(skipped []install.Step) {
	if len(skipped) == 0 {
		return
	}
	out := rt.out()
	if rt.Opts.JSON {
		out = rt.errOut()
	}
	names := make([]string, 0, len(skipped))
	for _, s := range skipped {
		names = append(names, s.ToolID)
	}
	sort.Strings(names)
	more := ""
	if len(names) > 8 {
		more = fmt.Sprintf(" 等 %d 个", len(names))
		names = names[:8]
	}
	fmt.Fprintf(out, "跳过 %d 个（无可用升级方式，例如系统包管理器管理的）：%s%s\n",
		len(skipped), strings.Join(names, "、"), more)
}

// planFor 计算计划：-i 时先勾选；显式给了工具名就用它们；否则用手写的选择条件。
func (rt *Runtime) planFor(cmd *cobra.Command, f toolSelectFlags, args []string, op install.Op) (install.Plan, error) {
	ids := args
	profile := f.profile
	switch {
	case f.select_:
		if len(args) > 0 {
			return install.Plan{}, usageErrf("--interactive 不能与显式工具名同时使用：用 --profile/--stack/--kind/--tag 缩小候选范围，再在界面里勾选")
		}
		picked, pickedProfile, err := rt.pickIDs(cmd, f, op)
		if err != nil {
			return install.Plan{}, err
		}
		ids, profile = picked, pickedProfile
	case len(ids) == 0:
		selected, err := rt.selectIDs(cmd, f)
		if err != nil {
			return install.Plan{}, err
		}
		ids = selected
	}
	return rt.app().ToolPlan(cmd.Context(), ids, profile, op)
}

// pickIDs 用交互界面让用户勾选要处理的工具（`-i`）。
//
// 候选范围由过滤参数决定（都不给时=清单里所有适用工具），默认勾选"当前不满足目标状态"的那些，
// 于是"看一遍再决定"与"直接回车照默认来"是同一个入口。
// "安装"额外先问一句默认勾上哪些：用户实测反馈默认全勾时想只装几个，缺少一个"先都不勾"的入口
// （upgrade/uninstall 的默认勾选规则不同，没开这一问；它们照样能按 ctrl+x 清空）。
//
// 安装且**没有**用参数圈定范围时，还会先问一句"想怎么选"（见 app.PickToolsScoped）：
// init 的向导里一直有"按套装"的入口，tool install -i 只有一张大列表，想整组装得先记住
// --profile 怎么拼（用户实测反馈）。返回值里的 profile 非空 = 用户选了"整套装安装"。
func (rt *Runtime) pickIDs(cmd *cobra.Command, f toolSelectFlags, op install.Op) ([]string, string, error) {
	if !rt.interactive() {
		return nil, "", coded(ExitNeedsInteraction, errors.New(
			"--interactive 需要终端才能勾选：换到终端里跑，或改用 --profile/--stack/--missing 等参数"))
	}
	opt := app.ToolListOptions{
		Profile: f.profile, Stack: f.stack, Kind: f.kind, Tags: f.tags,
		Missing: f.missing, Refresh: true,
	}
	pick := app.PickOptions{
		Title:      fmt.Sprintf("选择要%s的工具（候选 %s）", install.OpLabel(op), selectionLabel(f)),
		AskDefault: op == install.OpInstall,
	}
	if op == install.OpInstall && !f.scoped() {
		ids, profile, err := rt.app().PickToolsScoped(cmd.Context(), rt.prompter(), opt, pick)
		if err != nil {
			return nil, "", toolErr(err)
		}
		return ids, profile, nil
	}
	ids, err := rt.app().PickTools(cmd.Context(), opt, rt.prompter(), pick)
	if err != nil {
		return nil, "", toolErr(err)
	}
	return ids, f.profile, nil
}

// scoped 报告用户是否已经用参数圈定了候选范围（圈定了就直接进勾选器，不再问"想怎么选"）。
func (f toolSelectFlags) scoped() bool {
	return f.profile != "" || f.stack != "" || f.kind != "" || len(f.tags) > 0 || f.missing
}

// selectionLabel 把过滤条件说成人话，让勾选界面的标题能自解释。
func selectionLabel(f toolSelectFlags) string {
	parts := make([]string, 0, 5)
	if f.profile != "" {
		parts = append(parts, "profile="+f.profile)
	}
	if f.stack != "" {
		parts = append(parts, "stack="+f.stack)
	}
	if f.kind != "" {
		parts = append(parts, "kind="+f.kind)
	}
	for _, t := range f.tags {
		parts = append(parts, "tag="+t)
	}
	if f.missing {
		parts = append(parts, "仅缺失")
	}
	if len(parts) == 0 {
		return "全部适用工具"
	}
	return strings.Join(parts, " ")
}

// selectIDs 把 --stack/--kind/--tag/--missing/--profile/--all 变成一组工具 id。
func (rt *Runtime) selectIDs(cmd *cobra.Command, f toolSelectFlags) ([]string, error) {
	if f.stack == "" && f.kind == "" && len(f.tags) == 0 && !f.missing && f.profile == "" && !f.upgradeAll {
		return nil, usageErrf("请给出工具名，或使用 --profile/--stack/--kind/--missing/--all 选择")
	}
	opt := app.ToolListOptions{
		Profile: f.profile, Stack: f.stack, Kind: f.kind, Tags: f.tags,
		Missing: f.missing, Installed: f.upgradeAll, Refresh: true,
	}
	rows, err := rt.app().ToolRows(cmd.Context(), opt)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		if f.upgradeAll {
			return nil, errNothingToUpgrade
		}
		return nil, preconditionErrf("没有匹配的工具（检查 --stack/--kind/--tag 是否写对）")
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

func (rt *Runtime) runToolOp(cmd *cobra.Command, f toolSelectFlags, args []string, op install.Op) error {
	if err := f.apply(rt); err != nil {
		return err
	}
	plan, err := rt.planFor(cmd, f, args, op)
	if errors.Is(err, errNothingToUpgrade) {
		// "机器上本来就没有已安装的工具"是成功，不是失败：脚本里不该因此中断。
		if rt.Opts.JSON {
			return rt.emit(install.Plan{Steps: []install.Step{}, Order: []string{}})
		}
		fmt.Fprintln(rt.out(), "没有已安装的工具，无需升级（先跑 `bs init` 或 `bs tool install`）。")
		return nil
	}
	if err != nil {
		return toolErr(err)
	}
	// --all 是批量操作：设计上"不做"的那些（系统包管理器升级，§7.6）应当被跳过并列出，
	// 而不是让整条命令以"前置条件不满足"收场——否则 `upgrade --all` 在 Ubuntu 上
	// 几乎必然什么都不做。真正的问题（缺校验和、via 写错）仍然照旧整体失败。
	var skipped []install.Step
	if f.upgradeAll {
		plan, skipped = withoutUnsupported(plan)
	}
	if rt.Opts.DryRun {
		// 干跑到这里就结束：一行命令、一个字节都不会写（安全红线 §8）。
		if rt.Opts.JSON {
			// JSON 的 stdout 只允许一份完整文档：跳过项走 stderr，别把 stdout 拼成两个 JSON。
			rt.renderSkipped(skipped)
			return rt.emit(plan)
		}
		if err := rt.renderPlan(plan, "（dry-run：以下操作都不会被执行）"); err != nil {
			return err
		}
		rt.renderSkipped(skipped)
		return nil
	}
	if !rt.Opts.JSON {
		// 先让人看清"要做什么、按什么顺序、用哪种方式"，再动手。
		if err := rt.renderPlan(plan, ""); err != nil {
			return err
		}
		rt.renderSkipped(skipped)
	} else {
		// --json 下也要能看到"谁被跳过了"，否则脚本无从得知少了哪些工具。
		rt.renderSkipped(skipped)
	}
	// 计划期就失败（所有候选方式都不可用、缺校验和等）时不能返回 0：
	// 那会让脚本以为"装好了"。这里的失败属于前置条件不满足（退出码 4）。
	if failed := failedSteps(plan); len(failed) > 0 {
		return preconditionErrf("%s失败：%s", install.OpLabel(op), strings.Join(failed, "；"))
	}
	if !planHasWork(plan) {
		if rt.Opts.JSON {
			// 脚本靠 stdout 解析结果：这里什么都不输出会让 `bs tool install x --json`
			// 看起来像"命令没跑"，而退出码又是 0。给一份和 --dry-run 一致的计划文档。
			rt.renderSkipped(skipped)
			return rt.emit(plan)
		}
		if len(skipped) > 0 {
			fmt.Fprintln(rt.out(), "没有可升级的工具（下面这些没有可用的升级方式）。")
		}
		rt.renderSkipped(skipped)
		return nil
	}
	// 真会动系统之前先要一句明确同意（安装/升级/卸载都算破坏性操作）。
	if err := rt.confirmDestructive(fmt.Sprintf("即将%s %d 个工具，继续？",
		install.OpLabel(op), plannedCount(plan))); err != nil {
		if errors.Is(err, ui.ErrCancelled) {
			return nil
		}
		return err
	}
	report, err := rt.app().ToolApply(cmd.Context(), plan, op)
	if rt.Opts.JSON {
		if emitErr := rt.emit(report); emitErr != nil {
			return emitErr
		}
		if err != nil {
			return toolApplyErr(err, report)
		}
		return nil
	}
	if rerr := rt.renderReport(report, op); rerr != nil {
		return rerr
	}
	if err != nil {
		return toolApplyErr(err, report)
	}
	return nil
}

// toolApplyErr 把"部分成功"映射为退出码 3，全失败映射为 1（§10）。
func toolApplyErr(err error, report install.Report) error {
	failed, done := 0, 0
	for _, s := range report.Steps {
		switch s.Status {
		case install.StepFailed:
			failed++
		case install.StepDone:
			done++
		}
	}
	if failed > 0 && done > 0 {
		return coded(ExitPartial, err)
	}
	return coded(ExitFail, err)
}

// toolErr 把应用层错误映射到退出码。
func toolErr(err error) error {
	// 已经带退出码的错误（例如命令层的用法错误）原样返回，
	// 否则会被再包一层 failf 变成退出码 1。
	var coded0 *codedError
	if errors.As(err, &coded0) {
		return err
	}
	var via *install.ErrViaUnavailable
	if errors.As(err, &via) {
		if via.Declared {
			return usageErr(err)
		}
		return preconditionErr(err)
	}
	var unknown *app.UnknownToolError
	if errors.As(err, &unknown) {
		return usageErr(err)
	}
	var noMethod *install.ErrNoMethod
	if errors.As(err, &noMethod) {
		return preconditionErr(err)
	}
	var profile *manifest.UnknownProfileError
	if errors.As(err, &profile) {
		return usageErr(err)
	}
	var notFound *install.ErrMethodNotFound
	if errors.As(err, &notFound) {
		return usageErr(err)
	}
	var cycle *install.ErrUnsupported
	if errors.As(err, &cycle) {
		return coded(ExitFail, err)
	}
	return failf("%v", err)
}

func (rt *Runtime) renderToolTable(rows []app.ToolRow) error {
	if len(rows) == 0 {
		fmt.Fprintln(rt.out(), "（没有工具）")
		return nil
	}
	w := rt.newTable()
	fmt.Fprintln(w, "工具\t技术栈\t类型\t状态\t方式\t说明")
	for _, r := range rows {
		status := r.Status
		if status == "" {
			status = "-"
		}
		if r.Version != "" {
			status += " " + r.Version
		}
		note := r.Description
		if note == "" {
			note = r.Detail
		}
		if r.Disabled {
			note = strings.TrimSpace("已禁用 " + note)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Stack, r.Kind, status, r.ViaDisplay, note)
	}
	return w.Flush()
}

func (rt *Runtime) renderToolDetail(d *app.ToolDetail) error {
	out := rt.out()
	fmt.Fprintf(out, "%s", d.ID)
	if d.Name != "" && d.Name != d.ID {
		fmt.Fprintf(out, "（%s）", d.Name)
	}
	fmt.Fprintf(out, "\n  技术栈: %s\t类型: %s\n", d.Stack, d.Kind)
	if d.Description != "" {
		fmt.Fprintf(out, "  说明: %s\n", d.Description)
	}
	if d.Homepage != "" {
		fmt.Fprintf(out, "  主页: %s\n", d.Homepage)
	}
	status := d.Status
	if status == "" {
		status = "-"
	}
	fmt.Fprintf(out, "  状态: %s", status)
	if d.Version != "" {
		fmt.Fprintf(out, " %s", d.Version)
	}
	if d.Detail != "" {
		fmt.Fprintf(out, "（%s）", d.Detail)
	}
	fmt.Fprintln(out)
	if len(d.DependsOn) > 0 {
		fmt.Fprintf(out, "  依赖: %s\n", strings.Join(d.DependsOn, ", "))
	}
	if len(d.Dependents) > 0 {
		fmt.Fprintf(out, "  被依赖: %s\n", strings.Join(d.Dependents, ", "))
	}
	if len(d.Provides) > 0 {
		fmt.Fprintf(out, "  提供: %s\n", strings.Join(d.Provides, ", "))
	}
	if d.Verify.Command != "" {
		fmt.Fprintf(out, "  探测: %s", d.Verify.Command)
		if len(d.Verify.VersionArgs) > 0 {
			fmt.Fprintf(out, " %s", strings.Join(d.Verify.VersionArgs, " "))
		}
		if d.Verify.MinVersion != "" {
			fmt.Fprintf(out, "（最低 %s）", d.Verify.MinVersion)
		}
		if d.Verify.ExactVersion != "" {
			fmt.Fprintf(out, "（要求 %s）", d.Verify.ExactVersion)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, "  安装方式（按清单顺序）:")
	for _, m := range d.Methods {
		mark := "  "
		switch {
		case m.Preferred:
			mark = "→ "
		case !m.Enabled:
			mark = "× "
		case !m.Available:
			mark = "· "
		}
		via := m.Display
		if via == "" {
			via = m.Via
		}
		line := fmt.Sprintf("    %s%s", mark, via)
		if m.Skip != "" {
			line += "（" + m.Skip + "）"
		} else if !m.Available && m.Note != "" {
			line += "（" + m.Note + "）"
		}
		fmt.Fprintln(out, line)
		if len(m.Params) > 0 {
			fmt.Fprintf(out, "        %s\n", app.FormatParams(m.Params))
		}
	}
	return nil
}

// confirmDestructive 在执行破坏性操作前请求确认（docs/design.md §7.5 的 [4] Confirm）。
//
// 三条语义，缺一不可：
//   - `--yes`：直接通过（脚本与 CI 的正规做法）；
//   - 非交互（stdin/输出不是终端、`--json`、`--non-interactive`、`CI=1`）：
//     **绝不阻塞等待输入**，而是以退出码 5 告诉调用方"这里需要授权"；
//   - 交互：打印提示并读一行，只有 y/yes 才继续（默认回车＝拒绝）。
func (rt *Runtime) confirmDestructive(prompt string) error {
	if rt.Opts.Yes {
		return nil
	}
	if !rt.interactive() {
		return coded(ExitNeedsInteraction, errors.New(
			"需要确认但当前是非交互模式：请加 --yes 明确授权（或用 --dry-run 先看计划）"))
	}
	// ui.confirm=false 只在"能交互"时被尊重：用户明确表示过"别问我"。
	// 非交互环境的 --yes 要求不受影响（AGENTS.md §8.6 的红线）。
	if !rt.app().Cfg.UI.Confirm {
		return nil
	}
	ok, err := rt.prompter().Confirm(ui.Question{Title: prompt})
	if err != nil {
		if errors.Is(err, ui.ErrNotInteractive) {
			return coded(ExitNeedsInteraction, errors.New(
				"需要确认但当前是非交互模式：请加 --yes 明确授权（或用 --dry-run 先看计划）"))
		}
		return failf("读取确认输入失败：%v", err)
	}
	if !ok {
		fmt.Fprintln(rt.out(), "已取消，未做任何修改。")
		return ui.ErrCancelled
	}
	return nil
}

// interactive 报告当前是否可以进行交互式提问。
//
// 判定顺序有意分成两组：**用户显式声明**（--json/--non-interactive/--interactive）
// 排在前面，**环境推断**（测试注入、CI 变量、终端探测）排在后面。
// 测试注入之所以要压过 CI：`interactiveOverride` 就是用来替代"环境里有没有人"这件事的，
// 如果它输给 CI 变量，那这套用例在 CI 里必然红——而 CI 正是最需要它们的地方
// （本地有终端、CI 没有，于是"本地绿、CI 红"）。产品行为不受影响：这个字段只在测试里被设过。
func (rt *Runtime) interactive() bool {
	if rt.Opts.JSON || rt.Opts.NonInteractive {
		return false
	}
	// --interactive 是"我的终端被包装器挡住了，但确实有人"时的显式声明。
	if rt.Opts.Interactive {
		return true
	}
	if rt.interactiveOverride != nil {
		return *rt.interactiveOverride
	}
	if os.Getenv("CI") != "" {
		return false
	}
	// 输入输出都得是终端：重定向到文件/管道的场景不能阻塞等待输入。
	in, ok := rt.Opts.In.(io.Writer)
	return ok && app.IsTerminalWriter(in) && app.IsTerminalWriter(rt.Opts.Out)
}

// renderPlan 渲染计划。banner 是首行提示（空表示不提示）——
// 由调用方说明"为什么不会执行"，比在渲染函数里猜调用场景更清楚。
// summarizeList 把一长串 id 压成一行：超过 limit 个就只列前 limit 个，
// 剩下的用"等 N 个"带过——收尾提示不该比表格还长。
func summarizeList(ids []string, limit int) string {
	if len(ids) <= limit {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:limit], ", ") + fmt.Sprintf(" 等 %d 个", len(ids))
}

func (rt *Runtime) renderPlan(plan install.Plan, banner string) error {
	if rt.Opts.JSON {
		return rt.emit(plan)
	}
	out := rt.out()
	if len(plan.Steps) == 0 {
		fmt.Fprintln(out, "没有需要处理的工具。")
		return nil
	}
	if banner != "" {
		fmt.Fprintln(out, banner)
	}
	w := rt.newTable()
	fmt.Fprintln(w, "工具\t动作\t方式\t说明")
	planned := 0
	// "无需处理"的行（已装好、或卸载时本来就没装）不占表格：新机器上真正要看的是
	// "要装什么"。名字不丢——收尾那行把它们列出来，-v 则照旧逐行展开。
	var noop []string
	for _, s := range plan.Steps {
		label := string(s.Status)
		switch s.Status {
		case install.StepInstall, install.StepUpgrade, install.StepUninstall:
			planned++
		case install.StepSkip:
			noop = append(noop, s.ToolID)
			if rt.Opts.Verbose == 0 {
				continue
			}
		}
		note := s.Summary
		if note == "" {
			note = s.Reason
		}
		if s.Error != "" {
			note = s.Error
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.ToolID, label, s.ViaLabel(), note)
		if rt.Opts.Verbose > 0 && s.Action != nil {
			for _, c := range s.Action.Commands {
				fmt.Fprintf(w, "\t\t\t$ %s\n", c.String())
			}
			for _, d := range s.Action.Downloads {
				fmt.Fprintf(w, "\t\t\t↓ %s\n", d.URL)
			}
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, p := range plan.Problems {
		fmt.Fprintf(rt.errOut(), "提示：%s\n", p)
	}
	fmt.Fprintf(out, "共 %d 个工具：%d 个需要处理", len(plan.Steps), planned)
	if len(noop) > 0 {
		fmt.Fprintf(out, "，%d 个无需处理", len(noop))
		if rt.Opts.Verbose == 0 {
			fmt.Fprintf(out, "（%s）", summarizeList(noop, 12))
		}
	}
	fmt.Fprintln(out, "。")
	return nil
}

func (rt *Runtime) renderReport(report install.Report, op install.Op) error {
	out := rt.out()
	for _, fb := range report.Fallbacks {
		fmt.Fprintf(rt.errOut(), "注意：%s 用 %s 安装失败 → 已改用 %s（%s）\n",
			fb.ToolID, fb.From, fb.To, firstLine(fb.Reason))
	}
	var ok, skipped, failed, unsupported int
	var skipNames []string
	w := rt.newTable()
	fmt.Fprintln(w, "工具\t结果\t方式\t说明")
	for _, s := range report.Steps {
		var label string
		switch s.Status {
		case install.StepDone:
			label = "完成"
			ok++
		case install.StepSkip:
			label = "跳过"
			skipped++
			skipNames = append(skipNames, s.ToolID)
			if rt.Opts.Verbose == 0 {
				// 这些在计划表里已经看过一遍了，别再占一行（-v 才逐行展开）。
				continue
			}
		case install.StepFailed:
			label = "失败"
			failed++
		case install.StepUnsupported:
			label = "不支持"
			unsupported++
		default:
			label = string(s.Status)
		}
		note := s.Summary
		if s.Error != "" {
			note = s.Error
		} else if s.Reason != "" && note == "" {
			note = s.Reason
		}
		if s.FallbackFrom != "" {
			note = strings.TrimSpace(fmt.Sprintf("%s（%s 失败后改用）", note, s.FallbackFrom))
		}
		if s.Detection != nil && s.Status == install.StepDone {
			note = strings.TrimSpace(note + " " + s.Detection.Label())
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.ToolID, label, s.ViaLabel(), note)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s：完成 %d，跳过 %d，失败 %d", opLabel(op), ok, skipped, failed)
	if unsupported > 0 {
		fmt.Fprintf(out, "，不支持 %d", unsupported)
	}
	fmt.Fprintf(out, "（耗时 %s）\n", report.Elapsed.Round(1e6))
	if len(skipNames) > 0 && rt.Opts.Verbose == 0 {
		fmt.Fprintf(out, "跳过的 %d 个：%s\n", len(skipNames), summarizeList(skipNames, 12))
	}
	if failed > 0 {
		// 失败时把日志位置说清楚：apt/cargo 的输出很长，只在终端刷一遍是不够的。
		for _, s := range report.Steps {
			if s.Status == install.StepFailed && s.LogPath != "" {
				fmt.Fprintf(out, "日志：%s\n", s.LogPath)
			}
		}
	}
	if report.Changed() {
		fmt.Fprintln(out, "提示：新装的工具可能需要重开 shell 或执行 `eval \"$(bs env path)\"` 才能进入 PATH。")
	}
	return nil
}

func opLabel(op install.Op) string {
	switch op {
	case install.OpUpgrade:
		return "升级"
	case install.OpUninstall:
		return "卸载"
	default:
		return "安装"
	}
}

// plannedCount 数出计划里真正要执行的步骤数（用于确认提示）。
func plannedCount(plan install.Plan) int {
	n := 0
	for _, s := range plan.Steps {
		switch s.Status {
		case install.StepInstall, install.StepUpgrade, install.StepUninstall:
			n++
		}
	}
	return n
}

// failedSteps 收集计划期就失败的步骤（按 "id: 原因" 描述），没有则返回 nil。
func failedSteps(plan install.Plan) []string {
	var out []string
	for _, s := range plan.Steps {
		if s.Status == install.StepFailed {
			out = append(out, s.ToolID+": "+firstLine(s.Error))
		}
	}
	return out
}

// planHasWork 报告计划里是否有真正要执行的动作。
func planHasWork(plan install.Plan) bool {
	for _, s := range plan.Steps {
		switch s.Status {
		case install.StepInstall, install.StepUpgrade, install.StepUninstall:
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
