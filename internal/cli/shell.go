package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/pkg/fsx"
)

func (rt *Runtime) shellCmd() *cobra.Command {
	var profile string
	cmd := &cobra.Command{
		Use:   "shell [--profile P]",
		Short: "打开已装配环境的子 shell（不修改任何 rc）",
		Long: `打开一个交互式子 shell，其中已加载 bs 的全部片段与代理设置。

这是"不想动 rc 文件"时的替代方案；想让每次登录都生效，请用 ` + "`bs shell install`" + `。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			code, err := rt.app().ShellSubshell(cmd.Context(), rt.app().Platform.Shell, profile)
			if err != nil {
				return failf("%v", err)
			}
			if code != 0 {
				return coded(code, fmt.Errorf("子 shell 退出码 %d", code))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "在子 shell 中激活的代理 profile")
	cmd.AddCommand(
		rt.shellInitCmd(),
		rt.shellInstallCmd(),
		rt.shellUninstallCmd(),
		rt.shellStatusCmd(),
		rt.shellSnippetCmd(),
	)
	return cmd
}

func (rt *Runtime) shellInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init [shell]",
		Short: "输出受管块内容（片段 + 包装函数），供 rc 里 eval",
		Long: `输出将被 eval 到当前 shell 的内容。

受管块里只有一行：
  eval "$(bs shell init bash)"

真正的片段内容（代理包装函数、PATH 装配、别名）都在 bs 内部维护，
升级 bs 即可获得新片段，rc 文件永远不需要改。`,
		Args:         cobra.MaximumNArgs(1),
		Annotations:  map[string]string{AnnotationTolerateConfigErrors: "true"},
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			shell := rt.detectedShell()
			if len(args) == 1 {
				shell = args[0]
			}
			init, err := rt.app().ShellInit(shell)
			if err != nil {
				return usageErr(err)
			}
			if rt.Opts.JSON {
				return rt.emit(init)
			}
			fmt.Fprint(rt.out(), init.Content)
			return nil
		},
	}
}

func (rt *Runtime) shellInstallCmd() *cobra.Command {
	var shell, rcFile string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "把受管块幂等写入 rc 文件（写前自动备份）",
		Long: `把受管块写入 rc 文件。

特性：
  * 幂等：重复执行不会产生重复内容，内容一致时完全不触碰文件；
  * 可精确卸载：bs shell uninstall 只移除受管块，块外内容字节级不动；
  * 可预览：--dry-run 只看会改什么，不写文件。`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			sh := shell
			if sh == "" {
				sh = rt.detectedShell()
			}
			res, err := rt.app().ShellInstall(sh, rcFile)
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(res)
			}
			switch {
			case !res.Changed:
				fmt.Fprintf(rt.out(), "%s 中的受管块已是最新，未修改\n", res.RCFile)
			case rt.Opts.DryRun:
				fmt.Fprintf(rt.out(), "[dry-run] 将更新 %s（%s）\n", res.RCFile, res.DryRunDiff)
			default:
				fmt.Fprintf(rt.out(), "已更新 %s（shell=%s）\n", res.RCFile, res.Shell)
				if res.Backup != "" {
					fmt.Fprintf(rt.out(), "备份：%s\n", res.Backup)
				}
				fmt.Fprintln(rt.errOut(), "提示：新开的 shell 会自动加载；当前 shell 可执行 `exec $SHELL -l` 立即生效。")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "目标 shell（默认自动探测）")
	cmd.Flags().StringVar(&rcFile, "rc", "", "指定 rc 文件路径（默认按 shell 约定探测）")
	return cmd
}

func (rt *Runtime) shellUninstallCmd() *cobra.Command {
	var shell, rcFile string
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "移除 rc 文件中的受管块（块外内容不变）",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			sh := shell
			if sh == "" {
				sh = rt.detectedShell()
			}
			res, err := rt.app().ShellUninstall(sh, rcFile)
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(res)
			}
			switch {
			case !res.Removed:
				fmt.Fprintf(rt.out(), "%s 中没有受管块，未修改\n", res.RCFile)
			case rt.Opts.DryRun:
				fmt.Fprintf(rt.out(), "[dry-run] 将从 %s 移除受管块\n", res.RCFile)
			default:
				fmt.Fprintf(rt.out(), "已从 %s 移除受管块\n", res.RCFile)
				if res.Backup != "" {
					fmt.Fprintf(rt.out(), "备份：%s\n", res.Backup)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "目标 shell（默认自动探测）")
	cmd.Flags().StringVar(&rcFile, "rc", "", "指定 rc 文件路径")
	return cmd
}

func (rt *Runtime) shellStatusCmd() *cobra.Command {
	var shell, rcFile string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "显示受管块安装状态",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			sh := shell
			if sh == "" {
				sh = rt.detectedShell()
			}
			st, err := rt.app().ShellStatusOf(sh, rcFile)
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(st)
			}
			state := "未安装"
			switch {
			case st.HasBlock && st.UpToDate:
				state = "已安装且为最新"
			case st.HasBlock:
				state = "已安装但内容过期（`bs shell install` 可原地更新）"
			case st.RCExists:
				state = "rc 存在，但未安装受管块"
			}
			fmt.Fprintf(rt.out(), "shell:  %s\nrc:     %s\n状态:   %s\n片段:   %d 个\n",
				st.Shell, st.RCFile, state, st.Snippets)
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "目标 shell（默认自动探测）")
	cmd.Flags().StringVar(&rcFile, "rc", "", "指定 rc 文件路径")
	return cmd
}

func (rt *Runtime) shellSnippetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snippet",
		Short: "查看/创建 shell 片段",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(rt.shellSnippetListCmd(), rt.shellSnippetPathCmd(), rt.shellSnippetNewCmd())
	return cmd
}

func (rt *Runtime) shellSnippetListCmd() *cobra.Command {
	var shell string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出片段及其来源（内置或用户覆盖）",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			sh := shell
			if sh == "" {
				sh = rt.detectedShell()
			}
			snippets, userDir, err := rt.app().ShellSnippets(sh)
			if err != nil {
				return usageErr(err)
			}
			if rt.Opts.JSON {
				return rt.emit(map[string]any{"shell": sh, "user_dir": userDir, "snippets": snippets})
			}
			w := rt.newTable()
			for _, s := range snippets {
				origin := "builtin"
				if !s.Builtin {
					origin = s.Source
				}
				fmt.Fprintf(w, "  %s\t%s\n", s.Name, origin)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(rt.out(), "\n用户片段目录：%s\n（同名文件整体覆盖内置片段，编号决定加载顺序）\n", userDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "目标 shell（默认自动探测）")
	return cmd
}

func (rt *Runtime) shellSnippetPathCmd() *cobra.Command {
	var shell string
	cmd := &cobra.Command{
		Use:   "path",
		Short: "打印用户片段目录",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			sh := shell
			if sh == "" {
				sh = rt.detectedShell()
			}
			dir, err := rt.app().SnippetUserDir(sh)
			if err != nil {
				return usageErr(err)
			}
			fmt.Fprintln(rt.out(), dir)
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "目标 shell（默认自动探测）")
	return cmd
}

func (rt *Runtime) shellSnippetNewCmd() *cobra.Command {
	var shell string
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "在用户片段目录创建一个空片段文件",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			sh := shell
			if sh == "" {
				sh = rt.detectedShell()
			}
			dir, err := rt.app().SnippetUserDir(sh)
			if err != nil {
				return usageErr(err)
			}
			name := args[0]
			if filepath.Ext(name) == "" {
				name += ".sh"
			}
			path := filepath.Join(dir, name)
			if fsx.IsFile(path) {
				return preconditionErrf("%s 已存在，未修改", path)
			}
			if rt.Opts.DryRun {
				fmt.Fprintf(rt.out(), "[dry-run] 将创建 %s\n", path)
				return nil
			}
			content := fmt.Sprintf("# %s —— 用户片段，覆盖同名内置片段\n", name)
			if err := fsx.AtomicWriteFile(path, []byte(content), 0o644); err != nil {
				return failf("%v", err)
			}
			fmt.Fprintf(rt.out(), "已创建 %s\n", path)
			if editor := os.Getenv("EDITOR"); editor != "" {
				fmt.Fprintf(rt.out(), "编辑：%s %s\n", editor, path)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "目标 shell（默认自动探测）")
	return cmd
}
