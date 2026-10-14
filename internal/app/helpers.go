package app

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/internal/gitrepo"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/hostinfo"
)

func getenv(key string) string { return os.Getenv(key) }

func dirExists(p string) bool { return fsx.IsDir(p) }

// asExitError 是 errors.As 的薄封装，保持调用点简洁。
func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}

func (a *App) lookPath(name string) (string, bool) { return a.Runner.LookPath(name) }

func gitrepoDiscover(root string) ([]string, error) { return gitrepo.Discover(root, 0) }

// IsTerminalWriter 判断输出是不是交互式终端（命令层需要它来决定默认策略）。
func IsTerminalWriter(w io.Writer) bool { return isTerminal(w) }

// isTerminal 判断输出是不是交互式终端（实现收敛在 pkg/hostinfo，供 ui 层共用）。
func isTerminal(w io.Writer) bool { return hostinfo.IsTerminal(w) }

// allowedKinds 把配置里的字符串转成合法的 kind 列表（忽略非法值）。
func allowedKinds(list []string) []config.Kind {
	var out []config.Kind
	for _, s := range list {
		k := config.Kind(strings.TrimSpace(s))
		if config.IsKind(string(k)) {
			out = append(out, k)
		}
	}
	return out
}
