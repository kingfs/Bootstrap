package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/pkg/cmdx"
)

// mergeFake 用内存里的提交时间/引用/脏状态模拟 git，让合并逻辑可以在没有
// 真实 git 仓库的情况下被完整覆盖（文件系统移动仍然是真操作）。
type mergeFake struct {
	times     map[string]int64
	heads     map[string]string
	dirty     map[string]bool
	stashes   map[string]bool
	worktrees map[string]int
	refs      map[string][]string
	objects   map[string]map[string]bool
}

func newMergeFake() *mergeFake {
	return &mergeFake{
		times:     map[string]int64{},
		heads:     map[string]string{},
		dirty:     map[string]bool{},
		stashes:   map[string]bool{},
		worktrees: map[string]int{},
		refs:      map[string][]string{},
		objects:   map[string]map[string]bool{},
	}
}

func (f *mergeFake) addRepo(path string, commitTime int64, head string) {
	f.times[path] = commitTime
	f.heads[path] = head
	f.objects[path] = map[string]bool{}
	if head != "" {
		f.objects[path][head] = true
		f.refs[path] = []string{head + " refs/heads/main"}
	}
}

// contain 模拟"较新一侧包含较旧一侧的历史"：把 commit 作为 newer 的一个 ref。
func (f *mergeFake) contain(newer, commit string) {
	if f.objects[newer] == nil {
		f.objects[newer] = map[string]bool{}
	}
	f.objects[newer][commit] = true
	f.refs[newer] = append(f.refs[newer], commit+" refs/heads/old-"+commit)
}

func (f *mergeFake) Run(_ context.Context, c cmdx.Command) (cmdx.Result, error) {
	path := mergeArgPath(c.Args)
	switch {
	case containsSeq(c.Args, "log") && containsSeq(c.Args, "--all"):
		return cmdx.Result{Stdout: fmt.Sprintf("%d\n", f.times[path])}, nil
	case containsSeq(c.Args, "log"):
		if f.times[path] == 0 {
			return cmdx.Result{}, errors.New("no commits")
		}
		return cmdx.Result{Stdout: fmt.Sprintf("%d %s\n", f.times[path], f.heads[path])}, nil
	case containsSeq(c.Args, "status"):
		if f.dirty[path] {
			return cmdx.Result{Stdout: " M file\n"}, nil
		}
		return cmdx.Result{}, nil
	case containsSeq(c.Args, "stash"):
		if f.stashes[path] {
			return cmdx.Result{Stdout: "stash@{0}: WIP\n"}, nil
		}
		return cmdx.Result{}, nil
	case containsSeq(c.Args, "worktree"):
		n := f.worktrees[path]
		if n == 0 {
			n = 1
		}
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "worktree %s-%d\n", path, i)
		}
		return cmdx.Result{Stdout: b.String()}, nil
	case containsSeq(c.Args, "for-each-ref") && containsSeq(c.Args, "--contains"):
		hash := mergeArgAfter(c.Args, "--contains")
		for _, ref := range f.refs[path] {
			if strings.HasPrefix(ref, hash+" ") {
				return cmdx.Result{Stdout: strings.TrimPrefix(ref, hash+" ") + "\n"}, nil
			}
		}
		return cmdx.Result{}, nil
	case containsSeq(c.Args, "for-each-ref"):
		return cmdx.Result{Stdout: strings.Join(f.refs[path], "\n") + "\n"}, nil
	case containsSeq(c.Args, "rev-parse") && containsSeq(c.Args, "--verify"):
		hash := strings.TrimSuffix(mergeArgAfter(c.Args, "--quiet"), "^{commit}")
		if f.objects[path][hash] {
			return cmdx.Result{Stdout: hash + "\n"}, nil
		}
		return cmdx.Result{}, errors.New("not found")
	case containsSeq(c.Args, "rev-parse"):
		return cmdx.Result{Stdout: "main\n"}, nil
	case containsSeq(c.Args, "remote"):
		return cmdx.Result{Stdout: "https://github.com/team/repo.git\n"}, nil
	case containsSeq(c.Args, "symbolic-ref"):
		return cmdx.Result{Stdout: "main\n"}, nil
	}
	return cmdx.Result{Command: c}, nil
}

func (f *mergeFake) IsDryRun() bool { return false }

func mergeArgPath(args []string) string { return mergeArgAfter(args, "-C") }

func mergeArgAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func makeMergeRepo(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mergeRoots(t *testing.T) (toRoot, fromRoot string) {
	t.Helper()
	base := t.TempDir()
	return filepath.Join(base, "primary"), filepath.Join(base, "secondary")
}

func TestDiscoverAtDepth(t *testing.T) {
	root := t.TempDir()
	makeMergeRepo(t, filepath.Join(root, "github.com", "team", "repo"), "repo")
	makeMergeRepo(t, filepath.Join(root, "github.com", "team", "repo", "vendor", "nested"), "nested")
	makeMergeRepo(t, filepath.Join(root, "a", "b", "c"), "deep")
	// worktree/submodule 的 .git 是文件，不算主检出。
	wt := filepath.Join(root, "github.com", "team", "worktree")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /tmp/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := DiscoverAtDepth(context.Background(), root, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(root, "a", "b", "c"),
		filepath.Join(root, "github.com", "team", "repo"),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("depth 3 = %v, want %v", got, want)
	}

	got, err = DiscoverAtDepth(context.Background(), root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("depth 2 should not find host/group/repo: %v", got)
	}
}

func TestDiscoverAtDepthRejectsRepoRoot(t *testing.T) {
	root := t.TempDir()
	makeMergeRepo(t, root, "repo")
	if _, err := DiscoverAtDepth(context.Background(), root, 1); err == nil {
		t.Fatal("root itself is a checkout; want an error")
	}
}

func TestPlanMergeMovesNonConflictingRepo(t *testing.T) {
	toRoot, fromRoot := mergeRoots(t)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeMergeRepo(t, source, "from")
	fake := newMergeFake()
	fake.addRepo(source, 100, "aaa")

	m := testManager(fake)
	plans, err := m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Action != MergeActionMove {
		t.Fatalf("plans = %+v, want one move", plans)
	}
	target := filepath.Join(toRoot, "github.com", "team", "repo")
	if plans[0].TargetPath != target {
		t.Fatalf("target = %s, want %s", plans[0].TargetPath, target)
	}

	results, err := m.ApplyMerge(context.Background(), plans, MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != "moved" {
		t.Fatalf("status = %s", results[0].Status)
	}
	if data, err := os.ReadFile(filepath.Join(target, "README.md")); err != nil || string(data) != "from" {
		t.Fatalf("target content = %q, %v", data, err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists: %v", err)
	}
}

func TestPlanMergeMovesIntoEmptyTarget(t *testing.T) {
	toRoot, fromRoot := mergeRoots(t)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeMergeRepo(t, source, "from")
	target := filepath.Join(toRoot, "github.com", "team", "repo")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := newMergeFake()
	fake.addRepo(source, 100, "aaa")

	m := testManager(fake)
	plans, err := m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Action != MergeActionMove {
		t.Fatalf("plans = %+v, want move into empty target", plans)
	}
	if _, err := m.ApplyMerge(context.Background(), plans, MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "README.md")); err != nil || string(data) != "from" {
		t.Fatalf("target content = %q, %v", data, err)
	}
}

func TestPlanMergeReplacesOlderPrimary(t *testing.T) {
	toRoot, fromRoot := mergeRoots(t)
	primary := filepath.Join(toRoot, "github.com", "team", "repo")
	secondary := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeMergeRepo(t, primary, "old-primary")
	makeMergeRepo(t, secondary, "new-secondary")
	fake := newMergeFake()
	fake.addRepo(primary, 100, "old")
	fake.addRepo(secondary, 200, "new")
	fake.contain(secondary, "old") // 新仓库包含旧历史，允许删除旧的

	m := testManager(fake)
	plans, err := m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Action != MergeActionReplace {
		t.Fatalf("plans = %+v, want replace", plans)
	}
	results, err := m.ApplyMerge(context.Background(), plans, MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != "replaced" {
		t.Fatalf("status = %s", results[0].Status)
	}
	if data, err := os.ReadFile(filepath.Join(primary, "README.md")); err != nil || string(data) != "new-secondary" {
		t.Fatalf("primary content = %q, %v", data, err)
	}
	if _, err := os.Stat(secondary); !os.IsNotExist(err) {
		t.Fatalf("secondary still exists: %v", err)
	}
}

func TestPlanMergeDeletesOlderSecondary(t *testing.T) {
	toRoot, fromRoot := mergeRoots(t)
	primary := filepath.Join(toRoot, "github.com", "team", "repo")
	secondary := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeMergeRepo(t, primary, "new-primary")
	makeMergeRepo(t, secondary, "old-secondary")
	fake := newMergeFake()
	fake.addRepo(primary, 200, "new")
	fake.addRepo(secondary, 100, "old")
	fake.contain(primary, "old")

	m := testManager(fake)
	plans, err := m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Action != MergeActionDelete {
		t.Fatalf("plans = %+v, want delete", plans)
	}
	results, err := m.ApplyMerge(context.Background(), plans, MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != "deleted" {
		t.Fatalf("status = %s", results[0].Status)
	}
	if data, err := os.ReadFile(filepath.Join(primary, "README.md")); err != nil || string(data) != "new-primary" {
		t.Fatalf("primary content = %q, %v", data, err)
	}
	if _, err := os.Stat(secondary); !os.IsNotExist(err) {
		t.Fatalf("secondary still exists: %v", err)
	}
}

func TestPlanMergeDirtyLoserNeedsForce(t *testing.T) {
	toRoot, fromRoot := mergeRoots(t)
	primary := filepath.Join(toRoot, "github.com", "team", "repo")
	secondary := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeMergeRepo(t, primary, "old-primary")
	makeMergeRepo(t, secondary, "new-secondary")
	fake := newMergeFake()
	fake.addRepo(primary, 100, "old")
	fake.addRepo(secondary, 200, "new")
	fake.contain(secondary, "old")
	fake.dirty[primary] = true

	m := testManager(fake)
	plans, err := m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Action != MergeActionConflict {
		t.Fatalf("plans = %+v, want conflict", plans)
	}
	if !strings.Contains(plans[0].Reason, "--force") {
		t.Fatalf("reason should mention --force: %s", plans[0].Reason)
	}
	if _, err := m.ApplyMerge(context.Background(), plans, MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(primary, "README.md")); err != nil || string(data) != "old-primary" {
		t.Fatalf("conflict must not touch primary: %q, %v", data, err)
	}

	plans, err = m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Action != MergeActionReplace {
		t.Fatalf("force plans = %+v, want replace", plans)
	}
}

func TestPlanMergeMissingRefsNeedsForce(t *testing.T) {
	toRoot, fromRoot := mergeRoots(t)
	primary := filepath.Join(toRoot, "github.com", "team", "repo")
	secondary := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeMergeRepo(t, primary, "old-primary")
	makeMergeRepo(t, secondary, "new-secondary")
	fake := newMergeFake()
	fake.addRepo(primary, 100, "old")
	fake.addRepo(secondary, 200, "new")
	// 故意不让 newer 包含 older 的历史：older 有唯一本地提交。

	m := testManager(fake)
	plans, err := m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Action != MergeActionConflict {
		t.Fatalf("plans = %+v, want conflict", plans)
	}
	if !strings.Contains(plans[0].Reason, "本地引用") {
		t.Fatalf("reason should mention local refs: %s", plans[0].Reason)
	}

	plans, err = m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Action != MergeActionReplace {
		t.Fatalf("force plans = %+v, want replace", plans)
	}
}

func TestPlanMergeSkipsNonRepoNonEmptyTarget(t *testing.T) {
	toRoot, fromRoot := mergeRoots(t)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeMergeRepo(t, source, "from")
	target := filepath.Join(toRoot, "github.com", "team", "repo")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(target, "notes.md")
	if err := os.WriteFile(precious, []byte("do not lose me"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := newMergeFake()
	fake.addRepo(source, 100, "aaa")

	m := testManager(fake)
	plans, err := m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Action != MergeActionSkip {
		t.Fatalf("plans = %+v, want skip", plans)
	}
	if _, err := m.ApplyMerge(context.Background(), plans, MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(precious); err != nil || string(data) != "do not lose me" {
		t.Fatalf("precious file changed: %q, %v", data, err)
	}
}

func TestPlanMergeSkipsLinkedWorktree(t *testing.T) {
	toRoot, fromRoot := mergeRoots(t)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeMergeRepo(t, source, "from")
	if err := os.MkdirAll(filepath.Join(source, ".git", "worktrees", "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := newMergeFake()
	fake.addRepo(source, 100, "aaa")
	fake.worktrees[source] = 2

	m := testManager(fake)
	plans, err := m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Action != MergeActionConflict {
		t.Fatalf("plans = %+v, want conflict", plans)
	}
	if !strings.Contains(plans[0].Reason, "worktree") {
		t.Fatalf("reason should mention worktree: %s", plans[0].Reason)
	}
}

func TestPlanMergeKeepBackup(t *testing.T) {
	t.Run("replace", func(t *testing.T) {
		toRoot, fromRoot := mergeRoots(t)
		primary := filepath.Join(toRoot, "github.com", "team", "repo")
		secondary := filepath.Join(fromRoot, "github.com", "team", "repo")
		makeMergeRepo(t, primary, "old-primary")
		makeMergeRepo(t, secondary, "new-secondary")
		fake := newMergeFake()
		fake.addRepo(primary, 100, "old")
		fake.addRepo(secondary, 200, "new")
		fake.contain(secondary, "old")

		m := testManager(fake)
		plans, err := m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
		if err != nil {
			t.Fatal(err)
		}
		results, err := m.ApplyMerge(context.Background(), plans, MergeOptions{
			ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3, KeepBackup: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(results[0].Message, "备份") {
			t.Fatalf("message should mention backup: %s", results[0].Message)
		}
		if data, err := os.ReadFile(filepath.Join(primary, "README.md")); err != nil || string(data) != "new-secondary" {
			t.Fatalf("primary content = %q, %v", data, err)
		}
		backup := findBackup(t, toRoot, filepath.Join("github.com", "team", "repo"))
		if data, err := os.ReadFile(backup); err != nil || string(data) != "old-primary" {
			t.Fatalf("backup content = %q, %v", data, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		toRoot, fromRoot := mergeRoots(t)
		primary := filepath.Join(toRoot, "github.com", "team", "repo")
		secondary := filepath.Join(fromRoot, "github.com", "team", "repo")
		makeMergeRepo(t, primary, "new-primary")
		makeMergeRepo(t, secondary, "old-secondary")
		fake := newMergeFake()
		fake.addRepo(primary, 200, "new")
		fake.addRepo(secondary, 100, "old")
		fake.contain(primary, "old")

		m := testManager(fake)
		plans, err := m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
		if err != nil {
			t.Fatal(err)
		}
		results, err := m.ApplyMerge(context.Background(), plans, MergeOptions{
			ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3, KeepBackup: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(results[0].Message, "备份") {
			t.Fatalf("message should mention backup: %s", results[0].Message)
		}
		if _, err := os.Stat(secondary); !os.IsNotExist(err) {
			t.Fatalf("secondary still exists: %v", err)
		}
		backup := findBackup(t, fromRoot, filepath.Join("github.com", "team", "repo"))
		if data, err := os.ReadFile(backup); err != nil || string(data) != "old-secondary" {
			t.Fatalf("backup content = %q, %v", data, err)
		}
	})
}

func findBackup(t *testing.T, root, rel string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, ".bootstrap", "merge-backup", "*", rel, "README.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("backup not found under %s: %v %v", root, matches, err)
	}
	return matches[0]
}

func TestPlanMergeDryRunDoesNotTouchFilesystem(t *testing.T) {
	toRoot, fromRoot := mergeRoots(t)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeMergeRepo(t, source, "from")
	fake := newMergeFake()
	fake.addRepo(source, 100, "aaa")

	m := testManager(fake)
	plans, err := m.PlanMerge(context.Background(), MergeOptions{ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3})
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, fromRoot)
	results, err := m.ApplyMerge(context.Background(), plans, MergeOptions{
		ToRoot: toRoot, FromRoots: []string{fromRoot}, Depth: 3, DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != "planned" {
		t.Fatalf("status = %s, want planned", results[0].Status)
	}
	if after := snapshotTree(t, fromRoot); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("dry-run changed filesystem:\nbefore=%v\nafter=%v", before, after)
	}
	if _, err := os.Stat(toRoot); !os.IsNotExist(err) {
		t.Fatalf("dry-run created target root: %v", err)
	}
}

func TestPruneEmptyDirs(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "github.com", "team", "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(root, "github.com", "team", "keep")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keep, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 整条分支都空：empty、group、other 应被级联删除。
	if err := os.MkdirAll(filepath.Join(root, "other", "group", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".bootstrap"), 0o755); err != nil {
		t.Fatal(err)
	}

	n, err := PruneEmptyDirs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // team/empty + other/group/empty + other/group + other
		t.Fatalf("pruned = %d, want 4", n)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatalf("empty dir still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "other")); !os.IsNotExist(err) {
		t.Fatalf("empty branch still exists: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("non-empty dir removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".bootstrap")); err != nil {
		t.Fatalf(".bootstrap removed: %v", err)
	}
}

func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
