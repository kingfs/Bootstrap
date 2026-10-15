package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kingfs/Bootstrap/internal/install"
)

// toolManifestConfig 让清单只从用户目录加载，从而让 CLI 测试完全自包含
// （不依赖 tools/*.yaml 的实际内容，那些内容会在后续里程碑里持续变化）。
const toolManifestConfig = `version: 1
log:
  level: error
tool:
  manifest:
    builtin: false
    user_dirs:
      - %USERDIR%
`

const toolManifest = `
version: 1
tools:
  - id: g
    name: Go 版本管理器
    stack: golang
    kind: versionmgr
    description: 管理多个 Go 版本
    tags: [golang, version-manager]
    verify: {command: g}
    install:
      - via: release
        params:
          repo: voidint/g
          asset: g{{version}}-{{os}}.tar.gz
          dest: ~/.local/bin
  - id: golang
    name: Go
    stack: golang
    kind: runtime
    description: Go 工具链
    tags: [golang, runtime]
    verify:
      command: go
      version_args: [version]
      version_regex: 'go(?P<v>\d+\.\d+(?:\.\d+)?)'
      min_version: "1.22"
    depends_on: [g]
    install:
      - via: manager
        params: {manager: g, action: install, version: "1.24.1"}
      - via: apt
        params: {packages: [golang-go], allow_system: true}
  - id: btop
    stack: base
    kind: cli
    description: 资源监视器
    tags: [monitor]
    verify: {command: btop}
    install:
      - via: brew
        params: {formula: btop}
  # 只走 release 的工具：用来验证 plan 不联网（联网解析会卡住/报错）
  - id: dash
    stack: infra
    kind: cli
    description: 只从 release 安装
    tags: [dash]
    verify: {command: dash}
    install:
      - via: release
        params:
          repo: example/dash
          asset: dash-{{version}}-{{os}}-{{arch}}.tar.gz
          binary: dash
          dest: ~/.local/bin
profiles:
  base:
    description: 基础
    tools: [g, golang]
  extra:
    extends: [base]
    tools: [btop]
`

// sandboxWithManifest 准备一个"清单来自用户目录"的沙箱。
func sandboxWithManifest(t *testing.T) (*Runtime, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	rt, out, errBuf, root := sandbox(t)
	userDir := filepath.Join(root, "config", "tools")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "tools.yaml"), []byte(toolManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	writeConfig(t, root, strings.ReplaceAll(toolManifestConfig, "%USERDIR%", userDir))
	// 把 PATH 指向只含假命令的目录：探测与计划都只看到我们放进去的东西，
	// 既不会执行宿主机上的 go/g，也不会被宿主环境影响
	// （例如 go 会往 ~/.config/go/telemetry 写计数文件）。这是"测试不依赖宿主"的硬要求。
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFakeCmd(t, binDir, "g", fakeCmd{stdout: "g"})
	writeFakeCmd(t, binDir, "cargo", fakeCmd{stdout: "cargo"})
	// 故意低于 min_version 1.22 → 计划里应出现"安装"而不是"跳过"
	writeFakeCmd(t, binDir, "go", fakeCmd{stdout: "go version go1.20.0 linux/amd64"})
	t.Setenv("PATH", binDir)
	return rt, out, errBuf, root
}

// writeFakeBin 放一个假的可执行文件，让"已安装/可用"的判断完全由测试决定。
func writeFakeBin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// fakeCmd 描述一个假命令被调用时该做什么。
//
// 用"行为规格"而不是脚本正文来表达，是因为同一份意图要在两种平台上都能成立：
// POSIX 上是 `#!/bin/sh` 脚本，Windows 上没有 sh、也不认无扩展名的可执行文件
// （LookPath 靠 PATHEXT 找 `.cmd`）。这些用例的 PATH 只放假命令，所以假命令
// 必须真的能被找到并执行，否则用例测的就不是"产品怎么选方式"，而是"宿主有没有装 g"。
type fakeCmd struct {
	// stdout 是固定输出（版本探测用）。
	stdout string
	// logEnv 非空时，把收到的参数追加到该环境变量指向的文件（断言"到底执行了没有"）。
	logEnv string
	// sleep 非零时先等这么久（超时用例要一个不会自己结束的命令）。
	sleep time.Duration
}

// writeFakeCmd 按当前平台写一个假命令。
func writeFakeCmd(t *testing.T, dir, name string, spec fakeCmd) {
	t.Helper()
	if runtime.GOOS == "windows" {
		var b strings.Builder
		b.WriteString("@echo off\r\n")
		if spec.sleep > 0 {
			// ping 是 Windows 上最稳的"睡一会儿"（timeout 需要交互式控制台）。
			fmt.Fprintf(&b, "ping -n %d 127.0.0.1 >nul\r\n", int(spec.sleep.Seconds())+1)
		}
		if spec.stdout != "" {
			b.WriteString("echo " + spec.stdout + "\r\n")
		}
		if spec.logEnv != "" {
			b.WriteString("echo %*>>\"%" + spec.logEnv + "%\"\r\n")
		}
		writeFakeBin(t, dir, name+".cmd", b.String())
		return
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if spec.sleep > 0 {
		fmt.Fprintf(&b, "sleep %d\n", int(spec.sleep.Seconds()))
	}
	if spec.stdout != "" {
		b.WriteString("echo '" + spec.stdout + "'\n")
	}
	if spec.logEnv != "" {
		b.WriteString("echo \"$@\" >> \"$" + spec.logEnv + "\"\n")
	}
	writeFakeBin(t, dir, name, b.String())
}

func TestToolListCommand(t *testing.T) {
	rt, out, _, _ := sandboxWithManifest(t)
	code, stdout, stderr := run(t, rt, "tool", "list", "--no-probe")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"golang", "btop", "versionmgr"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
	_ = out

	// 按 stack 过滤
	code, stdout, _ = run(t, rt, "tool", "list", "--no-probe", "--stack", "base")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(stdout, "golang") || !strings.Contains(stdout, "btop") {
		t.Errorf("stack filter failed:\n%s", stdout)
	}

	// 按标签过滤
	code, stdout, _ = run(t, rt, "tool", "list", "--no-probe", "--tag", "monitor")
	if code != 0 || !strings.Contains(stdout, "btop") || strings.Contains(stdout, "golang") {
		t.Errorf("tag filter: code=%d\n%s", code, stdout)
	}

	// profile 过滤：base 只含 g 与 golang
	code, stdout, _ = run(t, rt, "tool", "list", "--no-probe", "--profile", "extra")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "btop") {
		t.Errorf("profile extends not applied:\n%s", stdout)
	}
}

func TestToolListJSON(t *testing.T) {
	rt, _, _, _ := sandboxWithManifest(t)
	code, stdout, stderr := run(t, rt, "tool", "list", "--no-probe", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0]["id"] != "g" || rows[0]["stack"] != "golang" {
		t.Errorf("row = %+v", rows[0])
	}
}

func TestToolInfoCommand(t *testing.T) {
	rt, _, _, _ := sandboxWithManifest(t)
	code, stdout, stderr := run(t, rt, "tool", "info", "golang")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"golang", "runtime", "manager:g", "依赖: g", "最低 1.22"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}

	// JSON 形态
	code, stdout, _ = run(t, rt, "tool", "info", "golang", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(stdout), &detail); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout)
	}
	if detail["id"] != "golang" {
		t.Errorf("detail = %+v", detail)
	}
	methods, ok := detail["methods"].([]any)
	if !ok || len(methods) != 2 {
		t.Fatalf("methods = %+v", detail["methods"])
	}

	// 未知工具 → 退出码 2（用法错误），并提示可用 id
	code, _, stderr = run(t, rt, "tool", "info", "nope")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d (stderr=%s)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "unknown tool") {
		t.Errorf("stderr = %s", stderr)
	}
}

func TestToolSearchCommand(t *testing.T) {
	rt, _, _, _ := sandboxWithManifest(t)
	code, stdout, _ := run(t, rt, "tool", "search", "监视", "--no-probe")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "btop") || strings.Contains(stdout, "golang") {
		t.Errorf("search result:\n%s", stdout)
	}
	code, stdout, _ = run(t, rt, "tool", "search", "zzz", "--no-probe")
	if code != 0 || !strings.Contains(stdout, "没有匹配") {
		t.Errorf("empty search: code=%d\n%s", code, stdout)
	}
}

// TestToolPlanDryRunTouchesNothing 是安全断言：
// plan 与 --dry-run 都不能创建任何文件。
func TestToolPlanDryRunTouchesNothing(t *testing.T) {
	rt, _, _, root := sandboxWithManifest(t)
	before := snapshotTree(t, root)

	code, stdout, stderr := run(t, rt, "tool", "plan", "golang")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "golang") || !strings.Contains(stdout, "manager:g") {
		t.Errorf("plan output:\n%s", stdout)
	}
	// 依赖 g 也应出现在计划里（拓扑序）
	if !strings.Contains(stdout, "g") {
		t.Errorf("dependency missing from plan:\n%s", stdout)
	}

	code, _, stderr = run(t, rt, "tool", "install", "golang", "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run exit = %d, stderr = %s", code, stderr)
	}
	if diff := diffTree(t, before, root); diff != "" {
		t.Errorf("plan/dry-run must not create anything, but:\n%s", diff)
	}
	// --no-probe 同样只读
	code, _, _ = run(t, rt, "tool", "list")
	if code != 0 {
		t.Fatalf("list exit = %d", code)
	}
	if diff := diffTree(t, before, root); diff != "" {
		t.Errorf("tool list must not create anything, but:\n%s", diff)
	}

	// 选择条件（--stack）也需要能工作
	code, stdout, _ = run(t, rt, "tool", "plan", "--stack", "golang")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "golang") {
		t.Errorf("stack selection:\n%s", stdout)
	}

	// 不给任何选择条件 → 用法错误（退出码 2）
	code, _, stderr = run(t, rt, "tool", "install")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d (stderr=%s)", code, ExitUsage, stderr)
	}
}

func TestToolPlanJSONAndViaOverride(t *testing.T) {
	rt, _, _, _ := sandboxWithManifest(t)
	code, stdout, stderr := run(t, rt, "tool", "plan", "--stack", "golang", "--json", "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var plan install.Plan
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("plan = %+v", plan.Steps)
	}
	// --json → 非交互 → 默认不开降级
	if !strings.Contains(stdout, "\"dry_run\"") {
		t.Errorf("plan json = %s", stdout)
	}

	// --via 指定一个清单里存在但不可用的方式 → 前置条件错误（4）
	code, _, stderr = run(t, rt, "tool", "plan", "golang", "--via", "apt", "--dry-run")
	if code != ExitPrecondition {
		t.Errorf("exit = %d, want %d (stderr=%s)", code, ExitPrecondition, stderr)
	}
	// --via 一个不存在的方式 → 用法错误（2）
	code, _, stderr = run(t, rt, "tool", "plan", "golang", "--via", "nope", "--dry-run")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d (stderr=%s)", code, ExitUsage, stderr)
	}
	// --fallback 与 --no-fallback 互斥
	code, _, _ = run(t, rt, "tool", "plan", "golang", "--fallback", "--no-fallback", "--dry-run")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
}

func TestManifestCommands(t *testing.T) {
	rt, _, _, _ := sandboxWithManifest(t)

	code, stdout, stderr := run(t, rt, "tool", "manifest", "validate")
	if code != 0 {
		t.Fatalf("validate exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "校验通过") {
		t.Errorf("stdout = %s", stdout)
	}

	code, stdout, _ = run(t, rt, "tool", "manifest", "path")
	if code != 0 {
		t.Fatalf("path exit = %d", code)
	}
	if !strings.Contains(stdout, "user") || !strings.Contains(stdout, "tools.yaml") {
		t.Errorf("path output = %s", stdout)
	}

	// 写一个坏清单 → 校验失败且退出码 1
	badDir := filepath.Join(t.TempDir(), "bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "bad.yaml"), []byte(`
version: 1
tools:
  - id: broken
    stack: golang
    kind: cli
    verify: {command: broken}
    install:
      - via: no-such-via
        params: {package: x}
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	code, _, stderr = run(t, rt, "tool", "manifest", "validate", "--config", writeBadConfig(t, badDir))
	if code != ExitFail {
		t.Errorf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stderr, "no-such-via") {
		t.Errorf("stderr = %s", stderr)
	}
}

func writeBadConfig(t *testing.T, userDir string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "version: 1\nlog: {level: error}\ntool:\n  manifest:\n    builtin: false\n    user_dirs:\n      - " + userDir + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestManifestDiffCommand 覆盖"本地草稿 vs 生效清单"的差异视图。
func TestManifestDiffCommand(t *testing.T) {
	rt, _, _, root := sandboxWithManifest(t)
	path := filepath.Join(root, "draft.yaml")
	if err := os.WriteFile(path, []byte(`
version: 1
tools:
  - id: golang
    stack: rust
    kind: runtime
    verify: {command: go}
    install:
      - via: manager
        params: {manager: rustup, action: install, version: "1.80"}
  - id: newtool
    stack: base
    kind: cli
    verify: {command: newtool}
    install:
      - via: brew
        params: {formula: newtool}
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	code, stdout, stderr := run(t, rt, "tool", "manifest", "diff", path)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "newtool") || !strings.Contains(stdout, "golang") {
		t.Errorf("diff output:\n%s", stdout)
	}
}

// snapshotTree / diffTree 用于断言"干跑没有产生任何文件"。
func snapshotTree(t *testing.T, root string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out[path] = info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// diffTree 比较两次快照。
//
// 有意忽略 cache/ 子树：探测缓存是"可随时删除的加速数据"（ADR-0011），
// 查询类命令刷新它是设计内行为；**config/ 与 state/ 一个字节都不能动**，
// 那才是"不产生副作用"的实质。
func diffTree(t *testing.T, before map[string]int64, root string) string {
	t.Helper()
	after := snapshotTree(t, root)
	cacheDir := filepath.Join(root, "cache")
	var sb strings.Builder
	for path := range after {
		if strings.HasPrefix(path, cacheDir) {
			continue
		}
		if _, ok := before[path]; !ok {
			sb.WriteString("created: " + path + "\n")
		}
	}
	for path := range before {
		if strings.HasPrefix(path, cacheDir) {
			continue
		}
		if _, ok := after[path]; !ok {
			sb.WriteString("removed: " + path + "\n")
		}
	}
	return sb.String()
}

// TestToolInstallEndToEndWithFakeManager 走完整的 plan → execute → verify → report：
// 用一个假 g（记录自己收到的参数）证明"委托管理器安装"这条主线真的能跑通，
// 同时证明 --dry-run 时一行都不会执行。
func TestToolInstallEndToEndWithFakeManager(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假命令用 POSIX shell 脚本实现")
	}
	rt, _, _, root := sandboxWithManifest(t)
	logPath := filepath.Join(root, "fake-g.log")
	t.Setenv("FAKE_G_LOG", logPath)
	writeFakeBin(t, filepath.Join(root, "bin"), "g", `#!/bin/sh
case "$1" in
  ls-remote) echo "1.23.0"; echo "1.24.1" ;;
  install)   echo "$@" >> "$FAKE_G_LOG" ;;
esac
`)

	// dry-run：必须一行都不执行
	code, stdout, stderr := run(t, rt, "tool", "install", "golang", "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "dry-run") {
		t.Errorf("dry-run output:\n%s", stdout)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("dry-run must not execute anything")
	}

	// 真跑：g install <解析出的最新版本>
	code, stdout, stderr = run(t, rt, "tool", "install", "golang", "--yes")
	if code != 0 {
		t.Fatalf("install exit = %d, stderr = %s\nstdout=%s", code, stderr, stdout)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake g was not called: %v", err)
	}
	if !strings.Contains(string(log), "install 1.24.1") {
		t.Errorf("g log = %q, want it to contain `install 1.24.1`", log)
	}
	if !strings.Contains(stdout, "完成 1") {
		t.Errorf("stdout =\n%s", stdout)
	}
	// 装完立刻用真实探测复核：假 go 仍报 1.20（低于 min_version）→ 不能算完成
	if strings.Contains(stdout, "golang\t完成") {
		t.Errorf("verification should have failed (fake go is still 1.20):\n%s", stdout)
	}
}

// TestToolInstallSkipsInstalled：已安装的工具会直接跳过，不执行任何命令。
func TestToolInstallSkipsInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假命令用 POSIX shell 脚本实现")
	}
	rt, _, _, root := sandboxWithManifest(t)
	logPath := filepath.Join(root, "fake-g.log")
	t.Setenv("FAKE_G_LOG", logPath)
	// go 报 1.24.1（>= min_version）→ 视为已安装
	writeFakeCmd(t, filepath.Join(root, "bin"), "go", fakeCmd{stdout: "go version go1.24.1 linux/amd64"})

	code, stdout, stderr := run(t, rt, "tool", "install", "golang", "--yes")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "0 个需要处理") {
		t.Errorf("stdout =\n%s", stdout)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Error("nothing should be executed for an installed tool")
	}
}

// TestToolInstallNothingToDoStillEmitsJSON：全部"已就绪"时 stdout 以前是空的——
// 脚本看到 `{}` 大小的输出会以为命令没跑（退出码却是 0）。现在必须给出一份完整文档，
// 与 `--dry-run --json` 的形状一致（计划 + 每步状态）。
func TestToolInstallNothingToDoStillEmitsJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假命令用 POSIX shell 脚本实现")
	}
	rt, _, _, root := sandboxWithManifest(t)
	writeFakeCmd(t, filepath.Join(root, "bin"), "go", fakeCmd{stdout: "go version go1.24.1 linux/amd64"})

	code, stdout, stderr := run(t, rt, "tool", "install", "golang", "--json", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	var plan struct {
		Steps []struct {
			ToolID string `json:"tool_id"`
			Status string `json:"status"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("stdout 必须是一份可解析的 JSON 文档：%v\nstdout=%q", err, stdout)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("至少要有一行说明这个工具的状态")
	}
	// golang 会带上依赖 g（版本管理器），两行都应当是 skip。
	found := false
	for _, st := range plan.Steps {
		if st.Status != "skip" {
			t.Errorf("已安装的工具状态应当是 skip：%+v", st)
		}
		if st.ToolID == "golang" {
			found = true
		}
	}
	if !found {
		t.Errorf("计划里应当有 golang：%+v", plan.Steps)
	}
}

// TestToolInstallFailsWhenNoMethodIsUsable：方法链全部不可用时，
// 计划里那一行是 failed，命令必须以非 0 结束（否则脚本会误判为装好了）。
func TestToolInstallFailsWhenNoMethodIsUsable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假命令用 POSIX shell 脚本实现")
	}
	rt, _, _, _ := sandboxWithManifest(t)
	// btop 只有 brew 一条路，而沙箱的 PATH 里没有 brew。
	code, stdout, stderr := run(t, rt, "tool", "install", "btop", "--yes")
	if code != ExitPrecondition {
		t.Fatalf("exit = %d, want %d\nstdout=%s\nstderr=%s", code, ExitPrecondition, stdout, stderr)
	}
	if !strings.Contains(stderr, "no usable install method") {
		t.Errorf("stderr 应说明为什么装不了：\n%s", stderr)
	}
	// plan 只是预览，仍然是 0
	if code, _, _ := run(t, rt, "tool", "plan", "btop"); code != 0 {
		t.Errorf("plan exit = %d, want 0", code)
	}
	// 不带 --yes 的非交互调用应该先撞上"需要确认"（退出码 5），
	// 但那要等计划里真的有活干；这里 failed 计划优先报 4。
}

// TestToolPlanIsOffline：plan 只做预览，因此不联网解析"最新版本"，
// 也不该在磁盘上留下任何东西（缓存/工作目录都不行）。
func TestToolPlanIsOffline(t *testing.T) {
	rt, _, _, root := sandboxWithManifest(t)
	before := snapshotTree(t, root)

	code, stdout, stderr := run(t, rt, "tool", "plan", "dash", "-v")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "<latest>") {
		t.Errorf("plan 应保留版本占位符（不联网解析）：\n%s", stdout)
	}
	if strings.Contains(stdout, "example/dash/releases/download/v") {
		t.Errorf("plan 不该解析出真实 tag：\n%s", stdout)
	}
	if diff := diffTree(t, before, root); diff != "" {
		t.Errorf("plan 必须什么都不留下，但有变化：\n%s", diff)
	}
}

// TestToolInstallRequiresConfirmation：破坏性操作在没有 --yes 的非交互环境里
// 必须拒绝执行并以退出码 5 结束，绝不能静默安装、也不能阻塞等输入。
func TestToolInstallRequiresConfirmation(t *testing.T) {
	rt, _, _, root := sandboxWithManifest(t)
	logPath := filepath.Join(root, "fake-g.log")
	t.Setenv("FAKE_G_LOG", logPath)
	writeFakeCmd(t, filepath.Join(root, "bin"), "g", fakeCmd{logEnv: "FAKE_G_LOG"})

	code, stdout, stderr := run(t, rt, "tool", "install", "golang")
	if code != ExitNeedsInteraction {
		t.Fatalf("exit = %d, want %d\nstdout=%s\nstderr=%s", code, ExitNeedsInteraction, stdout, stderr)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("错误信息要告诉用户怎么办：\n%s", stderr)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("未确认时不能执行任何安装命令")
	}

	// --non-interactive 与 --json 同理（后者是脚本消费，更不该弹提示）
	code, _, _ = run(t, rt, "tool", "install", "golang", "--non-interactive")
	if code != ExitNeedsInteraction {
		t.Errorf("--non-interactive exit = %d", code)
	}
	code, _, _ = run(t, rt, "tool", "install", "golang", "--json")
	if code != ExitNeedsInteraction {
		t.Errorf("--json exit = %d", code)
	}
}

// TestToolInstallInteractiveConfirm：交互式下只有明确回答 y 才执行，
// 回车或 n 都算拒绝（默认拒绝比默认执行安全）。
func TestToolInstallInteractiveConfirm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假命令用 POSIX shell 脚本实现")
	}
	for _, tc := range []struct {
		name     string
		answer   string
		executed bool
	}{
		{"回答 y", "y\n", true},
		{"回答 yes", "yes\n", true},
		{"回答 n", "n\n", false},
		{"直接回车", "\n", false},
		{"回答别的", "maybe\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, _, _, root := sandboxWithManifest(t)
			logPath := filepath.Join(root, "fake-g.log")
			t.Setenv("FAKE_G_LOG", logPath)
			writeFakeCmd(t, filepath.Join(root, "bin"), "g", fakeCmd{logEnv: "FAKE_G_LOG"})

			yes := true
			rt.interactiveOverride = &yes
			rt.Opts.In = strings.NewReader(tc.answer)

			code, stdout, stderr := run(t, rt, "tool", "install", "golang")
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %s", code, stderr)
			}
			if !strings.Contains(stderr, "继续？") {
				t.Errorf("交互式下应打印确认提示：\n%s", stderr)
			}
			_, err := os.Stat(logPath)
			switch {
			case tc.executed && err != nil:
				t.Errorf("回答 y 后应该执行安装：%v\nstdout=%s", err, stdout)
			case !tc.executed && err == nil:
				t.Errorf("拒绝后不该执行任何安装命令\nstdout=%s", stdout)
			case !tc.executed && !strings.Contains(stdout, "已取消"):
				t.Errorf("拒绝后应明确说明已取消：\n%s", stdout)
			}
		})
	}
}

// TestInteractiveDetection：interactive() 的判定必须是"任一条件不满足就不交互"。
func TestInteractiveDetection(t *testing.T) {
	rt, _, _, _ := sandboxWithManifest(t)
	rt.Opts.In = strings.NewReader("y\n")
	if rt.interactive() {
		t.Error("非终端输入不应判定为交互")
	}
	rt.Opts.In = os.Stdin
	if rt.interactive() {
		t.Error("测试进程的输出不是终端，不应判定为交互")
	}
	rt.Opts.NonInteractive = true
	if rt.interactive() {
		t.Error("--non-interactive 必须强制非交互")
	}
	rt.Opts.NonInteractive = false
	rt.Opts.JSON = true
	if rt.interactive() {
		t.Error("--json 必须强制非交互")
	}
}

// TestToolSelectorErrorsAreUsageErrors：写错的 --profile / --via 属于用法错误（退出码 2），
// 而且即使目标工具已安装、本该跳过，也不能把写错的方式悄悄咽下去。
func TestToolSelectorErrorsAreUsageErrors(t *testing.T) {
	rt, _, _, _ := sandboxWithManifest(t)

	code, _, stderr := run(t, rt, "tool", "plan", "--profile", "nosuch")
	if code != ExitUsage {
		t.Errorf("unknown profile exit = %d, want %d (stderr=%s)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr = %s", stderr)
	}

	// g 在沙箱里是"已安装"（假命令存在），但没有声明 via=release 之外的任何方式。
	code, _, stderr = run(t, rt, "tool", "install", "g", "--via", "nosuch", "--yes")
	if code != ExitUsage {
		t.Errorf("unknown via exit = %d, want %d (stderr=%s)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "no method via=nosuch") {
		t.Errorf("stderr = %s", stderr)
	}
}

// TestToolInstallInteractiveSelectionDrivesPlan：-i 在交互终端里问"勾哪些"，
// 勾选结果直接决定计划内容（这里勾掉默认项之外的一个）。
func TestToolInstallInteractiveSelectionDrivesPlan(t *testing.T) {
	rt, out, errBuf, _ := sandboxWithManifest(t)
	yes := true
	rt.interactiveOverride = &yes
	// 纯文本问答依次是：想怎么选（3=全部工具里勾选）、默认勾选状态（回车＝缺失的默认勾上）、
	// 多选（按 id 勾 golang）、破坏性操作确认 y。裸编号依赖清单里的排序，名字更稳。
	rt.Opts.In = strings.NewReader("3\n\ngolang\ny\n")

	code, stdout, stderr := run(t, rt, "tool", "install", "-i", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(errBuf.String(), "选择要安装的工具") {
		t.Errorf("交互式下应当先问勾哪些：\n%s", errBuf.String())
	}
	if !strings.Contains(out.String(), "golang") {
		t.Errorf("计划里应当有勾选的 golang：\n%s", out.String())
	}
	if strings.Contains(out.String(), "btop") {
		t.Errorf("没勾的工具不该进计划：\n%s", out.String())
	}
}

// TestToolInstallInteractiveProfileScopeInstallsWholeProfile：`-i` 里选"按套装→整套装安装"
// 时不再逐个勾选，直接按套装出计划（用户实测反馈：想整组装却得先记住 --profile 怎么拼）。
func TestToolInstallInteractiveProfileScopeInstallsWholeProfile(t *testing.T) {
	rt, out, errBuf, _ := sandboxWithManifest(t)
	yes := true
	rt.interactiveOverride = &yes
	// 想怎么选（1=按套装）→ 选一个套装（extra，extends base）→ 怎么装（1=整套装安装）
	rt.Opts.In = strings.NewReader("1\nextra\n1\n")

	code, stdout, stderr := run(t, rt, "tool", "install", "-i", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(errBuf.String(), "想怎么选工具？") {
		t.Errorf("没给范围参数时应当先问想怎么选：\n%s", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "选择要安装的工具") {
		t.Errorf("整套装安装不该再进勾选列表：\n%s", errBuf.String())
	}
	if !strings.Contains(out.String(), "btop") {
		t.Errorf("extra 套装里的 btop 应当进计划：\n%s", out.String())
	}
}

// TestToolInstallInteractiveScopedSkipsScopeMenu：命令行已经用参数圈定范围时不再问
// "想怎么选"——`-i --stack` 与改动前一样直接进勾选器。
func TestToolInstallInteractiveScopedSkipsScopeMenu(t *testing.T) {
	rt, _, errBuf, _ := sandboxWithManifest(t)
	yes := true
	rt.interactiveOverride = &yes
	rt.Opts.In = strings.NewReader("\n\n\ny\n")

	code, stdout, stderr := run(t, rt, "tool", "install", "-i", "--stack", "golang", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if strings.Contains(errBuf.String(), "想怎么选工具？") {
		t.Errorf("给了 --stack 就不该再问想怎么选：\n%s", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "选择要安装的工具") {
		t.Errorf("应当直接进勾选器：\n%s", errBuf.String())
	}
}

// TestToolInstallInteractiveNeedsTerminal：--json/管道等非交互场景下 -i 必须以
// 退出码 5 明确拒绝，而不是静默退化成"全部安装"。
func TestToolInstallInteractiveNeedsTerminal(t *testing.T) {
	rt, _, _, _ := sandboxWithManifest(t)
	no := false
	rt.interactiveOverride = &no

	code, stdout, stderr := run(t, rt, "tool", "install", "-i", "--dry-run")
	if code != ExitNeedsInteraction {
		t.Fatalf("exit = %d，期望 %d\nstdout=%s\nstderr=%s", code, ExitNeedsInteraction, stdout, stderr)
	}
	if !strings.Contains(stderr, "需要终端") {
		t.Errorf("应当说明需要终端：\n%s", stderr)
	}
}

// TestToolInstallInteractiveRejectsArgs：-i 与显式工具名同时给是用法错误，
// 否则"我点名的工具被勾掉了"这种意外会很难查。
func TestToolInstallInteractiveRejectsArgs(t *testing.T) {
	rt, _, _, _ := sandboxWithManifest(t)
	yes := true
	rt.interactiveOverride = &yes

	code, _, stderr := run(t, rt, "tool", "install", "-i", "golang")
	if code != ExitUsage {
		t.Fatalf("exit = %d，期望 %d\nstderr=%s", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "不能与显式工具名同时使用") {
		t.Errorf("错误信息应当说清冲突：\n%s", stderr)
	}
}
