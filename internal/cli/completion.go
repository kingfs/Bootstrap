package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/app"
)

// 本文件只做两件事：给 cobra 自动生成的 `completion` 命令补中文用法说明，
// 并挂上 `bs completion install` 子命令。
//
// 刻意**不自己生成补全脚本**：脚本仍由 cobra 维护，才能永远与命令树同步
// （新增子命令/参数后无需改这里）。所以这里不改 `completion` 的生成逻辑，
// 只在命令树上加说明与一个安装入口。

// completionGroupLong 是 `bs completion --help` 的中文说明。
const completionGroupLong = `生成 bs 的 shell 补全脚本。

两种用法：

1) 打印脚本（由 cobra 生成，适合脚本/包管理器接管）：
     eval "$(bs completion bash)"                 # 当前 bash 会话临时加载
     bs completion zsh > "${fpath[1]}/_bs"        # 交给 zsh 的 fpath 机制加载
     bs completion fish > ~/.config/fish/completions/bs.fish

2) 交给 bs 自动装好（推荐）：
     bs completion install [bash|zsh|fish|pwsh]

   install 打开配置里的 shell.completion 开关，并幂等写入 rc 的统一受管块
   （与 bs shell install 是同一个块：块里只有一行 eval "$(bs shell init <shell>)"，
   补全内容每次开 shell 现算）。写前都会备份，块外内容字节级不动；--dry-run 只预览。

   rc 文件：bash → ~/.bashrc（macOS 优先 ~/.bash_profile）
            zsh  → $ZDOTDIR/.zshrc 或 ~/.zshrc
            fish → ~/.config/fish/conf.d/bootstrap.fish
            pwsh → ~/.config/powershell/Microsoft.PowerShell_profile.ps1

   bash 的补全脚本运行时还需要系统的 bash-completion 包（cobra 生成脚本自身的依赖），
   install 时会替你检查并提示。zsh 由 bs 负责补一次 compinit。

   关闭：bs config set shell.completion false（受管块可留给 PATH/代理包装继续用）。`

// completionPerShellHint 追加在每个 shell 子命令的英文说明之后。
const completionPerShellHint = `
提示：不想手工重定向就用 ` + "`bs completion install <shell>`" + `——打开 shell.completion 开关
并幂等写入 rc 的统一受管块（写前备份，--dry-run 可预览）；上面的 > 重定向写法依然可用。`

// decorateCompletionCmd 在 cobra 生成完默认补全命令后给它补说明与 install 入口。
func (rt *Runtime) decorateCompletionCmd(root *cobra.Command) {
	var comp *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "completion" {
			comp = c
			break
		}
	}
	if comp == nil {
		return
	}
	comp.Long = completionGroupLong
	// 根帮助里列的是 Short，也换成中文，免得"生成的"和"帮装的"两种用法只能看 Long 才知道。
	comp.Short = "生成 shell 补全脚本，或交给 bs 自动装进 rc"
	comp.AddCommand(rt.completionInstallCmd())

	// 这四个子命令会被 `eval` 进新 shell（shell.completion=true 时），因此必须能在
	// 配置损坏时照样工作——否则每次登录都会因为配置写错而报错。
	comp.Annotations = tolerateConfigErrors(comp.Annotations)
	for _, sub := range comp.Commands() {
		if sub.Name() == "install" {
			continue
		}
		sub.Long = strings.TrimRight(sub.Long, "\n") + "\n" + completionPerShellHint
		sub.Annotations = tolerateConfigErrors(sub.Annotations)
	}
}

// tolerateConfigErrors 把"配置损坏也要能用"的注解合并进一个命令。
func tolerateConfigErrors(ann map[string]string) map[string]string {
	if ann == nil {
		ann = map[string]string{}
	}
	ann[AnnotationTolerateConfigErrors] = "true"
	return ann
}

func (rt *Runtime) completionInstallCmd() *cobra.Command {
	var shell, rcFile string
	cmd := &cobra.Command{
		Use:   "install [shell]",
		Short: "打开补全开关并写进 rc 受管块（写前备份，--dry-run 可预览）",
		Long: `把 bs 补全装到目标 shell 的每个新会话里。

它做两件幂等的事：
  1. 打开用户级配置的 shell.completion（已经是 true 就完全不碰配置文件）；
  2. 把 rc 的统一受管块写到最新（与 bs shell install 是同一个块）。

不额外写"补全专用"的受管块：rc 里始终只有一个受管块，补全内容由
eval "$(bs shell init <shell>)" 现算——升级 bs 后重开 shell 就拿到新补全，
不必重跑本命令。

所有写入都先备份；--dry-run 一个字节都不落盘。
关闭补全：bs config set shell.completion false（受管块可继续给 PATH/代理包装用）。

前提：bash 的补全脚本运行时需要系统的 bash-completion 包（cobra 生成脚本自身的依赖，
Debian/Ubuntu 用 apt install bash-completion）；缺了它本命令会提示，但不会失败——
zsh 的 compinit 由 bs 在片段里补一次，不需要额外准备。`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			sh := shell
			if len(args) == 1 {
				sh = args[0]
			}
			if sh == "" {
				sh = rt.detectedShell()
			}
			res, err := rt.app().CompletionInstall(sh, rcFile)
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(res)
			}
			rt.renderCompletionInstall(res)
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "目标 shell（默认自动探测）")
	cmd.Flags().StringVar(&rcFile, "rc", "", "指定 rc 文件路径（默认按 shell 约定探测）")
	return cmd
}

func (rt *Runtime) renderCompletionInstall(res app.CompletionInstallResult) {
	prefix := ""
	if res.DryRun {
		prefix = "[dry-run] "
	}
	switch {
	case res.ConfigSet && res.DryRun:
		fmt.Fprintf(rt.out(), "%s将打开 %s 的 shell.completion\n", prefix, res.ConfigFile)
	case res.ConfigSet:
		fmt.Fprintf(rt.out(), "已打开 %s 的 shell.completion\n", res.ConfigFile)
	default:
		fmt.Fprintf(rt.out(), "%s%s 的 shell.completion 已是 true，未修改\n", prefix, res.ConfigFile)
	}
	if res.RCChanged {
		fmt.Fprintf(rt.out(), "%s将把受管块写入/更新到 %s（shell=%s）\n", prefix, res.RCFile, res.Shell)
	} else {
		fmt.Fprintf(rt.out(), "%s%s 中的受管块已是最新，未修改\n", prefix, res.RCFile)
	}
	if res.ConfigBackup != "" {
		fmt.Fprintf(rt.out(), "配置备份：%s\n", res.ConfigBackup)
	}
	if res.RCBackup != "" {
		fmt.Fprintf(rt.out(), "rc 备份：%s\n", res.RCBackup)
	}
	for _, h := range res.Hints {
		fmt.Fprintf(rt.errOut(), "注意：%s\n", h)
	}
	if res.DryRun {
		return
	}
	fmt.Fprintln(rt.errOut(), "提示：新开的 shell 会自动带上补全；当前 shell 可执行 `exec $SHELL -l` 立即生效。")
	fmt.Fprintln(rt.errOut(), "关闭：`bs config set shell.completion false`（受管块保留给 PATH/代理包装）。")
}
