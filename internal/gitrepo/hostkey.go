package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kingfs/Bootstrap/pkg/cmdx"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/giturl"
	"github.com/kingfs/Bootstrap/pkg/sshkeys"
)

// keyscanTimeout 是单次 ssh-keyscan 的进程级时限。
//
// ssh-keyscan 自己有 -T（每次连接的超时），但 DNS 卡住或链路半开时进程可能迟迟不退
// ——外部命令必须始终有上限（AGENTS §2 第 5 条）。
const keyscanTimeout = 20 * time.Second

// keyscanHostTimeout 是 ssh-keyscan 内部每次连接的等待秒数（-T）。
const keyscanHostTimeout = 5

// prepareSSHFor 是 clone / pull 两条路径的共同入口：判断这次网络操作是否走 SSH，
// 是的话按 SSH 约定把主机公钥准备好，并返回给用户看的提示与要注入 git 的环境变量。
//
// raw 交给 git 自己展开（`ls-remote --get-url` 会让 `url.<base>.insteadOf` 生效）：
// 内网很常见的"https 地址被 git 改写成 ssh"因此也能被识破——只看用户输入的协议
// 是不够的（实测：用户粘的是 https 地址，git 却走 ssh 弹主机公钥确认）。
// repo 非空时按本地仓库里的远端名解析（pull 传 Path 与 "origin"）。
func (m *Manager) prepareSSHFor(ctx context.Context, repo, raw string) (string, map[string]string) {
	if !m.Cfg.Repo.TrustNewSSHHostKey || m.Runner.IsDryRun() {
		// 干跑直接收工：既不能让 `ls-remote --get-url` 变成一条多余的用户可见日志，
		// 更不可能去写用户的 known_hosts（AGENTS §4.2）。
		return "", nil
	}
	target, ok := giturl.ParseSSHTarget(m.expandCloneURL(ctx, repo, raw))
	if !ok {
		return "", nil
	}

	added, trusted, scanErr := m.ensureHostKey(ctx, target)
	if trusted {
		// 已经信任过：不需要再打网络，也不需要兜底——accept-new 对已记录的主机没有作用。
		return "", nil
	}
	env := m.acceptNewHostKeyEnv(ctx)

	switch {
	case len(added) > 0:
		return fmt.Sprintf("已按 SSH 约定把 %s 的主机公钥写入 %s（首次连接，TOFU）",
			target.Host, fsx.ShortenHome(m.knownHostsPath(), m.homeDir())), env
	case scanErr != nil && env == nil:
		// 预写与兜底都不成：git 仍可能弹出交互式确认。把原因说出来，别让用户
		// 又看到一个"没有任何输出、就是不动"的卡住。
		return fmt.Sprintf("无法自动信任 %s 的主机公钥（%s）；若 git 询问是否继续连接，请手动确认",
			target.Host, oneLine(scanErr)), nil
	}
	return "", env
}

// expandCloneURL 让 git 自己算出这次 clone/fetch 最终会用的地址。
//
// 解析失败（老 git、没有 origin）或干跑时原样返回：判断不出来就什么都不做，
// 绝不凭猜测往用户的 known_hosts 里写东西。
func (m *Manager) expandCloneURL(ctx context.Context, repo, raw string) string {
	args := make([]string, 0, 5)
	if repo != "" {
		args = append(args, "-C", repo)
	}
	args = append(args, "ls-remote", "--get-url", raw)
	res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: args})
	if err != nil || res.DryRun {
		return raw
	}
	if out := strings.TrimSpace(res.Stdout); out != "" {
		return out
	}
	return raw
}

// ensureHostKey 保证 target 的主机公钥已经在 known_hosts 里。
//
// trusted 为真表示文件里本来就有（本次没有做任何事）；否则 added 是这次新追加的条目，
// err 是"想追加但失败了"的原因。三条边界：只写用户自己的 `~/.ssh/known_hosts`
// （不碰 /etc/ssh）；只在**文件尾部追加**（已有内容一个字节都不动，重复执行不改变 mtime）；
// 只写 ssh-keyscan 扫回来、且能解析成 known_hosts 行的条目。干跑时一条命令都不执行，
// 自然也不写文件。
func (m *Manager) ensureHostKey(ctx context.Context, target giturl.SSHTarget) (added []sshkeys.Entry, trusted bool, err error) {
	path := m.knownHostsPath()
	if path == "" {
		return nil, false, errors.New("cannot locate known_hosts: home directory is unknown")
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	// 已经信任过就不打扰：既不再打网络，也不改文件。
	if sshkeys.Contains(existing, target.Host, target.Port) {
		return nil, true, nil
	}

	pattern := sshkeys.HostPattern(target.Host, target.Port)
	args := []string{"-T", strconv.Itoa(keyscanHostTimeout)}
	if target.Port != 22 {
		args = append(args, "-p", strconv.Itoa(target.Port))
	}
	args = append(args, target.Host)
	res, err := m.Runner.Run(ctx, cmdx.Command{Name: "ssh-keyscan", Args: args, Timeout: keyscanTimeout})
	if res.DryRun {
		return nil, false, nil // 干跑：命令没执行，任何文件都不能写
	}
	if err != nil {
		return nil, false, fmt.Errorf("ssh-keyscan %s: %w", target.Host, err)
	}

	var entries []sshkeys.Entry
	for _, e := range sshkeys.ParseScan([]byte(res.Stdout)) {
		// 不同版本的 ssh-keyscan 带端口时可能写 `[h]:p` 也可能写 `h`；
		// 我们清楚自己扫的是哪个 host:port，统一改写成 OpenSSH 认得的写法。
		if e.Host != pattern && !strings.EqualFold(e.Host, target.Host) {
			continue
		}
		entries = append(entries, sshkeys.Entry{Host: pattern, Type: e.Type, Key: e.Key})
	}
	missing := sshkeys.Missing(existing, entries)
	if len(missing) == 0 {
		return nil, false, nil
	}
	if err := appendKnownHosts(path, missing); err != nil {
		return nil, false, err
	}
	return missing, false, nil
}

// appendKnownHosts 把条目追加到 known_hosts 尾部。
//
// 用 O_APPEND 而不是"读出来重写"：用户可能同时在别处连别的机器，重写会把并发的
// 追加覆盖掉；而且这里的原则就是"只加不改"。
func appendKnownHosts(path string, entries []sshkeys.Entry) error {
	dir := filepath.Dir(path)
	if err := fsx.EnsureDir(dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.String())
		b.WriteString("\n")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return fmt.Errorf("append %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// acceptNewHostKeyEnv 是"兜底"：让 OpenSSH 自己把新主机公钥写进 known_hosts。
//
// `StrictHostKeyChecking=accept-new`（OpenSSH ≥ 7.6）只接受**新**主机，遇到已记录
// 但公钥变了的仍然报错——自动 TOFU，但不掩盖中间人。它是 ssh-keyscan 缺失/受限、
// 或用户把 UserKnownHostsFile 配到别处时的保险。
//
// 前提是用户没有自定义 ssh 调用方式：GIT_SSH_COMMAND / GIT_SSH / core.sshCommand
// 一旦被我们覆盖，用户配的 key、ProxyJump、wrapper 会一起失效——那个代价比
// "首次连接多点一次确认"大得多，所以宁可不注入。
func (m *Manager) acceptNewHostKeyEnv(ctx context.Context) map[string]string {
	if strings.TrimSpace(os.Getenv("GIT_SSH_COMMAND")) != "" || strings.TrimSpace(os.Getenv("GIT_SSH")) != "" {
		return nil
	}
	res, err := m.Runner.Run(ctx, cmdx.Command{Name: "git", Args: []string{"config", "--get", "core.sshCommand"}})
	if err == nil && !res.DryRun && strings.TrimSpace(res.Stdout) != "" {
		return nil
	}
	return map[string]string{"GIT_SSH_COMMAND": "ssh -o StrictHostKeyChecking=accept-new"}
}

func (m *Manager) knownHostsPath() string {
	home := m.homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

func (m *Manager) homeDir() string {
	if m.HomeDir != "" {
		return m.HomeDir
	}
	return fsx.HomeDir()
}

// setCommandEnv 把环境变量叠加到命令上（c.Env 为 nil 也安全）。
func setCommandEnv(c *cmdx.Command, env map[string]string) {
	if len(env) == 0 {
		return
	}
	if c.Env == nil {
		c.Env = make(map[string]string, len(env))
	}
	for k, v := range env {
		c.Env[k] = v
	}
}

// oneLine 把可能多行的错误压成一行：提示语是按行渲染的，多行会散架。
func oneLine(err error) string {
	s := strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "\n", " ")), " ")
	const max = 160
	// 按字节截断可能切坏多字节字符；提示语不能是乱码，所以按 rune 截。
	if runes := []rune(s); len(runes) > max {
		s = string(runes[:max]) + "…"
	}
	return s
}
