package cli

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/config"
)

func (rt *Runtime) configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "查看与管理配置文件",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(rt.configPathCmd(), rt.configShowCmd(), rt.configValidateCmd(), rt.configInitCmd(),
		rt.configGetCmd(), rt.configSetCmd(), rt.configEditCmd(), rt.configKeysCmd())
	return cmd
}

func (rt *Runtime) configPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "打印各配置/状态目录的实际路径",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			items := rt.app().Paths.Describe(runtime.GOOS)
			if rt.Opts.JSON {
				out := map[string]string{}
				for _, kv := range items {
					out[kv[0]] = kv[1]
				}
				out["loaded_configs"] = fmt.Sprint(rt.app().ConfigSources)
				return rt.emit(out)
			}
			w := rt.newTable()
			for _, kv := range items {
				fmt.Fprintf(w, "%s\t%s\n", kv[0], kv[1])
			}
			if len(rt.app().ConfigSources) > 0 {
				fmt.Fprintf(w, "loaded\t%s\n", rt.app().ConfigSources)
			} else {
				fmt.Fprintf(w, "loaded\t(仅内置默认值)\n")
			}
			return w.Flush()
		},
	}
}

func (rt *Runtime) configShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "打印合并后的生效配置",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if rt.Opts.JSON {
				return rt.emit(rt.app().Cfg)
			}
			data, err := config.Marshal(rt.app().Cfg)
			if err != nil {
				return failf("%v", err)
			}
			_, err = rt.out().Write(data)
			return err
		},
	}
}

func (rt *Runtime) configValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "校验配置文件的合法性",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg := rt.app().Cfg
			if err := cfg.Validate(); err != nil {
				return preconditionErr(fmt.Errorf("配置校验失败：\n%w", err))
			}
			result := map[string]any{
				"ok":      true,
				"sources": rt.app().ConfigSources,
				"version": cfg.Version,
			}
			if rt.Opts.JSON {
				return rt.emit(result)
			}
			if len(rt.app().ConfigSources) == 0 {
				fmt.Fprintln(rt.out(), "配置合法（当前仅使用内置默认值）")
				return nil
			}
			fmt.Fprintf(rt.out(), "配置合法：%v\n", rt.app().ConfigSources)
			return nil
		},
	}
}

func (rt *Runtime) configInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "生成默认配置文件（已存在时不覆盖）",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// 走 app 层（AGENTS.md §2：命令层不直接读写配置文件），
			// 也让 `bs init` 与这里共用同一份"生成默认配置"的逻辑。
			path, created, err := rt.app().EnsureConfig(force)
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(map[string]any{"file": path, "created": created, "dry_run": rt.Opts.DryRun})
			}
			switch {
			case rt.Opts.DryRun:
				fmt.Fprintf(rt.out(), "[dry-run] 将生成 %s\n", path)
			case created:
				fmt.Fprintf(rt.out(), "已生成 %s\n", path)
			default:
				fmt.Fprintf(rt.out(), "%s 已存在，未修改（如需覆盖请加 --force）\n", path)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "覆盖已存在的配置文件（会先备份）")
	return cmd
}

func (rt *Runtime) configGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <键>",
		Short: "打印某个配置键的生效值（以及它来自哪个文件）",
		Long: `键用点分路径，例如 repo.root、tool.versions.golang、ui.confirm、timeouts.command。
列表/映射会以 YAML 输出；列表元素可以用下标访问（repo.hosts.0）。

输出的是**合并后**的生效值（内置默认 < 系统级 < 用户级 < 项目级 < 环境变量 < flag），
所以它同时也是一个排查"这个值到底哪来的"的工具。`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			val, err := rt.app().ConfigGet(args[0])
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(val)
			}
			fmt.Fprintln(rt.out(), val.Value)
			if rt.Opts.Verbose > 0 {
				switch {
				case val.Source != "":
					fmt.Fprintf(rt.errOut(), "来源：%s\n", val.Source)
				case val.Derived:
					fmt.Fprintln(rt.errOut(), "来源：运行时自动检测（配置里为空）")
				default:
					fmt.Fprintln(rt.errOut(), "来源：内置默认值")
				}
			}
			return nil
		},
	}
	return cmd
}

func (rt *Runtime) configSetCmd() *cobra.Command {
	var unset bool
	cmd := &cobra.Command{
		Use:   "set <键> <值>",
		Short: "修改用户级配置文件里的一个键（只改那一行，注释与顺序都保留）",
		Long: `只写用户级配置文件；系统级、项目级、环境变量都不碰（那些需要 root 或属于某个项目）。

类型由内置默认配置推断：ui.confirm 只接受 true/false、repo.clone.depth 只接受整数。
列表/映射用 YAML 写法：bs config set repo.hosts '["github.com", "gitlab.com"]'。
写入前会备份，写入后立刻校验；不合法就把原文件放回去。

--unset 删除该键（回到"由默认值/上层配置决定"的状态）。`,
		Args: func(_ *cobra.Command, args []string) error {
			want := 2
			if unset {
				want = 1
			}
			if len(args) != want {
				return usageErrf("需要 %d 个参数（收到 %d 个）", want, len(args))
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			key := args[0]
			value := ""
			if !unset {
				value = args[1]
			}
			res, err := rt.app().ConfigSet(key, value, unset)
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(res)
			}
			verb := "已写入"
			if unset {
				verb = "已删除"
			}
			prefix := ""
			if res.DryRun {
				prefix = "[dry-run] "
				verb = "将" + strings.TrimPrefix(verb, "已")
			}
			switch {
			case unset:
				fmt.Fprintf(rt.out(), "%s%s %s（旧值 %q）\n", prefix, verb, res.Key, res.Old)
			default:
				fmt.Fprintf(rt.out(), "%s%s %s = %s（旧值 %q）\n", prefix, verb, res.Key, res.New, res.Old)
			}
			if res.Created {
				fmt.Fprintf(rt.out(), "已生成配置文件 %s\n", res.File)
			} else {
				fmt.Fprintf(rt.out(), "文件：%s\n", res.File)
			}
			if res.Backup != "" {
				fmt.Fprintf(rt.out(), "备份：%s\n", res.Backup)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&unset, "unset", false, "删除该键（回到由默认值/上层配置决定）")
	return cmd
}

func (rt *Runtime) configEditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit",
		Short: "用 $EDITOR 打开用户级配置文件（改完立刻校验）",
		Long: `打开用户级配置文件（不存在则先生成默认配置），退出后立刻按真实装载路径校验。
校验不过会打印全部问题，但**不会**回滚你的手改——交给你决定（备份路径会打出来）。

没有终端时拒绝执行（退出码 5）：编辑器需要一个真实终端。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// 先判交互性再准备文件：否则"没有终端"时会顺手生成一份配置——
			// 一个被拒绝的命令不该留下任何痕迹。
			if !rt.interactive() {
				return coded(ExitNeedsInteraction, errors.New(
					"`config edit` 需要一个真实终端来运行编辑器：请直接编辑 "+rt.app().Paths.ConfigFile()))
			}
			path, created, backup, err := rt.app().ConfigEditPrepare()
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.DryRun {
				fmt.Fprintf(rt.out(), "[dry-run] 将用编辑器打开 %s（created=%v）\n", path, created)
				return nil
			}
			if created {
				fmt.Fprintf(rt.out(), "已生成默认配置 %s\n", path)
			}
			if backup != "" {
				fmt.Fprintf(rt.out(), "改前备份：%s\n", backup)
			}
			if err := rt.app().ConfigEdit(cmd.Context(), path); err != nil {
				return preconditionErr(fmt.Errorf("配置校验未通过：\n%w", err))
			}
			fmt.Fprintln(rt.out(), "配置合法。")
			return nil
		},
	}
	return cmd
}

func (rt *Runtime) configKeysCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "keys",
		Short: "列出可用的配置键路径（便于 get/set）",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			keys, err := rt.app().ConfigKeys()
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(map[string]any{"keys": keys})
			}
			for _, k := range keys {
				fmt.Fprintln(rt.out(), k)
			}
			return nil
		},
	}
}
