package cmdx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDryRunHasNoSideEffects 是本项目"无副作用测试"的核心断言：
// dry-run 模式下即使命令本身会写文件，也不得真正执行。
func TestDryRunHasNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "should-not-exist")

	r := &Runner{DryRun: true, DefaultTimeout: time.Minute}
	res, err := r.Run(context.Background(), Command{Name: "touch", Args: []string{marker}})
	if err != nil {
		t.Fatalf("dry-run should not error: %v", err)
	}
	if !res.DryRun {
		t.Error("Result.DryRun = false, want true")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("dry-run executed the command: marker exists (%v)", err)
	}
}

func TestRunCapturesOutput(t *testing.T) {
	r := &Runner{DefaultTimeout: time.Minute}
	res, err := r.Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "echo out; echo err >&2"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "out" {
		t.Errorf("Stdout = %q, want out", res.Stdout)
	}
	if strings.TrimSpace(res.Stderr) != "err" {
		t.Errorf("Stderr = %q, want err", res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestRunExitError(t *testing.T) {
	r := &Runner{DefaultTimeout: time.Minute}
	res, err := r.Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "echo boom >&2; exit 3"}})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error type = %T, want *ExitError", err)
	}
	if exitErr.Result.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", exitErr.Result.ExitCode)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error message should include stderr, got %q", err.Error())
	}
	if res.ExitCode != 3 {
		t.Errorf("Result.ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestRunEnvOverlay(t *testing.T) {
	r := &Runner{Env: map[string]string{"BS_BASE": "base"}, DefaultTimeout: time.Minute}
	res, err := r.Run(context.Background(), Command{
		Name: "sh",
		Args: []string{"-c", "echo $BS_BASE-$BS_EXTRA"},
		Env:  map[string]string{"BS_EXTRA": "extra"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "base-extra" {
		t.Errorf("stdout = %q, want base-extra", got)
	}
}

func TestRunDir(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{DefaultTimeout: time.Minute}
	res, err := r.Run(context.Background(), Command{Name: "pwd", Dir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) == "" {
		t.Error("pwd produced no output")
	}
	// macOS 上 /tmp 是符号链接，比较真实路径。
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(res.Stdout))
	if got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}

func TestRunTimeout(t *testing.T) {
	r := &Runner{DefaultTimeout: 50 * time.Millisecond}
	_, err := r.Run(context.Background(), Command{Name: "sleep", Args: []string{"5"}})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %v, want it to mention timeout", err)
	}
}

func TestEmptyCommandName(t *testing.T) {
	r := &Runner{}
	if _, err := r.Run(context.Background(), Command{}); err == nil {
		t.Error("empty command name should fail")
	}
}

func TestRedact(t *testing.T) {
	r := &Runner{Secrets: []string{"http://user:pw@proxy:8123"}}
	in := "run with http://user:pw@proxy:8123 as proxy"
	out := r.Redact(in)
	if strings.Contains(out, "user:pw") {
		t.Errorf("secret not redacted: %q", out)
	}
	if !strings.Contains(out, "***") {
		t.Errorf("expected mask in %q", out)
	}
}

func TestCommandStringQuoting(t *testing.T) {
	c := Command{Name: "git", Args: []string{"clone", "https://x/y.git", "/data/src/a b"}}
	got := c.String()
	if !strings.Contains(got, "'/data/src/a b'") {
		t.Errorf("String() = %q, want quoted path", got)
	}
	plain := Command{Name: "ls", Args: []string{"-la"}}
	if got := plain.String(); got != "ls -la" {
		t.Errorf("String() = %q, want %q", got, "ls -la")
	}
}

func TestMergeEnvOverrides(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/root"}
	out := mergeEnv(base, map[string]string{"HOME": "/home/x", "NEW": "1"})
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "HOME=/home/x") {
		t.Errorf("overlay did not win:\n%s", joined)
	}
	if strings.Count(joined, "HOME=") != 1 {
		t.Errorf("duplicate HOME entries:\n%s", joined)
	}
	if !strings.Contains(joined, "NEW=1") {
		t.Errorf("new key missing:\n%s", joined)
	}
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Errorf("base key lost:\n%s", joined)
	}
}

func TestLookPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "mytool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	r := &Runner{Env: map[string]string{"PATH": dir}}
	got, ok := r.LookPath("mytool")
	if !ok || got != exe {
		t.Errorf("LookPath = (%q, %v), want (%q, true)", got, ok, exe)
	}
	if _, ok := r.LookPath("definitely-not-here"); ok {
		t.Error("LookPath found a missing binary")
	}
}
