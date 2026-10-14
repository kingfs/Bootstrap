package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/app"
	"github.com/kingfs/Bootstrap/internal/selfupdate"
)

// selfUpdateCmd 是 `bs self-update`：把自己换成 GitHub Release 上的新版。
//
// 这是**破坏性**操作（会覆盖正在运行的可执行文件），因此和 `bs tool install`
// 一样先要一句明确同意；`--check` 与 `--dry-run` 什么都不改，不需要同意。
func (rt *Runtime) selfUpdateCmd() *cobra.Command {
	var (
		repo  string
		tag   string
		force bool
		check bool
	)
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "把 bs 自身更新到最新发布版（sha256 校验）",
		Long: `把 bs 自身更新到 GitHub Release 上的最新版本。

流程：查询目标版本 → 展示归档与校验和来源 → 确认 → 下载并**校验 sha256**
（拿不到校验和就中止，绝不"跳过校验"）→ 自检新二进制能跑 → 备份旧二进制 →
原子替换。任何一步失败都会保留原来的 bs。

安全约定：
  * --check    只查版本号，不下载、不替换；
  * --dry-run  把要下载的归档与校验和都查出来给你看，但不下载、不替换；
  * 非交互环境（管道/CI/--json）必须显式给 --yes，否则以退出码 5 结束；
  * 旧版本留在 <bs 路径>.bak-<时间戳>，需要回滚时手工换回即可。

环境变量：
  * BS_REPO              覆盖发布仓库（默认 kingfs/Bootstrap）
  * BS_GITHUB_API        覆盖 GitHub API 前缀（默认 https://api.github.com）
  * BS_GITHUB_ASSET_BASE 覆盖产物与校验和的下载前缀（默认 https://github.com）
                        上面两个用于镜像/加速站；注意校验和与产物来自同一个前缀，
                        镜像必须完全可信（它有能力同时换掉产物和 checksums.txt）。`,
		Example: `  bs self-update --check          # 有没有新版本
  bs self-update                  # 交互确认后更新到最新
  bs self-update --yes            # 不问（脚本/CI）
  bs self-update --dry-run        # 只看会做什么
  bs self-update --version v0.3.0 # 装指定版本（也可用于"降级"）
  bs self-update --force          # 同版本也重装一遍`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// 先判"能不能得到同意"，再去联网：非交互 + 没有 --yes 时不该白跑一趟网络，
			// 更不该在 CI 里悄悄发起请求后才拒绝。
			if !check && !rt.Opts.DryRun && !rt.Opts.Yes && !rt.interactive() {
				return coded(ExitNeedsInteraction, errors.New(
					"需要确认但当前是非交互模式：请加 --yes 明确授权（或用 --check/--dry-run 先看）"))
			}
			opt := app.SelfUpdateOptions{Repo: repo, Tag: tag, Force: force, CheckOnly: check}
			plan, err := rt.app().SelfUpdatePlan(cmd.Context(), opt)
			if err != nil {
				return selfUpdateErr(err)
			}
			if plan.UpToDate {
				return rt.renderSelfUpdateUpToDate(plan)
			}
			if check || rt.Opts.DryRun {
				// 两者都不改文件；dry-run 多一步"校验和从哪来"的信息。
				return rt.renderSelfUpdatePlan(plan, check)
			}
			if err := rt.confirmDestructive(fmt.Sprintf(
				"即将把 %s 从 %s 更新到 %s（%s），继续？", plan.Exe, humanVersion(plan.Current), plan.Tag, plan.Asset)); err != nil {
				return err
			}
			if plan.SystemManaged {
				fmt.Fprintf(rt.errOut(), "注意：%s 不在用户主目录下，可能由系统包管理器安装；自更新会被包管理器覆盖。\n", plan.Exe)
			}
			res, err := rt.app().SelfUpdate(cmd.Context(), opt)
			if err != nil {
				return selfUpdateErr(err)
			}
			return rt.renderSelfUpdateResult(res)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", app.SelfUpdateRepo(), "发布仓库（owner/repo，可用 BS_REPO 覆盖）")
	cmd.Flags().StringVar(&tag, "version", "", "目标版本（如 v0.3.0；缺省取最新）")
	cmd.Flags().BoolVar(&force, "force", false, "当前已是最新时也重装一遍")
	cmd.Flags().BoolVar(&check, "check", false, "只检查有没有新版本，不下载也不替换")
	return cmd
}

// renderSelfUpdateUpToDate 报告"已经是最新"，退出码 0（不是错误）。
func (rt *Runtime) renderSelfUpdateUpToDate(plan selfupdate.Plan) error {
	if rt.Opts.JSON {
		return rt.emit(map[string]any{
			"up_to_date": true, "current": plan.Current, "latest": plan.Tag, "exe": plan.Exe,
		})
	}
	fmt.Fprintf(rt.out(), "已是最新：%s（%s）\n", plan.Tag, plan.Exe)
	return nil
}

// renderSelfUpdatePlan 展示"将要做什么"：--check 只报版本，--dry-run 连归档与校验和一起报。
func (rt *Runtime) renderSelfUpdatePlan(plan selfupdate.Plan, checkOnly bool) error {
	if rt.Opts.JSON {
		return rt.emit(map[string]any{
			"dry_run": rt.Opts.DryRun, "check_only": checkOnly,
			"current": plan.Current, "latest": plan.Tag, "version": plan.Version,
			"asset": plan.Asset, "asset_url": plan.AssetURL,
			"checksums_url": plan.ChecksumsURL, "sha256": plan.SHA256,
			"exe": plan.Exe, "system_managed": plan.SystemManaged,
		})
	}
	out := rt.out()
	fmt.Fprintf(out, "当前版本  %s\n", humanVersion(plan.Current))
	fmt.Fprintf(out, "最新版本  %s\n", plan.Tag)
	fmt.Fprintf(out, "可执行文件 %s\n", plan.Exe)
	fmt.Fprintf(out, "归档      %s\n", plan.Asset)
	if checkOnly {
		fmt.Fprintf(out, "\n（--check 只查询版本；要更新请去掉 --check）\n")
		return nil
	}
	fmt.Fprintf(out, "下载地址  %s\n", plan.AssetURL)
	if plan.SHA256 != "" {
		fmt.Fprintf(out, "校验和文件 %s\n", plan.ChecksumsURL)
		fmt.Fprintf(out, "sha256    %s\n", plan.SHA256)
	}
	if plan.SystemManaged {
		fmt.Fprintf(out, "\n注意：可执行文件不在用户主目录下，可能由系统包管理器安装。\n")
	}
	fmt.Fprintf(out, "\n（dry-run：没有下载、没有替换）\n")
	return nil
}

// renderSelfUpdateResult 报告更新结果与回滚位置。
func (rt *Runtime) renderSelfUpdateResult(res selfupdate.Result) error {
	if rt.Opts.JSON {
		return rt.emit(res)
	}
	out := rt.out()
	if !res.Updated {
		fmt.Fprintln(out, "没有做任何改动。")
		return nil
	}
	fmt.Fprintf(out, "已更新 %s → %s\n", humanVersion(res.Current), res.Tag)
	fmt.Fprintf(out, "可执行文件 %s（sha256 校验通过）\n", res.Exe)
	if res.Backup != "" {
		fmt.Fprintf(out, "旧版本备份 %s\n", res.Backup)
	}
	for _, p := range res.Pruned {
		fmt.Fprintf(out, "已清理旧文件 %s\n", p)
	}
	fmt.Fprintf(out, "\n提示：bash/zsh 的哈希表可能还指向旧文件，必要时执行 `hash -r`。\n")
	return nil
}

// selfUpdateErr 给自更新特有的失败补上"怎么办"，其余交给统一的错误处理。
func selfUpdateErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, selfupdate.ErrNoChecksum):
		return failf("拿不到校验和，出于安全考虑已中止（不会在无校验的情况下替换 bs）：%v", err)
	case errors.Is(err, selfupdate.ErrNoRelease):
		return failf("这个仓库还没有发布任何 Release（先用 goreleaser 发一版，或用 --repo 指向有 Release 的仓库）：%v", err)
	case errors.Is(err, selfupdate.ErrAssetMissing):
		return failf("该平台没有对应的发布产物：%v", err)
	}
	return err
}

// humanVersion 让 "dev" 之类的版本号在提示里不显得像错误。
func humanVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "未知版本"
	}
	return v
}
