package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// sandbox 把配置/状态/缓存目录全部重定向到临时目录，并返回一个可注入输出缓冲区的 Runtime。
//
// 测试在进程内直接执行 cobra 命令（不 fork 子进程），因此能精确断言退出码与
// stdout/stderr 内容；同时因为所有路径都在 t.TempDir() 下，绝不会碰到真实 $HOME。
func sandbox(t *testing.T) (*Runtime, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	t.Setenv("BS_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("BS_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("BS_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("NO_COLOR", "1")
	// 清掉可能影响代理判定的继承变量，保证用例确定。
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy", "NO_PROXY"} {
		t.Setenv(k, "")
	}
	if err := os.MkdirAll(filepath.Join(root, "config"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	rt := &Runtime{Opts: &Options{Out: out, Err: errBuf}}
	return rt, out, errBuf, root
}

// run 在进程内执行一条命令，返回退出码与输出。
//
// 走的是与真实二进制完全相同的 Runtime.Run 路径（含错误报告与退出码映射），
// 因此不需要 fork 子进程就能断言 CLI 行为。
func run(t *testing.T, rt *Runtime, args ...string) (int, string, string) {
	t.Helper()
	outBuf, errBuf := rt.Opts.Out.(*bytes.Buffer), rt.Opts.Err.(*bytes.Buffer)
	outBuf.Reset()
	errBuf.Reset()
	code := rt.Run(args)
	return code, outBuf.String(), errBuf.String()
}

func writeConfig(t *testing.T, root, content string) {
	t.Helper()
	path := filepath.Join(root, "config", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

const proxyConfig = `version: 1
log:
  level: info
  color: never
repo:
  root: ~/src
proxy:
  default_profile: local
  profiles:
    local:
      http_proxy: http://127.0.0.1:7890
      https_proxy: http://127.0.0.1:7890
      no_proxy: 127.0.0.1,localhost
    corp:
      http_proxy: http://user:pw@proxy.corp:8123
      https_proxy: http://user:pw@proxy.corp:8123
tool:
  extra_path:
    - ~/.local/bin
`

func TestVersionCommand(t *testing.T) {
	rt, _, _, _ := sandbox(t)
	code, out, _ := run(t, rt, "version")
	if code != ExitOK {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	if !strings.HasPrefix(out, "bs ") {
		t.Errorf("out = %q", out)
	}

	code, out, _ = run(t, rt, "version", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"version", "commit", "go_version", "os", "arch"} {
		if _, ok := info[key]; !ok {
			t.Errorf("json missing %q: %v", key, info)
		}
	}
}

func TestConfigPathUsesSandbox(t *testing.T) {
	rt, _, _, root := sandbox(t)
	code, out, _ := run(t, rt, "config", "path")
	if code != ExitOK {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	if !strings.Contains(out, filepath.Join(root, "config")) {
		t.Errorf("config path should point into the sandbox:\n%s", out)
	}
}

func TestConfigInitIsIdempotent(t *testing.T) {
	rt, _, _, root := sandbox(t)
	code, out, _ := run(t, rt, "config", "init")
	if code != ExitOK || !strings.Contains(out, "已生成") {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	cfgPath := filepath.Join(root, "config", "config.yaml")
	before, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	code, out, _ = run(t, rt, "config", "init")
	if code != ExitOK || !strings.Contains(out, "已存在") {
		t.Fatalf("second init: exit = %d, out = %s", code, out)
	}
	after, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("repeated config init modified the file")
	}

	code, out, _ = run(t, rt, "config", "validate")
	if code != ExitOK || !strings.Contains(out, "配置合法") {
		t.Fatalf("validate: exit = %d, out = %s", code, out)
	}
}

func TestConfigValidateReportsAllProblems(t *testing.T) {
	rt, _, _, root := sandbox(t)
	writeConfig(t, root, "version: 1\nlog:\n  level: loud\nrepo:\n  pull:\n    strategy: yolo\n")

	code, _, errOut := run(t, rt, "config", "validate")
	if code != ExitPrecondition {
		t.Fatalf("exit = %d, want %d (precondition)", code, ExitPrecondition)
	}
	for _, want := range []string{"log.level", "repo.pull.strategy"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr should mention %s:\n%s", want, errOut)
		}
	}
}

func TestMissingExplicitConfigIsPreconditionError(t *testing.T) {
	rt, _, _, root := sandbox(t)
	code, _, _ := run(t, rt, "--config", filepath.Join(root, "nope.yaml"), "doctor")
	if code != ExitPrecondition {
		t.Fatalf("exit = %d, want %d", code, ExitPrecondition)
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	rt, _, _, _ := sandbox(t)
	code, _, _ := run(t, rt, "definitely-not-a-command")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	code, _, _ = run(t, rt, "proxy", "env", "--shell", "nonsense")
	if code != ExitUsage {
		t.Fatalf("invalid --shell exit = %d, want %d", code, ExitUsage)
	}
}

func TestProxyCommandsAndShellRendering(t *testing.T) {
	rt, _, _, root := sandbox(t)
	writeConfig(t, root, proxyConfig)

	code, out, _ := run(t, rt, "proxy", "list")
	if code != ExitOK || !strings.Contains(out, "local") || !strings.Contains(out, "corp") {
		t.Fatalf("proxy list: exit = %d, out = %s", code, out)
	}

	// 显式 --shell bash：`--apply` 单独用时按"当前 shell"渲染，而"当前 shell"
	// 在 CI 的 Windows 机器上是 pwsh（`$env:HTTPS_PROXY = …`）。这里要断言的是
	// bash 的渲染形式，就不该把结果交给宿主环境决定。
	code, out, _ = run(t, rt, "proxy", "env", "--apply", "--shell", "bash")
	if code != ExitOK || !strings.Contains(out, "export HTTPS_PROXY='http://127.0.0.1:7890'") {
		t.Fatalf("proxy env: exit = %d, out = %s", code, out)
	}

	code, out, _ = run(t, rt, "proxy", "env", "--shell", "fish")
	if code != ExitOK || !strings.Contains(out, "set -gx HTTPS_PROXY") {
		t.Fatalf("fish env: exit = %d, out = %s", code, out)
	}

	code, out, _ = run(t, rt, "proxy", "env", "--shell", "json")
	if code != ExitOK {
		t.Fatalf("json env exit = %d", code)
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("json env output invalid: %v\n%s", err, out)
	}
	if env["HTTPS_PROXY"] != "http://127.0.0.1:7890" {
		t.Errorf("json env = %v", env)
	}

	code, out, _ = run(t, rt, "proxy", "use", "corp")
	if code != ExitOK || !strings.Contains(out, "corp") {
		t.Fatalf("proxy use: exit = %d, out = %s", code, out)
	}
	code, out, _ = run(t, rt, "proxy", "status")
	if code != ExitOK {
		t.Fatalf("status exit = %d", code)
	}
	if strings.Contains(out, "user:pw") {
		t.Errorf("credentials leaked:\n%s", out)
	}
	if !strings.Contains(out, "***@proxy.corp:8123") {
		t.Errorf("expected masked proxy in status:\n%s", out)
	}

	code, out, _ = run(t, rt, "proxy", "off")
	if code != ExitOK || !strings.Contains(out, "停用") {
		t.Fatalf("off: exit = %d, out = %s", code, out)
	}
	// 同上：断言 POSIX 的取消形式（`unset …`）就给 --shell bash。
	code, out, _ = run(t, rt, "proxy", "env", "--apply", "--shell", "bash")
	if code != ExitOK || !strings.HasPrefix(out, "unset ") {
		t.Fatalf("off env: exit = %d, out = %s", code, out)
	}
}

func TestProxyUseUnknownProfileIsPreconditionError(t *testing.T) {
	rt, _, _, root := sandbox(t)
	writeConfig(t, root, proxyConfig)
	code, _, errOut := run(t, rt, "proxy", "use", "nope")
	if code != ExitPrecondition {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitPrecondition, errOut)
	}
}

func TestProxyAddDefaultsToDrySnippet(t *testing.T) {
	rt, out, _, root := sandbox(t)
	writeConfig(t, root, proxyConfig)
	before, err := os.Stat(filepath.Join(root, "config", "config.yaml"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	code, _, _ := run(t, rt, "proxy", "add", "smoke", "--http", "http://127.0.0.1:7890")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "http_proxy: http://127.0.0.1:7890") {
		t.Errorf("expected a YAML snippet:\n%s", out.String())
	}
	after, err := os.Stat(filepath.Join(root, "config", "config.yaml"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("proxy add without --write modified the config file")
	}
}

// TestShellInstallAndUninstallThroughCLI 走完整命令行路径，验证 rc 处理与幂等。
func TestShellInstallAndUninstallThroughCLI(t *testing.T) {
	rt, out, _, root := sandbox(t)
	rc := filepath.Join(root, "home", ".bashrc")
	if err := os.MkdirAll(filepath.Dir(rc), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	original := "# 我的配置\nexport EDITOR=vim\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, _, _ := run(t, rt, "shell", "init", "bash")
	if code != ExitOK {
		t.Fatalf("shell init exit = %d", code)
	}
	if !strings.Contains(out.String(), "command bs") {
		t.Errorf("shell init output missing the wrapper:\n%s", out.String())
	}

	code, _, _ = run(t, rt, "shell", "install", "--shell", "bash", "--rc", rc)
	if code != ExitOK || !strings.Contains(out.String(), "已更新") {
		t.Fatalf("install: exit = %d, out = %s", code, out.String())
	}
	installed := readFileString(t, rc)
	if !strings.Contains(installed, ">>> bootstrap") || !strings.Contains(installed, "export EDITOR=vim") {
		t.Fatalf("rc after install:\n%s", installed)
	}

	code, _, _ = run(t, rt, "shell", "install", "--shell", "bash", "--rc", rc)
	if code != ExitOK || !strings.Contains(out.String(), "已是最新") {
		t.Fatalf("second install should be a no-op: exit = %d, out = %s", code, out.String())
	}

	code, _, _ = run(t, rt, "shell", "status", "--shell", "bash", "--rc", rc)
	if code != ExitOK || !strings.Contains(out.String(), "已安装且为最新") {
		t.Fatalf("status: exit = %d, out = %s", code, out.String())
	}

	code, _, _ = run(t, rt, "shell", "uninstall", "--shell", "bash", "--rc", rc)
	if code != ExitOK {
		t.Fatalf("uninstall exit = %d", code)
	}
	if got := readFileString(t, rc); got != original {
		t.Errorf("rc not restored:\n%q\nwant\n%q", got, original)
	}
}

// TestShellInstallDryRunDoesNotWrite 是命令层的 dry-run 断言。
func TestShellInstallDryRunDoesNotWrite(t *testing.T) {
	rt, out, _, root := sandbox(t)
	rc := filepath.Join(root, "home", ".bashrc")

	code, _, _ := run(t, rt, "--dry-run", "shell", "install", "--shell", "bash", "--rc", rc)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("expected a dry-run notice:\n%s", out.String())
	}
	if _, err := os.Stat(rc); !os.IsNotExist(err) {
		t.Fatalf("dry-run created the rc file (%v)", err)
	}
}

func TestEnvPathAndShow(t *testing.T) {
	rt, out, _, root := sandbox(t)
	writeConfig(t, root, proxyConfig)

	// 同上：断言 bash 的渲染形式，就显式给 --shell bash。
	code, _, _ := run(t, rt, "env", "path", "--shell", "bash")
	if code != ExitOK {
		t.Fatalf("env path exit = %d", code)
	}
	if !strings.Contains(out.String(), "export PATH=") {
		t.Errorf("env path output = %q", out.String())
	}

	code, _, _ = run(t, rt, "env", "path", "--shell", "fish")
	if code != ExitOK || !strings.Contains(out.String(), "set -gx PATH") {
		t.Fatalf("fish env path: exit = %d, out = %s", code, out.String())
	}

	code, _, _ = run(t, rt, "env", "show")
	if code != ExitOK || !strings.Contains(out.String(), "tool.extra_path") {
		t.Fatalf("env show: exit = %d, out = %s", code, out.String())
	}

	code, _, _ = run(t, rt, "env", "show", "--json")
	if code != ExitOK {
		t.Fatalf("env show --json exit = %d", code)
	}
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if _, ok := report["path_entries"]; !ok {
		t.Errorf("json missing path_entries: %v", report)
	}
}

// TestRepoGetDryRunThroughCLI 断言命令行路径下 dry-run 不创建任何目录。
func TestRepoGetDryRunThroughCLI(t *testing.T) {
	rt, out, _, root := sandbox(t)
	writeConfig(t, root, proxyConfig)

	code, _, _ := run(t, rt, "repo", "get", "kingfs/Bootstrap", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d, out = %s", code, out.String())
	}
	if !strings.Contains(out.String(), "git clone") {
		t.Errorf("expected the clone command in the plan:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "home", "src")); !os.IsNotExist(err) {
		t.Fatal("dry-run created the repo root")
	}

	code, _, _ = run(t, rt, "repo", "root")
	if code != ExitOK || !strings.Contains(out.String(), filepath.Join(root, "home", "src")) {
		t.Fatalf("repo root: exit = %d, out = %s", code, out.String())
	}

	code, _, _ = run(t, rt, "repo", "list")
	if code != ExitOK {
		t.Fatalf("repo list on a missing root should succeed, exit = %d", code)
	}
	if !strings.Contains(out.String(), "暂未发现仓库") {
		t.Errorf("repo list output = %q", out.String())
	}
}

// TestRepoSyncStatesMissingRootAsPrecondition 断言"根目录还不存在"被当作
// 前置条件（退出码 4）而不是失败（退出码 1），并给出下一步提示。
func TestRepoSyncStatesMissingRootAsPrecondition(t *testing.T) {
	rt, _, errOut, _ := sandbox(t)
	code, _, _ := run(t, rt, "repo", "sync")
	if code != ExitPrecondition {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitPrecondition, errOut)
	}
	if !strings.Contains(errOut.String(), "bs repo get") {
		t.Errorf("stderr should point at the next step:\n%s", errOut)
	}
}

func TestRepoSyncOnEmptyRootSucceeds(t *testing.T) {
	rt, out, _, root := sandbox(t)
	if err := os.MkdirAll(filepath.Join(root, "home", "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	code, _, _ := run(t, rt, "repo", "sync")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "没有需要更新的仓库") {
		t.Errorf("out = %q", out.String())
	}
}

func TestRepoGetRejectsGarbageInput(t *testing.T) {
	rt, _, _, _ := sandbox(t)
	code, _, _ := run(t, rt, "repo", "get", "not a repo at all", "--dry-run")
	if code != ExitPartial {
		t.Fatalf("exit = %d, want %d (partial)", code, ExitPartial)
	}
}

func TestDoctorRunsOfflineAndReadOnly(t *testing.T) {
	rt, _, _, root := sandbox(t)
	writeConfig(t, root, proxyConfig)
	before := listTree(t, root)

	code, doctorOut, _ := run(t, rt, "doctor")
	if code != ExitOK && code != ExitPrecondition {
		t.Fatalf("doctor exit = %d, out = %s", code, doctorOut)
	}
	if !strings.Contains(doctorOut, "platform") || !strings.Contains(doctorOut, "git") {
		t.Errorf("doctor output incomplete:\n%s", doctorOut)
	}
	if got := listTree(t, root); strings.Join(before, "\n") != strings.Join(got, "\n") {
		t.Errorf("doctor modified the filesystem:\n新增：%v\n消失：%v", diffPaths(got, before), diffPaths(before, got))
	}

	code, jsonOut, _ := run(t, rt, "doctor", "--json")
	if code != ExitOK && code != ExitPrecondition {
		t.Fatalf("doctor --json exit = %d", code)
	}
	var payload struct {
		Checks  []map[string]any `json:"checks"`
		Summary map[string]int   `json:"summary"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, jsonOut)
	}
	if len(payload.Checks) == 0 {
		t.Error("doctor --json returned no checks")
	}
}

// TestShellStartupSurvivesBrokenConfig 断言"开 shell 时会被调用的命令"不会因为
// 配置损坏而失败——否则一个手误就会让人登录不进 shell。
func TestShellStartupSurvivesBrokenConfig(t *testing.T) {
	rt, out, errOut, root := sandbox(t)
	writeConfig(t, root, "this: [is: not: valid: yaml\n")

	code, _, _ := run(t, rt, "shell", "init", "bash")
	if code != ExitOK {
		t.Fatalf("shell init must survive a broken config, exit = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out.String(), "command bs") {
		t.Errorf("shell init should still emit the wrapper:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "警告") {
		t.Errorf("expected a warning on stderr, got %q", errOut)
	}

	code, _, _ = run(t, rt, "env", "path")
	if code != ExitOK {
		t.Errorf("env path must survive a broken config, exit = %d", code)
	}
	code, _, _ = run(t, rt, "proxy", "env", "--apply")
	if code != ExitOK {
		t.Errorf("proxy env must survive a broken config, exit = %d", code)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			out = append(out, path+"|dir")
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// diffPaths 返回 a 中有而 b 中没有的路径（用于展示文件系统差异）。
func diffPaths(a, b []string) []string {
	index := make(map[string]bool, len(b))
	for _, p := range b {
		index[p] = true
	}
	var out []string
	for _, p := range a {
		if !index[p] {
			out = append(out, p)
		}
	}
	return out
}

// TestGlobalTimeoutStopsProcessTree：`--timeout` 必须真的把慢命令连同子进程一起停掉。
//
// 假 git 里套一个后台 sleep：只杀直接子进程的话，孤儿 sleep 会抱着管道不放，
// 命令要等它自然结束才返回（这条用例以前真的会卡满）。
func TestGlobalTimeoutStopsProcessTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 sh/sleep 构造进程树只在类 Unix 上成立")
	}
	rt, _, _, root := sandbox(t)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFakeBin(t, bin, "git", "#!/bin/sh\nsleep 30 & sleep 30\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	code, _, stderr := run(t, rt, "--timeout", "300ms", "repo", "get", "kingfs/Bootstrap")
	elapsed := time.Since(start)

	if code != ExitFail {
		t.Fatalf("exit = %d，期望 %d\nstderr=%s", code, ExitFail, stderr)
	}
	if elapsed > 5*time.Second {
		t.Errorf("超时后 %v 才返回：进程树没被停掉", elapsed)
	}
	if !strings.Contains(stderr, "超过总时限") || !strings.Contains(stderr, "300ms") {
		t.Errorf("应当说清是总时限到了：\n%s", stderr)
	}
	// 沙箱里不该留下半个仓库（假的 git 什么也不做，这里主要防"停不掉还写盘"）
	if _, err := os.Stat(filepath.Join(root, "home", "src", "github.com", "kingfs", "Bootstrap")); err == nil {
		t.Error("失败的 clone 不该留下目录")
	}
}

// TestTimeoutFromConfigAndCommandRunner：配置里的 timeouts.global 与 timeouts.command
// 都要生效，并且 flag 覆盖配置。
func TestTimeoutFromConfigAndCommandRunner(t *testing.T) {
	rt, _, _, root := sandbox(t)
	writeConfig(t, root, `version: 1
log: {level: error}
timeouts:
  global: 2s
  command: 1500ms
`)

	code, _, stderr := run(t, rt, "version")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstderr=%s", code, stderr)
	}
	if got := rt.App.Runner.DefaultTimeout; got != 1500*time.Millisecond {
		t.Errorf("Runner.DefaultTimeout = %v，期望取 timeouts.command", got)
	}

	// flag 覆盖配置：配置说 5s，flag 说 300ms，实际必须按 300ms 掐断。
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFakeCmd(t, bin, "git", fakeCmd{sleep: 30 * time.Second})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeConfig(t, root, `version: 1
log: {level: error}
timeouts:
  global: 5s
`)
	start := time.Now()
	code, _, stderr = run(t, rt, "--timeout", "300ms", "repo", "get", "kingfs/Bootstrap")
	elapsed := time.Since(start)
	if code != ExitFail {
		t.Fatalf("exit = %d，期望 %d\nstderr=%s", code, ExitFail, stderr)
	}
	// 阈值要算上 cmdx 的 WaitDelay（3s）：取消后如果有子孙进程仍抱着管道，
	// `cmd.Wait` 会再等最多 3 秒才放弃读取（见 pkg/cmdx Run 的注释）。Windows 上
	// 假命令是 `.cmd`，被杀的 `cmd.exe` 底下还留着一个 ping，正好走这条兜底路径
	// （CI 实测 3.4s）。判断"flag 有没有覆盖配置"仍然成立：配置里的 5s 若生效，
	// 耗时会 ≥5s（再加 WaitDelay），远超过这里的两档阈值。
	limit := 2 * time.Second
	if runtime.GOOS == "windows" {
		limit = 4 * time.Second
	}
	if elapsed > limit {
		t.Errorf("%v 才返回：flag 没有覆盖配置里的 5s", elapsed)
	}
	if !strings.Contains(stderr, "300ms") {
		t.Errorf("报错应当提到 flag 给的 300ms：\n%s", stderr)
	}
}

// TestDoctorFixRequiresConsentAndWritesNothing：`--fix` 也是破坏性操作：
// 非交互环境没有 --yes 就退出码 5，而且**一个文件都不能创建**。
func TestDoctorFixRequiresConsentAndWritesNothing(t *testing.T) {
	rt, _, _, root := sandbox(t)

	code, stdout, stderr := run(t, rt, "doctor", "--fix")
	if code != ExitNeedsInteraction {
		t.Fatalf("exit = %d，期望 %d\nstdout=%s\nstderr=%s", code, ExitNeedsInteraction, stdout, stderr)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("应当提示加 --yes：\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "config.yaml")); err == nil {
		t.Error("没同意就生成了配置文件")
	}
	if _, err := os.Stat(filepath.Join(root, "home", "src")); err == nil {
		t.Error("没同意就创建了仓库根目录")
	}
	if _, err := os.Stat(filepath.Join(root, "home", ".bashrc")); err == nil {
		t.Error("没同意就写了 rc")
	}
}

// TestDoctorFixDryRunPlansOnly：--dry-run 只说会修什么，不动手。
func TestDoctorFixDryRunPlansOnly(t *testing.T) {
	rt, stdout, _, root := sandbox(t)
	code, out, _ := run(t, rt, "doctor", "--fix", "--dry-run")
	if code != ExitOK && code != ExitPrecondition {
		t.Fatalf("exit = %d\n%s%s", code, out, stdout)
	}
	if !strings.Contains(out, "可自动修复") || !strings.Contains(out, "不会被执行") {
		t.Errorf("应当列出计划并标注干跑：\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "config.yaml")); err == nil {
		t.Error("干跑不该生成配置")
	}
}

// TestDoctorFixAppliesAndRechecks：--yes 时真的修，并且复查结果要反映修复后的状态。
func TestDoctorFixAppliesAndRechecks(t *testing.T) {
	rt, _, _, root := sandbox(t)
	rc := hostRCFile(root)
	if err := os.MkdirAll(filepath.Dir(rc), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(rc, []byte("# 我自己的 rc\n"), 0o644); err != nil {
		t.Fatalf("write rc: %v", err)
	}

	code, out, stderr := run(t, rt, "doctor", "--fix", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, out, stderr)
	}
	if !strings.Contains(out, "修复结果") {
		t.Errorf("应当打印修复结果：\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "config.yaml")); err != nil {
		t.Errorf("配置没有被生成：%v", err)
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("read rc: %v", err)
	}
	if !strings.HasPrefix(string(data), "# 我自己的 rc\n") {
		t.Errorf("rc 原有内容被改动：\n%s", data)
	}
	if !strings.Contains(string(data), ">>> bootstrap") {
		t.Errorf("受管块没有写进 rc：\n%s", data)
	}
	// 复查：只看"修复结果"之后的第二张表（第一张是修复前的状态，理应还带着 warn）。
	idx := strings.Index(out, "修复结果")
	if idx < 0 {
		t.Fatalf("找不到修复结果段：\n%s", out)
	}
	tail := out[idx:]
	// 表格行形如 "  ok   shell rc          /home/…/.bashrc 已安装且为最新"。
	statusOf := func(name string) (string, bool) {
		for _, line := range strings.Split(tail, "\n") {
			for _, glyph := range []string{"  ok ", "warn ", "FAIL ", "skip "} {
				if strings.HasPrefix(line, glyph) && strings.Contains(line, name+" ") {
					return strings.TrimSpace(glyph), true
				}
			}
		}
		return "", false
	}
	for _, name := range []string{"shell rc", "config", "repo root"} {
		status, found := statusOf(name)
		if !found {
			t.Errorf("复查表里缺少 %s：\n%s", name, tail)
			continue
		}
		if status != "ok" {
			t.Errorf("复查里 %s = %s，期望 ok：\n%s", name, status, tail)
		}
	}
}

// TestDoctorFixJSONShape：--json 下体检、修复、复查一起给，形状要稳定。
func TestDoctorFixJSONShape(t *testing.T) {
	rt, _, _, _ := sandbox(t)
	code, out, stderr := run(t, rt, "doctor", "--fix", "--dry-run", "--json")
	if code != ExitOK && code != ExitPrecondition {
		t.Fatalf("exit = %d\nstderr=%s", code, stderr)
	}
	var payload struct {
		Checks  []map[string]any `json:"checks"`
		Summary map[string]int   `json:"summary"`
		Fixes   []map[string]any `json:"fixes"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(payload.Checks) == 0 || len(payload.Fixes) == 0 {
		t.Errorf("JSON 里应当同时有 checks 与 fixes：%s", out)
	}
	if payload.Summary["ok"] == 0 {
		t.Errorf("summary 缺失：%s", out)
	}
}

// TestConfigGetSetRoundTrip：set 只改那一行（注释与顺序都留着），get 读的是生效值。
func TestConfigGetSetRoundTrip(t *testing.T) {
	rt, _, _, root := sandbox(t)
	writeConfig(t, root, `# 我自己的配置（这行注释必须留着）
version: 1
repo:
  root: ~/src   # 仓库根
ui:
  confirm: true
`)
	cfgPath := filepath.Join(root, "config", "config.yaml")

	code, out, errOut := run(t, rt, "config", "get", "ui.confirm")
	if code != ExitOK || strings.TrimSpace(out) != "true" {
		t.Fatalf("exit = %d, out = %q, err = %s", code, out, errOut)
	}

	code, out, errOut = run(t, rt, "config", "set", "ui.confirm", "false")
	if code != ExitOK {
		t.Fatalf("set: exit = %d, err = %s", code, errOut)
	}
	if !strings.Contains(out, "已写入 ui.confirm = false") || !strings.Contains(out, "备份：") {
		t.Errorf("set 输出缺少要点：%s", out)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"# 我自己的配置（这行注释必须留着）", "# 仓库根", "confirm: false"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("配置文件里缺少 %q：\n%s", want, data)
		}
	}

	// 生效值跟着变（新进程会重新读盘，这里直接用第二个 Runtime 模拟）
	rt2, _, _, _ := sandboxReuse(t, root)
	code, out, _ = run(t, rt2, "config", "get", "ui.confirm")
	if code != ExitOK || strings.TrimSpace(out) != "false" {
		t.Errorf("重启后 get: exit = %d, out = %q", code, out)
	}
}

// TestConfigSetRejectsBadValueAndKeepsFile：类型写错必须在写入前拦住，
// 文件内容一个字节都不能动。
func TestConfigSetRejectsBadValueAndKeepsFile(t *testing.T) {
	rt, _, _, root := sandbox(t)
	writeConfig(t, root, "version: 1\nui:\n  confirm: true\n")
	cfgPath := filepath.Join(root, "config", "config.yaml")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	code, out, errOut := run(t, rt, "config", "set", "repo.clone.depth", "deep")
	if code == ExitOK {
		t.Fatalf("非法值不该成功：%s%s", out, errOut)
	}
	if !strings.Contains(errOut, "期望整数") {
		t.Errorf("应当说明期望类型：%s", errOut)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("非法值改动或破坏了配置文件：\n%s", after)
	}
}

// TestConfigSetUnknownKeySuggests：拼错的键要给出候选，并保持用法类退出码。
func TestConfigSetUnknownKeySuggests(t *testing.T) {
	rt, _, _, _ := sandbox(t)
	code, _, errOut := run(t, rt, "config", "get", "ui.confrim")
	if code == ExitOK {
		t.Fatal("未知键不该成功")
	}
	if !strings.Contains(errOut, "ui.confirm") {
		t.Errorf("应当给出候选键：%s", errOut)
	}
}

// TestConfigSetDryRunWritesNothing：--dry-run 是"零写入"，连配置文件都不该被创建。
func TestConfigSetDryRunWritesNothing(t *testing.T) {
	rt, _, _, root := sandbox(t)
	// 注意：dry-run 必须作为参数传进来——newRootCmd 会按 flag 默认值重写 rt.Opts。
	code, out, errOut := run(t, rt, "config", "set", "log.level", "debug", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d, err = %s", code, errOut)
	}
	if !strings.Contains(out, "[dry-run]") {
		t.Errorf("应当标注干跑：%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "config.yaml")); err == nil {
		t.Error("干跑不该创建配置文件")
	}
}

// TestConfigSetUnsetAndKeys：unset 回到默认值；keys 列出可用的键。
func TestConfigSetUnsetAndKeys(t *testing.T) {
	rt, _, _, root := sandbox(t)
	writeConfig(t, root, "version: 1\nui:\n  confirm: false\n")
	code, _, errOut := run(t, rt, "config", "set", "ui.confirm", "--unset")
	if code != ExitOK {
		t.Fatalf("unset: exit = %d, err = %s", code, errOut)
	}
	code, out, _ := run(t, rt, "config", "get", "ui.confirm")
	if code != ExitOK || strings.TrimSpace(out) != "true" {
		t.Errorf("unset 后应回到默认值 true：exit = %d, out = %q", code, out)
	}
	code, out, errOut = run(t, rt, "config", "keys")
	if code != ExitOK {
		t.Fatalf("keys: exit = %d, err = %s", code, errOut)
	}
	if !strings.Contains(out, "repo.root") || !strings.Contains(out, "timeouts.download") {
		t.Errorf("keys 输出不完整：%s", out)
	}
}

// TestConfigEditNeedsTerminalAndDryRun：非交互环境拒绝跑编辑器（退出码 5），
// 但 --dry-run 只打印计划。
func TestConfigEditNeedsTerminal(t *testing.T) {
	rt, _, _, root := sandbox(t)
	code, out, errOut := run(t, rt, "config", "edit")
	if code != ExitNeedsInteraction {
		t.Fatalf("exit = %d, out = %s, err = %s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "config.yaml")); err == nil {
		t.Error("拒绝执行时不该顺手创建配置")
	}
}

// sandboxReuse 复用已有沙箱目录再建一个 Runtime（模拟"重新运行 bs 读盘"）。
func sandboxReuse(t *testing.T, root string) (*Runtime, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	t.Setenv("BS_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("BS_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("BS_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("NO_COLOR", "1")
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	return &Runtime{Opts: &Options{Out: out, Err: errBuf}}, out, errBuf, root
}

// TestToolUpgradeAllSelection：--all 是"所有已安装的工具"；
// 一台什么都没装的机器上它是**成功且无事可做**（脚本里不该因为"本来就是干净的"而中断）。
func TestToolUpgradeAllSelection(t *testing.T) {
	rt, _, _, root := sandbox(t)
	// 空的 PATH：探测结果只可能是 missing，与宿主机装了什么无关。
	empty := filepath.Join(root, "emptybin")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("PATH", empty)

	code, out, errOut := run(t, rt, "tool", "upgrade", "--all", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d, out = %s, err = %s", code, out, errOut)
	}
	if !strings.Contains(out, "没有已安装的工具") {
		t.Errorf("应当明确说明无事可做：%s", out)
	}

	// 与显式工具名同时给是用法错误（和 -i 一样，避免"我点名的被忽略"）。
	code, _, errOut = run(t, rt, "tool", "upgrade", "--all", "git")
	if code != ExitUsage {
		t.Fatalf("exit = %d, err = %s", code, errOut)
	}
	if !strings.Contains(errOut, "不能与显式工具名同时使用") {
		t.Errorf("应当说明冲突：%s", errOut)
	}
}

// hostRCFile 返回"当前平台会被 bs 写受管块的那个 rc 文件"。
//
// 这些用例断言的是"受管块写进了 rc 且块外内容一字不动"，所以 rc 路径必须按宿主平台算：
// POSIX 上是 ~/.bashrc，Windows 上 shell 会被认成 pwsh、rc 在 ~/Documents/PowerShell/ 下。
func hostRCFile(root string) string {
	home := filepath.Join(root, "home")
	if runtime.GOOS == "windows" {
		return filepath.Join(home, "Documents", "PowerShell", "Microsoft.PowerShell_profile.ps1")
	}
	return filepath.Join(home, ".bashrc")
}
