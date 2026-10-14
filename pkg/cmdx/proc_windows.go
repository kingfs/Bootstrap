//go:build windows

package cmdx

import "os/exec"

// setProcessGroup 在 Windows 上是空操作：Go 的 exec 没有进程组概念，
// 真正的"整棵树一起停"要靠 Job Object，留到 Windows 清单那一步再做（ADR-0014）。
func setProcessGroup(*exec.Cmd) {}

// killProcessGroup 只杀直接子进程（Windows 上 TerminateProcess 语义）。
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
