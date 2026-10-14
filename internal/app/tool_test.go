package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/internal/install"
	"github.com/kingfs/Bootstrap/internal/ui"
)

// toolSandboxApp 构造一个"只从临时用户目录加载清单"的 App，让用例与内置清单内容解耦。
func toolSandboxApp(t *testing.T, manifestBody string) (*App, string) {
	t.Helper()
	a, root := newTestApp(t)
	userDir := filepath.Join(root, "config", "tools")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "tools.yaml"), []byte(manifestBody), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	a.Cfg.Tool.Manifest = config.ManifestConfig{Builtin: false, UserDirs: []string{userDir}}
	a.Cfg.Tool.Versions = map[string]string{"golang": "1.24.1"}
	// PATH 指向空目录：探测结果只由清单决定，不受宿主机上装了什么的干扰。
	bin := filepath.Join(root, "emptybin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("PATH", bin)
	return a, bin
}

// addFakeBin 往沙箱 PATH 里放一个假命令（POSIX shell 脚本），
// 让"某个安装方式是否可用"完全由用例决定。
func addFakeBin(t *testing.T, binDir, name, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("假命令用 POSIX shell 脚本实现")
	}
	path := filepath.Join(binDir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

const testManifest = `
version: 1
tools:
  - id: g
    stack: golang
    kind: versionmgr
    description: Go 版本管理器
    tags: [golang]
    verify: {command: g}
    install:
      - via: brew
        params: {formula: g}
  - id: golang
    stack: golang
    kind: runtime
    description: Go 工具链
    tags: [golang, runtime]
    depends_on: [g, helper]
    verify: {command: go, version_args: [version], version_regex: 'go(?P<v>\d+\.\d+(?:\.\d+)?)', min_version: "1.22"}
    install:
      - via: manager
        params: {action: install, version: "{{versions.golang}}"}
  - id: helper
    stack: base
    kind: cli
    description: 辅助工具（apt 提供）
    verify: {command: helper}
    install:
      - via: brew
        when: {os: [darwin]}
        params: {formula: helper}
      - via: apt
        when: {os: [linux], distro: [ubuntu, debian]}
        params: {packages: [helper]}
  - id: btop
    stack: base
    kind: cli
    description: 监视器
    tags: [monitor]
    verify: {command: btop}
    install:
      - via: brew
        params: {formula: btop}
  - id: legacy
    stack: base
    kind: cli
    description: 已禁用
    disabled: true
    verify: {command: legacy}
    install:
      - via: brew
        params: {formula: legacy}
  - id: darwin-only
    stack: base
    kind: cli
    description: 只在 macOS
    when: {os: [darwin]}
    verify: {command: donly}
    install:
      - via: brew
        params: {formula: donly}
profiles:
  base:
    tools: [g, golang]
  extra:
    extends: [base]
    tools: [btop]
`

// TestToolRowsFilters 覆盖列表的全部过滤维度（这是用户最常用的命令）。
func TestToolRowsFilters(t *testing.T) {
	a, _ := toolSandboxApp(t, testManifest)
	ctx := context.Background()

	base := ToolListOptions{NoProbe: true}
	rows, err := a.ToolRows(ctx, base)
	if err != nil {
		t.Fatalf("ToolRows: %v", err)
	}
	ids := rowIDs(rows)
	// 默认隐藏 disabled 与本机不适用（darwin-only）的条目
	if !equalStrings(ids, []string{"g", "golang", "helper", "btop"}) {
		t.Fatalf("ids = %v", ids)
	}

	all, _ := a.ToolRows(ctx, ToolListOptions{NoProbe: true, All: true})
	if !equalStrings(rowIDs(all), []string{"g", "golang", "helper", "btop", "legacy", "darwin-only"}) {
		t.Fatalf("all ids = %v", rowIDs(all))
	}
	for _, r := range all {
		if r.ID == "darwin-only" && !r.NotMatching {
			t.Error("darwin-only should be marked NotMatching on linux")
		}
	}

	cases := []struct {
		name string
		opt  ToolListOptions
		want []string
	}{
		{"stack", ToolListOptions{NoProbe: true, Stack: "base"}, []string{"helper", "btop"}},
		{"kind", ToolListOptions{NoProbe: true, Kind: "versionmgr"}, []string{"g"}},
		{"tag", ToolListOptions{NoProbe: true, Tags: []string{"runtime"}}, []string{"golang"}},
		{"search", ToolListOptions{NoProbe: true, Search: "监视"}, []string{"btop"}},
		{"profile", ToolListOptions{NoProbe: true, Profile: "extra"}, []string{"g", "golang", "btop"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := a.ToolRows(ctx, tc.opt)
			if err != nil {
				t.Fatalf("ToolRows: %v", err)
			}
			if !equalStrings(rowIDs(rows), tc.want) {
				t.Errorf("ids = %v, want %v", rowIDs(rows), tc.want)
			}
		})
	}

	// 未知 profile 要报错（提示可用名字），而不是静默返回空
	if _, err := a.ToolRows(ctx, ToolListOptions{NoProbe: true, Profile: "nope"}); err == nil {
		t.Error("unknown profile must fail")
	}
}

// TestToolRowsPrefersMachineApplicableMethod：Linux 上不该显示 macOS 才用的 brew。
func TestToolRowsPrefersMachineApplicableMethod(t *testing.T) {
	a, _ := toolSandboxApp(t, `
version: 1
tools:
  - id: demo
    stack: base
    kind: cli
    verify: {command: demo}
    install:
      - via: brew
        when: {os: [darwin]}
        params: {formula: demo}
      - via: apt
        when: {os: [linux], distro: [ubuntu, debian]}
        params: {packages: [demo]}
`)
	rows, err := a.ToolRows(context.Background(), ToolListOptions{NoProbe: true})
	if err != nil {
		t.Fatalf("ToolRows: %v", err)
	}
	if len(rows) != 1 || rows[0].ViaDisplay != "apt" {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestToolInfoUnknownAndChain：未知 id 报 UnknownToolError；已知 id 能看到完整方法链。
func TestToolInfoUnknownAndChain(t *testing.T) {
	a, _ := toolSandboxApp(t, testManifest)
	if _, err := a.ToolInfo(context.Background(), "nope", false); err == nil {
		t.Fatal("unknown tool must fail")
	}
	d, err := a.ToolInfo(context.Background(), "golang", false)
	if err != nil {
		t.Fatalf("ToolInfo: %v", err)
	}
	if len(d.Methods) != 1 {
		t.Fatalf("methods = %+v", d.Methods)
	}
	if d.Methods[0].Via != "manager" || d.Methods[0].Display != "manager:g" {
		t.Errorf("method = %+v", d.Methods[0])
	}
	if d.Verify.MinVersion != "1.22" {
		t.Errorf("verify = %+v", d.Verify)
	}
	if !equalStrings(d.DependsOn, []string{"g", "helper"}) {
		t.Errorf("depends_on = %v", d.DependsOn)
	}
	// 被依赖关系是反查出来的
	g, err := a.ToolInfo(context.Background(), "g", false)
	if err != nil {
		t.Fatalf("ToolInfo(g): %v", err)
	}
	if !equalStrings(g.Dependents, []string{"golang"}) {
		t.Errorf("dependents = %v", g.Dependents)
	}
}

// TestToolPlanResolvesDepsAndVia：依赖按拓扑序展开，且 --via 只约束"显式点名的工具"。
func TestToolPlanResolvesDepsAndVia(t *testing.T) {
	a, binDir := toolSandboxApp(t, testManifest)
	// g 与 apt-get 存在 → 两种"可用方式"都能被链解析到；go 报 1.20 低于 min_version
	// → golang 视为未达标，计划里才会有真正的动作。
	addFakeBin(t, binDir, "g", "#!/bin/sh\necho 1.24.1\n")
	addFakeBin(t, binDir, "apt-get", "#!/bin/sh\nexit 1\n")
	addFakeBin(t, binDir, "go", "#!/bin/sh\necho 'go version go1.20.0 linux/amd64'\n")

	plan, err := a.ToolPlan(context.Background(), []string{"golang"}, "", install.OpInstall)
	if err != nil {
		t.Fatalf("ToolPlan: %v", err)
	}
	var order []string
	for _, s := range plan.Steps {
		order = append(order, s.ToolID)
	}
	if !equalStrings(order, []string{"g", "helper", "golang"}) {
		t.Fatalf("order = %v", order)
	}

	// --via manager：显式的 golang 走 manager；依赖 g 不在显式集合里，
	// 仍按自己的清单声明解析（brew），不受 --via 影响。
	a.DefaultViaOverride = "manager"
	plan, err = a.ToolPlan(context.Background(), []string{"golang"}, "", install.OpInstall)
	if err != nil {
		t.Fatalf("ToolPlan: %v", err)
	}
	via := map[string]string{}
	for _, s := range plan.Steps {
		via[s.ToolID] = s.Via
	}
	if via["golang"] != "manager" {
		t.Errorf("explicit tool should use --via: %v", via)
	}
	if via["helper"] != "apt" {
		t.Errorf("dependency must keep its own methods: %v", via)
	}

	// --via 指向该工具根本没有的方法 → 明确报错（不静默回落）
	if _, err := a.ToolPlan(context.Background(), []string{"helper"}, "manager", install.OpInstall); err == nil {
		t.Fatal("unknown via for explicit tool must fail")
	}
}

// TestToolRowsMissingFilterNeedsProbe：--missing / --installed 依赖探测结果。
func TestToolRowsMissingFilterNeedsProbe(t *testing.T) {
	a, _ := toolSandboxApp(t, testManifest)
	// 沙箱里的 lookPath 找不到任何命令 → 全部 missing
	rows, err := a.ToolRows(context.Background(), ToolListOptions{Missing: true})
	if err != nil {
		t.Fatalf("ToolRows: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("missing rows = %v", rowIDs(rows))
	}
	installed, err := a.ToolRows(context.Background(), ToolListOptions{Installed: true})
	if err != nil {
		t.Fatalf("ToolRows: %v", err)
	}
	if len(installed) != 0 {
		t.Errorf("installed rows = %v", rowIDs(installed))
	}
}

// TestManifestDiffAndUserDirs：diff 只报告"会影响安装结果"的差异。
func TestManifestDiffAndUserDirs(t *testing.T) {
	a, _ := toolSandboxApp(t, testManifest)
	dirs := a.ManifestUserDirs()
	if len(dirs) != 1 {
		t.Fatalf("dirs = %v", dirs)
	}
	draft := filepath.Join(filepath.Dir(dirs[0]), "draft.yaml")
	body := `
version: 1
tools:
  - id: golang
    stack: rust
    kind: runtime
    verify: {command: go}
    install:
      - via: manager
        params: {action: install, version: "1.80"}
  - id: brand-new
    stack: base
    kind: cli
    verify: {command: brandnew}
    install:
      - via: brew
        params: {formula: brandnew}
`
	if err := os.WriteFile(draft, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	diff, err := a.ManifestDiff(context.Background(), draft)
	if err != nil {
		t.Fatalf("ManifestDiff: %v", err)
	}
	if len(diff.Added) != 1 || diff.Added[0] != "brand-new" {
		t.Errorf("added = %v", diff.Added)
	}
	if len(diff.Changed) != 1 || diff.Changed[0].ID != "golang" {
		t.Fatalf("changed = %+v", diff.Changed)
	}
	for _, want := range []string{"stack", "install[0].params"} {
		if !strings.Contains(diff.Changed[0].Detail, want) {
			t.Errorf("detail = %q, want %q", diff.Changed[0].Detail, want)
		}
	}
}

func rowIDs(rows []ToolRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pickTestManifest 覆盖勾选逻辑需要的形状：正常条目 + 仅某平台 + disabled。
const pickTestManifest = `
version: 1
tools:
  - id: g
    stack: golang
    kind: versionmgr
    description: Go 版本管理器
    verify: {command: g}
    install: [{via: brew, params: {formula: g}}]
  - id: golang
    stack: golang
    kind: runtime
    description: Go 工具链
    verify: {command: go}
    install: [{via: manager, params: {action: install, version: "1.24.1"}}]
  - id: helper
    stack: base
    kind: cli
    description: 辅助工具
    verify: {command: helper}
    install: [{via: apt, params: {packages: [helper], allow_system: true}}]
  - id: win-only
    stack: base
    kind: cli
    description: 只在 Windows
    when: {os: [windows]}
    verify: {command: win-only}
    install: [{via: brew, params: {formula: win-only}}]
  - id: off
    stack: base
    kind: cli
    description: 停用的条目
    disabled: true
    verify: {command: off}
    install: [{via: brew, params: {formula: off}}]
`

// TestPickToolsPreselectsAndFilters：勾选列表的候选与默认勾选规则。
//
// 这里锁定三条：disabled / 本机不适用的条目不进候选；"非已安装"默认勾选；
// 注入的 PreSelected 能覆盖默认规则（`-i` 与 `bs init` 共用同一实现）。
func TestPickToolsPreselectsAndFilters(t *testing.T) {
	a, bin := toolSandboxApp(t, pickTestManifest)
	// 让 helper 已安装：它就不该被默认勾选。
	addFakeBin(t, bin, "helper", "#!/bin/sh\necho helper 1.0\n")

	f := &fakePrompter{interactive: true, multis: [][]string{{"golang", "g"}}}
	got, err := a.PickTools(context.Background(), ToolListOptions{}, f, PickOptions{Title: "选点什么"})
	if err != nil {
		t.Fatalf("PickTools: %v", err)
	}
	if len(got) != 2 || got[0] != "golang" {
		t.Errorf("PickTools = %v，期望原样返回勾选结果", got)
	}
	if len(f.multiSeen) != 1 {
		t.Fatalf("应当只问一次，实际 %d 次", len(f.multiSeen))
	}
	seen := map[string]ui.Option{}
	for _, o := range f.multiSeen[0].Options {
		seen[o.Value] = o
	}
	for _, banned := range []string{"win-only", "off"} {
		if _, ok := seen[banned]; ok {
			t.Errorf("%s 不该出现在候选里（禁用/不适用）", banned)
		}
	}
	if o := seen["golang"]; !o.Selected {
		t.Error("缺失的工具应当默认勾选")
	}
	if o := seen["helper"]; o.Selected {
		t.Error("已安装的工具不该默认勾选")
	}
	if !strings.Contains(f.multiSeen[0].Title, "选点什么") {
		t.Errorf("标题没有被采用：%q", f.multiSeen[0].Title)
	}
	if f.multiSeen[0].MinSelected != 1 {
		t.Errorf("MinSelected = %d，期望 1（不允许空选）", f.multiSeen[0].MinSelected)
	}

	// PreSelected 覆盖默认规则：这次只勾"已安装"的（升级场景）。
	f2 := &fakePrompter{interactive: true, multis: [][]string{{"helper"}}}
	if _, err := a.PickTools(context.Background(), ToolListOptions{}, f2, PickOptions{
		PreSelected: func(r ToolRow) bool { return r.Status == "installed" },
	}); err != nil {
		t.Fatalf("PickTools: %v", err)
	}
	seen2 := map[string]ui.Option{}
	for _, o := range f2.multiSeen[0].Options {
		seen2[o.Value] = o
	}
	if !seen2["helper"].Selected || seen2["golang"].Selected {
		t.Error("PreSelected 应当覆盖默认勾选规则")
	}
}

// TestPickToolsNonInteractiveRefuses：非交互通道必须直接把"问不了"传上来，
// 由命令层映射成退出码 5——绝不能返回空列表让上层以为"用户什么都没选"。
func TestPickToolsNonInteractiveRefuses(t *testing.T) {
	a, _ := toolSandboxApp(t, pickTestManifest)
	if _, err := a.PickTools(context.Background(), ToolListOptions{}, ui.NonInteractive{}, PickOptions{}); err == nil {
		t.Fatal("非交互时应当返回错误")
	}
}

// TestPickToolsNoApplicable：候选为空时给出明确错误（而不是弹一个空列表）。
func TestPickToolsNoApplicable(t *testing.T) {
	a, _ := toolSandboxApp(t, `
version: 1
tools:
  - id: win-only
    stack: base
    kind: cli
    when: {os: [windows]}
    verify: {command: win-only}
    install: [{via: brew, params: {formula: win-only}}]
`)
	f := &fakePrompter{interactive: true}
	if _, err := a.PickTools(context.Background(), ToolListOptions{}, f, PickOptions{}); err == nil {
		t.Fatal("没有候选时应当报错")
	}
	if len(f.multiSeen) != 0 {
		t.Error("没有候选时不该弹界面")
	}
}
