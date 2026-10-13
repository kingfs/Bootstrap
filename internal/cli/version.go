package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kingfs/Bootstrap/internal/buildinfo"
)

func (rt *Runtime) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "打印 bs 版本信息",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			if rt.Opts.JSON {
				return rt.emit(info)
			}
			fmt.Fprintln(rt.out(), buildinfo.String())
			return nil
		},
	}
}
