package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/app"
)

func (rt *Runtime) doctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "体检：检查环境、配置、代理与 shell 集成状态（只读）",
		Long: `检查项全部是只读的：doctor 不会创建目录、不会写配置文件、不会发起网络请求。

需要验证网络/代理连通性时请显式运行 ` + "`bs proxy test`" + `。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := rt.app().Doctor(cmd.Context())
			if rt.Opts.JSON {
				ok, warn, fail, skip := app.CheckSummary(checks)
				if err := rt.emit(map[string]any{
					"checks":  checks,
					"summary": map[string]int{"ok": ok, "warn": warn, "fail": fail, "skip": skip},
				}); err != nil {
					return err
				}
				if fail > 0 {
					return coded(ExitPrecondition, fmt.Errorf("%d 项检查未通过", fail))
				}
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
			if fail > 0 {
				return coded(ExitPrecondition, fmt.Errorf("%d 项检查未通过", fail))
			}
			return nil
		},
	}
	return cmd
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
