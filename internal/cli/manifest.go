package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kingfs/Bootstrap/internal/app"
	"github.com/kingfs/Bootstrap/internal/manifest"
)

// manifestCmd 是 `bs tool manifest` 的根：清单本身的运维命令。
func (rt *Runtime) manifestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "查看与校验工具清单",
		Long: `工具清单来自两处，后者覆盖前者：

  1. 二进制内置的 tools/*.yaml（随 bs 一起发布，换机器不用同步文件）
  2. 用户目录 <config>/tools/*.yaml（同名 id 按字段合并，用 "+install" 追加）

用 ` + "`bs tool manifest path`" + ` 看实际加载顺序，用 ` + "`validate`" + ` 提前发现写错的地方。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(rt.manifestPathCmd(), rt.manifestValidateCmd(), rt.manifestDiffCmd(), rt.manifestShowCmd())
	return cmd
}

func (rt *Runtime) manifestPathCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "path",
		Short: "显示清单的加载顺序与来源",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			loaded, err := rt.app().LoadedManifest()
			if err != nil {
				return toolErr(err)
			}
			views := manifestViews(loaded)
			if rt.Opts.JSON {
				return rt.emit(views)
			}
			w := rt.newTable()
			fmt.Fprintln(w, "来源\t类型\t文件\t工具数")
			for _, v := range views {
				fmt.Fprintf(w, "%d\t%s\t%s\t%d\n", v.Order, v.Kind, v.Path, v.Tools)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(rt.out(), "用户覆盖目录: %s\n", strings.Join(rt.app().ManifestUserDirs(), ", "))
			return nil
		},
	}
	return cmd
}

// ManifestSourceView 是给命令层用的来源视图。
type ManifestSourceView struct {
	Order int    `json:"order"`
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Tools int    `json:"tools"`
}

func manifestViews(loaded *manifest.Loaded) []ManifestSourceView {
	views := make([]ManifestSourceView, 0, len(loaded.Sources))
	for i, s := range loaded.Sources {
		views = append(views, ManifestSourceView{Order: i + 1, Kind: string(s.Kind), Path: s.Path, Tools: s.Tools})
	}
	return views
}

func (rt *Runtime) manifestValidateCmd() *cobra.Command {
	var goos string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "校验清单（重复 id、未知 via、参数缺失、模板拼写、依赖环）",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			rt.app().GOOSOverride = goos
			loaded, err := rt.app().ValidateManifest()
			if err != nil {
				if loaded == nil {
					return failf("%v", err)
				}
				return failf("%v", err)
			}
			if rt.Opts.JSON {
				return rt.emit(map[string]any{
					"ok": true, "tools": len(loaded.Tools), "profiles": len(loaded.Profiles),
				})
			}
			fmt.Fprintf(rt.out(), "清单校验通过：%d 个工具，%d 个 profile，%d 个来源。\n",
				len(loaded.Tools), len(loaded.Profiles), len(loaded.Sources))
			return nil
		},
	}
	cmd.Flags().StringVar(&goos, "os", "", "按指定系统加载清单（linux/darwin/windows），用于跨平台校验")
	return cmd
}

func (rt *Runtime) manifestShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [工具...]",
		Short: "以规范化形式输出清单内容（便于对比合并结果）",
		RunE: func(_ *cobra.Command, args []string) error {
			loaded, err := rt.app().LoadedManifest()
			if err != nil {
				return toolErr(err)
			}
			tools := loaded.Tools
			if len(args) > 0 {
				tools = nil
				for _, id := range args {
					t, ok := loaded.ByID(id)
					if !ok {
						return toolErr(&app.UnknownToolError{ID: id, Known: loaded.IDs()})
					}
					tools = append(tools, t)
				}
			}
			if rt.Opts.JSON {
				return rt.emit(tools)
			}
			// 默认输出就是 YAML：这个子命令的用途是"看清合并后的结果"，
			// 只支持 --json 的话不加参数就什么都不打印，等于骗人。
			data, err := yaml.Marshal(map[string]any{"version": 1, "tools": tools})
			if err != nil {
				return failf("渲染 YAML 失败：%v", err)
			}
			_, err = rt.out().Write(data)
			return err
		},
	}
}

func (rt *Runtime) manifestDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <文件>",
		Short: "对比某个 YAML 文件与当前合并结果的差异",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			diff, err := rt.app().ManifestDiff(context.Background(), args[0])
			if err != nil {
				return toolErr(err)
			}
			if rt.Opts.JSON {
				return rt.emit(diff)
			}
			if len(diff.Added) == 0 && len(diff.Changed) == 0 {
				fmt.Fprintln(rt.out(), "无差异。")
				return nil
			}
			w := rt.newTable()
			fmt.Fprintln(w, "变化\t工具\t说明")
			for _, id := range diff.Added {
				fmt.Fprintf(w, "新增\t%s\t该文件里定义、当前清单里没有\n", id)
			}
			for _, c := range diff.Changed {
				fmt.Fprintf(w, "不同\t%s\t%s\n", c.ID, c.Detail)
			}
			return w.Flush()
		},
	}
}
