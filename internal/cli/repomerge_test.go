package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeCLIMergeRepo(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// resolveCLIPath 把临时目录解析成真实路径：macOS 的 /var、Windows 的 RUNNER~1
// 都是符号链接/短路径，而 repo merge 出于安全会 EvalSymlinks。
func resolveCLIPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

func TestRepoMergeRequiresFlags(t *testing.T) {
	rt, _, _, _ := sandbox(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing to", []string{"repo", "merge", "--from", "/tmp/x"}, "--to"},
		{"missing from", []string{"repo", "merge", "--to", "/tmp/x"}, "--from"},
		{"bad depth", []string{"repo", "merge", "--to", "/tmp/x", "--from", "/tmp/y", "--depth", "0"}, "--depth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := run(t, rt, tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d（stderr=%s）", code, ExitUsage, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr should mention %q: %s", tc.want, stderr)
			}
		})
	}
}

func TestRepoMergeDryRunDoesNotMove(t *testing.T) {
	rt, out, _, root := sandbox(t)
	toRoot := filepath.Join(root, "primary")
	fromRoot := filepath.Join(root, "secondary")
	if err := os.MkdirAll(fromRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fromRoot = resolveCLIPath(t, fromRoot)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeCLIMergeRepo(t, source, "from")

	code, stdout, stderr := run(t, rt, "repo", "merge",
		"--to", toRoot, "--from", fromRoot, "--depth", "3", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "move") || !strings.Contains(stdout, source) {
		t.Fatalf("dry-run should show the move plan:\n%s", stdout)
	}
	if !strings.Contains(stdout, "dry-run") {
		t.Fatalf("dry-run should say nothing was changed:\n%s", stdout)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("dry-run moved the repo: %v", err)
	}
	if _, err := os.Stat(toRoot); !os.IsNotExist(err) {
		t.Fatalf("dry-run created target root: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("expected table output")
	}
}

func TestRepoMergeNeedsYesInNonInteractive(t *testing.T) {
	rt, _, _, root := sandbox(t)
	rt.Opts.NonInteractive = true
	toRoot := filepath.Join(root, "primary")
	fromRoot := filepath.Join(root, "secondary")
	if err := os.MkdirAll(fromRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fromRoot = resolveCLIPath(t, fromRoot)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeCLIMergeRepo(t, source, "from")

	code, _, stderr := run(t, rt, "repo", "merge",
		"--to", toRoot, "--from", fromRoot, "--depth", "3")
	if code != ExitNeedsInteraction {
		t.Fatalf("exit = %d, want %d（stderr=%s）", code, ExitNeedsInteraction, stderr)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Fatalf("stderr should mention --yes: %s", stderr)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("non-interactive run moved the repo: %v", err)
	}
}

func TestRepoMergeYesMoves(t *testing.T) {
	rt, _, _, root := sandbox(t)
	toRoot := filepath.Join(root, "primary")
	fromRoot := filepath.Join(root, "secondary")
	if err := os.MkdirAll(fromRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fromRoot = resolveCLIPath(t, fromRoot)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeCLIMergeRepo(t, source, "from")

	code, stdout, stderr := run(t, rt, "repo", "merge",
		"--to", toRoot, "--from", fromRoot, "--depth", "3", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "moved") {
		t.Fatalf("stdout should report moved:\n%s", stdout)
	}
	toRoot = resolveCLIPath(t, toRoot)
	target := filepath.Join(toRoot, "github.com", "team", "repo")
	if data, err := os.ReadFile(filepath.Join(target, "README.md")); err != nil || string(data) != "from" {
		t.Fatalf("target content = %q, %v", data, err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source still exists: %v", err)
	}
}

func TestRepoMergeJSON(t *testing.T) {
	rt, _, _, root := sandbox(t)
	toRoot := filepath.Join(root, "primary")
	fromRoot := filepath.Join(root, "secondary")
	if err := os.MkdirAll(fromRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fromRoot = resolveCLIPath(t, fromRoot)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeCLIMergeRepo(t, source, "from")

	code, stdout, stderr := run(t, rt, "repo", "merge",
		"--to", toRoot, "--from", fromRoot, "--depth", "3", "--dry-run", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	var report struct {
		DryRun  bool `json:"dry_run"`
		Entries []struct {
			Plan struct {
				Action     string `json:"action"`
				RepoPath   string `json:"repo_path"`
				TargetPath string `json:"target_path"`
			} `json:"plan"`
			Status string `json:"status"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if !report.DryRun || len(report.Entries) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.Entries[0].Plan.Action != "move" || report.Entries[0].Status != "planned" {
		t.Fatalf("entry = %+v", report.Entries[0])
	}
	if report.Entries[0].Plan.RepoPath != source {
		t.Fatalf("repo path = %s, want %s", report.Entries[0].Plan.RepoPath, source)
	}
}

// TestRepoMergeUnsafeTargetExitsPartial：目标非空且不是 git 仓库时只报告 skip，
// 不覆盖、不删除，但整体退出码要是 3（有需要人工处理的东西）。
func TestRepoMergeUnsafeTargetExitsPartial(t *testing.T) {
	rt, _, _, root := sandbox(t)
	toRoot := filepath.Join(root, "primary")
	fromRoot := filepath.Join(root, "secondary")
	if err := os.MkdirAll(toRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fromRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	toRoot = resolveCLIPath(t, toRoot)
	fromRoot = resolveCLIPath(t, fromRoot)
	source := filepath.Join(fromRoot, "github.com", "team", "repo")
	makeCLIMergeRepo(t, source, "from")
	target := filepath.Join(toRoot, "github.com", "team", "repo")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(target, "notes.md")
	if err := os.WriteFile(precious, []byte("do not lose me"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run(t, rt, "repo", "merge",
		"--to", toRoot, "--from", fromRoot, "--depth", "3")
	if code != ExitPartial {
		t.Fatalf("exit = %d, want %d\nstdout=%s\nstderr=%s", code, ExitPartial, stdout, stderr)
	}
	if !strings.Contains(stdout, "skipped") {
		t.Fatalf("stdout should report skipped:\n%s", stdout)
	}
	if data, err := os.ReadFile(precious); err != nil || string(data) != "do not lose me" {
		t.Fatalf("precious file changed: %q, %v", data, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source should remain: %v", err)
	}
}
