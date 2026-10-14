//go:build unix

package cmdx

import (
	"os/exec"
	"syscall"
)

// setProcessGroup 让子进程自成一个进程组。
//
// 目的是"超时/取消时把整棵进程树停掉"：git 会拉起 ssh、apt 会拉起 dpkg、
// cargo 会拉起 rustc——只杀直接子进程会留下继续写磁盘的孤儿进程。
// 只有让子进程当组长，kill(-pid) 才能一次覆盖全组。
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 向整个进程组发 SIGKILL（含已经改了名字/多层的子孙进程）。
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// 负号＝进程组；组不存在时退回单进程 Kill，保证取消一定生效。
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
