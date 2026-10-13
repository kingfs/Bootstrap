package app

import (
	"errors"
	"os"
	"os/exec"

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
