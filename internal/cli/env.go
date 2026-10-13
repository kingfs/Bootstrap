package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/pkg/proxyenv"
)

func (rt *Runtime) envCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "查看/装配环境变量（PATH、代理）",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(rt.envShowCmd(), rt.envPathCmd(), rt.envExportCmd())
	return cmd
}

func (rt *Runtime) envShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "显示将被装配的 PATH 条目与代理状态",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			report, err := rt.app().ShowEnv()
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(report)
			}
			fmt.Fprintln(rt.out(), "PATH 条目:")
			w := rt.newTable()
			for _, e := range report.PathEntries {
				state := "缺失"
				if e.Exists {
					state = "存在"
				}
				fmt.Fprintf(w, "  %s\t%s\t%s\n", e.Path, state, e.Source)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(rt.out(), "\n代理: %s (%s)\n", orDash(report.Proxy.Active), report.Proxy.Detail)
			return nil
		},
	}
}

func (rt *Runtime) envPathCmd() *cobra.Command {
	var shell string
	cmd := &cobra.Command{
		Use:          "path",
		Short:        "输出把工具目录加入 PATH 的 shell 语句（供 eval 使用）",
		Args:         cobra.NoArgs,
		Annotations:  map[string]string{AnnotationTolerateConfigErrors: "true"},
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			sh := shell
			if sh == "" {
				sh = rt.detectedShell()
			}
			normalized, err := proxyenv.NormalizeShell(sh)
			if err != nil {
				return usageErr(err)
			}
			text, err := rt.app().RenderPath(normalized)
			if err != nil {
				return failf("%v", err)
			}
			if text == "" {
				rt.Log.Debug("PATH 已包含全部条目，无需修改")
				return nil
			}
			fmt.Fprintln(rt.out(), text)
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "目标 shell：bash|zsh|fish|pwsh|cmd|dotenv")
	return cmd
}

func (rt *Runtime) envExportCmd() *cobra.Command {
	var shell string
	cmd := &cobra.Command{
		Use:          "export",
		Short:        "输出 PATH + 代理的完整 shell 语句（供 eval 使用）",
		Args:         cobra.NoArgs,
		Annotations:  map[string]string{AnnotationTolerateConfigErrors: "true"},
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			sh := shell
			if sh == "" {
				sh = rt.detectedShell()
			}
			normalized, err := proxyenv.NormalizeShell(sh)
			if err != nil {
				return usageErr(err)
			}
			pathText, err := rt.app().RenderPath(normalized)
			if err != nil {
				return failf("%v", err)
			}
			proxyText, err := rt.app().RenderProxyEnv("", true, sh)
			if err != nil {
				return failf("%v", err)
			}
			if pathText != "" {
				fmt.Fprintln(rt.out(), pathText)
			}
			if proxyText != "" {
				fmt.Fprint(rt.out(), proxyText)
				if proxyText[len(proxyText)-1] != '\n' {
					fmt.Fprintln(rt.out())
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "目标 shell：bash|zsh|fish|pwsh|cmd|dotenv")
	return cmd
}
