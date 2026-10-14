package cmdx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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
	// 打印当前工作目录：Windows 没有 pwd，用 cmd 内建的 cd。
	// 断言的是"Dir 真的被设成了 RunDir"，不依赖任何 POSIX 命令。
	name, args := "pwd", []string(nil)
	if runtime.GOOS == "windows" {
		name, args = "cmd", []string{"/c", "cd"}
	}
	res, err := r.Run(context.Background(), Command{Name: name, Args: args, Dir: dir})
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
	// 含空格的参数必须被引用；引号风格随宿主 shell 走：Windows 不认 POSIX 单引号。
	want := "'/data/src/a b'"
	if runtime.GOOS == "windows" {
		want = `"/data/src/a b"`
	}
	if !strings.Contains(got, want) {
		t.Errorf("String() = %q, want it to contain %q", got, want)
	}
	plain := Command{Name: "ls", Args: []string{"-la"}}
	if got := plain.String(); got != "ls -la" {
		t.Errorf("String() = %q, want %q", got, "ls -la")
	}
}

// TestShellQuotePerPlatform 用显式 goos 覆盖两套引用规则。
//
// 本机跑不了 Windows 二进制，如果只靠 runtime.GOOS 分支，Windows 那半边
// 在 Linux CI 上永远不会被执行；这里直接把 goos 传进去验证。
func TestShellQuotePerPlatform(t *testing.T) {
	tests := []struct {
		goos, in, want string
	}{
		{"linux", "/usr/local/bin/tool", "/usr/local/bin/tool"},
		{"darwin", "/home/a b/nvm.sh", `'/home/a b/nvm.sh'`},
		{"linux", "/it's/here", `'/it'\''s/here'`},
		{"linux", "", "''"},
		// Windows：路径里的反斜杠是分隔符，不能用 POSIX 单引号，改用双引号。
		{"windows", `C:\Users\me\.npm-global`, `"C:\Users\me\.npm-global"`},
		{"windows", `C:\Program Files\tool.exe`, `"C:\Program Files\tool.exe"`},
		{"windows", "", `""`},
		// 参数内部的双引号在 cmd.exe 里用 "" 表示，反斜杠不参与转义。
		{"windows", `C:\a "b"\c`, `"C:\a ""b""\c"`},
	}
	for _, tc := range tests {
		if got := shellQuoteFor(tc.goos, tc.in); got != tc.want {
			t.Errorf("shellQuoteFor(%q, %q) = %q, want %q", tc.goos, tc.in, got, tc.want)
		}
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

// TestRunKillsProcessTreeOnTimeout：超时必须把整棵进程树停掉。
//
// 只杀直接子进程是不够的：`sh -c "sleep 30 & sleep 30"` 里孤儿 `sleep` 会抱着
// 写管道不放，`cmd.Wait` 于是被拖到 30 秒——超时形同虚设（这条用例曾经真的跑满 30s）。
func TestRunKillsProcessTreeOnTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 sh/sleep 构造进程树只在类 Unix 上成立")
	}
	r := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := r.Run(ctx, Command{Name: "sh", Args: []string{"-c", "sleep 30 & sleep 30"}})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("超时应当返回错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("错误链里应当能找到 context.DeadlineExceeded：%v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("超时后 %v 才返回：进程树没有被停掉", elapsed)
	}
}
