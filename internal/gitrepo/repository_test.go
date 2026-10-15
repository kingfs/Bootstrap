package gitrepo

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kingfs/Bootstrap/pkg/cmdx"
)

func TestIndexReadOnlyAndRoundTrip(t *testing.T) {
	root := t.TempDir()
	if _, err := LoadIndex(context.Background(), root); !errors.Is(err, ErrNoIndex) {
		t.Fatalf("missing index: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".bootstrap")); !os.IsNotExist(err) {
		t.Fatal("read created index directory")
	}
	entries := []IndexEntry{{Path: filepath.Join(root, "repo"), Slug: "gitlab.com/team/sub/repo", Branch: "develop", DefaultBranch: "develop", Worktree: true}}
	if err := SaveIndex(context.Background(), root, entries); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(indexPath(root))
	got, err := LoadIndex(context.Background(), root)
	if err != nil || len(got) != 1 || got[0] != entries[0] {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	after, _ := os.Stat(indexPath(root))
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("read modified index")
	}
	if err := SaveIndex(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadIndex(context.Background(), root); err != nil || len(got) != 0 {
		t.Fatalf("empty index: %v %v", got, err)
	}
}

func TestRealGitNonstandardDefaultAndWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	runner := &cmdx.Runner{DefaultTimeout: time.Minute, Env: map[string]string{
		"HOME": root, "USERPROFILE": root, "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.invalid", "GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.invalid",
	}}
	git := func(args ...string) string {
		t.Helper()
		res, err := runner.Run(context.Background(), cmdx.Command{Name: "git", Args: args})
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}
	source := filepath.Join(root, "source")
	git("init", "--initial-branch=release/next", source)
	git("-C", source, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "initial")
	target := filepath.Join(root, "github.com", "team", "repo")
	git("clone", source, target)
	m := testManager(runner)
	st, err := m.Inspect(context.Background(), target)
	if err != nil || st.Branch != "release/next" || st.DefaultBranch != "release/next" {
		t.Fatalf("inspect: %+v %v", st, err)
	}
	git("-C", source, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "update")
	plan, err := m.Plan("team/repo", root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.Apply(context.Background(), plan, Options{})
	if err != nil || out.Status != "updated" {
		t.Fatalf("apply: %+v %v", out, err)
	}
	if git("-C", target, "rev-parse", "HEAD") != git("-C", source, "rev-parse", "HEAD") {
		t.Fatal("nonstandard default not updated")
	}
	wt := filepath.Join(target, "nested", "a", "b", "c", "d", "worktree")
	git("-C", target, "worktree", "add", "-b", "feature/local", wt)
	paths, err := Discover(root, DefaultScanOptions())
	// 只应看到 github.com/team/repo 这一个主检出：深埋在仓库里的 worktree
	// 不在枚举范围内（root 下的 source 也不是 <host>/<owner>/<repo> 布局）。
	if err != nil || len(paths) != 1 || paths[0] != target {
		t.Fatalf("discover should only see the primary checkout: %v %v", paths, err)
	}
	if commonGitDir(target) != commonGitDir(wt) {
		t.Fatal("worktree shared directory not recognized")
	}
	layout := m.Cfg.Repo.Layout
	m.Cfg.Repo.Layout = "{{.Repo}}"
	wtPlan, err := m.Plan("team/worktree", filepath.Dir(wt), Options{})
	m.Cfg.Repo.Layout = layout
	if err != nil || wtPlan.Action != ActionPull {
		t.Fatalf("worktree plan: %+v %v", wtPlan, err)
	}
	st, _ = m.Inspect(context.Background(), wt)
	if !st.Worktree || st.Branch != "feature/local" {
		t.Fatalf("worktree: %+v", st)
	}
	out, err = m.Apply(context.Background(), Plan{Path: wt, Action: ActionPull, Commands: m.pullCommands(wt)}, Options{})
	if err != nil || out.Status != "skipped" || !strings.Contains(out.Message, "upstream") {
		t.Fatalf("no upstream: %+v %v", out, err)
	}
	// 功能分支更新自己的 upstream，不切换分支，也不移动默认分支。
	defaultHead := git("-C", target, "rev-parse", "HEAD")
	git("-C", source, "checkout", "-b", "feature/remote")
	git("-C", source, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "feature update")
	git("-C", wt, "fetch", "origin")
	git("-C", wt, "branch", "--set-upstream-to=origin/feature/remote")
	out, err = m.Apply(context.Background(), Plan{Path: wt, Action: ActionPull, Commands: m.pullCommands(wt)}, Options{})
	if err != nil || out.Status != "updated" {
		t.Fatalf("feature pull: %+v %v", out, err)
	}
	if git("-C", wt, "branch", "--show-current") != "feature/local" || git("-C", target, "rev-parse", "HEAD") != defaultHead {
		t.Fatal("feature pull changed the checked out branch or default branch")
	}
	git("-C", wt, "checkout", "--detach")
	out, err = m.Apply(context.Background(), Plan{Path: wt, Action: ActionPull, Commands: m.pullCommands(wt)}, Options{})
	if err != nil || !strings.Contains(out.Message, "detached") {
		t.Fatalf("detached: %+v %v", out, err)
	}
	// 本地 remote 不在白名单，sync 必须在任何 fetch/pull 前拒绝。
	st = m.syncOne(context.Background(), target, Options{})
	if st.Action != "skipped" || !strings.Contains(st.Message, "repo.hosts") {
		t.Fatalf("host guard: %+v", st)
	}
	// 真 git 建出来的 submodule：它的 .git 是 gitdir 文件，Discover 不能把它
	// ——以及它内部再 clone 出来的嵌套仓库——当成用户管理的顶层仓库。
	subSource := t.TempDir()
	git("init", "--initial-branch=main", subSource)
	git("-C", subSource, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "sub")
	git("-C", target, "-c", "protocol.file.allow=always", "submodule", "add", subSource, filepath.Join("engines", "zydis"))
	git("clone", subSource, filepath.Join(target, "engines", "zydis", "inner"))
	paths, err = Discover(root, DefaultScanOptions())
	if err != nil || len(paths) != 1 || paths[0] != target {
		t.Fatalf("submodule leaked into discovery: %v %v", paths, err)
	}
}

func TestSyncDisallowedRemoteNeverFetches(t *testing.T) {
	for _, remote := range []string{"git@git.yongzhen.wang:team/repo.git", "https://gitlab.com/team/repo.git\nbackup https://private.example/team/repo.git (fetch)"} {
		t.Run(remote, func(t *testing.T) {
			runner := &fakeRunner{handler: func(c cmdx.Command) (cmdx.Result, error) {
				if containsSeq(c.Args, "-v") {
					return cmdx.Result{Stdout: "origin " + remote + " (fetch)"}, nil
				}
				return cmdx.Result{}, nil
			}}
			m := testManager(runner)
			st := m.syncOne(context.Background(), t.TempDir(), Options{})
			if st.Action != "skipped" || anyCallContains(runner.calls, "fetch") || anyCallContains(runner.calls, "pull") {
				t.Fatalf("network attempted: %+v %v", st, runner.calls)
			}
		})
	}
}

func TestDiscoverCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverContext(ctx, t.TempDir(), DefaultScanOptions()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLegacyIndexReadAndMigration(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(indexPath(root)), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", indexPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE repos(path TEXT PRIMARY KEY,slug TEXT NOT NULL,branch TEXT NOT NULL); INSERT INTO repos VALUES('repo','github.com/a/b','develop')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, _ := os.ReadFile(indexPath(root))
	entries, err := LoadIndex(context.Background(), root)
	if err != nil || len(entries) != 1 || entries[0].Branch != "develop" {
		t.Fatalf("legacy read: %+v %v", entries, err)
	}
	after, _ := os.ReadFile(indexPath(root))
	if string(before) != string(after) {
		t.Fatal("legacy read changed schema")
	}
	if err := SaveIndex(context.Background(), root, entries); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIndex(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	// 重复键导致重建失败时，原有记录必须仍在。
	if err := SaveIndex(context.Background(), root, append(entries, entries[0])); err == nil {
		t.Fatal("expected duplicate key error")
	}
	got, err := LoadIndex(context.Background(), root)
	if err != nil || len(got) != 1 || got[0] != entries[0] {
		t.Fatalf("transaction lost entries: %+v %v", got, err)
	}
}
