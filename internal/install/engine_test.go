package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
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
	info, _ := os.Stat(target)
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("installed binary must be executable")
	}
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
