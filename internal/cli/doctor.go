package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/app"
	"github.com/kingfs/Bootstrap/internal/ui"
)

func (rt *Runtime) doctorCmd() *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "体检：检查环境、配置、代理与 shell 集成状态（只读）",
		Long: `检查项默认全部是只读的：doctor 不会创建目录、不会写配置文件、不会发起网络请求。

加 --fix 时才会动手，而且只修"结论明确、修法唯一"的问题：
缺配置文件（生成默认配置）、缺 bs 自己的目录、缺仓库根目录、缺 shell 受管块（写前备份）。
装工具、改代理、修清单这类要拿主意的事只给提示，不代劳。

--fix 一样支持 --dry-run（只看会修什么）；非交互环境需要 --yes，否则以退出码 5 结束。

需要验证网络/代理连通性时请显式运行 ` + "`bs proxy test`" + `。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return rt.runDoctor(cmd, fix)
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "自动修复可安全修复的问题（缺配置/目录/仓库根/受管块）")
	return cmd
}

// runDoctor 是 `bs doctor [--fix]` 的完整流程：体检 → （可选）修复 → 复查。
func (rt *Runtime) runDoctor(cmd *cobra.Command, fix bool) error {
	checks := rt.app().Doctor(cmd.Context())
	if err := rt.renderChecks(checks); err != nil {
		return err
	}
	if !fix {
		return rt.finishDoctor(checks)
	}

	plan := rt.app().PlanFixes(checks)
	if rt.Opts.JSON {
		// JSON 模式下把体检与修复一起交给调用方，不在终端上打表格。
	} else if err := rt.renderFixPlan(plan); err != nil {
		return err
	}
	if plan.Fixable() == 0 {
		if !rt.Opts.JSON {
			fmt.Fprintln(rt.out(), "没有可自动修复的问题（其余项请按提示处理）。")
		}
		if rt.Opts.DryRun {
			return rt.emitDoctorFix(checks, plan, checks)
		}
		return rt.finishDoctor(checks)
	}
	if rt.Opts.DryRun {
		if !rt.Opts.JSON {
			fmt.Fprintln(rt.out(), "（dry-run：以上修复都不会被执行）")
		}
		return rt.emitDoctorFix(checks, plan, checks)
	}

	// 真会写文件（配置/目录/rc）之前先要一句同意：与工具安装同一套规则。
	if err := rt.confirmDestructive(fmt.Sprintf("即将自动修复 %d 项，继续？", plan.Fixable())); err != nil {
		if errors.Is(err, ui.ErrCancelled) {
			fmt.Fprintln(rt.out(), "已取消，未做任何修改。")
			return nil
		}
		return err
	}
	result := rt.app().ApplyFixes(cmd.Context(), plan)
	if !rt.Opts.JSON {
		if err := rt.renderFixResult(result); err != nil {
			return err
		}
	}
	// 复查：修完立刻看还剩什么（同一个 App 实例即可，doctor 的检查都读实时状态）。
	after := rt.app().Doctor(cmd.Context())
	if !rt.Opts.JSON {
		fmt.Fprintln(rt.out())
		if err := rt.renderChecks(after); err != nil {
			return err
		}
	}
	if err := rt.emitDoctorFix(checks, result, after); err != nil {
		return err
	}
	if failed := failedFixes(result); failed > 0 {
		return coded(ExitFail, fmt.Errorf("%d 项自动修复失败", failed))
	}
	return rt.doctorResult(after)
}

// finishDoctor 输出"没有修复动作"时的结果（JSON 也在这里出现），再决定退出码。
func (rt *Runtime) finishDoctor(checks []app.Check) error {
	if err := rt.emitDoctorFix(checks, app.DoctorFixPlan{}, checks); err != nil {
		return err
	}
	return rt.doctorResult(checks)
}

// doctorResult 按体检结论决定退出码（有 fail 就是前置条件不满足）。
func (rt *Runtime) doctorResult(checks []app.Check) error {
	_, _, fail, _ := app.CheckSummary(checks)
	if fail > 0 {
		return coded(ExitPrecondition, fmt.Errorf("%d 项检查未通过", fail))
	}
	return nil
}

func (rt *Runtime) renderChecks(checks []app.Check) error {
	if rt.Opts.JSON {
		return nil
	}
	w := rt.newTable()
	for _, c := range checks {
		fmt.Fprintf(w, "%s\t%s\t%s\n", glyph(c.Status), c.Name, c.Detail)
		if c.Hint != "" && c.Status != app.StatusOK {
			fmt.Fprintf(w, "\t\t→ %s\n", c.Hint)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	ok, warn, fail, skip := app.CheckSummary(checks)
	fmt.Fprintf(rt.out(), "\n通过 %d，警告 %d，失败 %d，跳过 %d\n", ok, warn, fail, skip)
	return nil
}

// renderFixPlan 列出"将要修什么"，每项带上具体路径——同意之前必须看得见。
func (rt *Runtime) renderFixPlan(plan app.DoctorFixPlan) error {
	if len(plan.Actions) == 0 {
		return nil
	}
	fmt.Fprintf(rt.out(), "\n可自动修复 %d 项：\n", plan.Fixable())
	for _, a := range plan.Actions {
		mark := "·"
		if a.Blocked {
			mark = "!"
		}
		fmt.Fprintf(rt.out(), "  %s %s — %s\n", mark, a.Title, a.Detail)
		if a.Blocked && a.Message != "" {
			fmt.Fprintf(rt.out(), "      → %s\n", a.Message)
		}
	}
	return nil
}

func (rt *Runtime) renderFixResult(result app.DoctorFixPlan) error {
	fmt.Fprintln(rt.out(), "\n修复结果：")
	for _, a := range result.Actions {
		status := "已修复"
		if a.Blocked {
			status = "跳过"
		}
		fmt.Fprintf(rt.out(), "  %s %s — %s\n", status, a.Title, a.Message)
	}
	return nil
}

// emitDoctorFix 在 `--json` 下输出体检 + 修复 + 复查的合并结果。
func (rt *Runtime) emitDoctorFix(before []app.Check, fixes app.DoctorFixPlan, after []app.Check) error {
	if !rt.Opts.JSON {
		return nil
	}
	checks := after
	if len(checks) == 0 {
		checks = before
	}
	return rt.emit(map[string]any{
		"checks":  checks,
		"summary": app.DoctorFixSummary(checks),
		"fixes":   fixes.Actions,
	})
}

func failedFixes(plan app.DoctorFixPlan) int {
	n := 0
	for _, a := range plan.Actions {
		if a.Err != "" {
			n++
		}
	}
	return n
}

func glyph(s app.CheckStatus) string {
	switch s {
	case app.StatusOK:
		return "  ok "
	case app.StatusWarn:
		return "warn "
	case app.StatusFail:
		return "FAIL "
	default:
		return "skip "
	}
}
