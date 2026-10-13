package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
	"github.com/kingfs/Bootstrap/pkg/hostinfo"
)

// fakeRunner 记录被调用的命令并按需返回结果，用于在没有 git/网络的情况下测试编排逻辑。
type fakeRunner struct {
	calls   []cmdx.Command
	handler func(c cmdx.Command) (cmdx.Result, error)
}

func (f *fakeRunner) Run(_ context.Context, c cmdx.Command) (cmdx.Result, error) {
	f.calls = append(f.calls, c)
	if f.handler == nil {
		return cmdx.Result{Command: c}, nil
	}
	return f.handler(c)
}

func (f *fakeRunner) called(sub string) bool {
	for _, c := range f.calls {
		if c.String() == sub || (len(c.Args) > 0 && c.Args[len(c.Args)-1] == sub) {
			return true
		}
	}
	return false
}

func testManager(r Runner) *Manager {
	cfg := config.Defaults()
	return NewManager(&cfg, r)
}

func TestPlanCloneForMissingDir(t *testing.T) {
	root := t.TempDir()
	m := testManager(&fakeRunner{})

	plan, err := m.Plan("https://github.com/kingfs/Bootstrap", root, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Action != ActionClone {
		t.Errorf("Action = %q, want clone", plan.Action)
	}
	want := filepath.Join(root, "github.com", "kingfs", "Bootstrap")
	if plan.Path != want {
		t.Errorf("Path = %q, want %q", plan.Path, want)
	}
	if len(plan.Commands) != 1 || plan.Commands[0].Name != "git" {
		t.Fatalf("Commands = %v, want a single git command", plan.Commands)
	}
	args := plan.Commands[0].Args
	if args[0] != "clone" || args[len(args)-1] != want {
		t.Errorf("clone args = %v", args)
	}
	if plan.Commands[0].Args[len(args)-2] != "https://github.com/kingfs/Bootstrap.git" {
		t.Errorf("clone URL = %q", plan.Commands[0].Args[len(args)-2])
	}
}

func TestPlanCloneWithRefAndDepth(t *testing.T) {
	root := t.TempDir()
	m := testManager(&fakeRunner{})

	plan, err := m.Plan("kingfs/Bootstrap", root, Options{Ref: "v1.2.3", Depth: 1, Protocol: "ssh"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	args := plan.Commands[0].Args
	if !containsPair(args, "--depth", "1") {
		t.Errorf("depth missing: %v", args)
	}
	if !containsPair(args, "--branch", "v1.2.3") {
		t.Errorf("ref should be taken from --ref override: %v", args)
	}
	if !containsSeq(args, "git@github.com:kingfs/Bootstrap.git") {
		t.Errorf("ssh protocol should produce a scp-style URL: %v", args)
	}
}

func TestPlanPullForExistingRepo(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "github.com", "kingfs", "Bootstrap")
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	m := testManager(&fakeRunner{})

	plan, err := m.Plan("kingfs/Bootstrap", root, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Action != ActionPull {
		t.Fatalf("Action = %q, want pull", plan.Action)
	}
	if !plan.Existed {
		t.Error("Existed should be true for an existing repo")
	}
	if len(plan.Commands) != 2 {
		t.Fatalf("Commands = %v, want fetch + pull", plan.Commands)
	}
	if !containsSeq(plan.Commands[1].Args, "--ff-only") {
		t.Errorf("default pull strategy should be ff-only: %v", plan.Commands[1].Args)
	}
	if !containsSeq(plan.Commands[0].Args, "--prune") {
		t.Errorf("prune should be enabled by default: %v", plan.Commands[0].Args)
	}
}

// TestPlanSkipProtectsPreciousDirectory 是本工具最重要的安全属性之一：
// 目标目录非空且不是 git 仓库时，绝不覆盖、绝不删除。
func TestPlanSkipProtectsPreciousDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "github.com", "kingfs", "Bootstrap")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	precious := filepath.Join(target, "notes.md")
	if err := os.WriteFile(precious, []byte("do not lose me"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := testManager(&fakeRunner{})

	plan, err := m.Plan("kingfs/Bootstrap", root, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Action != ActionSkip {
		t.Fatalf("Action = %q, want skip", plan.Action)
	}
	if len(plan.Commands) != 0 {
		t.Errorf("skip plan must not contain commands: %v", plan.Commands)
	}

	out, err := m.Apply(context.Background(), plan, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", out.Status)
	}
	data, err := os.ReadFile(precious)
	if err != nil {
		t.Fatalf("the user's file disappeared: %v", err)
	}
	if string(data) != "do not lose me" {
		t.Errorf("content changed: %q", data)
	}
}

func TestPlanCloneIntoEmptyDir(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "github.com", "kingfs", "Bootstrap")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	m := testManager(&fakeRunner{})
	plan, err := m.Plan("kingfs/Bootstrap", root, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Action != ActionClone {
		t.Errorf("Action = %q, want clone into the empty dir", plan.Action)
	}
}

// TestApplyDryRunHasNoSideEffects 断言 dry-run 下即便命令本身会写文件也不执行。
func TestApplyDryRunHasNoSideEffects(t *testing.T) {
	root := t.TempDir()
	m := testManager(&cmdx.Runner{DryRun: true, DefaultTimeout: time.Minute})

	plan, err := m.Plan("kingfs/Bootstrap", root, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := m.Apply(context.Background(), plan, Options{}); err != nil {
		t.Fatalf("dry-run Apply should succeed: %v", err)
	}
	if _, err := os.Stat(plan.Path); !os.IsNotExist(err) {
		t.Fatalf("dry-run created %s (stat err = %v)", plan.Path, err)
	}
	if _, err := os.Stat(filepath.Join(root, "github.com")); !os.IsNotExist(err) {
		t.Fatal("dry-run created intermediate directories")
	}
}

func TestApplyPullSkipsDirtyWorktree(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "github.com", "kingfs", "Bootstrap")
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	m := testManager(&fakeRunner{})
	plan, err := m.Plan("kingfs/Bootstrap", root, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	runner := &fakeRunner{handler: func(c cmdx.Command) (cmdx.Result, error) {
		if containsSeq(c.Args, "status") {
			return cmdx.Result{Command: c, Stdout: " M internal/app/repo.go\n"}, nil
		}
		return cmdx.Result{Command: c}, nil
	}}
	out, err := NewManager(m.Cfg, runner).Apply(context.Background(), plan, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Status != "skipped" {
		t.Errorf("Status = %q, want skipped for a dirty worktree", out.Status)
	}
	for _, c := range runner.calls {
		if containsSeq(c.Args, "pull") {
			t.Error("pull must not run on a dirty worktree without --autostash")
		}
	}
}

func TestApplyPullUpToDate(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "github.com", "kingfs", "Bootstrap")
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	m := testManager(&fakeRunner{})
	plan, err := m.Plan("kingfs/Bootstrap", root, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	runner := &fakeRunner{handler: func(c cmdx.Command) (cmdx.Result, error) {
		if containsSeq(c.Args, "rev-parse") {
			return cmdx.Result{Command: c, Stdout: "deadbeef\n"}, nil
		}
		return cmdx.Result{Command: c}, nil // status 干净
	}}
	out, err := NewManager(m.Cfg, runner).Apply(context.Background(), plan, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Status != "up-to-date" {
		t.Errorf("Status = %q, want up-to-date", out.Status)
	}
	if !runner.called("fetch") && !anyCallContains(runner.calls, "fetch") {
		t.Error("fetch should run before pull")
	}
	if !anyCallContains(runner.calls, "pull") {
		t.Error("pull should run for a clean worktree")
	}
}

func TestApplyPullReportsUpdate(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "github.com", "kingfs", "Bootstrap")
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	m := testManager(&fakeRunner{})
	plan, err := m.Plan("kingfs/Bootstrap", root, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	revParseCalls := 0
	runner := &fakeRunner{handler: func(c cmdx.Command) (cmdx.Result, error) {
		if containsSeq(c.Args, "rev-parse") {
			revParseCalls++
			if revParseCalls == 1 {
				return cmdx.Result{Command: c, Stdout: "old\n"}, nil
			}
			return cmdx.Result{Command: c, Stdout: "new\n"}, nil
		}
		return cmdx.Result{Command: c}, nil
	}}
	out, err := NewManager(m.Cfg, runner).Apply(context.Background(), plan, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Status != "updated" {
		t.Errorf("Status = %q, want updated", out.Status)
	}
}

func TestApplyPullAutostash(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "github.com", "kingfs", "Bootstrap")
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	m := testManager(&fakeRunner{})
	plan, err := m.Plan("kingfs/Bootstrap", root, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	runner := &fakeRunner{handler: func(c cmdx.Command) (cmdx.Result, error) {
		if containsSeq(c.Args, "status") {
			return cmdx.Result{Command: c, Stdout: " M x.go\n"}, nil
		}
		return cmdx.Result{Command: c}, nil
	}}
	out, err := NewManager(m.Cfg, runner).Apply(context.Background(), plan, Options{Autostash: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Status == "skipped" {
		t.Error("autostash should let the pull proceed")
	}
	if !anyCallContains(runner.calls, "stash") {
		t.Error("expected a git stash call")
	}
}

func TestDiscoverFindsRepos(t *testing.T) {
	root := t.TempDir()
	repos := []string{
		filepath.Join(root, "github.com", "kingfs", "Bootstrap"),
		filepath.Join(root, "github.com", "kingfs", "dotfiles"),
		filepath.Join(root, "gitlab.com", "team", "svc"),
	}
	for _, r := range repos {
		if err := os.MkdirAll(filepath.Join(r, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// 仓库内部的子目录不应该被当作独立仓库。
	nested := filepath.Join(repos[0], "internal", "app")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 无关目录也不应该出现。
	if err := os.MkdirAll(filepath.Join(root, "github.com", "not-a-repo"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	found, err := Discover(root, 0)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != len(repos) {
		t.Fatalf("Discover = %v, want %d repos", found, len(repos))
	}
	for i, r := range repos {
		if found[i] != r {
			t.Errorf("found[%d] = %q, want %q", i, found[i], r)
		}
	}
}

func TestDiscoverMissingRoot(t *testing.T) {
	_, err := Discover(filepath.Join(t.TempDir(), "nope"), 0)
	if err == nil {
		t.Fatal("Discover on a missing root should fail")
	}
	// 必须是哨兵错误：CLI 依赖 errors.Is 把它映射成"前置条件不满足"（退出码 4），
	// 而不是一般失败（退出码 1）。
	if !errors.Is(err, ErrNoRoot) {
		t.Errorf("err = %v, want it to wrap ErrNoRoot", err)
	}
}

func TestSyncMissingRootWrapsSentinel(t *testing.T) {
	cfg := config.Defaults()
	m := &Manager{Cfg: &cfg, Runner: &fakeRunner{}}
	if _, err := m.Sync(context.Background(), filepath.Join(t.TempDir(), "nope"), 0, Options{}); !errors.Is(err, ErrNoRoot) {
		t.Errorf("Sync err = %v, want it to wrap ErrNoRoot", err)
	}
}

func TestTargetPathCustomLayout(t *testing.T) {
	cfg := config.Defaults()
	cfg.Repo.Layout = "{{.Owner}}/{{.Repo}}"
	m := NewManager(&cfg, &fakeRunner{})
	root := t.TempDir()
	plan, err := m.Plan("kingfs/Bootstrap", root, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if want := filepath.Join(root, "kingfs", "Bootstrap"); plan.Path != want {
		t.Errorf("Path = %q, want %q", plan.Path, want)
	}
}

func TestPlanRejectsBadInput(t *testing.T) {
	m := testManager(&fakeRunner{})
	if _, err := m.Plan("not a repo at all", t.TempDir(), Options{}); err == nil {
		t.Error("expected parse error for garbage input")
	}
}

func TestRootDefault(t *testing.T) {
	cfg := config.Defaults()
	m := NewManager(&cfg, &fakeRunner{})
	home := t.TempDir()
	root, source := m.Root(platformFor(home, "linux"))
	if want := filepath.Join(home, "src"); root != want {
		t.Errorf("Root = %q, want %q", root, want)
	}
	if source == "" {
		t.Error("source description should not be empty")
	}

	cfg.Repo.Root = "/custom/src"
	root, source = NewManager(&cfg, &fakeRunner{}).Root(platformFor(home, "linux"))
	if root != "/custom/src" || source != "config repo.root" {
		t.Errorf("config root should win: %q (%s)", root, source)
	}
}

func containsPair(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

func containsSeq(args []string, want string) bool {
	for _, a := range args {
		if a == want || filepath.Base(a) == want {
			return true
		}
	}
	return false
}

func anyCallContains(calls []cmdx.Command, sub string) bool {
	for _, c := range calls {
		for _, a := range c.Args {
			if a == sub {
				return true
			}
		}
	}
	return false
}

// platformFor 构造一个只用于测试的 Platform。
func platformFor(home, goos string) hostinfo.Platform {
	return hostinfo.Platform{OS: goos, Arch: "amd64", HomeDir: home, Shell: "bash"}
}
