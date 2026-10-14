package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kingfs/Bootstrap/internal/detect"
	"github.com/kingfs/Bootstrap/internal/manifest"
)

const engineManifest = `
version: 1
tools:
  - id: g
    stack: golang
    kind: versionmgr
    verify: {command: g}
    install:
      - via: release
        params:
          repo: voidint/g
          asset: g{{version}}-{{os}}-{{arch}}.tar.gz
          dest: ~/.local/bin
  - id: golang
    stack: golang
    kind: runtime
    verify:
      command: go
      version_args: [version]
      version_regex: 'go(?P<v>\d+\.\d+(?:\.\d+)?)'
    depends_on: [g]
    install:
      - via: manager
        params: {manager: g, action: install, version: "1.24.1"}
      - via: apt
        params: {packages: [golang-go]}
`

func newEngine(t *testing.T, env *fakeEnv, loaded *manifest.Loaded, det *fakeDetector, opt EngineOptions) *Engine {
	t.Helper()
	if opt.Registry == nil {
		opt.Registry = NewRegistry()
	}
	opt.Env = env
	opt.Loaded = loaded
	if det != nil {
		opt.Detector = newDetector(t, det)
	}
	if opt.Now == nil {
		opt.Now = func() time.Time { return time.Unix(1700000000, 0) }
	}
	eng, err := NewEngine(opt)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng
}

func TestPlanToolsTopoOrderAndDetection(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["go"] = "/usr/bin/go"
	env.bins["g"] = "/usr/bin/g"
	loaded := loadManifest(t, engineManifest)
	det := newFakeDetector(map[string]detect.Status{"go": detect.StatusInstalled, "g": detect.StatusInstalled})
	det.version["go"] = "go1.24.1"
	eng := newEngine(t, env, loaded, det, EngineOptions{})
	plan, err := eng.PlanTools(context.Background(), []string{"golang"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	// 依赖 g 自动补进来，且排在 golang 之前（拓扑序）
	if strings.Join(plan.Order, ",") != "g,golang" {
		t.Fatalf("order = %v", plan.Order)
	}
	// 两个都"已安装" → 都跳过，不需要任何命令
	for _, s := range plan.Steps {
		if s.Status != StepSkip {
			t.Errorf("%s status = %s (%s)", s.ToolID, s.Status, s.Reason)
		}
	}
	if len(env.commands) != 0 {
		t.Errorf("plan must not run anything: %v", env.commandLines())
	}
}

func TestPlanToolsInstallsMissing(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["g"] = "/usr/bin/g"
	env.tags["voidint/g"] = "v1.5.0"
	env.files["https://github.com/voidint/g/releases/download/v1.5.0/checksums.txt"] =
		strings.Repeat("a", 64) + "  g1.5.0-linux-amd64.tar.gz\n"
	loaded := loadManifest(t, engineManifest)
	det := newFakeDetector(map[string]detect.Status{"g": detect.StatusInstalled, "go": detect.StatusMissing})
	eng := newEngine(t, env, loaded, det, EngineOptions{})

	plan, err := eng.PlanTools(context.Background(), []string{"golang"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %+v", plan.Steps)
	}
	if plan.Steps[0].ToolID != "g" || plan.Steps[0].Status != StepSkip {
		t.Errorf("g should be skipped: %+v", plan.Steps[0])
	}
	if plan.Steps[1].ToolID != "golang" || plan.Steps[1].Status != StepInstall {
		t.Fatalf("golang should be planned for install: %+v", plan.Steps[1])
	}
	if plan.Steps[1].Via != "manager" {
		t.Errorf("via = %s", plan.Steps[1].Via)
	}
	if plan.Steps[1].Action == nil || len(plan.Steps[1].Action.Commands) != 1 {
		t.Fatalf("action = %+v", plan.Steps[1].Action)
	}
	mustContain(t, plan.Steps[1].Action.Commands[0].String(), "g install 1.24.1")
}

// TestApplyDryRunTouchesNothing 是安全红线：干跑一行命令都不执行。
func TestApplyDryRunTouchesNothing(t *testing.T) {
	env := newFakeEnv(t)
	env.dry = true
	env.bins["g"] = "/usr/bin/g"
	loaded := loadManifest(t, engineManifest)
	det := newFakeDetector(map[string]detect.Status{})
	eng := newEngine(t, env, loaded, det, EngineOptions{})

	plan, err := eng.PlanTools(context.Background(), []string{"golang"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	report, err := eng.Apply(context.Background(), plan, OpInstall)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(env.commands) != 0 || len(env.downloads) != 0 {
		t.Fatalf("dry-run executed things: %v %v", env.commandLines(), env.downloads)
	}
	if report.Changed() {
		t.Error("dry-run must not report changes")
	}
	for _, s := range report.Steps {
		if s.Status != StepStatus("planned") && s.Status != StepSkip {
			t.Errorf("%s status = %s", s.ToolID, s.Status)
		}
	}
}

func TestApplyInstallsAndVerifies(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["g"] = "/usr/bin/g"
	loaded := loadManifest(t, engineManifest)
	// 第一次探测（plan）时 go 缺失；install 之后探测变成已安装。
	det := newFakeDetector(map[string]detect.Status{"go": detect.StatusMissing, "g": detect.StatusInstalled})
	eng := newEngine(t, env, loaded, det, EngineOptions{})
	plan, err := eng.PlanTools(context.Background(), []string{"golang"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	// 模拟"装完就真的有了"：验证阶段的主命令探测设为 installed
	det.status["go"] = detect.StatusInstalled
	det.version["go"] = "go1.24.1"

	report, err := eng.Apply(context.Background(), plan, OpInstall)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var done int
	for _, s := range report.Steps {
		if s.Status == StepDone {
			done++
			if s.Detection == nil || s.Detection.Status != detect.StatusInstalled {
				t.Errorf("%s verification missing: %+v", s.ToolID, s.Detection)
			}
		}
	}
	if done != 1 {
		t.Fatalf("done = %d, steps = %+v", done, report.Steps)
	}
	if !report.Changed() {
		t.Error("report should mark changes")
	}
	ran := env.ran("g")
	if len(ran) != 1 {
		t.Fatalf("expected g install to run once, got %v", env.commandLines())
	}
}

// TestApplyVerifyFailure 覆盖"命令成功但工具仍然不在"的情况。
func TestApplyVerifyFailure(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["g"] = "/usr/bin/g"
	loaded := loadManifest(t, engineManifest)
	det := newFakeDetector(map[string]detect.Status{"go": detect.StatusMissing, "g": detect.StatusInstalled})
	eng := newEngine(t, env, loaded, det, EngineOptions{})
	plan, err := eng.PlanTools(context.Background(), []string{"golang"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	report, err := eng.Apply(context.Background(), plan, OpInstall)
	if err == nil {
		t.Fatal("verify failure must surface as an error")
	}
	mustContain(t, err.Error(), "golang")
	var failed bool
	for _, s := range report.Steps {
		if s.ToolID == "golang" && s.Status == StepFailed {
			failed = true
			mustContain(t, s.Error, "验证失败")
		}
	}
	if !failed {
		t.Fatalf("steps = %+v", report.Steps)
	}
}

func TestApplyFallback(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["g"] = "/usr/bin/g"
	env.bins["apt-get"] = "/usr/bin/apt-get"
	env.runErr["g"] = errors.New("g failed")
	loaded := loadManifest(t, engineManifest)
	det := newFakeDetector(map[string]detect.Status{"go": detect.StatusMissing, "g": detect.StatusInstalled})
	eng := newEngine(t, env, loaded, det, EngineOptions{Fallback: true})
	plan, err := eng.PlanTools(context.Background(), []string{"golang"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	det.status["go"] = detect.StatusInstalled

	report, err := eng.Apply(context.Background(), plan, OpInstall)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(report.Fallbacks) != 1 {
		t.Fatalf("fallbacks = %+v", report.Fallbacks)
	}
	fb := report.Fallbacks[0]
	if fb.ToolID != "golang" || fb.From != "manager" || fb.To != "apt" {
		t.Errorf("fallback = %+v", fb)
	}
	mustContain(t, fb.Reason, "g failed")
	var step Step
	for _, s := range report.Steps {
		if s.ToolID == "golang" {
			step = s
		}
	}
	if step.Status != StepDone || step.FallbackFrom != "manager" || step.Via != "apt" {
		t.Fatalf("step = %+v", step)
	}
	if len(step.TriedMethods) != 2 {
		t.Errorf("tried = %v", step.TriedMethods)
	}
	if len(env.ran("apt-get")) == 0 {
		t.Error("fallback command was not executed")
	}
}

func TestApplyNoFallbackReportsAllFailures(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["g"] = "/usr/bin/g"
	env.bins["apt-get"] = "/usr/bin/apt-get"
	env.runErr["g"] = errors.New("g failed")
	loaded := loadManifest(t, engineManifest)
	det := newFakeDetector(map[string]detect.Status{"go": detect.StatusMissing, "g": detect.StatusInstalled})
	eng := newEngine(t, env, loaded, det, EngineOptions{Fallback: false})
	plan, err := eng.PlanTools(context.Background(), []string{"golang"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	report, err := eng.Apply(context.Background(), plan, OpInstall)
	if err == nil {
		t.Fatal("expected failure")
	}
	mustContain(t, err.Error(), "g failed")
	if len(report.Fallbacks) != 0 {
		t.Errorf("no fallback expected: %+v", report.Fallbacks)
	}
	if len(env.ran("apt-get")) != 0 {
		t.Error("must not silently switch methods when fallback is off")
	}
}

func TestUninstallSkipsMissing(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["cargo"] = "/usr/bin/cargo"
	loaded := loadManifest(t, `
version: 1
tools:
  - id: rg
    stack: base
    kind: cli
    verify: {command: rg}
    install:
      - via: cargo
        params: {crate: ripgrep}
`)
	det := newFakeDetector(map[string]detect.Status{"rg": detect.StatusMissing})
	eng := newEngine(t, env, loaded, det, EngineOptions{})
	plan, err := eng.PlanTools(context.Background(), []string{"rg"}, OpUninstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	if plan.Steps[0].Status != StepSkip {
		t.Fatalf("steps = %+v", plan.Steps)
	}
	// 已安装时给出卸载计划
	det.status["rg"] = detect.StatusInstalled
	plan, err = eng.PlanTools(context.Background(), []string{"rg"}, OpUninstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	if plan.Steps[0].Status != StepUninstall {
		t.Fatalf("steps = %+v", plan.Steps)
	}
	mustContain(t, plan.Steps[0].Action.Commands[0].String(), "cargo uninstall ripgrep")
}

func TestPlanToolsNoMethod(t *testing.T) {
	env := newFakeEnv(t)
	loaded := loadManifest(t, `
version: 1
tools:
  - id: rg
    stack: base
    kind: cli
    verify: {command: rg}
    install:
      - via: brew
        params: {formula: ripgrep}
`)
	eng := newEngine(t, env, loaded, newFakeDetector(map[string]detect.Status{}), EngineOptions{})
	plan, err := eng.PlanTools(context.Background(), []string{"rg"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	if plan.Steps[0].Status != StepFailed {
		t.Fatalf("steps = %+v", plan.Steps[0])
	}
	mustContain(t, plan.Steps[0].Error, "no usable install method")
	// 这一条同时要被标记成"这台机器上没方式可用"：`upgrade --all` 靠它区分
	// "清单要求的工具都没装"（可以跳过）与"少了校验和/模板写错"（必须报错）。
	if !plan.Steps[0].NoMethod {
		t.Errorf("NoMethod = false，期望 true：%+v", plan.Steps[0])
	}
}

// TestPlanToolsUnsupportedIsNotNoMethod：系统包管理器拒绝升级属于"设计上不做"，
// 与"没有可用方式"是两回事——前者是 unsupported，后者才带 NoMethod。
func TestPlanToolsUnsupportedIsNotNoMethod(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["apt-get"] = "/usr/bin/apt-get"
	loaded := loadManifest(t, `
version: 1
tools:
  - id: htop
    stack: base
    kind: cli
    verify: {command: htop}
    install:
      - via: apt
        params: {packages: [htop]}
`)
	eng := newEngine(t, env, loaded, newFakeDetector(map[string]detect.Status{}), EngineOptions{})
	plan, err := eng.PlanTools(context.Background(), []string{"htop"}, OpUpgrade)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	step := plan.Steps[0]
	if step.Status != StepUnsupported {
		t.Fatalf("status = %v，期望 unsupported", step.Status)
	}
	if step.NoMethod {
		t.Errorf("NoMethod = true，但这是 unsupported（设计上不升级系统包）")
	}
}

func TestPlanToolsDisabledAndWhen(t *testing.T) {
	env := newFakeEnv(t)
	loaded := loadManifest(t, `
version: 1
tools:
  - id: only-linux
    stack: base
    kind: cli
    verify: {command: x}
    when: {os: [linux]}
    install:
      - via: apt
        params: {packages: [x]}
  - id: off
    stack: base
    kind: cli
    verify: {command: y}
    disabled: true
    install:
      - via: apt
        params: {packages: [y]}
`)
	eng := newEngine(t, env, loaded, newFakeDetector(map[string]detect.Status{}), EngineOptions{})
	plan, err := eng.PlanTools(context.Background(), []string{"only-linux"}, OpInstall)
	if err != nil {
		t.Fatalf("on linux the tool applies: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("steps = %+v", plan.Steps)
	}
	// Windows 上整条工具都不适用
	env.os = "windows"
	if _, err := eng.PlanTools(context.Background(), []string{"only-linux"}, OpInstall); err == nil {
		t.Error("tool whose when does not match must not be planned")
	}
}

// ── release 安装器的真实落地测试（在 t.TempDir 里完成，无副作用）──

// requireExecBit 断言路径带 POSIX 可执行位。
//
// Windows 没有权限位，普通文件即可执行（产品判定见 fsx.IsExecutable），
// 所以这里只在类 Unix 上断言。刻意不用 t.Skip：那会连同本用例后面的
// 软链、干跑等断言一起跳过，掩盖真正的问题（文件没装出来/内容不对）。
func requireExecBit(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Windows 上"是否可执行"由扩展名/文件关联决定，权限位恒为 0666 或 0444，
		// 断言 0o111 必然失败，且失败也不说明产品有问题。
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary must be executable: %s mode=%v", path, info.Mode())
	}
}

func TestReleaseInstallerExecute(t *testing.T) {
	env := newFakeEnv(t)
	env.tags["owner/tool"] = "v2.0.0"
	archiveBytes := tarGz(t, map[string]string{"tool-2.0.0/bin/tool": "#!/bin/sh\necho hi\n"})
	asset := "tool-2.0.0-linux-amd64.tar.gz"
	env.files["https://github.com/owner/tool/releases/download/v2.0.0/"+asset] = string(archiveBytes)
	env.files["https://github.com/owner/tool/releases/download/v2.0.0/checksums.txt"] =
		strings.Repeat("b", 64) + "  " + asset + "\n"

	loaded := loadManifest(t, `
version: 1
tools:
  - id: tool
    stack: base
    kind: cli
    verify: {command: tool}
    install:
      - via: release
        params:
          repo: owner/tool
          asset: tool-{{version}}-{{os}}-{{arch}}.tar.gz
          binary: tool
          strip_components: 2
          dest: ~/.local/bin
          symlink: {from: ~/.local/bin/tool, to: ~/.local/bin/tt}
`)
	tool, _ := loaded.ByID("tool")
	in, _ := NewRegistry().Get("release")
	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if action.Downloads[0].SHA256 != strings.Repeat("b", 64) {
		t.Fatalf("checksum not resolved: %+v", action.Downloads[0])
	}
	if err := in.Execute(context.Background(), action, env); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	target := filepath.Join(env.home, ".local", "bin", "tool")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}
	if !strings.Contains(string(body), "echo hi") {
		t.Errorf("content = %q", body)
	}
	// 可执行位只在类 Unix 上有意义（Windows 上文件存在即可执行）。
	requireExecBit(t, target)
	link := filepath.Join(env.home, ".local", "bin", "tt")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink missing: %v", err)
	}

	// 干跑：不下载、不落盘
	env2 := newFakeEnv(t)
	env2.dry = true
	if err := in.Execute(context.Background(), action, env2); err != nil {
		t.Fatalf("dry-run Execute: %v", err)
	}
	if len(env2.downloads) != 0 {
		t.Error("dry-run must not download")
	}
	if _, err := os.Stat(filepath.Join(env2.home, ".local")); !os.IsNotExist(err) {
		t.Error("dry-run must not create directories")
	}
}

// TestReleaseSymlinkRefusesRealFile 是安全断言：不覆盖用户自己的真实文件。
func TestReleaseSymlinkRefusesRealFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("user data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := replaceSymlink(src, real); err == nil {
		t.Fatal("must refuse to replace a real file")
	}
	body, _ := os.ReadFile(real)
	if string(body) != "user data" {
		t.Errorf("user file changed: %q", body)
	}
}

func TestReleaseDryRunUsesPlaceholder(t *testing.T) {
	env := newFakeEnv(t)
	env.dry = true
	loaded := loadManifest(t, `
version: 1
tools:
  - id: tool
    stack: base
    kind: cli
    verify: {command: tool}
    install:
      - via: release
        params:
          repo: owner/tool
          asset: tool-{{version}}-{{os}}.tar.gz
          dest: ~/.local/bin
`)
	tool, _ := loaded.ByID("tool")
	in, _ := NewRegistry().Get("release")
	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// 预览阶段不打网络：用占位符 + 默认 sha256 关闭
	mustContain(t, action.Summary, "<latest>")
	mustContain(t, action.Downloads[0].URL, "<latest>")
}

// TestRecorderIdempotent：内容不变时不写盘（mtime 不变）。
func TestRecorderIdempotent(t *testing.T) {
	dir := t.TempDir()
	rec := &Recorder{Path: filepath.Join(dir, "state", "installed.yaml")}
	now := time.Unix(1700000000, 0)
	report := Report{Steps: []Step{{ToolID: "rg", Via: "cargo", Status: StepDone,
		Detection: &detect.Result{Version: "14.1.0"}}}}
	if err := rec.Record(report, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
	before, err := os.Stat(rec.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := rec.Record(report, now.Add(time.Hour)); err != nil {
		t.Fatalf("Record again: %v", err)
	}
	after, err := os.Stat(rec.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("identical record content must not rewrite the file")
	}
	data, _ := os.ReadFile(rec.Path)
	mustContain(t, string(data), "rg")
	mustContain(t, string(data), "cargo")

	// 记录损坏时不应崩溃，而是重建
	if err := os.WriteFile(rec.Path, []byte("{{{{not yaml"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := rec.Record(report, now); err != nil {
		t.Fatalf("Record over corrupt file: %v", err)
	}
}

func TestOSEnvLookPathAndDryRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		// 本用例的核心前提是"没有可执行位的文件不该被当成命令"：
		// 它注入 GOOS=linux 走 executableName 的 POSIX 分支，但 fsx.IsExecutable
		// 用的是宿主 GOOS——Windows 上没有可执行位（普通文件即可执行），
		// 而且 os/exec.LookPath 还认 PATHEXT。该前提在 Windows 上不成立，
		// 整条跳过；放宽成"Windows 也认"会把这条 Linux 语义断言废掉。
		t.Skip("Windows 无可执行位语义且 LookPath 按 PATHEXT 查找，本用例的 POSIX 前提不成立")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "extra")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	exe := filepath.Join(binDir, "mytool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	env := NewOSEnv(OSEnvOptions{
		Env: map[string]string{"PATH": "/nonexistent"}, ExtraPath: []string{binDir},
		HomeDir: dir, GOOS: "linux", Arch: "amd64", DryRun: true,
	})
	if p, ok := env.LookPath("mytool"); !ok || p != exe {
		t.Fatalf("LookPath = %q, %v", p, ok)
	}
	if _, ok := env.LookPath("nope"); ok {
		t.Error("LookPath must not find missing tools")
	}
	// 不可执行的文件不算
	ro := filepath.Join(binDir, "readonly")
	if err := os.WriteFile(ro, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := env.LookPath("readonly"); ok {
		t.Error("non-executable file must not be found")
	}
	// 干跑下 Run 不执行
	if _, err := env.Run(context.Background(), Command{Name: "echo"}); !errors.Is(err, ErrDryRun) {
		t.Errorf("dry-run Run err = %v", err)
	}
	// 干跑下 Download 不下载
	if _, err := env.Download(context.Background(), Download{URL: "https://x/y"}); !errors.Is(err, ErrDryRun) {
		t.Errorf("dry-run Download err = %v", err)
	}
	if env.DryRun() != true || env.OS() != "linux" || env.Arch() != "amd64" || env.HomeDir() != dir {
		t.Error("OSEnv accessors are wrong")
	}
	if env.Env()["PATH"] != "/nonexistent" {
		t.Error("Env must expose the injected environment")
	}
	if env.Log() == nil {
		t.Error("Log must never be nil")
	}
}

func TestNewEngineValidation(t *testing.T) {
	if _, err := NewEngine(EngineOptions{Loaded: loadManifest(t, "version: 1\n")}); err == nil {
		t.Error("missing ExecEnv must fail")
	}
	if _, err := NewEngine(EngineOptions{Env: newFakeEnv(t)}); err == nil {
		t.Error("missing manifest must fail")
	}
}

func TestEngineNoToolsSelected(t *testing.T) {
	env := newFakeEnv(t)
	loaded := loadManifest(t, engineManifest)
	eng := newEngine(t, env, loaded, newFakeDetector(map[string]detect.Status{}), EngineOptions{})
	if _, err := eng.PlanTools(context.Background(), nil, OpInstall); err != nil {
		t.Fatalf("empty selection should be a no-op: %v", err)
	}
	if _, err := eng.PlanTools(context.Background(), []string{"nope"}, OpInstall); err == nil {
		t.Error("unknown tool id must fail")
	}
}

func TestLogPath(t *testing.T) {
	got := LogPath("/state", "golang/gopls", time.Unix(1700000000, 0))
	// 本地时区相关，只断言形状：logs/<日期>-<时间>-<工具>.log
	ok, err := regexp.MatchString(`logs/\d{8}-\d{6}-golang_gopls\.log$`, filepath.ToSlash(got))
	if err != nil || !ok {
		t.Errorf("LogPath = %q", got)
	}
}

// TestReleaseInstallerExtractTree：目录型发布物整包解压（Go 工具链、nvm 用得上）。
func TestReleaseInstallerExtractTree(t *testing.T) {
	env := newFakeEnv(t)
	env.tags["owner/tree"] = "v1.2.0"
	archiveBytes := tarGz(t, map[string]string{
		"tree-1.2.0/bin/tree":     "#!/bin/sh\necho tree\n",
		"tree-1.2.0/lib/notes.md": "docs\n",
	})
	asset := "tree-1.2.0-linux-amd64.tar.gz"
	env.files["https://github.com/owner/tree/releases/download/v1.2.0/"+asset] = string(archiveBytes)
	env.files["https://github.com/owner/tree/releases/download/v1.2.0/checksums.txt"] =
		strings.Repeat("c", 64) + "  " + asset + "\n"

	loaded := loadManifest(t, `
version: 1
tools:
  - id: tree
    stack: base
    kind: cli
    verify: {command: tree}
    install:
      - via: release
        params:
          repo: owner/tree
          asset: tree-{{version}}-{{os}}-{{arch}}.tar.gz
          strip_components: 1
          extract: true
          dest: ~/.local/tree
          symlink: {from: ~/.local/tree/bin/tree, to: ~/.local/bin/tree}
`)
	tool, _ := loaded.ByID("tree")
	in, _ := NewRegistry().Get("release")
	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	mustContain(t, action.Summary, "解压到")
	if err := in.Execute(context.Background(), action, env); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 整棵树都在，而不是只挑出 bin/tree
	for _, rel := range []string{"bin/tree", "lib/notes.md"} {
		if _, err := os.Stat(filepath.Join(env.home, ".local", "tree", rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	link := filepath.Join(env.home, ".local", "bin", "tree")
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("symlink missing: %v", err)
	}
}

// TestReleaseExtractNeedsArchive：extract 只能用在归档资源上，写错要在计划期就报错。
func TestReleaseExtractNeedsArchive(t *testing.T) {
	env := newFakeEnv(t)
	loaded := loadManifest(t, `
version: 1
tools:
  - id: flat
    stack: base
    kind: cli
    verify: {command: flat}
    install:
      - via: release
        params:
          repo: owner/flat
          asset: flat-{{version}}-{{os}}
          extract: true
          dest: ~/.local/flat
`)
	tool, _ := loaded.ByID("flat")
	in, _ := NewRegistry().Get("release")
	if _, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall); err == nil {
		t.Fatal("extract without an archive must fail")
	}
}

// TestNormalizeManagerConfig：把 nodejs 的管理器从 fnm 换成 nvm 时，
// 依赖与 via=manager 的落地目标都应自动跟着换，清单不用分叉（§6.2）。
func TestNormalizeManagerConfig(t *testing.T) {
	env := newFakeEnv(t)
	// nvm 是 shell 函数型管理器：需要 source 脚本存在，且要有 sh 可用。
	env.has("sh")
	if err := os.MkdirAll(filepath.Join(env.home, ".nvm"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.home, ".nvm", "nvm.sh"), []byte("# nvm\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded := loadManifest(t, `
version: 1
tools:
  - id: fnm
    stack: nodejs
    kind: versionmgr
    verify: {command: fnm}
    install:
      - via: brew
        params: {formula: fnm}
  - id: nvm
    stack: nodejs
    kind: versionmgr
    verify: {command: nvm}
    install:
      - via: script
        params: {url: "https://example.com/nvm.sh", insecure: true}
  - id: nodejs
    stack: nodejs
    kind: runtime
    depends_on: [fnm]
    verify: {command: node}
    install:
      - via: manager
        params: {action: install, version: "22.0.0"}
`)
	eng, err := NewEngine(EngineOptions{
		Env: env, Loaded: loaded, Registry: NewRegistry(),
		Detector: newDetector(t, newFakeDetector(map[string]detect.Status{})),
		Managers: map[string]string{"nodejs": "nvm"},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	plan, err := eng.PlanTools(context.Background(), []string{"nodejs"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	var order []string
	for _, s := range plan.Steps {
		order = append(order, s.ToolID)
		if s.ToolID != "nodejs" {
			continue
		}
		if s.Via != "manager" || s.Manager != "nvm" {
			t.Errorf("nodejs should be installed by nvm, got via=%s manager=%s (%s)", s.Via, s.Manager, s.Error)
		}
	}
	if len(order) != 2 || order[0] != "nvm" || order[1] != "nodejs" {
		t.Fatalf("order = %v, want [nvm nodejs]", order)
	}
}

// TestReleaseChecksumCandidates：真实仓库的校验和文件名五花八门，
// 必须能顺着候选列表找到（g 用 sha256sum.txt，k9s 用 checksums.sha256，
// Rust 生态用 <asset>.sha256）。
func TestReleaseChecksumCandidates(t *testing.T) {
	asset := "tool-1.0.0-linux-amd64.tar.gz"
	hash := strings.Repeat("d", 64)
	cases := []struct {
		name  string
		files map[string]string
		param string
	}{
		{"sha256sum.txt", map[string]string{"sha256sum.txt": hash + "  " + asset + "\n"}, ""},
		{"checksums.sha256", map[string]string{"checksums.sha256": hash + " *" + asset + "\n"}, ""},
		{"bare asset.sha256", map[string]string{asset + ".sha256": hash + "\n"}, ""},
		{"explicit checksum_file", map[string]string{"SUMS-1.0.0": hash + "  " + asset + "\n"}, "SUMS-{{version}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newFakeEnv(t)
			env.tags["owner/tool"] = "v1.0.0"
			for name, body := range tc.files {
				env.files["https://github.com/owner/tool/releases/download/v1.0.0/"+name] = body
			}
			loaded := loadManifest(t, `
version: 1
tools:
  - id: tool
    stack: base
    kind: cli
    verify: {command: tool}
    install:
      - via: release
        params:
          repo: owner/tool
          asset: tool-{{version}}-{{os}}-{{arch}}.tar.gz
          dest: ~/.local/bin
`+checksumFileLine(tc.param))
			tool, _ := loaded.ByID("tool")
			in, _ := NewRegistry().Get("release")
			action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if action.Downloads[0].SHA256 != hash {
				t.Fatalf("sha = %q, want %q", action.Downloads[0].SHA256, hash)
			}
		})
	}
}

func checksumFileLine(name string) string {
	if name == "" {
		return ""
	}
	return "          checksum_file: " + name + "\n"
}

// TestReleaseChecksumMissing：找不到校验和就必须失败（AGENTS.md §8.3），
// 而不是"提示一下照样装"。
func TestReleaseChecksumMissing(t *testing.T) {
	env := newFakeEnv(t)
	env.tags["owner/tool"] = "v1.0.0"
	loaded := loadManifest(t, `
version: 1
tools:
  - id: tool
    stack: base
    kind: cli
    verify: {command: tool}
    install:
      - via: release
        params:
          repo: owner/tool
          asset: tool-{{version}}-{{os}}.tar.gz
          dest: ~/.local/bin
`)
	tool, _ := loaded.ByID("tool")
	in, _ := NewRegistry().Get("release")
	_, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err == nil {
		t.Fatal("missing checksum must fail the plan")
	}
	mustContain(t, err.Error(), "cannot find a sha256")
}

// TestReleasePinnedSHA256：清单里固化 sha256 时不再联网找校验文件。
func TestReleasePinnedSHA256(t *testing.T) {
	env := newFakeEnv(t)
	env.tags["owner/tool"] = "v1.0.0"
	hash := strings.Repeat("e", 64)
	loaded := loadManifest(t, `
version: 1
tools:
  - id: tool
    stack: base
    kind: cli
    verify: {command: tool}
    install:
      - via: release
        params:
          repo: owner/tool
          asset: tool-linux.zip
          dest: ~/.local/bin
          sha256: `+hash+`
`)
	tool, _ := loaded.ByID("tool")
	in, _ := NewRegistry().Get("release")
	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if action.Downloads[0].SHA256 != hash {
		t.Fatalf("sha = %q", action.Downloads[0].SHA256)
	}
}

// TestSourceCloneToDest：nvm 这类"仓库即安装物"的工具，直接按固定 tag 克隆到目标目录。
func TestSourceCloneToDest(t *testing.T) {
	env := newFakeEnv(t)
	env.has("git")
	loaded := loadManifest(t, `
version: 1
tools:
  - id: nvm
    stack: nodejs
    kind: versionmgr
    verify: {command: nvm}
    install:
      - via: source
        params: {repo: nvm-sh/nvm, ref: v0.40.3, dest: ~/.nvm}
`)
	tool, _ := loaded.ByID("nvm")
	in, _ := NewRegistry().Get("source")
	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	args := action.Commands[0].Args
	joined := strings.Join(args, " ")
	mustContain(t, joined, "--branch v0.40.3")
	mustContain(t, joined, filepath.Join(env.home, ".nvm"))
	mustContain(t, action.Summary, "git 克隆")
}

// TestUpdateAndUninstallChainsAreHonored：tool.update / tool.uninstall 写了就用它，
// 没写才复用 install 链（否则这两个字段就是纯装饰，写清单的人会被骗）。
func TestUpdateAndUninstallChainsAreHonored(t *testing.T) {
	loaded := loadManifest(t, `
version: 1
tools:
  - id: demo
    stack: base
    kind: cli
    verify: {command: demo, version_args: [--version], version_regex: 'v(?P<v>\d+\.\d+\.\d+)', min_version: "2.0"}
    install:
      - via: brew
        params: {formula: demo}
    update:
      via: cargo
      params: {crate: demo}
    uninstall:
      - via: npm
        params: {package: demo, global: true}
  - id: plain
    stack: base
    kind: cli
    verify: {command: plain, version_args: [--version], version_regex: 'v(?P<v>\d+\.\d+\.\d+)', min_version: "2.0"}
    install:
      - via: brew
        params: {formula: plain}
`)
	env := newFakeEnv(t).has("brew").has("cargo").has("npm")
	eng, err := NewEngine(EngineOptions{
		Env: env, Loaded: loaded,
		// outdated：install 会继续（已安装才跳过），uninstall 也不会被当成"没装"。
		Detector: newDetector(t, newFakeDetector(map[string]detect.Status{
			"demo": detect.StatusOutdated, "plain": detect.StatusOutdated,
		})),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	cases := []struct {
		op   Op
		id   string
		want string
	}{
		{OpInstall, "demo", "brew"},
		{OpUpgrade, "demo", "cargo"},
		{OpUninstall, "demo", "npm"},
		{OpUpgrade, "plain", "brew"},
		{OpUninstall, "plain", "brew"},
	}
	for _, tc := range cases {
		plan, err := eng.PlanTools(context.Background(), []string{tc.id}, tc.op)
		if err != nil {
			t.Fatalf("%v/%s: %v", tc.op, tc.id, err)
		}
		if len(plan.Steps) != 1 {
			t.Fatalf("%v/%s: steps = %+v", tc.op, tc.id, plan.Steps)
		}
		if got := plan.Steps[0].Via; got != tc.want {
			t.Errorf("%v/%s: via = %s, want %s (status=%s reason=%s skipped=%v)",
				tc.op, tc.id, got, tc.want, plan.Steps[0].Status, plan.Steps[0].Reason, plan.Steps[0].Skipped)
		}
	}
}

// TestApplyWritesPerToolLog：真执行时每个工具的命令输出要留一份日志，
// 干跑则一行都不写（--dry-run 零副作用）。
func TestApplyWritesPerToolLog(t *testing.T) {
	loaded := loadManifest(t, `
version: 1
tools:
  - id: demo
    stack: base
    kind: cli
    verify: {command: demo, version_args: [--version], version_regex: 'v(?P<v>\d+\.\d+\.\d+)'}
    install:
      - via: brew
        params: {formula: demo}
`)
	stateDir := t.TempDir()
	env := newFakeEnv(t).has("brew")
	env.runOut["brew"] = "demo 1.2.3\n"
	eng, err := NewEngine(EngineOptions{
		Env: env, Loaded: loaded, LogDir: stateDir,
		Detector: newDetector(t, newFakeDetector(map[string]detect.Status{"demo": detect.StatusMissing})),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	plan, err := eng.PlanTools(context.Background(), []string{"demo"}, OpInstall)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	report, _ := eng.Apply(context.Background(), plan, OpInstall)
	if len(report.Steps) == 0 || report.Steps[0].LogPath == "" {
		t.Fatalf("no log path: %+v", report.Steps)
	}
	if !strings.HasPrefix(report.Steps[0].LogPath, filepath.Join(stateDir, "logs")) {
		t.Errorf("log path = %q", report.Steps[0].LogPath)
	}
	data, err := os.ReadFile(report.Steps[0].LogPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "brew install demo") || !strings.Contains(string(data), "demo 1.2.3") {
		t.Errorf("log content = %q", data)
	}

	// 干跑：同样的计划，不该产生任何日志（LogPath 为空，logs 目录不存在）。
	dry := newFakeEnv(t).has("brew")
	dry.dry = true
	eng2, err := NewEngine(EngineOptions{
		Env: dry, Loaded: loaded, LogDir: stateDir,
		Detector: newDetector(t, newFakeDetector(map[string]detect.Status{"demo": detect.StatusMissing})),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	plan2, err := eng2.PlanTools(context.Background(), []string{"demo"}, OpInstall)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if rep, _ := eng2.Apply(context.Background(), plan2, OpInstall); len(rep.Steps) > 0 && rep.Steps[0].LogPath != "" {
		t.Errorf("dry-run must not write logs: %q", rep.Steps[0].LogPath)
	}
}

// TestRefreshProbeCacheAfterApply：装完/卸完要把结论写回探测缓存。
//
// 这是"装完却看不见"的回归保护：计划阶段探测到 missing 会写进 probe.json，
// 若装完不更新，`bs tool list --installed` 会在整个 TTL 周期里继续显示没装。
func TestRefreshProbeCacheAfterApply(t *testing.T) {
	now := time.Now()
	cache := &detect.Cache{
		Path: filepath.Join(t.TempDir(), "probe.json"), TTL: time.Hour,
		Now: func() time.Time { return now },
	}
	if err := cache.Merge(map[string]detect.Result{
		"mytool": {ToolID: "mytool", Status: detect.StatusMissing, CheckedAt: now},
		"gone":   {ToolID: "gone", Status: detect.StatusInstalled, Version: "1.0.0", CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	env := newFakeEnv(t)
	env.has("go")
	fd := newFakeDetector(map[string]detect.Status{"mytool": detect.StatusMissing})
	det := &detect.Detector{
		Executor: fd, LookPath: fd, HomeDir: t.TempDir(),
		Timeout: time.Second, Concurrency: 2, Cache: cache,
	}
	loaded := loadManifest(t, `
version: 1
tools:
  - id: mytool
    stack: base
    kind: cli
    verify: {command: mytool, version_args: [--version], version_regex: 'mytool (?P<v>\d+\.\d+\.\d+)'}
    install:
      - via: goinstall
        when: {has: [go]}
        params: {module: example.com/mytool@latest}
`)
	eng, err := NewEngine(EngineOptions{Env: env, Loaded: loaded, Detector: det, LogDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	ctx := context.Background()
	plan, err := eng.PlanTools(ctx, []string{"mytool"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	if plan.Steps[0].Status != StepInstall {
		t.Fatalf("plan = %+v", plan.Steps[0])
	}

	// 模拟"装上了"：探测结果变成 installed（真实流程里这是刚跑完的安装命令的结果）。
	fd.status["mytool"] = detect.StatusInstalled
	fd.version["mytool"] = "mytool 1.2.3"
	report, err := eng.Apply(ctx, plan, OpInstall)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if report.Steps[0].Status != StepDone {
		t.Fatalf("step = %+v", report.Steps[0])
	}

	got, err := cache.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r, ok := got["mytool"]
	if !ok {
		t.Fatal("装完之后缓存里必须有 mytool：否则 tool list --installed 仍显示没装")
	}
	if r.Status != detect.StatusInstalled || r.Version != "1.2.3" {
		t.Errorf("缓存 = %+v，期望 installed 1.2.3", r)
	}

	// 卸载：即使 Step 里带着"计划阶段探测到已安装"的结果，也必须按 id 失效，
	// 否则会把"已安装"重新写回缓存（卸载后列表还显示装着）。
	installed := detect.Result{ToolID: "gone", Status: detect.StatusInstalled, Version: "1.0.0", CheckedAt: now}
	eng.refreshProbeCache(Step{ToolID: "gone", Detection: &installed}, OpUninstall)
	got, err = cache.Load()
	if err != nil {
		t.Fatalf("Load#2: %v", err)
	}
	if _, ok := got["gone"]; ok {
		t.Error("卸载后该工具的缓存项应当消失")
	}
}
