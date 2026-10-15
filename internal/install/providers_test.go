package install

import (
	"context"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/internal/detect"
	"github.com/kingfs/Bootstrap/internal/manifest"
)

// providerManifest 是"命令提供者"推断用的最小清单：
//
//	g (cli) ← golang (runtime, depends_on: [g]) ← gopls (devdep, goinstall 需要 go)
//
// gopls **没有**写 depends_on：它缺的就是"go 这个命令"，而清单里正好有工具是
// 靠 go 验收的（golang）。这正是 `bs tool install gopls` 在一台没装 Go 的机器上
// 该自动补出 golang（再补出 g）的场景。
const providerManifest = `
version: 1
tools:
  - id: g
    stack: golang
    kind: cli
    verify: {command: g}
    install:
      - via: apt
        params: {packages: [golang-go]}
  - id: golang
    stack: golang
    kind: runtime
    verify: {command: go}
    depends_on: [g]
    install:
      - via: manager
        params: {manager: g, action: install, version: latest}
  - id: gopls
    stack: golang
    kind: devdep
    verify: {command: gopls}
    install:
      - via: goinstall
        when: {has: [go]}
        params: {module: golang.org/x/tools/gopls@latest}
`

func stepsByID(plan Plan) map[string]Step {
	out := make(map[string]Step, len(plan.Steps))
	for _, s := range plan.Steps {
		out[s.ToolID] = s
	}
	return out
}

// TestPlanInfersMissingCommandProvider 锁定"只勾 gopls 也要把 golang/g 带上"。
func TestPlanInfersMissingCommandProvider(t *testing.T) {
	env := newFakeEnv(t)
	env.has("apt-get") // g 用 apt 装（测试清单里只是个假命令，不会真的执行）
	loaded := loadManifest(t, providerManifest)
	det := newFakeDetector(map[string]detect.Status{})
	eng := newEngine(t, env, loaded, det, EngineOptions{})

	plan, err := eng.PlanTools(context.Background(), []string{"gopls"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	// 提供者必须先装：顺序由推断出的依赖边决定，不是清单里的排列顺序。
	if got := strings.Join(plan.Order, ","); got != "g,golang,gopls" {
		t.Fatalf("order = %s, want g,golang,gopls", got)
	}
	byID := stepsByID(plan)
	if s := byID["gopls"]; s.Status != StepInstall || s.Via != "goinstall" {
		t.Fatalf("gopls step = %+v", s)
	}
	// golang 走版本管理器；g 这会儿还没装出来，解析 latest 必须推迟到执行前。
	got := byID["golang"]
	if got.Via != "manager" || got.Status != StepInstall {
		t.Fatalf("golang step = %+v", got)
	}
	if got.Action == nil || got.Action.Resolve == nil {
		t.Fatalf("golang 的 latest 解析应推迟到执行前：%+v", got.Action)
	}
	if _, err := ResolveChain(context.Background(), NewRegistry(), env, mustTool(t, loaded, "gopls"), ChainOptions{}); err == nil {
		t.Error("没有 go 时 gopls 的链本来就不该可用（推断发生在 select 阶段）")
	}
}

// TestPlanKeepsUsableMethodWithoutInference：已经有可用方式的工具不补依赖，
// 否则清单里那些"偏好分支"（has: [mise] 之类）会统统变成必装。
func TestPlanKeepsUsableMethodWithoutInference(t *testing.T) {
	env := newFakeEnv(t)
	env.has("apt-get")
	loaded := loadManifest(t, providerManifest+`
  - id: shfmt
    stack: base
    kind: cli
    verify: {command: shfmt}
    install:
      - via: apt
        params: {packages: [shfmt]}
      - via: goinstall
        when: {has: [go]}
        params: {module: mvdan.cc/sh/v3/cmd/shfmt@latest}
`)
	eng := newEngine(t, env, loaded, newFakeDetector(map[string]detect.Status{}), EngineOptions{})
	plan, err := eng.PlanTools(context.Background(), []string{"shfmt"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	if got := strings.Join(plan.Order, ","); got != "shfmt" {
		t.Fatalf("order = %s, want shfmt（apt 已经可用，不该补 golang）", got)
	}
}

// TestPlanDoesNotInferCrossStackProvider：跨 stack 的"提供者"不算依赖。
//
// fnm(nodejs) 的 cargo 方式缺 cargo，而 rust(rust) 正好靠 cargo 验收——
// 但为了一个 Node 版本管理器把整条 Rust 工具链拉进来太意外，清单作者要用就该写
// depends_on。所以这里必须保持"fnm 装不了"，而不是悄悄多装 rust。
func TestPlanDoesNotInferCrossStackProvider(t *testing.T) {
	env := newFakeEnv(t)
	env.has("apt-get")
	loaded := loadManifest(t, `
version: 1
tools:
  - id: rust
    stack: rust
    kind: runtime
    verify: {command: cargo}
    install:
      - via: apt
        params: {packages: [rustup], allow_system: true}
  - id: fnm
    stack: nodejs
    kind: versionmgr
    verify: {command: fnm}
    install:
      - via: cargo
        when: {has: [cargo]}
        params: {crate: fnm}
`)
	eng := newEngine(t, env, loaded, newFakeDetector(map[string]detect.Status{}), EngineOptions{})
	plan, err := eng.PlanTools(context.Background(), []string{"fnm"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	if got := strings.Join(plan.Order, ","); got != "fnm" {
		t.Fatalf("order = %s, want fnm（不得跨 stack 补 rust）", got)
	}
	if s := stepsByID(plan)["fnm"]; s.Status != StepFailed {
		t.Fatalf("fnm 应如实报告没有可用方式：%+v", s)
	}
}

// TestDeferredManagerResolvesAtApplyTime：执行到该步时管理器已经装好了，
// 占位动作要被展开成"用真实版本号"的命令。
func TestDeferredManagerResolvesAtApplyTime(t *testing.T) {
	env := newFakeEnv(t)
	env.has("apt-get")
	loaded := loadManifest(t, providerManifest)
	det := newFakeDetector(map[string]detect.Status{"g": detect.StatusMissing, "go": detect.StatusMissing})
	eng := newEngine(t, env, loaded, det, EngineOptions{})

	plan, err := eng.PlanTools(context.Background(), []string{"golang"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	if plan.Steps[1].Action == nil || plan.Steps[1].Action.Resolve == nil {
		t.Fatalf("计划期 g 还不存在，latest 解析应被推迟：%+v", plan.Steps[1])
	}

	// 模拟"g 那一步真的把 g 装出来了"，并且 list-remote 能报出版本。
	env.has("g")
	env.runOut["g"] = "1.21.0\n1.24.1\n"
	det.status["g"] = detect.StatusInstalled
	det.status["go"] = detect.StatusInstalled

	report, err := eng.Apply(context.Background(), plan, OpInstall)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, s := range report.Steps {
		if s.Status != StepDone && s.Status != StepSkip {
			t.Fatalf("%s status = %s (%s)", s.ToolID, s.Status, s.Error)
		}
	}
	lines := env.commandLines()
	if !containsLine(lines, "g install 1.24.1") {
		t.Fatalf("应把 latest 解析成具体版本再装：%v", lines)
	}
	for _, s := range report.Steps {
		if s.ToolID == "golang" && strings.Contains(s.Summary, "latest") {
			t.Errorf("执行后摘要应带真实版本：%q", s.Summary)
		}
	}
}

// TestPlanInferenceSkippedForUninstall：卸载不因为"缺命令"补依赖，也不多卸东西。
func TestPlanInferenceSkippedForUninstall(t *testing.T) {
	env := newFakeEnv(t)
	env.has("apt-get")
	loaded := loadManifest(t, providerManifest)
	det := newFakeDetector(map[string]detect.Status{"gopls": detect.StatusInstalled})
	eng := newEngine(t, env, loaded, det, EngineOptions{})
	plan, err := eng.PlanTools(context.Background(), []string{"gopls"}, OpUninstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	if got := strings.Join(plan.Order, ","); got != "gopls" {
		t.Fatalf("order = %s, want gopls（卸载不该补装 golang/g）", got)
	}
}

func mustTool(t *testing.T, l *manifest.Loaded, id string) manifest.Tool {
	t.Helper()
	tool, ok := l.ByID(id)
	if !ok {
		t.Fatalf("manifest has no tool %q", id)
	}
	return tool
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
