package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/app"
	"github.com/kingfs/Bootstrap/pkg/proxyenv"
)

func (rt *Runtime) proxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "管理代理 profile（并让它在当前 shell 生效）",
		Long: `代理 profile 定义在配置文件的 proxy.profiles 下。

激活后：
  * bs 自己发起的网络操作（如 bs repo get）自动带上代理；
  * 安装了受管块的 shell 里，HTTP(S)_PROXY 会立即生效（bs shell install）。

子进程无法修改父 shell 的环境变量，因此"让当前 shell 生效"依赖
` + "`bs shell install`" + ` 安装的同名 shell 包装函数。不装 rc 也有替代方案：
` + "`bs proxy exec -- <cmd>`" + ` 与 ` + "`bs shell`" + ` 子 shell。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		rt.proxyListCmd(),
		rt.proxyStatusCmd(),
		rt.proxyUseCmd("use"),
		rt.proxyUseCmd("on"),
		rt.proxyOffCmd(),
		rt.proxyEnvCmd(),
		rt.proxyExecCmd(),
		rt.proxyTestCmd(),
		rt.proxyAddCmd(),
	)
	return cmd
}

func (rt *Runtime) proxyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出所有代理 profile",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			profiles, err := rt.app().ProxyProfiles()
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(profiles)
			}
			w := rt.newTable()
			fmt.Fprintln(w, " \tNAME\tHTTP\tHTTPS\tNO_PROXY")
			for _, p := range profiles {
				mark := " "
				if p.Active {
					mark = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", mark, p.Name, orDash(p.HTTPProxy), orDash(p.HTTPSProxy), orDash(p.NoProxy))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if len(profiles) == 0 {
				fmt.Fprintln(rt.out(), "尚未配置任何 profile。可在配置文件 proxy.profiles 下添加，或运行 `bs proxy add <name> --http ...`。")
			}
			return nil
		},
	}
}

func (rt *Runtime) proxyStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "显示当前生效的代理",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			summary, err := rt.app().ProxyStatus()
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(summary)
			}
			switch summary.Source {
			case "none":
				if summary.Mode == "off" {
					fmt.Fprintln(rt.out(), "代理已被显式关闭（bs proxy off）")
					fmt.Fprintln(rt.out(), "提示：`bs proxy use <profile>` 可重新启用。")
				} else {
					fmt.Fprintln(rt.out(), "未启用代理")
				}
			default:
				name := summary.Active
				if name == "" {
					name = "(来自环境变量)"
				}
				fmt.Fprintf(rt.out(), "profile: %s\n来源:    %s\n", name, summary.Detail)
				if summary.HTTPProxy != "" {
					fmt.Fprintf(rt.out(), "http:    %s\n", summary.HTTPProxy)
				}
				if summary.HTTPSProxy != "" {
					fmt.Fprintf(rt.out(), "https:   %s\n", summary.HTTPSProxy)
				}
				if summary.NoProxy != "" {
					fmt.Fprintf(rt.out(), "no_proxy:%s\n", summary.NoProxy)
				}
			}
			if len(summary.ShellEnv) > 0 {
				fmt.Fprintln(rt.out(), "\n当前 shell 已存在的代理变量:")
				w := rt.newTable()
				for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
					if v, ok := summary.ShellEnv[k]; ok {
						fmt.Fprintf(w, "  %s\t%s\n", k, v)
					}
				}
				if err := w.Flush(); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func (rt *Runtime) proxyUseCmd(use string) *cobra.Command {
	return &cobra.Command{
		Use:   use + " [profile]",
		Short: "激活一个代理 profile（省略名字则用 proxy.default_profile）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			res, err := rt.app().ProxyUse(cmd.Context(), name)
			if err != nil {
				return preconditionErr(err)
			}
			if rt.Opts.JSON {
				return rt.emit(map[string]any{"active": res.Name, "source": res.Source})
			}
			fmt.Fprintf(rt.out(), "代理 profile 已激活：%s\n", res.Name)
			if !rt.Opts.DryRun {
				fmt.Fprintln(rt.errOut(), "提示：已装受管块时当前 shell 立即生效；否则用 `bs proxy env --apply` 手动 eval。")
			}
			return nil
		},
	}
}

func (rt *Runtime) proxyOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "停用当前代理 profile",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := rt.app().ProxyOff(cmd.Context()); err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(map[string]any{"active": ""})
			}
			fmt.Fprintln(rt.out(), "代理已停用")
			return nil
		},
	}
}

func (rt *Runtime) proxyEnvCmd() *cobra.Command {
	var (
		shell   string
		profile string
		apply   bool
		off     bool
	)
	cmd := &cobra.Command{
		Use:   "env",
		Short: "输出设置/清除代理环境变量的 shell 语句（供 eval 使用）",
		Long: `把代理环境变量渲染成 shell 语句输出到 stdout，例如：

  eval "$(bs proxy env --apply)"     # 按当前活跃 profile 设置（无则清除）
  bs proxy env --profile chaitin     # 只看某个 profile 的语句
  bs proxy env --off                 # 清除代理变量`,
		Args:         cobra.NoArgs,
		Annotations:  map[string]string{AnnotationTolerateConfigErrors: "true"},
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			sh := shell
			if sh == "" {
				sh = rt.detectedShell()
			}
			// 非法 --shell 属于用法错误（退出码 2），必须在渲染前判定。
			if _, err := proxyenv.NormalizeShell(sh); err != nil {
				return usageErr(err)
			}
			if off {
				profile = ""
				apply = false
			}
			text, err := rt.app().RenderProxyEnv(profile, apply && !off, sh)
			if err != nil {
				return failf("%v", err)
			}
			if text != "" {
				fmt.Fprint(rt.out(), text)
				if text[len(text)-1] != '\n' {
					fmt.Fprintln(rt.out())
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "目标 shell：bash|zsh|fish|pwsh|cmd|dotenv|json")
	cmd.Flags().StringVar(&profile, "profile", "", "指定 profile（默认取当前活跃项）")
	cmd.Flags().BoolVar(&apply, "apply", false, "按当前活跃 profile 应用（无活跃项时输出清除语句）")
	cmd.Flags().BoolVar(&off, "off", false, "输出清除代理变量的语句")
	return cmd
}

func (rt *Runtime) proxyExecCmd() *cobra.Command {
	var profile string
	cmd := &cobra.Command{
		Use:   "exec [--profile P] -- <command> [args...]",
		Short: "在带代理的子进程里执行命令",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := rt.app().ProxyExec(cmd.Context(), profile, args)
			if err != nil {
				return failf("%v", err)
			}
			if code != 0 {
				return coded(code, fmt.Errorf("命令退出码 %d", code))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "使用指定 profile（默认取当前活跃项）")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func (rt *Runtime) proxyTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test [url]",
		Short: "分别测试直连与走代理的连通性（会发起真实网络请求）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			res, err := rt.app().ProxyTest(cmd.Context(), target)
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(res)
			}
			fmt.Fprintf(rt.out(), "目标: %s\n", res.Target)
			printAttempt(rt, "直连", res.Direct)
			printAttempt(rt, "代理("+orDash(res.Profile)+")", res.Proxied)
			return nil
		},
	}
}

func printAttempt(rt *Runtime, label string, a app.TestAttempt) {
	switch {
	case a.OK:
		fmt.Fprintf(rt.out(), "%-16s ok   %s\n", label, a.Latency)
	default:
		fmt.Fprintf(rt.out(), "%-16s 失败 %s\n", label, a.Error)
	}
}

func (rt *Runtime) proxyAddCmd() *cobra.Command {
	var httpProxy, httpsProxy, noProxy string
	var write bool
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "新增一个代理 profile（默认只打印 YAML 片段）",
		Long: `新增代理 profile。

默认只把 YAML 片段打印出来，由你自己粘贴到配置文件——避免工具擅自重排你的注释。
加 --write 才会直接合并进用户配置文件（写前自动备份）。`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			res, err := rt.app().ProxyAdd(args[0], httpProxy, httpsProxy, noProxy, write)
			if err != nil {
				return usageErr(err)
			}
			if rt.Opts.JSON {
				return rt.emit(res)
			}
			if res.Written {
				fmt.Fprintf(rt.out(), "已写入 %s\n", res.File)
				return nil
			}
			if res.File != "" {
				fmt.Fprintf(rt.out(), "将写入 %s（dry-run 未执行）；片段如下：\n\n", res.File)
			} else {
				fmt.Fprintf(rt.out(), "把下面的片段加到 %s 的 proxy.profiles 下：\n\n", rt.app().Paths.ConfigFile())
			}
			fmt.Fprint(rt.out(), res.Snippet)
			return nil
		},
	}
	cmd.Flags().StringVar(&httpProxy, "http", "", "http_proxy 值，如 http://127.0.0.1:7890")
	cmd.Flags().StringVar(&httpsProxy, "https", "", "https_proxy 值（通常与 --http 相同）")
	cmd.Flags().StringVar(&noProxy, "no-proxy", "", "no_proxy 值（逗号分隔）")
	cmd.Flags().BoolVar(&write, "write", false, "直接写入用户配置文件（写前备份）")
	return cmd
}

func (rt *Runtime) detectedShell() string {
	if rt.app() != nil && rt.app().Platform.Shell != "" {
		return rt.app().Platform.Shell
	}
	return "bash"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
