package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// runInteractiveShell 启动一个已经 source 过受管内容的交互式子 shell。
//
// 做法：把内容写到 cache 目录下的临时 rc 文件，再用对应 shell 的 --rcfile/-c 机制加载。
// 全程不修改用户的任何 rc 文件。
func (a *App) runInteractiveShell(ctx context.Context, shell, script string) (int, error) {
	if err := a.EnsureRuntimeDirs(); err != nil {
		return 1, err
	}
	rcFile := filepath.Join(a.Paths.CacheDir, "subshell-rc."+shell)
	if err := fsx.AtomicWriteFile(rcFile, []byte(script), 0o600); err != nil {
		return 1, fmt.Errorf("write temp rc: %w", err)
	}
	defer func() { _ = os.Remove(rcFile) }()

	exe, args := shellInvocation(shell, rcFile)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, a.Out, a.Err
	netEnv := a.NetworkEnv()
	cmd.Env = os.Environ()
	for k, v := range netEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if asExitError(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

func shellInvocation(shell, rcFile string) (string, []string) {
	switch normalizeShellKey(shell) {
	case "zsh":
		return "zsh", []string{"-i"}
	case "fish":
		return "fish", []string{"-i", "-C", "source " + rcFile}
	case "pwsh":
		return "pwsh", []string{"-NoLogo", "-NoExit", "-Command", ". " + rcFile}
	default:
		return "bash", []string{"--rcfile", rcFile, "-i"}
	}
}
