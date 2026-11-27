package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/internal/gitrepo"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
)

// resolvePath 把临时目录解析成真实路径：macOS 的 /var、Windows 的 RUNNER~1
// 都是符号链接/短路径，而 RepoMergePlan 出于安全会 EvalSymlinks。
func resolvePath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

func TestRepoMergePlanDryRunDoesNotTouchFilesystem(t *testing.T) {
	a, _ := newTestApp(t)
	base := t.TempDir()
	toRoot := filepath.Join(base, "primary")
	fromRoot := filepath.Join(base, "secondary")
	if err := os.MkdirAll(fromRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fromRoot = resolvePath(t, fromRoot)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	if err := os.MkdirAll(filepath.Join(source, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("from"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.DryRun = true

	report, err := a.RepoMergePlan(context.Background(), RepoMergeOptions{
		ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.DryRun || len(report.Entries) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.Entries[0].Plan.Action != gitrepo.MergeActionMove || report.Entries[0].Status != "planned" {
		t.Fatalf("entry = %+v", report.Entries[0])
	}
	if _, err := os.Stat(toRoot); !os.IsNotExist(err) {
		t.Fatalf("dry-run created target root: %v", err)
	}
}

func TestRepoMergeApplyMovesAndUpdatesIndexes(t *testing.T) {
	a, _ := newTestApp(t)
	base := t.TempDir()
	toRoot := filepath.Join(base, "primary")
	fromRoot := filepath.Join(base, "secondary")
	if err := os.MkdirAll(toRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fromRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	toRoot = resolvePath(t, toRoot)
	fromRoot = resolvePath(t, fromRoot)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	if err := os.MkdirAll(filepath.Join(source, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("from"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 两边都先放一个索引：主索引里有一个无关仓库，副索引里有这个仓库。
	if err := gitrepo.SaveIndex(context.Background(), toRoot, []gitrepo.IndexEntry{
		{Path: filepath.Join(toRoot, "github.com", "other", "repo"), Slug: "github.com/other/repo"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := gitrepo.SaveIndex(context.Background(), fromRoot, []gitrepo.IndexEntry{
		{Path: source, Slug: "github.com/team/repo"},
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := a.RepoMergePlan(context.Background(), RepoMergeOptions{
		ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := a.RepoMergeApply(context.Background(), plan, RepoMergeOptions{
		ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Status != "moved" {
		t.Fatalf("entries = %+v", report.Entries)
	}
	target := filepath.Join(toRoot, "github.com", "team", "repo")
	if data, err := os.ReadFile(filepath.Join(target, "README.md")); err != nil || string(data) != "from" {
		t.Fatalf("target content = %q, %v", data, err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists: %v", err)
	}

	primary, err := gitrepo.LoadIndex(context.Background(), toRoot)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range primary {
		if filepath.Clean(e.Path) == filepath.Clean(target) {
			found = true
		}
	}
	if !found {
		t.Fatalf("primary index missing moved repo: %+v", primary)
	}
	secondary, err := gitrepo.LoadIndex(context.Background(), fromRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range secondary {
		if filepath.Clean(e.Path) == filepath.Clean(source) {
			t.Fatalf("secondary index still has moved repo: %+v", secondary)
		}
	}
}

func TestRepoMergeRejectsOverlappingRoots(t *testing.T) {
	a, _ := newTestApp(t)
	base := t.TempDir()
	toRoot := filepath.Join(base, "primary")
	fromRoot := filepath.Join(toRoot, "secondary")
	if err := os.MkdirAll(fromRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := a.RepoMergePlan(context.Background(), RepoMergeOptions{
		ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("err = %v, want overlap", err)
	}

	other := filepath.Join(base, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = a.RepoMergePlan(context.Background(), RepoMergeOptions{
		ToRoot: filepath.Join(fromRoot, "target"), FromRoots: []string{fromRoot, other}, Depth: 2,
	})
	// toRoot 在 fromRoot 里：目标会落进来源树，必须拒绝。
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("err = %v, want overlap", err)
	}

	_, err = a.RepoMergePlan(context.Background(), RepoMergeOptions{
		ToRoot: filepath.Join(base, "target"), FromRoots: []string{other, other}, Depth: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want duplicate", err)
	}
}

func TestRepoMergeRejectsMissingFrom(t *testing.T) {
	a, _ := newTestApp(t)
	base := t.TempDir()
	_, err := a.RepoMergePlan(context.Background(), RepoMergeOptions{
		ToRoot: filepath.Join(base, "primary"), FromRoots: []string{filepath.Join(base, "missing")}, Depth: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want does not exist", err)
	}
}

// TestRepoMergeRealGitKeepsNewer：真实 git 仓库上验证"按提交时间保留最新"。
func TestRepoMergeRealGitKeepsNewer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	a, _ := newTestApp(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	a.Runner.Env = map[string]string{
		"HOME": home, "USERPROFILE": home,
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.invalid",
		"GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.invalid",
	}
	ctx := context.Background()
	git := func(date string, args ...string) string {
		t.Helper()
		env := map[string]string{}
		if date != "" {
			env["GIT_AUTHOR_DATE"] = date
			env["GIT_COMMITTER_DATE"] = date
		}
		res, err := a.Runner.Run(ctx, cmdx.Command{Name: "git", Args: args, Env: env})
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, res.Stderr)
		}
		return strings.TrimSpace(res.Stdout)
	}

	origin := filepath.Join(base, "origin")
	git("", "init", "--initial-branch=main", origin)
	git("@100 +0000", "-C", origin, "commit", "--allow-empty", "-m", "old")
	oldHead := git("", "-C", origin, "rev-parse", "HEAD")

	toRoot := filepath.Join(base, "primary")
	fromRoot := filepath.Join(base, "secondary")
	if err := os.MkdirAll(toRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fromRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	toRoot = resolvePath(t, toRoot)
	fromRoot = resolvePath(t, fromRoot)
	primary := filepath.Join(toRoot, "github.com", "team", "repo")
	secondary := filepath.Join(fromRoot, "github.com", "team", "repo")
	git("", "clone", origin, primary)
	git("@200 +0000", "-C", origin, "commit", "--allow-empty", "-m", "new")
	git("", "clone", origin, secondary)
	newHead := git("", "-C", secondary, "rev-parse", "HEAD")
	if oldHead == newHead {
		t.Fatal("test setup: heads should differ")
	}

	plan, err := a.RepoMergePlan(ctx, RepoMergeOptions{
		ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Plan.Action != gitrepo.MergeActionReplace {
		t.Fatalf("plan = %+v, want replace", plan.Entries)
	}
	report, err := a.RepoMergeApply(ctx, plan, RepoMergeOptions{
		ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Entries[0].Status != "replaced" {
		t.Fatalf("status = %s", report.Entries[0].Status)
	}
	if got := git("", "-C", primary, "rev-parse", "HEAD"); got != newHead {
		t.Fatalf("primary HEAD = %s, want %s", got, newHead)
	}
	if _, err := os.Stat(secondary); !os.IsNotExist(err) {
		t.Fatalf("secondary still exists: %v", err)
	}
}
