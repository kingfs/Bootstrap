package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kingfs/Bootstrap/internal/gitrepo"
)

func TestRepoListRequiresIndexAndDoesNotScan(t *testing.T) {
	a, _ := newTestApp(t)
	root := a.Cfg.Repo.Root
	if err := os.MkdirAll(filepath.Join(root, "team", "repo", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.RepoList(context.Background()); !errors.Is(err, gitrepo.ErrNoIndex) {
		t.Fatalf("list: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".bootstrap")); !os.IsNotExist(err) {
		t.Fatal("list created index")
	}
	if err := gitrepo.SaveIndex(context.Background(), root, nil); err != nil {
		t.Fatal(err)
	}
	if views, _, err := a.RepoList(context.Background()); err != nil || len(views) != 0 {
		t.Fatalf("empty index rescanned: %v %v", views, err)
	}
}

func TestRepoScanDryRunDoesNotCreateIndex(t *testing.T) {
	a, _ := newTestApp(t)
	if err := os.MkdirAll(a.Cfg.Repo.Root, 0755); err != nil {
		t.Fatal(err)
	}
	a.DryRun = true
	if _, _, err := a.RepoScan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Cfg.Repo.Root, ".bootstrap")); !os.IsNotExist(err) {
		t.Fatal("dry run created index")
	}
}

// TestRepoLookupAcceptsGetForms：`repo cd` 必须吃下 `repo get` 的所有写法。
// 用户刚 `get` 完手里就是那条 URL，没道理要求他换一种写法才能进去。
func TestRepoLookupAcceptsGetForms(t *testing.T) {
	a, _ := newTestApp(t)
	root := a.Cfg.Repo.Root
	target := filepath.Join(root, "github.com", "docker", "docker-agent")
	other := filepath.Join(root, "git.in.chaitin.net", "team", "agent-svc")
	if err := gitrepo.SaveIndex(context.Background(), root, []gitrepo.IndexEntry{
		{Path: target, Slug: "github.com/docker/docker-agent", Branch: "main"},
		{Path: other, Slug: "git.in.chaitin.net/team/agent-svc", Branch: "master"},
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, query, want string
	}{
		{"URL", "https://github.com/docker/docker-agent", target},
		{"URL 带 .git", "https://github.com/docker/docker-agent.git", target},
		{"scp 写法", "git@github.com:docker/docker-agent.git", target},
		{"owner/repo", "docker/docker-agent", target},
		{"网页地址（含 /tree）", "https://github.com/docker/docker-agent/tree/main/docs", target},
		{"完整 slug", "github.com/docker/docker-agent", target},
		{"目录名", "docker-agent", target},
		{"本地路径", target, target},
		// 默认 host 是 github.com，这条只能靠 slug 后缀命中——省略 host 的写法
		// 不该因为"它当初是从别的站点拉的"就找不到。
		{"省略 host 且非默认站点", "team/agent-svc", other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.RepoLookup(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("RepoLookup(%q): %v", tc.query, err)
			}
			if got != tc.want {
				t.Errorf("RepoLookup(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}

	if _, err := a.RepoLookup(context.Background(), "nope/missing"); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("not found: %v", err)
	}
	if _, err := a.RepoLookup(context.Background(), ""); !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("empty query: %v", err)
	}
}

// TestRepoLookupMissingIndexDoesNotScan：查不到索引时只报前置条件，绝不偷偷遍历用户目录。
func TestRepoLookupMissingIndexDoesNotScan(t *testing.T) {
	a, _ := newTestApp(t)
	if _, err := a.RepoLookup(context.Background(), "docker/docker-agent"); !errors.Is(err, gitrepo.ErrNoIndex) {
		t.Fatalf("err = %v, want ErrNoIndex", err)
	}
	if _, err := os.Stat(filepath.Join(a.Cfg.Repo.Root, ".bootstrap")); !os.IsNotExist(err) {
		t.Fatal("lookup created index")
	}
}

// TestRepoGetIndexesRepoEvenWhenPullSkipped：pull 因脏工作区被跳过时仓库确实在本地，
// 也必须进索引；否则 `get` 刚成功，紧接着的 `cd` 却是"未在索引中找到"。
func TestRepoGetIndexesRepoEvenWhenPullSkipped(t *testing.T) {
	a, _ := newTestApp(t)
	bin := filepath.Join(a.Paths.HomeDir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// 只实现这条路会被走到的子命令：脏工作区让 applyPull 提前返回 skipped。
	addFakeBin(t, bin, "git", `#!/bin/sh
case "$*" in
  *"ls-remote --get-url"*) echo "https://github.com/kingfs/Bootstrap.git" ;;
  *"status --porcelain"*) echo " M main.go" ;;
  *"symbolic-ref"*) echo "origin/main" ;;
  *"rev-parse --abbrev-ref HEAD"*) echo "main" ;;
  *"remote get-url origin"*) echo "https://github.com/kingfs/Bootstrap.git" ;;
esac
exit 0
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")

	target := filepath.Join(a.Cfg.Repo.Root, "github.com", "kingfs", "Bootstrap")
	if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	results, err := a.RepoGet(context.Background(), []string{"https://github.com/kingfs/Bootstrap"}, RepoOptions{})
	if err != nil {
		t.Fatalf("RepoGet: %v", err)
	}
	if len(results) != 1 || results[0].Status != "skipped" {
		t.Fatalf("results = %+v, want one skipped", results)
	}

	idx, err := gitrepo.LoadIndex(context.Background(), a.Cfg.Repo.Root)
	if err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	if len(idx) != 1 || idx[0].Path != target || idx[0].Slug != "github.com/kingfs/Bootstrap" {
		t.Fatalf("index = %+v, want the dirty repo at %s", idx, target)
	}
	got, err := a.RepoLookup(context.Background(), "https://github.com/kingfs/Bootstrap")
	if err != nil || got != target {
		t.Fatalf("RepoLookup after get = %q, %v", got, err)
	}
}
