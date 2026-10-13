package cli

import (
	"fmt"
	"runtime"

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
	cmd.AddCommand(rt.configPathCmd(), rt.configShowCmd(), rt.configValidateCmd(), rt.configInitCmd())
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
			path, created, err := config.WriteDefault(rt.app().Paths, force)
			if err != nil {
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(map[string]any{"file": path, "created": created})
			}
			if created {
				fmt.Fprintf(rt.out(), "已生成 %s\n", path)
			} else {
				fmt.Fprintf(rt.out(), "%s 已存在，未修改（如需覆盖请加 --force）\n", path)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "覆盖已存在的配置文件（会先备份）")
	return cmd
}
