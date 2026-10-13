package app

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/internal/gitrepo"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
	"github.com/kingfs/Bootstrap/pkg/fsx"
)

func getenv(key string) string { return os.Getenv(key) }

func dirExists(p string) bool { return fsx.IsDir(p) }

// asExitError 是 errors.As 的薄封装，保持调用点简洁。
func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}

func (a *App) lookPath(name string) (string, bool) { return a.Runner.LookPath(name) }

func cmdxCommand(name string, args ...string) cmdx.Command {
	return cmdx.Command{Name: name, Args: args}
}

func gitrepoDiscover(root string) ([]string, error) { return gitrepo.Discover(root, 0) }

// IsTerminalWriter 判断输出是不是交互式终端（命令层需要它来决定默认策略）。
func IsTerminalWriter(w io.Writer) bool { return isTerminal(w) }

// isTerminal 判断输出是不是交互式终端。
//
// 只依赖标准库：拿不到 *os.File 或不是字符设备就认为是非交互。
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

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
