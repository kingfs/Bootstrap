package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
)

// 一条语法合法（内容无所谓）的 ed25519 公钥，用来断言 known_hosts 的内容。
const testHostKey = "AAAAC3NzaC1lZDI1NTE5AAAAIB0h0h0h0h0h0h0h0h0h0h0h0h0h0h0h0h0h"

// sshFakeRunner 模拟 git 与 ssh-keyscan：`ls-remote --get-url` 按 expanded 展开
// （空则回显参数），ssh-keyscan 返回 scan，clone/fetch 一律成功。
type sshFakeRunner struct {
	calls          []cmdx.Command
	expanded       string
	scan           string
	scanErr        error
	coreSSHCommand string
	dryRun         bool
}

func (f *sshFakeRunner) Run(_ context.Context, c cmdx.Command) (cmdx.Result, error) {
	f.calls = append(f.calls, c)
	switch {
	case c.Name == "ssh-keyscan":
		return cmdx.Result{Command: c, Stdout: f.scan}, f.scanErr
	case containsSeq(c.Args, "ls-remote"):
		if f.expanded != "" {
			return cmdx.Result{Command: c, Stdout: f.expanded + "\n"}, nil
		}
		return cmdx.Result{Command: c, Stdout: c.Args[len(c.Args)-1] + "\n"}, nil
	case containsSeq(c.Args, "config") && containsSeq(c.Args, "core.sshCommand"):
		if f.coreSSHCommand != "" {
			return cmdx.Result{Command: c, Stdout: f.coreSSHCommand + "\n"}, nil
		}
		return cmdx.Result{Command: c, ExitCode: 1}, errors.New("exit status 1")
	}
	return cmdx.Result{Command: c}, nil
}

func (f *sshFakeRunner) IsDryRun() bool { return f.dryRun }

func (f *sshFakeRunner) callsNamed(name string) []cmdx.Command {
	var out []cmdx.Command
	for _, c := range f.calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// envOf 返回最后一条带指定参数的 git 命令的 Env（clone/fetch 用它断言注入）。
func (f *sshFakeRunner) envOf(arg string) map[string]string {
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Name == "git" && containsSeq(f.calls[i].Args, arg) {
			return f.calls[i].Env
		}
	}
	return nil
}

// scanOutput 是一段典型的 ssh-keyscan 输出（含它的版本注释行）。
func scanOutput(host string) string {
	return "# " + host + ":22 SSH-2.0-OpenSSH_9.6\n" +
		host + " ssh-rsa " + testHostKey + "\n" +
		host + " ssh-ed25519 " + testHostKey + "\n"
}

func newSSHManager(t *testing.T, runner Runner, tune func(*config.Config)) (*Manager, string) {
	t.Helper()
	cfg := config.Defaults()
	if tune != nil {
		tune(&cfg)
	}
	home := t.TempDir()
	m := NewManager(&cfg, runner)
	m.HomeDir = home
	return m, home
}

// TestApplyCloneWritesKnownHosts：用户粘的是 https 地址，但 git 的
// url.<base>.insteadOf 把它改写成 ssh（内网常见）——这时必须按 SSH 约定把主机
// 公钥写进 known_hosts，并给 clone 注入 accept-new 兜底，否则 ssh 会卡在
// "是否信任该主机"的提问上。
func TestApplyCloneWritesKnownHosts(t *testing.T) {
	const host = "git.in.chaitin.net"
	runner := &sshFakeRunner{
		expanded: "git@" + host + ":yongzhen.wang/agent-compose-sample.git",
		scan:     scanOutput(host),
	}
	m, home := newSSHManager(t, runner, nil)

	plan, err := m.Plan("https://"+host+"/yongzhen.wang/agent-compose-sample", t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	ctx := context.Background()
	out, err := m.Apply(ctx, plan, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Status != "cloned" {
		t.Fatalf("Status = %q, want cloned", out.Status)
	}
	if !strings.Contains(out.Note, host) || !strings.Contains(out.Note, "known_hosts") {
		t.Errorf("Note = %q, want 提到主机与 known_hosts", out.Note)
	}

	path := filepath.Join(home, ".ssh", "known_hosts")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("known_hosts 没写出来: %v", err)
	}
	if !strings.Contains(string(data), host+" ssh-ed25519 "+testHostKey) {
		t.Errorf("known_hosts 内容不对:\n%s", data)
	}
	// 注释行不能被写进用户的 known_hosts。
	if strings.Contains(string(data), "#") {
		t.Errorf("known_hosts 里混进了注释:\n%s", data)
	}

	env := runner.envOf("clone")
	if env["GIT_SSH_COMMAND"] != "ssh -o StrictHostKeyChecking=accept-new" {
		t.Errorf("clone 的 GIT_SSH_COMMAND = %q", env["GIT_SSH_COMMAND"])
	}
	scans := runner.callsNamed("ssh-keyscan")
	if len(scans) != 1 || !containsSeq(scans[0].Args, host) {
		t.Fatalf("ssh-keyscan 调用 = %v", scans)
	}

	// 幂等：第二次执行不再打网络、也不改文件（AGENTS §4.3）。
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(ctx, plan, Options{}); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("第二次执行改动了 known_hosts")
	}
	if got := len(runner.callsNamed("ssh-keyscan")); got != 1 {
		t.Errorf("ssh-keyscan 应只跑一次，实际 %d 次", got)
	}
}

// TestApplyDryRunSkipsSSHProbe：干跑时连"问 git 最终会用哪个地址"这一步都不做
// ——否则 `bs repo get --dry-run` 会多出一条 `dry-run cmd="git ls-remote --get-url ..."`
// 的用户可见日志，而它并不属于用户要看的计划。
func TestApplyDryRunSkipsSSHProbe(t *testing.T) {
	const host = "git.in.chaitin.net"
	runner := &sshFakeRunner{dryRun: true, expanded: "git@" + host + ":group/repo.git", scan: scanOutput(host)}
	m, home := newSSHManager(t, runner, nil)

	plan, err := m.Plan("https://"+host+"/group/repo", t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	out, err := m.Apply(context.Background(), plan, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Note != "" {
		t.Errorf("Note = %q, want empty", out.Note)
	}
	for _, c := range runner.calls {
		if containsSeq(c.Args, "ls-remote") || c.Name == "ssh-keyscan" {
			t.Errorf("干跑不该发出 %v", c)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); !os.IsNotExist(err) {
		t.Error("干跑创建了 ~/.ssh")
	}
}

// TestApplyCloneSkipsKnownHostWhenAlreadyTrusted：已经在 known_hosts 里的主机
// 不该被打扰（不打网络、不改文件、没有提示）。
func TestApplyCloneSkipsKnownHostWhenAlreadyTrusted(t *testing.T) {
	const host = "git.in.chaitin.net"
	runner := &sshFakeRunner{
		expanded: "git@" + host + ":group/repo.git",
		scan:     scanOutput(host),
	}
	m, home := newSSHManager(t, runner, nil)
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "known_hosts")
	original := host + " ssh-ed25519 " + testHostKey + "\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := m.Plan("git@"+host+":group/repo.git", t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	out, err := m.Apply(context.Background(), plan, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Note != "" {
		t.Errorf("Note = %q, want empty", out.Note)
	}
	if got := len(runner.callsNamed("ssh-keyscan")); got != 0 {
		t.Errorf("ssh-keyscan 不该被调用，实际 %d 次", got)
	}
	// 已知主机连"用户有没有自定义 ssh 命令"都不必问：accept-new 对它没有作用。
	for _, c := range runner.calls {
		if containsSeq(c.Args, "core.sshCommand") {
			t.Errorf("已知主机不该再去查 core.sshCommand：%v", c)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Errorf("known_hosts 被改动:\n%s", data)
	}
}

// TestApplyCloneRespectsTrustNewSSHHostKeyFalse：关掉这个开关就回到旧行为
// ——不跑 ssh-keyscan、不写 known_hosts、也不注入 accept-new。
func TestApplyCloneRespectsTrustNewSSHHostKeyFalse(t *testing.T) {
	const host = "git.in.chaitin.net"
	runner := &sshFakeRunner{expanded: "git@" + host + ":group/repo.git", scan: scanOutput(host)}
	m, home := newSSHManager(t, runner, func(c *config.Config) { c.Repo.TrustNewSSHHostKey = false })

	plan, err := m.Plan("git@"+host+":group/repo.git", t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	out, err := m.Apply(context.Background(), plan, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Note != "" || len(runner.callsNamed("ssh-keyscan")) != 0 {
		t.Errorf("开关关闭时不该有任何 SSH 准备工作: note=%q calls=%v", out.Note, runner.calls)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); !os.IsNotExist(err) {
		t.Error("开关关闭时不该创建 ~/.ssh")
	}
	if env := runner.envOf("clone"); env["GIT_SSH_COMMAND"] != "" {
		t.Errorf("开关关闭时不该注入 GIT_SSH_COMMAND: %q", env["GIT_SSH_COMMAND"])
	}
}

// TestApplyCloneHttpsUntouched：真正的 https 传输不碰 known_hosts。
func TestApplyCloneHttpsUntouched(t *testing.T) {
	const host = "github.com"
	runner := &sshFakeRunner{expanded: "https://" + host + "/kingfs/Bootstrap.git"}
	m, home := newSSHManager(t, runner, nil)

	plan, err := m.Plan("kingfs/Bootstrap", t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := m.Apply(context.Background(), plan, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(runner.callsNamed("ssh-keyscan")) != 0 {
		t.Error("https 传输不该跑 ssh-keyscan")
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); !os.IsNotExist(err) {
		t.Error("https 传输不该创建 ~/.ssh")
	}
	if env := runner.envOf("clone"); env["GIT_SSH_COMMAND"] != "" {
		t.Error("https 传输不该注入 GIT_SSH_COMMAND")
	}
}

// TestApplyDryRunWritesNoKnownHosts：干跑必须一个字节都不写。
func TestApplyDryRunWritesNoKnownHosts(t *testing.T) {
	runner := &cmdx.Runner{DryRun: true, DefaultTimeout: time.Minute}
	m, home := newSSHManager(t, runner, nil)

	plan, err := m.Plan("git@git.in.chaitin.net:group/repo.git", t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	out, err := m.Apply(context.Background(), plan, Options{})
	if err != nil {
		t.Fatalf("dry-run Apply: %v", err)
	}
	if out.Status != "planned" {
		t.Errorf("Status = %q, want planned", out.Status)
	}
	if out.Note != "" {
		t.Errorf("干跑不该报告写入了 known_hosts: %q", out.Note)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); !os.IsNotExist(err) {
		t.Error("干跑创建了 ~/.ssh")
	}
}

// TestApplyCloneKeepsUserSSHCommand：用户显式配了 core.sshCommand 时不注入
// accept-new——覆盖它会连带废掉用户配的 key/ProxyJump/wrapper。
func TestApplyCloneKeepsUserSSHCommand(t *testing.T) {
	const host = "git.in.chaitin.net"
	runner := &sshFakeRunner{
		expanded:       "git@" + host + ":group/repo.git",
		scan:           scanOutput(host),
		coreSSHCommand: "ssh -i ~/.ssh/work_key",
	}
	m, home := newSSHManager(t, runner, nil)

	plan, err := m.Plan("git@"+host+":group/repo.git", t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := m.Apply(context.Background(), plan, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if env := runner.envOf("clone"); env["GIT_SSH_COMMAND"] != "" {
		t.Errorf("不该覆盖用户的 core.sshCommand: %q", env["GIT_SSH_COMMAND"])
	}
	// 预写 known_hosts 仍然做：它不改变用户的 ssh 调用方式。
	if _, err := os.Stat(filepath.Join(home, ".ssh", "known_hosts")); err != nil {
		t.Errorf("known_hosts 应当照常写出来: %v", err)
	}
}

// TestApplyCloneReportsWhenItCannotTrustHost：预写与兜底都不可用时，提示里要说明
// 原因，而不是让用户又看到一次"没有输出的卡住"。
func TestApplyCloneReportsWhenItCannotTrustHost(t *testing.T) {
	const host = "git.in.chaitin.net"
	runner := &sshFakeRunner{
		expanded:       "git@" + host + ":group/repo.git",
		scanErr:        errors.New("ssh-keyscan: command not found"),
		coreSSHCommand: "ssh -i ~/.ssh/work_key",
	}
	m, _ := newSSHManager(t, runner, nil)

	plan, err := m.Plan("git@"+host+":group/repo.git", t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	out, err := m.Apply(context.Background(), plan, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(out.Note, host) || !strings.Contains(out.Note, "无法自动信任") {
		t.Errorf("Note = %q, want 说明无法自动信任", out.Note)
	}
}

// TestApplyPullPreparesHostKey：目录是拷贝过来的（这台机器没连过该主机）时，
// pull 也要先准备好主机公钥。
func TestApplyPullPreparesHostKey(t *testing.T) {
	const host = "git.in.chaitin.net"
	root := t.TempDir()
	target := filepath.Join(root, host, "group", "repo")
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &sshFakeRunner{
		expanded: "git@" + host + ":group/repo.git",
		scan:     scanOutput(host),
	}
	m, home := newSSHManager(t, runner, nil)

	plan, err := m.Plan("git@"+host+":group/repo.git", root, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Action != ActionPull {
		t.Fatalf("Action = %q, want pull", plan.Action)
	}
	out, err := m.Apply(context.Background(), plan, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Note == "" {
		t.Error("pull 路径也应当报告自动信任")
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "known_hosts")); err != nil {
		t.Errorf("pull 前应当写好 known_hosts: %v", err)
	}
	if env := runner.envOf("fetch"); env["GIT_SSH_COMMAND"] == "" {
		t.Error("fetch 也该拿到 accept-new 兜底")
	}
}
