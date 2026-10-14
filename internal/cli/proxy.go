package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/app"
	"github.com/kingfs/Bootstrap/internal/shellsnip"
	"github.com/kingfs/Bootstrap/pkg/proxyenv"
)

// inShellHook 报告这次调用是不是来自 rc 里的包装函数（tools/snippets/*/10-proxy.*）。
//
// 是的话，紧接着就会 `eval "$(bs proxy env --apply)"`，代理在当前 shell 真的生效，
// 不需要再教用户怎么做；不是的话必须提示一句，否则用户会以为改了 state 就等于生效。
func inShellHook() bool { return os.Getenv(shellsnip.HookEnv) == "1" }

// hintShellHook 只在"没人会 eval 我们的输出"时，提醒一次怎么让当前 shell 生效。
//
// 走 stderr：stdout 保持单行可被脚本消费（`bs proxy use x` 的结果仍然一眼能读）。
func (rt *Runtime) hintShellHook() {
	if rt.Opts.DryRun || inShellHook() {
		return
	}
	fmt.Fprintln(rt.errOut(), "提示：当前 shell 未装包装函数，代理只在 bs 自己的网络操作里生效；`bs shell install` 一次即可自动注入。")
}

func (rt *Runtime) proxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "管理代理 profile（并让它在当前 shell 生效）",
		Long: `代理 profile 定义在配置文件的 proxy.profiles 下。

激活后：
  * bs 自己发起的网络操作（如 bs repo get）自动带上代理；
  * 装了 shell 受管块的终端里，HTTP(S)_PROXY 立即生效。

子进程改不了父 shell 的环境变量，所以"当前 shell 立即生效"靠 ` + "`bs shell install`" + `
写进 rc 的同名包装函数：它拦截 on/off/use，执行完真二进制后再
` + "`eval \"$(bs proxy env --apply)\"`" + `。同一份片段里还有 ` + "`startproxy [profile]`" + `
与 ` + "`stopproxy`" + ` 两个函数，习惯手敲函数的人可以直接用。

不想碰 rc 的替代方案：` + "`bs proxy exec -- <cmd>`" + `（只给一条命令加代理）、
` + "`bs shell [--profile P]`" + `（开一个已装配环境的子 shell）、
` + "`eval \"$(bs proxy env --apply)\"`" + `（手动注入当前 shell）。`,
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
			// "shell 里已经存在的变量"只在排障时有价值（例如怀疑某个 rc 脚本抢先设了
			// 代理、覆盖了 bs 的输出），而它通常和上面的 profile 完全重复，默认就是噪音；
			// 要看加 -v（--json 里始终保留，脚本消费不受影响）。
			if len(summary.ShellEnv) > 0 && rt.Opts.Verbose > 0 {
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
		Long: `激活一个代理 profile（省略名字时用配置里的 proxy.default_profile）。

它只写 bs 自己的 state，**不碰你的 rc**。让"当前 shell 立即生效"的是 rc 里由
` + "`bs shell install`" + ` 装下的同名包装函数：它拦截 on/off/use，执行完真二进制后再
` + "`eval \"$(bs proxy env --apply)\"`" + `。

没装包装函数时（可用 ` + "`bs shell status`" + ` 查），本次只改 bs 的状态，当前 shell 不变；
可以改用：
  eval "$(bs proxy env --apply)"   # 手动注入当前 shell
  bs proxy exec -- <命令...>        # 只给这一条命令加代理
  bs shell [--profile P]           # 开一个已装配环境的子 shell`,
		Args: cobra.MaximumNArgs(1),
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
			rt.hintShellHook()
			return nil
		},
	}
}

func (rt *Runtime) proxyOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "停用当前代理 profile",
		Long: `停用当前代理 profile：清空 state 里的活跃项，此后不再回退到环境变量，
直到重新 ` + "`bs proxy use <name>`" + `。

它只写 bs 自己的 state，**不碰你的 rc**；让当前 shell 立即 unset 的仍是 rc 里的
同名包装函数（见 ` + "`bs proxy use --help`" + `）。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := rt.app().ProxyOff(cmd.Context()); err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(map[string]any{"active": ""})
			}
			fmt.Fprintln(rt.out(), "代理已停用")
			rt.hintShellHook()
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
  bs proxy env --profile corp        # 只看某个 profile 的语句
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
				fmt.Fprintf(rt.out(), "[dry-run] 将写入 %s，片段如下：\n", res.File)
			} else {
				// 用 YAML 注释而不是说明段落：用户可以直接把整段复制进配置文件。
				fmt.Fprintf(rt.out(), "# 粘到 %s 的 proxy.profiles 下（或加 --write 让 bs 写入）\n", rt.app().Paths.ConfigFile())
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
