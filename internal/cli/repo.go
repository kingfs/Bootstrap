package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/app"
	"github.com/kingfs/Bootstrap/internal/gitrepo"
)

func (rt *Runtime) repoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "克隆/更新仓库到统一目录布局",
		Long: `把仓库统一放到 <repo.root>/<host>/<owner>/<repo>。

输入可以是任意常见写法，bs 会归一化：
  https://github.com/kingfs/Bootstrap
  git@github.com:kingfs/Bootstrap.git
  kingfs/Bootstrap
  https://github.com/kingfs/Bootstrap/tree/main/internal/foo   （网页地址，自动识别分支/子目录）

目标已存在时执行 git pull（脏工作区默认跳过，不会覆盖你的改动）。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(rt.repoGetCmd(), rt.repoListCmd(), rt.repoSyncCmd(), rt.repoRootCmd())
	return cmd
}

func (rt *Runtime) repoGetCmd() *cobra.Command {
	opts := app.RepoOptions{}
	var protocol string
	cmd := &cobra.Command{
		Use:   "get <repo>...",
		Short: "克隆或更新一个/多个仓库",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Protocol = protocol
			results, err := rt.app().RepoGet(cmd.Context(), args, opts)
			renderRepoResults(rt, results)
			if err != nil {
				// "部分成功"才有退出码 3；全军覆没是普通失败（1）。
				// 否则脚本会把"一个都没拉下来"当成"拉下来一部分"。
				return coded(batchExitCode(results, func(r app.RepoGetResult) bool {
					return r.Status != "failed"
				}), err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Root, "root", "", "仓库根目录（默认取配置 repo.root 或 ~/src）")
	cmd.Flags().StringVar(&protocol, "protocol", "", "克隆协议：https|ssh（默认取配置）")
	cmd.Flags().IntVar(&opts.Depth, "depth", 0, "浅克隆深度（0 表示完整克隆）")
	cmd.Flags().StringVar(&opts.Ref, "ref", "", "显式指定分支/标签（覆盖从 URL 推断的结果）")
	cmd.Flags().BoolVar(&opts.Autostash, "autostash", false, "pull 前自动 stash 未提交改动")
	return cmd
}

func (rt *Runtime) repoListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出仓库根目录下已有的仓库",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			views, root, err := rt.app().RepoList(cmd.Context())
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(map[string]any{"root": root, "repos": views})
			}
			if len(views) == 0 {
				fmt.Fprintf(rt.out(), "%s 下暂未发现仓库\n", root)
				return nil
			}
			w := rt.newTable()
			fmt.Fprintln(w, "PATH\tBRANCH\tSLUG\tDIRTY")
			for _, v := range views {
				dirty := ""
				if v.Dirty {
					dirty = "yes"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Path, orDash(v.Branch), orDash(v.Slug), dirty)
			}
			return w.Flush()
		},
	}
}

func (rt *Runtime) repoSyncCmd() *cobra.Command {
	var jobs int
	opts := app.RepoOptions{}
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "并发更新根目录下所有仓库",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			views, err := rt.app().RepoSync(cmd.Context(), jobs, opts)
			if err != nil {
				// 全新机器上根目录还不存在，这不是"失败"而是"还没开始"：
				// 用前置条件退出码（4）并直接给出下一步该做什么。
				if errors.Is(err, gitrepo.ErrNoRoot) {
					return preconditionErrf("%v（先运行 `bs repo get <owner/repo>` 拉取第一个仓库）", err)
				}
				return failf("%v", err)
			}
			if len(views) == 0 {
				fmt.Fprintln(rt.out(), "没有需要更新的仓库")
				return nil
			}
			renderRepoStatuses(rt, views)
			failed, ok := 0, 0
			for _, v := range views {
				if v.Action == "failed" {
					failed++
					continue
				}
				ok++
			}
			if failed > 0 {
				return coded(batchExitCode2(ok, failed), fmt.Errorf("有 %d 个仓库更新失败", failed))
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&jobs, "jobs", "j", 0, "并发度（默认取配置 repo.sync.jobs）")
	cmd.Flags().StringVar(&opts.Root, "root", "", "仓库根目录")
	cmd.Flags().BoolVar(&opts.Autostash, "autostash", false, "pull 前自动 stash 未提交改动")
	return cmd
}

func (rt *Runtime) repoRootCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "root",
		Short: "打印仓库根目录及其来源",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			root, source := rt.app().RepoRoot()
			if rt.Opts.JSON {
				return rt.emit(map[string]string{"root": root, "source": source})
			}
			fmt.Fprintf(rt.out(), "%s\t(%s)\n", root, source)
			return nil
		},
	}
}

func renderRepoResults(rt *Runtime, results []app.RepoGetResult) {
	if rt.Opts.JSON {
		_ = rt.emit(results)
		return
	}
	w := rt.newTable()
	for _, r := range results {
		status := r.Status
		if status == "" {
			status = r.Action
		}
		fmt.Fprintf(w, "%-8s %s\n", status, r.Path)
		if rt.Opts.Verbose > 0 || status == "failed" || status == "skipped" || status == "planned" {
			fmt.Fprintf(w, "\t%s\n", r.Message)
			// 跳过的计划不会执行，打印命令只会让人误以为它跑过了。
			if status != "skipped" {
				for _, c := range r.Commands {
					fmt.Fprintf(w, "\t$ %s\n", c)
				}
			}
		}
	}
	_ = w.Flush()
}

func renderRepoStatuses(rt *Runtime, views []app.RepoStatusView) {
	if rt.Opts.JSON {
		_ = rt.emit(views)
		return
	}
	w := rt.newTable()
	fmt.Fprintln(w, "ACTION\tPATH\tMESSAGE")
	for _, v := range views {
		fmt.Fprintf(w, "%s\t%s\t%s\n", orDash(v.Action), v.Path, v.Message)
	}
	_ = w.Flush()
}

// batchExitCode 把"批量结果里有成功也有失败"映射成退出码 3，全失败则是 1。
func batchExitCode[T any](results []T, succeeded func(T) bool) int {
	ok, bad := 0, 0
	for _, r := range results {
		if succeeded(r) {
			ok++
		} else {
			bad++
		}
	}
	return batchExitCode2(ok, bad)
}

// batchExitCode2 是上面那条规则的"已经数好了"的版本。
func batchExitCode2(ok, failed int) int {
	if failed > 0 && ok == 0 {
		return ExitFail
	}
	return ExitPartial
}
