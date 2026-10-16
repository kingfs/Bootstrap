package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/internal/gitrepo"
)

func TestRepoListMissingAndEmptyIndex(t *testing.T) {
	rt, _, _, root := sandbox(t)
	repoRoot := filepath.Join(root, "home", "src")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"repo", "list"}, {"repo", "cd", "repo"}} {
		code, _, stderr := run(t, rt, args...)
		if code != ExitPrecondition || !strings.Contains(stderr, "bs repo scan") {
			t.Fatalf("missing index: code=%d %s", code, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".bootstrap")); !os.IsNotExist(err) {
		t.Fatal("read created index")
	}
	if err := gitrepo.SaveIndex(context.Background(), repoRoot, nil); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run(t, rt, "repo", "list"); code != ExitOK {
		t.Fatalf("empty index: %d %s", code, stderr)
	}
}

// TestRepoCdAcceptsGetForms：`repo cd` 要接受与 `repo get` 相同的写法。
// 用户刚 `get` 完手里就是那条 URL，只认 slug/目录名等于逼他重打一遍。
func TestRepoCdAcceptsGetForms(t *testing.T) {
	rt, _, _, root := sandbox(t)
	repoRoot := filepath.Join(root, "home", "src")
	target := filepath.Join(repoRoot, "github.com", "docker", "docker-agent")
	if err := gitrepo.SaveIndex(context.Background(), repoRoot, []gitrepo.IndexEntry{
		{Path: target, Slug: "github.com/docker/docker-agent", Branch: "main", DefaultBranch: "main"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"https://github.com/docker/docker-agent",
		"git@github.com:docker/docker-agent.git",
		"docker/docker-agent",
		"github.com/docker/docker-agent",
		"docker-agent",
		target,
	} {
		code, out, stderr := run(t, rt, "repo", "cd", q)
		if code != ExitOK || strings.TrimSpace(out) != target {
			t.Fatalf("cd %q: code=%d out=%q stderr=%q", q, code, out, stderr)
		}
	}
	if code, _, stderr := run(t, rt, "repo", "cd", "nope/missing"); code != ExitFail {
		t.Fatalf("missing repo: code=%d stderr=%q", code, stderr)
	} else if !strings.Contains(stderr, "未在索引中找到") || !strings.Contains(stderr, "bs repo get") {
		t.Fatalf("missing repo should say what to do next: %q", stderr)
	}
}

// TestRepoGetRejectsForce：`--force` 曾经是个**从不生效**的旗标（M1 起就没接到任何分支），
// 摆在那里只会让人以为"能绕过不覆盖护栏"。它已删除，重新出现必须是有人刻意加回来。
func TestRepoGetRejectsForce(t *testing.T) {
	rt, _, _, _ := sandbox(t)
	code, _, stderr := run(t, rt, "repo", "get", "kingfs/Bootstrap", "--force")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d（stderr=%s）", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "force") {
		t.Errorf("错误信息应当点出未知旗标：%s", stderr)
	}
}

// TestRepoGetSkipsNonEmptyNonGitDir：目标目录非空且不是 git 仓库 → 只 skip（退出码 0），
// 里面的东西一个字节都不能动（AGENTS §8.1 红线在 CLI 这一层的回归保护）。
//
// 注意：这里的"跳过"是真的走了 Apply，所以目标路径必须与 `repo.root` 的默认值一致
// （`$HOME/src`）——路径写错就会变成"真的去 clone"，测试会挂在网络上。
func TestRepoGetSkipsNonEmptyNonGitDir(t *testing.T) {
	rt, _, _, root := sandbox(t)
	target := filepath.Join(root, "home", "src", "github.com", "kingfs", "Bootstrap")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	keep := filepath.Join(target, "重要文件.txt")
	if err := os.WriteFile(keep, []byte("别动我\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 先看计划（不发网络、不执行命令）：必须判定为 skip。
	if code, stdout, stderr := run(t, rt, "repo", "get", "kingfs/Bootstrap", "--dry-run"); code != ExitOK {
		t.Fatalf("dry-run exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	} else if !strings.Contains(stdout, "不是 git 仓库") {
		t.Fatalf("计划里应当说明为什么不处理：\n%s", stdout)
	}

	code, stdout, stderr := run(t, rt, "repo", "get", "kingfs/Bootstrap")
	if code != ExitOK {
		t.Fatalf("跳过不是失败，exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "不是 git 仓库") {
		t.Errorf("应当说明为什么不处理：\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "别动我\n" {
		t.Fatalf("用户文件被动过：data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
		t.Error("不该在非空目录里建 .git")
	}
}
