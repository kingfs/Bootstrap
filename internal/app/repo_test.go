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
