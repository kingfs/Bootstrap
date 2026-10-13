package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/app"
	"github.com/kingfs/Bootstrap/internal/install"
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
	fl.BoolVar(&f.all, "all", false, "包含 disabled 与本机不适用的条目")
	fl.BoolVar(&f.refresh, "refresh", false, "忽略探测缓存，强制重新探测")
	fl.BoolVar(&f.noProbe, "no-probe", false, "不探测安装状态（只列清单，更快）")
}

func (f *toolFilterFlags) options() app.ToolListOptions {
	return app.ToolListOptions{
		Profile: f.profile, Stack: f.stack, Kind: f.kind, Tags: f.tags,
		Installed: f.installed, Missing: f.missing, All: f.all,
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
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.apply(rt); err != nil {
				return err
			}
			plan, err := rt.planFor(cmd, f, args, install.OpInstall)
			if err != nil {
				return toolErr(err)
			}
			return rt.renderPlan(plan, false)
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return rt.runToolOp(cmd, f, args, install.OpUpgrade)
		},
	}
	f.bind(cmd)
	return cmd
}

func (rt *Runtime) toolUninstallCmd() *cobra.Command {
	var f toolSelectFlags
	cmd := &cobra.Command{
		Use:   "uninstall [工具...]",
		Short: "卸载工具（系统包管理器安装的请用系统工具卸载）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return rt.runToolOp(cmd, f, args, install.OpUninstall)
		},
	}
	f.bind(cmd)
	return cmd
}

// planFor 计算计划：显式给了工具名就用它们，否则用手写的选择条件。
func (rt *Runtime) planFor(cmd *cobra.Command, f toolSelectFlags, args []string, op install.Op) (install.Plan, error) {
	ids := args
	if len(ids) == 0 {
		selected, err := rt.selectIDs(cmd, f)
		if err != nil {
			return install.Plan{}, err
		}
		ids = selected
	}
	return rt.app().ToolPlan(cmd.Context(), ids, f.profile, op)
}

// selectIDs 把 --stack/--kind/--tag/--missing/--profile 变成一组工具 id。
func (rt *Runtime) selectIDs(cmd *cobra.Command, f toolSelectFlags) ([]string, error) {
	if f.stack == "" && f.kind == "" && len(f.tags) == 0 && !f.missing && f.profile == "" {
		return nil, usageErrf("请给出工具名，或使用 --profile/--stack/--kind/--missing 选择")
	}
	opt := app.ToolListOptions{
		Profile: f.profile, Stack: f.stack, Kind: f.kind, Tags: f.tags,
		Missing: f.missing, Refresh: true,
	}
	rows, err := rt.app().ToolRows(cmd.Context(), opt)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
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
	if err != nil {
		return toolErr(err)
	}
	if rt.Opts.DryRun {
		// 干跑到这里就结束：一行命令、一个字节都不会写（安全红线 §8）。
		return rt.renderPlan(plan, true)
	}
	if !rt.Opts.JSON {
		// 先让人看清"要做什么、按什么顺序、用哪种方式"，再动手。
		if err := rt.renderPlan(plan, false); err != nil {
			return err
		}
	}
	if !planHasWork(plan) {
		return nil
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
			fmt.Fprintf(out, "        %s\n", formatParams(m.Params))
		}
	}
	return nil
}

func (rt *Runtime) renderPlan(plan install.Plan, dryRun bool) error {
	if rt.Opts.JSON {
		return rt.emit(plan)
	}
	out := rt.out()
	if len(plan.Steps) == 0 {
		fmt.Fprintln(out, "没有需要处理的工具。")
		return nil
	}
	if dryRun {
		fmt.Fprintln(out, "（dry-run：以下操作都不会被执行）")
	}
	w := rt.newTable()
	fmt.Fprintln(w, "工具\t动作\t方式\t说明")
	planned := 0
	for _, s := range plan.Steps {
		label := string(s.Status)
		switch s.Status {
		case install.StepInstall:
			planned++
		case install.StepUpgrade:
			planned++
		case install.StepUninstall:
			planned++
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
	fmt.Fprintf(out, "共 %d 个工具，其中 %d 个需要处理。\n", len(plan.Steps), planned)
	return nil
}

func (rt *Runtime) renderReport(report install.Report, op install.Op) error {
	out := rt.out()
	for _, fb := range report.Fallbacks {
		fmt.Fprintf(rt.errOut(), "注意：%s 用 %s 安装失败 → 已改用 %s（%s）\n",
			fb.ToolID, fb.From, fb.To, firstLine(fb.Reason))
	}
	var ok, skipped, failed, unsupported int
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

func formatParams(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, " ")
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
