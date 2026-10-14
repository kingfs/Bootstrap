package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/internal/install"
	"github.com/kingfs/Bootstrap/internal/ui"
)

// fakePrompter 是按脚本回答问题的假交互通道：既能驱动向导，也能断言"问了什么"。
type fakePrompter struct {
	interactive bool
	selects     []string   // Select 依次返回的值
	multis      [][]string // MultiSelect 依次返回的值
	confirms    []bool     // Confirm 依次返回的值
	// inputs 是 Input 依次返回的值；用完后回落到 q.Default
	// （这样只关心"问过没有"的用例不必给每一问都准备答案）。
	inputs []string
	calls  []string // 调用轨迹，例如 "select:..." 、"multiselect"、"confirm"
	// multiSeen 记录 MultiSelect 收到的选项，用来断言默认勾选是否符合预期。
	multiSeen []ui.MultiSelect
	// inputSeen / confirmSeen 记录收到的提问内容，用来断言"问的是什么、顺序对不对"。
	inputSeen   []ui.Question
	confirmSeen []ui.Question
}

func (f *fakePrompter) nextSelect(q ui.Question) (string, error) {
	f.calls = append(f.calls, "select:"+q.Title)
	if len(f.selects) == 0 {
		return "", errors.New("fakePrompter: 没有为 Select 准备答案")
	}
	v := f.selects[0]
	f.selects = f.selects[1:]
	return v, nil
}

func (f *fakePrompter) nextMulti(q ui.MultiSelect) ([]string, error) {
	f.calls = append(f.calls, "multiselect")
	f.multiSeen = append(f.multiSeen, q)
	if len(f.multis) == 0 {
		return nil, errors.New("fakePrompter: 没有为 MultiSelect 准备答案")
	}
	v := f.multis[0]
	f.multis = f.multis[1:]
	return v, nil
}

func (f *fakePrompter) nextConfirm(q ui.Question) (bool, error) {
	f.calls = append(f.calls, "confirm")
	f.confirmSeen = append(f.confirmSeen, q)
	if len(f.confirms) == 0 {
		return false, errors.New("fakePrompter: 没有为 Confirm 准备答案")
	}
	v := f.confirms[0]
	f.confirms = f.confirms[1:]
	return v, nil
}

// MultiSelect 实现 ui.Prompter。
func (f *fakePrompter) MultiSelect(q ui.MultiSelect) ([]string, error) { return f.nextMulti(q) }

// Select 实现 ui.Prompter。
func (f *fakePrompter) Select(q ui.Question, _ []ui.Option) (string, error) { return f.nextSelect(q) }

// Confirm 实现 ui.Prompter。
func (f *fakePrompter) Confirm(q ui.Question) (bool, error) { return f.nextConfirm(q) }

// Input 实现 ui.Prompter。
func (f *fakePrompter) Input(q ui.Question) (string, error) {
	f.calls = append(f.calls, "input:"+q.Title)
	f.inputSeen = append(f.inputSeen, q)
	if len(f.inputs) == 0 {
		return q.Default, nil
	}
	v := f.inputs[0]
	f.inputs = f.inputs[1:]
	return v, nil
}

// Interactive 实现 ui.Prompter。
func (f *fakePrompter) Interactive() bool { return f.interactive }

// initManifest 是 init 用例的清单：全部通过假管理器安装，因此执行阶段既真实又不联网。
// 探测用的命令名刻意用 bs-init-probe-*：它们在任何宿主上都不存在，
// 因此"装没装"完全由测试里的假管理器决定，不受开发机影响。
const initManifest = `
version: 1
tools:
  - id: g
    stack: golang
    kind: versionmgr
    description: Go 版本管理器（假管理器）
    verify: {command: g}
    install:
      - via: manager
        params: {manager: g, action: install, version: g}
  - id: runtime-a
    stack: golang
    kind: runtime
    description: 需要管理器安装的运行时
    depends_on: [g]
    verify:
      command: bs-init-probe-a
      version_args: [version]
      version_regex: '(?P<v>\d+\.\d+(?:\.\d+)?)'
      min_version: "1.22"
    install:
      - via: manager
        params: {manager: g, action: install, version: "1.24.1"}
  - id: cli-b
    stack: base
    kind: cli
    description: 普通的 CLI 工具
    verify: {command: bs-init-probe-b}
    install:
      - via: manager
        params: {manager: g, action: install, version: cli-b}
profiles:
  base:
    description: 基础
    tools: [g, runtime-a]
  tiny:
    tools: [cli-b]
`

// initApp 准备一个用 initManifest 的 App：配置文件已存在、PATH 里只有我们放的假命令。
func initApp(t *testing.T) *App {
	t.Helper()
	a, root := toolSandboxApp(t, initManifest)
	a.Cfg.Tool.DefaultProfile = "base"
	a.Cfg.Proxy.DefaultProfile = "local"
	// 配置文件存在 → 向导不会问"要不要生成配置"。
	if err := os.MkdirAll(filepath.Dir(a.Paths.ConfigFile()), 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.WriteFile(a.Paths.ConfigFile(), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	for _, dir := range []string{a.Paths.HomeDir, a.Paths.StateDir, a.Paths.CacheDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	_ = root
	return a
}

// addFakeManager 放一个假 g：收到 `g install <x>` 就"装"出对应命令，
// 让"计划 → 执行 → 探测验证"这条链路真的跑通，且完全不碰网络。
//
// PATH 里保留 /usr/bin:/bin 是必需的：脚本要调 chmod（shell 内建做不到改权限位），
// 而"探测命令名"用的是 bs-init-probe-*，宿主上不可能存在，所以不会引入宿主干扰。
func addFakeManager(t *testing.T, a *App) string {
	t.Helper()
	bin := filepath.Join(a.Paths.HomeDir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	script := `#!/bin/sh
dir="${0%/*}"
case "$1.$2" in
  install.1.24.1)
    printf '#!/bin/sh\necho "bs-init-probe-a 1.24.1"\n' > "$dir/bs-init-probe-a"; chmod +x "$dir/bs-init-probe-a" ;;
  install.cli-b)
    printf '#!/bin/sh\necho "bs-init-probe-b 0.8.1"\n' > "$dir/bs-init-probe-b"; chmod +x "$dir/bs-init-probe-b" ;;
  *)
    printf '#!/bin/sh\necho ok\n' > "$dir/$2"; chmod +x "$dir/$2" ;;
esac
exit 0
`
	addFakeBin(t, bin, "g", script)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
	return bin
}

func TestInitOverviewCountsAndPaths(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)

	ov, err := a.InitOverview(context.Background())
	if err != nil {
		t.Fatalf("InitOverview: %v", err)
	}
	if ov.OS != "linux" || ov.Arch != "arm64" || ov.Distro != "ubuntu 24.04" {
		t.Errorf("平台信息不对：%+v", ov)
	}
	if !ov.ConfigExists || ov.ConfigFile != a.Paths.ConfigFile() {
		t.Errorf("配置信息不对：%+v", ov)
	}
	if ov.RepoRoot == "" {
		t.Error("应当给出仓库根目录")
	}
	if ov.Proxy != "local" {
		t.Errorf("代理应报告配置里的 default_profile：%q", ov.Proxy)
	}
	if ov.Tools.Applicable != 3 {
		t.Errorf("适用工具 = %d, want 3（%+v）", ov.Tools.Applicable, ov.Tools)
	}
	// 假 g 在 PATH 里 → g 已装；两个 probe 还没"装"出来 → 缺失。
	if ov.Tools.Installed != 1 || ov.Tools.Missing != 2 {
		t.Errorf("装机统计不对：%+v", ov.Tools)
	}
}

func TestInitNonInteractiveUsesDefaultProfile(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)

	res, err := a.Init(context.Background(), InitOptions{
		Prompter:  ui.NonInteractive{},
		AssumeYes: true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Mode != SelectionProfile || res.Profile != "base" {
		t.Errorf("非交互应回退到默认套装：mode=%s profile=%s", res.Mode, res.Profile)
	}
	if res.Report == nil || res.Report.Failed() != nil {
		t.Fatalf("应当装成功：%+v", res.Report)
	}
	// 假 g 应该真的被调用，并把被管的命令"装"出来。
	if _, err := os.Stat(filepath.Join(a.Paths.HomeDir, "bin", "bs-init-probe-a")); err != nil {
		t.Errorf("假管理器没有被调用（probe-a 不存在）：%v", err)
	}
}

func TestInitNonInteractiveWithoutYesNeedsInteraction(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)

	_, err := a.Init(context.Background(), InitOptions{Prompter: ui.NonInteractive{}})
	if !errors.Is(err, ui.ErrNotInteractive) {
		t.Fatalf("err = %v, want ErrNotInteractive（非交互且没有 --yes 不能自己动手）", err)
	}
	if _, err := os.Stat(filepath.Join(a.Paths.HomeDir, "bin", "bs-init-probe-a")); err == nil {
		t.Error("没有授权就不该真的执行安装")
	}
}

func TestInitInteractiveManualSelectionPrefersMissing(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)

	p := &fakePrompter{
		interactive: true,
		selects:     []string{"manual"},
		multis:      [][]string{{"runtime-a"}},
	}
	res, err := a.Init(context.Background(), InitOptions{Prompter: p, AssumeYes: true})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Mode != SelectionManual || strings.Join(res.Tools, ",") != "runtime-a" {
		t.Fatalf("手工选择结果不对：mode=%s tools=%v", res.Mode, res.Tools)
	}
	if len(p.multiSeen) != 1 {
		t.Fatalf("应当只问一次多选，实际 %d 次", len(p.multiSeen))
	}
	selected := map[string]bool{}
	for _, o := range p.multiSeen[0].Options {
		selected[o.Value] = o.Selected
	}
	if selected["g"] {
		t.Error("已安装的 g 不该默认勾选（默认应当是「补缺」而不是「全都重来」）")
	}
	if !selected["runtime-a"] || !selected["cli-b"] {
		t.Errorf("缺失的工具应默认勾选：%v", selected)
	}
	if p.multiSeen[0].MinSelected != 1 {
		t.Errorf("MinSelected = %d, want 1（不能一个都不选）", p.multiSeen[0].MinSelected)
	}
}

func TestInitInteractiveProfileFlow(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)

	// 交互式：先选"按套装"（默认第一项），再选 tiny 套装，最后确认执行、不装 shell。
	p := &fakePrompter{
		interactive: true,
		selects:     []string{"profile", "tiny"},
		confirms:    []bool{true, false},
	}
	res, err := a.Init(context.Background(), InitOptions{Prompter: p})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Mode != SelectionProfile || res.Profile != "tiny" {
		t.Fatalf("mode=%s profile=%s, want profile/tiny", res.Mode, res.Profile)
	}
	if res.Report == nil || res.Report.Failed() != nil {
		t.Fatalf("应当装成功：%+v", res.Report)
	}
	if !res.SkippedShell || res.Shell != nil {
		t.Errorf("回答了不装 shell，就不该动 rc 文件：%+v", res.Shell)
	}
	// 两次 confirm：计划一次、shell 一次。
	if got := countCalls(p.calls, "confirm"); got != 2 {
		t.Errorf("confirm 次数 = %d, want 2（计划 + shell）", got)
	}
}

func TestInitMissingOnlySkipsInstalled(t *testing.T) {
	a := initApp(t)
	bin := addFakeManager(t, a)
	// 把 probe-a 也"装"好，于是只有 cli-b 是缺失的。
	addFakeBin(t, bin, "bs-init-probe-a", "#!/bin/sh\necho 'bs-init-probe-a 1.24.1'\n")

	res, err := a.Init(context.Background(), InitOptions{
		MissingOnly: true,
		Prompter:    ui.NonInteractive{},
		AssumeYes:   true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Mode != SelectionMissing {
		t.Fatalf("mode = %s, want missing", res.Mode)
	}
	if strings.Join(res.Tools, ",") != "cli-b" {
		t.Errorf("只该选中缺失的 cli-b：%v", res.Tools)
	}
}

func TestInitCancelOnConfirmDoesNothing(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)

	p := &fakePrompter{interactive: true, confirms: []bool{false}}
	res, err := a.Init(context.Background(), InitOptions{Prompter: p, Profile: "tiny"})
	if !errors.Is(err, ui.ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	if res.Report != nil {
		t.Error("取消后不该有执行结果")
	}
	if _, err := os.Stat(filepath.Join(a.Paths.HomeDir, "bin", "bs-init-probe-b")); err == nil {
		t.Error("取消后不该真的执行安装")
	}
	if _, err := os.Stat(filepath.Join(a.Paths.StateDir, "installed.yaml")); err == nil {
		t.Error("取消后不该写安装记录")
	}
}

func TestInitDryRunPlansConfigButWritesNothing(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)
	a.DryRun = true
	a.Runner.DryRun = true
	// 指向一个还不存在的配置目录：dry-run 下必须"只说要生成"。
	a.Paths.ConfigDir = filepath.Join(a.Paths.ConfigDir, "not-yet")

	beforeHome := snapshotSet(snapshot(t, a.Paths.HomeDir))
	beforeState := snapshotSet(snapshot(t, a.Paths.StateDir))

	res, err := a.Init(context.Background(), InitOptions{
		Prompter:  ui.NonInteractive{},
		AssumeYes: true,
		Profile:   "base",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !res.DryRunPlannedConfig || res.ConfigCreated {
		t.Errorf("dry-run 应报告「会生成配置」但不真生成：planned=%v created=%v",
			res.DryRunPlannedConfig, res.ConfigCreated)
	}
	if _, err := os.Stat(a.Paths.ConfigFile()); err == nil {
		t.Error("--dry-run 不能写配置文件")
	}
	if diff := diffSnapshot(t, beforeHome, a.Paths.HomeDir); diff != "" {
		t.Errorf("dry-run 不该改动 home：\n%s", diff)
	}
	if diff := diffSnapshot(t, beforeState, a.Paths.StateDir); diff != "" {
		t.Errorf("dry-run 不该改动 state：\n%s", diff)
	}
	if res.Report == nil {
		t.Fatal("dry-run 也要给出（干跑的）报告")
	}
	for _, s := range res.Report.Steps {
		// 干跑把 install/upgrade/uninstall 统一改写成 planned，绝不出现 done。
		if s.Status != install.StepStatus("planned") && s.Status != install.StepSkip {
			t.Errorf("dry-run 报告的步骤状态应为 planned/skip，实际 %s（%s）", s.Status, s.ToolID)
		}
	}
}

func TestInitCreatesConfigWhenMissing(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)
	if err := os.Remove(a.Paths.ConfigFile()); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	// 注意只有两次 confirm：配置文件缺失已经不再问"要不要生成"，
	// 剩下的两次是"执行计划"与"要不要碰 shell"。
	p := &fakePrompter{
		interactive: true,
		confirms:    []bool{true, false},
	}
	res, err := a.Init(context.Background(), InitOptions{Prompter: p, Profile: "tiny"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !res.ConfigCreated {
		t.Error("缺失的配置应当被生成")
	}
	if _, err := os.Stat(a.Paths.ConfigFile()); err != nil {
		t.Errorf("配置文件没生成：%v", err)
	}
	if res.DryRunPlannedConfig {
		t.Error("非 dry-run 不该标记为「计划生成」")
	}
	for _, q := range p.confirmSeen {
		if strings.Contains(q.Title, "生成") {
			t.Errorf("配置缺失不该再问一句：%q", q.Title)
		}
	}
}

// TestInitWizardRepoRootAndProxy：向导的前两问会落到配置文件，并在本次运行立即生效。
func TestInitWizardRepoRootAndProxy(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)

	newRoot := filepath.Join(a.Paths.HomeDir, "code")
	const proxyURL = "http://127.0.0.1:7890"
	p := &fakePrompter{
		interactive: true,
		inputs:      []string{newRoot, proxyURL},
		selects:     []string{"none"}, // 工具这步选"先不装"
	}
	res, err := a.Init(context.Background(), InitOptions{Prompter: p, AssumeYes: true})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if !res.Setup.RepoRootChanged || res.Setup.RepoRoot != newRoot {
		t.Errorf("仓库根目录应当被改成 %s：%+v", newRoot, res.Setup)
	}
	if res.Overview.RepoRoot != newRoot {
		t.Errorf("概览里的仓库根目录也该同步：%s", res.Overview.RepoRoot)
	}
	if root, _ := a.RepoRoot(); root != newRoot {
		t.Errorf("本次运行应立刻用新目录：%s", root)
	}

	if !res.Setup.ProxyConfigured || res.Setup.ProxyName != "default" {
		t.Errorf("代理应当被写入并启用：%+v", res.Setup)
	}
	if res.Setup.ProxyMasked != proxyURL {
		t.Errorf("没有凭据时脱敏结果应原样：%q", res.Setup.ProxyMasked)
	}

	// 落盘检查：重新装一遍配置，确认两个值都真的写进了文件。
	loaded, err := config.Load(config.LoadOptions{Paths: a.Paths, ExplicitFile: a.Paths.ConfigFile()})
	if err != nil {
		t.Fatalf("重新装载配置：%v", err)
	}
	if loaded.Config.Repo.Root != newRoot {
		t.Errorf("repo.root 没写进配置：%q", loaded.Config.Repo.Root)
	}
	if got := loaded.Config.Proxy.Profiles["default"].HTTPSProxy; got != proxyURL {
		t.Errorf("代理 profile 没写进配置：%q", got)
	}
	if loaded.Config.Proxy.DefaultProfile != "default" {
		t.Errorf("proxy.default_profile 没写进配置：%q", loaded.Config.Proxy.DefaultProfile)
	}
	if name, _, _ := a.ProxyService().ActiveName(); name != "default" {
		t.Errorf("代理应当已启用：%q", name)
	}
}

// TestInitWizardProxyMasksCredentials：带凭据的代理地址在其他输出里只能以 *** 出现。
func TestInitWizardProxyMasksCredentials(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)

	const proxyURL = "http://user:secret@127.0.0.1:7890"
	p := &fakePrompter{
		interactive: true,
		inputs:      []string{a.Paths.HomeDir, proxyURL},
		selects:     []string{"none"},
	}
	res, err := a.Init(context.Background(), InitOptions{Prompter: p, AssumeYes: true})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if strings.Contains(res.Setup.ProxyMasked, "secret") {
		t.Errorf("凭据不该出现在结果里：%q", res.Setup.ProxyMasked)
	}
	if !strings.Contains(res.Setup.ProxyMasked, "***") {
		t.Errorf("凭据应当被掩码：%q", res.Setup.ProxyMasked)
	}
}

// TestInitAutoAsksNothing：--yes（Auto）在真终端里也是脚本语义，
// 仓库根/代理/工具选择一个都不问，全部走配置默认值。
func TestInitAutoAsksNothing(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)

	// 没有为任何提问准备答案：一旦问出口，fakePrompter 就会报错。
	p := &fakePrompter{interactive: true}
	res, err := a.Init(context.Background(), InitOptions{Prompter: p, AssumeYes: true, Auto: true})
	if err != nil {
		t.Fatalf("全自动不该提问：%v", err)
	}
	if res.Mode != SelectionProfile || res.Profile != "base" {
		t.Errorf("应当回退到配置里的默认套装：mode=%s profile=%s", res.Mode, res.Profile)
	}
	if len(p.calls) != 0 {
		t.Errorf("--yes 下一个问题都不该问，实际：%v", p.calls)
	}
}

// TestInitUnknownShellSkipsShellIntegration：探测不到 shell 时不是失败，
// 只是跳过最后一步并说明原因（以前会一路走到 ShellInstall("") 报 unsupported shell）。
func TestInitUnknownShellSkipsShellIntegration(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)
	a.Platform.Shell = ""
	yes := true

	res, err := a.Init(context.Background(), InitOptions{
		Prompter:     ui.NonInteractive{},
		AssumeYes:    true,
		Profile:      "tiny",
		ShellInstall: &yes,
	})
	if err != nil {
		t.Fatalf("探测不到 shell 不该让 init 失败：%v", err)
	}
	if !res.SkippedShell || res.ShellSkippedReason == "" {
		t.Errorf("应当报告「跳过 shell 集成」及原因：%+v", res)
	}
	if res.Shell != nil {
		t.Errorf("不该写受管块：%+v", res.Shell)
	}
	if _, err := os.Stat(filepath.Join(a.Paths.HomeDir, ".bashrc")); err == nil {
		t.Error("不该创建 rc 文件")
	}
}

// TestInitUnsupportedShellSkipsShellIntegration：认得出 shell 但不在支持范围（nu/tcsh…）时，
// 自动流程也只跳过、不报错；但用户显式 --with-shell 时做不了就得如实报错。
func TestInitUnsupportedShellSkipsShellIntegration(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)
	a.Platform.Shell = "nu"

	res, err := a.Init(context.Background(), InitOptions{
		Prompter:  ui.NonInteractive{},
		AssumeYes: true,
		Profile:   "tiny",
	})
	if err != nil {
		t.Fatalf("自动识别出不支持的 shell 不该让 init 失败：%v", err)
	}
	if !res.SkippedShell || !strings.Contains(res.ShellSkippedReason, "nu") {
		t.Errorf("应当报告「跳过 shell 集成」并点明是哪个 shell：%+v", res)
	}

	yes := true
	if _, err := a.Init(context.Background(), InitOptions{
		Prompter:     ui.NonInteractive{},
		AssumeYes:    true,
		Profile:      "tiny",
		ShellInstall: &yes,
	}); err == nil {
		t.Error("显式要求装受管块却遇到不支持的 shell 时，应当报错而不是悄悄跳过")
	}
}

func TestInitShellInstallOptIn(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)
	yes := true

	res, err := a.Init(context.Background(), InitOptions{
		Prompter:     ui.NonInteractive{},
		AssumeYes:    true,
		Profile:      "tiny",
		ShellInstall: &yes,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Shell == nil {
		t.Fatal("明确要求装 shell 时必须写受管块")
	}
	data, err := os.ReadFile(res.Shell.RCFile)
	if err != nil {
		t.Fatalf("读 rc：%v", err)
	}
	if !strings.Contains(string(data), "# >>> bootstrap") {
		t.Errorf("rc 里应该有受管块：\n%s", data)
	}
	if !strings.HasPrefix(res.Shell.RCFile, a.Paths.HomeDir) {
		t.Errorf("rc 文件必须落在沙箱 home 里：%s", res.Shell.RCFile)
	}
}

func TestInitNoShellTouchesNothing(t *testing.T) {
	a := initApp(t)
	addFakeManager(t, a)
	no := false

	res, err := a.Init(context.Background(), InitOptions{
		Prompter:     ui.NonInteractive{},
		AssumeYes:    true,
		Profile:      "tiny",
		ShellInstall: &no,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Shell != nil || !res.SkippedShell {
		t.Errorf("明确不装 shell 时不该动 rc：%+v", res.Shell)
	}
	if _, err := os.Stat(filepath.Join(a.Paths.HomeDir, ".bashrc")); err == nil {
		t.Error("不该创建 .bashrc")
	}
}

func TestInitNothingToDoIsNotAnError(t *testing.T) {
	a := initApp(t)
	bin := addFakeManager(t, a)
	addFakeBin(t, bin, "bs-init-probe-a", "#!/bin/sh\necho 'bs-init-probe-a 1.24.1'\n")
	addFakeBin(t, bin, "bs-init-probe-b", "#!/bin/sh\necho 'bs-init-probe-b 0.8.1'\n")

	res, err := a.Init(context.Background(), InitOptions{
		MissingOnly: true,
		Prompter:    ui.NonInteractive{},
		AssumeYes:   true,
	})
	if err != nil {
		t.Fatalf("没活干不该报错：%v", err)
	}
	if res.Mode != SelectionNone {
		t.Errorf("mode = %s, want none（tools=%v）", res.Mode, res.Tools)
	}
	if res.Report != nil {
		t.Error("没活干时不该执行")
	}
}

func TestProfileNamesSortedAndOptions(t *testing.T) {
	a := initApp(t)
	names, err := a.ProfileNames()
	if err != nil {
		t.Fatalf("ProfileNames: %v", err)
	}
	if strings.Join(names, ",") != "base,tiny" {
		t.Errorf("ProfileNames = %v, want [base tiny]", names)
	}
	opts := profileOptions(names)
	if len(opts) != 2 || opts[0].Value != "base" || opts[0].Display() != "base" {
		t.Errorf("profileOptions = %+v", opts)
	}
}

// snapshotSet 把 snapshot 的切片变成集合，便于比较"有没有新增/消失的文件"。
func snapshotSet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, v := range list {
		out[v] = true
	}
	return out
}

func diffSnapshot(t *testing.T, before map[string]bool, root string) string {
	t.Helper()
	after := snapshotSet(snapshot(t, root))
	var sb strings.Builder
	for path := range after {
		if !before[path] {
			sb.WriteString("created: " + path + "\n")
		}
	}
	for path := range before {
		if !after[path] {
			sb.WriteString("removed: " + path + "\n")
		}
	}
	return sb.String()
}

func countCalls(calls []string, kind string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, kind) {
			n++
		}
	}
	return n
}
