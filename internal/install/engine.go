package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kingfs/Bootstrap/internal/detect"
	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"gopkg.in/yaml.v3"
)

// 本文件是安装引擎的流水线（docs/design.md §7.5）：
//
//	Preflight → Select → Plan → Confirm → Apply（拓扑序）→ Verify → Report
//
// Plan 与 Apply 严格分开：`bs tool plan` 只跑前三步，`--dry-run` 跑到 Apply 前停下。

// EngineOptions 是 Engine 的构造参数。
type EngineOptions struct {
	Registry *Registry
	Env      ExecEnv
	Loaded   *manifest.Loaded
	Detector *detect.Detector
	// Managers 是 stack → 默认版本管理器（补全 `via: manager`）。
	Managers map[string]string
	// DefaultVia 是 tool.default_via（显式指定安装方式）。
	DefaultVia string
	// Fallback 为真时，选中方法失败会按顺序换下一个候选（ADR-0013）。
	Fallback bool
	// Versions 是配置里的 tool.versions（模板变量 {{versions.x}} 与版本钉选用）。
	Versions map[string]string
	// Recorder 记录安装结果（可空，测试时不需要落盘）。
	Recorder *Recorder
	// LogDir 非空时，每个工具的命令输出会留一份日志（<LogDir>/logs/...）。
	LogDir string
	// Now 便于测试注入时间。
	Now func() time.Time
}

// Engine 负责把清单变成一次可预览、可执行的安装。
type Engine struct {
	reg      *Registry
	env      ExecEnv
	loaded   *manifest.Loaded
	detector *detect.Detector
	opt      EngineOptions
}

// NewEngine 构造安装引擎。
func NewEngine(opt EngineOptions) (*Engine, error) {
	if opt.Registry == nil {
		opt.Registry = NewRegistry()
	}
	if opt.Env == nil {
		return nil, errors.New("install: ExecEnv is required")
	}
	if opt.Loaded == nil {
		return nil, errors.New("install: manifest is required")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Engine{reg: opt.Registry, env: opt.Env, loaded: opt.Loaded, detector: opt.Detector, opt: opt}, nil
}

// Registry 暴露给调用方（doctor / manifest validate 也要用）。
func (e *Engine) Registry() *Registry { return e.reg }

// StepStatus 是一条计划步骤的处置结果。
type StepStatus string

// 步骤状态。
const (
	StepInstall     StepStatus = "install"
	StepUpgrade     StepStatus = "upgrade"
	StepSkip        StepStatus = "skip"
	StepUninstall   StepStatus = "uninstall"
	StepUnsupported StepStatus = "unsupported"
	StepFailed      StepStatus = "failed"
	StepDone        StepStatus = "done"
)

// Step 是一个工具的计划（或执行结果）。
type Step struct {
	ToolID    string         `json:"tool_id"`
	Name      string         `json:"name,omitempty"`
	Stack     string         `json:"stack,omitempty"`
	Kind      string         `json:"kind,omitempty"`
	Status    StepStatus     `json:"status"`
	Via       string         `json:"via,omitempty"`
	Manager   string         `json:"manager,omitempty"`
	Summary   string         `json:"summary,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	Action    *Action        `json:"action,omitempty"`
	Chain     Chain          `json:"-"`
	Detection *detect.Result `json:"detection,omitempty"`
	Skipped   []Candidate    `json:"skipped,omitempty"`
	// LogPath 是这个工具完整命令输出的日志文件（未写日志时为空）。
	LogPath        string        `json:"log_path,omitempty"`
	FallbackFrom   string        `json:"fallback_from,omitempty"`
	FallbackReason string        `json:"fallback_reason,omitempty"`
	Error          string        `json:"error,omitempty"`
	Duration       time.Duration `json:"-"`
	TriedMethods   []string      `json:"tried_methods,omitempty"`
}

// Plan 是一次完整安装计划。
type Plan struct {
	Steps []Step `json:"steps"`
	// DryRun 表示这只是预览。
	DryRun bool `json:"dry_run"`
	// Order 是拓扑序后的工具 id 顺序（便于测试与展示）。
	Order []string `json:"order"`
	// Problems 是预检查发现的问题（缺失依赖等）。
	Problems []string `json:"problems,omitempty"`
}

// Report 是一次执行的汇总。
type Report struct {
	Plan    Plan          `json:"plan"`
	Steps   []Step        `json:"steps"`
	Elapsed time.Duration `json:"-"`
	// Fallbacks 记录所有发生过降级的工具。
	Fallbacks []FallbackNote `json:"fallbacks,omitempty"`
}

// FallbackNote 记录一次降级（§7.2 要求显著标注）。
type FallbackNote struct {
	ToolID string `json:"tool_id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// Failed 返回非零退出需要的错误（把所有失败原因聚合）。
func (r Report) Failed() error {
	var errs []error
	for _, s := range r.Steps {
		if s.Status == StepFailed {
			errs = append(errs, fmt.Errorf("%s (%s): %s", s.ToolID, s.Via, s.Error))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// Changed 报告是否真的改动了系统（用于判断是否需要提示重启 shell）。
func (r Report) Changed() bool {
	for _, s := range r.Steps {
		switch s.Status {
		case StepDone, StepInstall, StepUpgrade, StepUninstall:
			return true
		}
	}
	return false
}

// PlanTools 计算安装计划（前四步，无副作用）。
func (e *Engine) PlanTools(ctx context.Context, ids []string, op Op) (Plan, error) {
	tools, explicit, err := e.selectTools(ids)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{DryRun: e.env.DryRun()}

	// 显式 --via 先按清单校验一遍：`bs tool install git --via nosuch` 不能因为
	// git 已安装、该步被跳过就悄悄返回 0——用户显然是想指定某种安装方式。
	if e.opt.DefaultVia != "" {
		for _, t := range tools {
			if !explicit[t.ID] {
				continue
			}
			methods := methodsForOp(t, op)
			if !DeclaresVia(methods, e.opt.DefaultVia) {
				return Plan{}, &ErrMethodNotFound{
					ToolID: t.ID, Via: e.opt.DefaultVia, Known: DeclaredVias(methods),
				}
			}
		}
	}

	// Preflight：依赖是否齐备（依赖不在本次集合里时，按"未安装"处理并提示）。
	missingDeps := map[string]bool{}
	for _, t := range tools {
		for _, dep := range t.DependsOn {
			found := false
			for _, other := range tools {
				if other.ID == dep {
					found = true
					break
				}
			}
			if !found {
				missingDeps[t.ID] = true
				plan.Problems = append(plan.Problems, fmt.Sprintf("%s 依赖 %s，但 %s 不在本次选择中（可能已安装）", t.ID, dep, dep))
			}
		}
	}

	// 探测：装没装、什么版本。跳过安装决策前必须强制重探（ADR-0011）。
	detections := map[string]detect.Result{}
	if e.detector != nil {
		// 安装前的跳过判断必须基于真实状态，所以强制重探；
		// 卸载/升级只用于展示与"没装就不用卸"，用缓存即可。
		results, derr := e.detector.Detect(ctx, tools, detect.Options{
			UseCache: !e.env.DryRun(),
			Refresh:  op == OpInstall && !e.env.DryRun(),
		})
		if derr != nil {
			return plan, fmt.Errorf("detect installed tools: %w", derr)
		}
		detections = results
	}

	for _, t := range tools {
		step := Step{ToolID: t.ID, Name: t.Name, Stack: t.Stack.String(), Kind: string(t.Kind)}
		if d, ok := detections[t.ID]; ok {
			dd := d
			step.Detection = &dd
		}
		switch op {
		case OpInstall:
			if d, ok := detections[t.ID]; ok && d.Status == detect.StatusInstalled {
				step.Status = StepSkip
				step.Reason = "已安装：" + d.Label()
				plan.Steps = append(plan.Steps, step)
				plan.Order = append(plan.Order, t.ID)
				continue
			}
		case OpUpgrade:
			// 升级不跳过：交给各自的包管理器判断。
		case OpUninstall:
			// 卸载只针对已安装的工具，未安装的直接跳过。
			if d, ok := detections[t.ID]; ok && d.Status == detect.StatusMissing {
				step.Status = StepSkip
				step.Reason = "未安装，无需卸载"
				plan.Steps = append(plan.Steps, step)
				plan.Order = append(plan.Order, t.ID)
				continue
			}
		}

		// --via 只作用于用户显式点名的工具：自动补进来的依赖必须按它自己的
		// 声明安装（否则 `install golang --via apt` 会连带把版本管理器也塞给 apt）。
		via := ""
		if explicit[t.ID] {
			via = e.opt.DefaultVia
		}
		// 方法链按操作类型取：升级/卸载可以各自声明方法，没声明就复用 install 链
		// （多数安装器能从 op 推导出对应命令，例如 brew install ↔ brew uninstall）。
		chainTool := t
		chainTool.Install = methodsForOp(t, op)
		chain, cerr := ResolveChain(ctx, e.reg, e.env, chainTool, ChainOptions{Via: via, Managers: e.opt.Managers})
		step.Chain = chain
		step.Skipped = chain.Skipped()
		// --via / tool.default_via 指定错了属于用户输入问题，必须整体失败，
		// 而不是悄悄降级成"这个工具装不了"的一行表格。
		var viaErr *ErrViaUnavailable
		var notFound *ErrMethodNotFound
		if errors.As(cerr, &viaErr) || errors.As(cerr, &notFound) {
			return Plan{}, cerr
		}
		if cerr != nil {
			step.Status = StepFailed
			step.Error = cerr.Error()
			plan.Steps = append(plan.Steps, step)
			plan.Order = append(plan.Order, t.ID)
			continue
		}
		cand, _ := chain.SelectedCandidate()
		step.Via = cand.Via
		step.Manager = cand.Method.Params.Manager
		in, _ := e.reg.Get(cand.Via)
		action, aerr := in.Plan(ctx, e.env, chainTool, cand.Method, op)
		if aerr != nil {
			if _, unsupported := aerr.(*ErrUnsupported); unsupported {
				step.Status = StepUnsupported
				step.Reason = aerr.Error()
			} else {
				step.Status = StepFailed
				step.Error = aerr.Error()
			}
			plan.Steps = append(plan.Steps, step)
			plan.Order = append(plan.Order, t.ID)
			continue
		}
		step.Action = &action
		step.Summary = action.Summary
		step.Reason = action.Reason
		if op == OpUninstall {
			step.Status = StepUninstall
		} else if op == OpUpgrade {
			step.Status = StepUpgrade
		} else {
			step.Status = StepInstall
		}
		plan.Steps = append(plan.Steps, step)
		plan.Order = append(plan.Order, t.ID)
	}
	return plan, nil
}

// methodsForOp 选出该操作要用的方法链。
//
// tool.update / tool.uninstall 是可选的显式声明：写了就用它，
// 没写就复用 install 链（安装器会按 op 换成对应的卸载/升级命令）。
func methodsForOp(t manifest.Tool, op Op) []manifest.Method {
	switch op {
	case OpUpgrade:
		if t.Update != nil {
			return []manifest.Method{*t.Update}
		}
	case OpUninstall:
		if len(t.Uninstall) > 0 {
			return t.Uninstall
		}
	}
	return t.Install
}

// Apply 执行计划。
func (e *Engine) Apply(ctx context.Context, plan Plan, op Op) (Report, error) {
	start := time.Now()
	report := Report{Plan: plan}
	if e.env.DryRun() {
		// 干跑：只回显计划，一行都不执行（§8.5 安全红线）。
		for _, s := range plan.Steps {
			s.Status = dryRunStatus(s.Status)
			report.Steps = append(report.Steps, s)
		}
		report.Elapsed = time.Since(start)
		return report, nil
	}

	// 按拓扑序执行（计划里的顺序已经是拓扑序，这里再确认一次）。
	order := map[string]int{}
	for i, id := range plan.Order {
		order[id] = i
	}
	steps := append([]Step{}, plan.Steps...)
	sort.SliceStable(steps, func(i, j int) bool { return order[steps[i].ToolID] < order[steps[j].ToolID] })

	for _, s := range steps {
		if s.Status != StepInstall && s.Status != StepUpgrade && s.Status != StepUninstall {
			report.Steps = append(report.Steps, s)
			continue
		}
		stepStart := time.Now()
		out, err := e.applyStep(ctx, s, op)
		out.Duration = time.Since(stepStart)
		if err != nil {
			stepErr := err
			// 降级：按顺序再试剩下的候选（ADR-0013）。
			if e.opt.Fallback {
				if fb, ferr := e.tryFallbacks(ctx, s, op); ferr == nil {
					report.Fallbacks = append(report.Fallbacks, FallbackNote{
						ToolID: s.ToolID, From: s.Via, To: fb.Via, Reason: stepErr.Error(),
					})
					fb.FallbackFrom = s.Via
					fb.FallbackReason = stepErr.Error()
					fb.TriedMethods = append([]string{s.Via}, fb.TriedMethods...)
					fb.Duration = time.Since(stepStart)
					report.Steps = append(report.Steps, fb)
					continue
				} else {
					out.Status = StepFailed
					out.Error = errors.Join(stepErr, ferr).Error()
				}
			} else {
				out.Status = StepFailed
				out.Error = stepErr.Error()
			}
		}
		report.Steps = append(report.Steps, out)
	}
	report.Elapsed = time.Since(start)

	if e.opt.Recorder != nil {
		if err := e.opt.Recorder.Record(report, e.opt.Now()); err != nil {
			return report, fmt.Errorf("record install result: %w", err)
		}
	}
	// 部分失败也要把 Report 交回给调用方（CLI 需要按步骤渲染），
	// 同时用聚合后的错误表达"哪些没装上"（退出码 1/3）。
	return report, report.Failed()
}

// stepEnv 为一次工具安装准备"带日志的执行环境"（docs/design.md §7.5 的日志与审计）。
//
// 返回的 finishLog 一定要在 Execute 之后调用：它负责关文件并给出日志路径
// （什么都没写时返回空串）。日志本身出问题不该让安装失败，只记警告。
func (e *Engine) stepEnv(toolID string) (ExecEnv, func() string) {
	if e.opt.LogDir == "" || e.env.DryRun() {
		return e.env, func() string { return "" }
	}
	path := LogPath(e.opt.LogDir, toolID, e.opt.Now())
	if err := fsx.EnsureDir(filepath.Dir(path), 0o755); err != nil {
		e.env.Log().Warn("cannot create log dir", "path", path, "err", err)
		return e.env, func() string { return "" }
	}
	l := &logEnv{ExecEnv: e.env, path: path}
	return l, func() string { p := l.Path(); l.close(); return p }
}

// applyStep 执行一个步骤并做安装后验证。
func (e *Engine) applyStep(ctx context.Context, s Step, op Op) (Step, error) {
	out := s
	if s.Action == nil {
		return out, fmt.Errorf("no action planned")
	}
	in, ok := e.reg.Get(s.Via)
	if !ok {
		return out, &UnknownViaError{Via: s.Via}
	}
	log := e.env.Log()
	log.Info("apply", "tool", s.ToolID, "via", s.Via, "op", string(op))

	// 每条命令的完整输出留一份日志（无副作用路径：干跑不写）。
	env, finishLog := e.stepEnv(s.ToolID)
	err := in.Execute(ctx, *s.Action, env)
	out.LogPath = finishLog()
	if err != nil {
		out.Status = StepFailed
		out.Error = err.Error()
		return out, err
	}
	// Verify：装完立刻用真实探测确认真的是装上了（而不是相信命令退出码）。
	if op != OpUninstall && e.detector != nil {
		tool, ok := e.loaded.ByID(s.ToolID)
		if ok {
			res := e.detector.Probe(ctx, tool)
			out.Detection = &res
			if res.Status == detect.StatusMissing {
				out.Status = StepFailed
				out.Error = "命令已执行，但验证失败：仍然找不到 " + tool.Verify.Command
				return out, errors.New(out.Error)
			}
		}
	}
	out.Status = StepDone
	return out, nil
}

// tryFallbacks 依次尝试链里其余候选。
func (e *Engine) tryFallbacks(ctx context.Context, s Step, op Op) (Step, error) {
	tool, ok := e.loaded.ByID(s.ToolID)
	if !ok {
		return s, fmt.Errorf("unknown tool %s", s.ToolID)
	}
	var errs []error
	out := s
	out.TriedMethods = append(out.TriedMethods, s.Via)
	for _, cand := range s.Chain.Fallbacks() {
		in, ok := e.reg.Get(cand.Via)
		if !ok || !in.Available(ctx, e.env, tool, cand.Method) {
			errs = append(errs, fmt.Errorf("%s: unavailable", cand.Via))
			out.TriedMethods = append(out.TriedMethods, cand.Via)
			continue
		}
		action, err := in.Plan(ctx, e.env, tool, cand.Method, op)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", cand.Via, err))
			out.TriedMethods = append(out.TriedMethods, cand.Via)
			continue
		}
		out.Via = cand.Via
		out.Action = &action
		out.Summary = action.Summary
		out.Reason = action.Reason
		if err := in.Execute(ctx, action, e.env); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", cand.Via, err))
			out.TriedMethods = append(out.TriedMethods, cand.Via)
			continue
		}
		if e.detector != nil && op != OpUninstall {
			res := e.detector.Probe(ctx, tool)
			out.Detection = &res
			if res.Status == detect.StatusMissing {
				errs = append(errs, fmt.Errorf("%s: verify failed", cand.Via))
				out.TriedMethods = append(out.TriedMethods, cand.Via)
				continue
			}
		}
		out.Status = StepDone
		out.Error = ""
		return out, nil
	}
	if len(errs) == 0 {
		errs = append(errs, errors.New("no alternative method"))
	}
	return out, errors.Join(errs...)
}

// normalize 把"配置里的管理器选择"落到清单上（§6.2：清单无需分叉）。
//
// 两件事：
//  1. `via: manager` 没写 params.manager 时，用 tool.managers.<stack> 补上；
//  2. `depends_on: [fnm]` 这类依赖，若该 stack 配的管理器换成了清单里另一个
//     版本管理器条目（如 nvm），自动改写依赖指向它。
//
// 只在真的用到配置映射时才拷贝清单。
func (e *Engine) normalize(l *manifest.Loaded) *manifest.Loaded {
	if l == nil || len(e.opt.Managers) == 0 {
		return l
	}
	changed := false
	out := &manifest.Loaded{Tools: make([]manifest.Tool, len(l.Tools)), Profiles: l.Profiles, Sources: l.Sources}
	copy(out.Tools, l.Tools)
	for i := range out.Tools {
		t := out.Tools[i]
		mgr := e.opt.Managers[t.Stack.String()]
		if mgr == "" {
			continue
		}
		// 1) depends_on 改写
		for j, dep := range t.DependsOn {
			if dep == mgr {
				continue
			}
			d, ok := l.ByID(dep)
			if !ok || d.Kind.String() != "versionmgr" || d.Stack != t.Stack {
				continue
			}
			if _, ok := l.ByID(mgr); !ok {
				continue // 清单里没有对应条目，别把依赖改成一个不存在的 id
			}
			t.DependsOn[j] = mgr
			changed = true
		}
		// 2) via: manager 缺 params.manager
		t.Install = fillManager(t.Install, mgr, &changed)
		t.Uninstall = fillManager(t.Uninstall, mgr, &changed)
		if t.Update != nil {
			upd := fillManager([]manifest.Method{*t.Update}, mgr, &changed)
			t.Update = &upd[0]
		}
		out.Tools[i] = t
	}
	if !changed {
		return l
	}
	return out
}

// resolveParams 展开清单参数里的模板（{{versions.*}} → 真实值）。
//
// 与 normalize 分开：normalize 管"管理器选择"，这里管"参数取值"，两者都只在
// 真的发生改动时才拷贝清单。
func (e *Engine) resolveParams(l *manifest.Loaded) (*manifest.Loaded, error) {
	if l == nil {
		return l, nil
	}
	data := RenderData{OS: e.env.OS(), Arch: e.env.Arch(), Versions: e.env.Versions()}
	changed := false
	out := &manifest.Loaded{Tools: make([]manifest.Tool, len(l.Tools)), Profiles: l.Profiles, Sources: l.Sources}
	copy(out.Tools, l.Tools)
	for i := range out.Tools {
		t := out.Tools[i]
		var err error
		if t.Install, err = resolveMethodParams(t.Install, data, &changed); err != nil {
			return nil, fmt.Errorf("tool %q: install: %w", t.ID, err)
		}
		if t.Uninstall, err = resolveMethodParams(t.Uninstall, data, &changed); err != nil {
			return nil, fmt.Errorf("tool %q: uninstall: %w", t.ID, err)
		}
		if t.Update != nil {
			upd := []manifest.Method{*t.Update}
			if upd, err = resolveMethodParams(upd, data, &changed); err != nil {
				return nil, fmt.Errorf("tool %q: update: %w", t.ID, err)
			}
			t.Update = &upd[0]
		}
		out.Tools[i] = t
	}
	if !changed {
		return l, nil
	}
	return out, nil
}

// resolveMethodParams 展开一组方法里的参数模板（始终返回新切片，避免改到原清单）。
func resolveMethodParams(methods []manifest.Method, data RenderData, changed *bool) ([]manifest.Method, error) {
	if methods == nil {
		return nil, nil
	}
	out := make([]manifest.Method, len(methods))
	copy(out, methods)
	for i := range out {
		c, err := resolveNameParams(&out[i].Params, data)
		if err != nil {
			return nil, err
		}
		if c {
			*changed = true
		}
	}
	return out, nil
}

// fillManager 给没写 manager 的 `via: manager` 方法补上配置里的管理器名。
func fillManager(methods []manifest.Method, mgr string, changed *bool) []manifest.Method {
	for i := range methods {
		if methods[i].Via != "manager" || methods[i].Params.Manager != "" {
			continue
		}
		if methods == nil {
			continue
		}
		out := methods
		out[i].Params.Manager = mgr
		methods = out
		*changed = true
	}
	return methods
}

// selectTools 按 id 选取并拓扑排序（含依赖自动补全）。
//
// 第二个返回值是"用户显式点名的 id"集合，用来决定 --via 的作用范围。
func (e *Engine) selectTools(ids []string) ([]manifest.Tool, map[string]bool, error) {
	explicit := map[string]bool{}
	for _, id := range ids {
		explicit[id] = true
	}
	if len(ids) == 0 {
		return nil, explicit, nil
	}
	normalized, err := e.resolveParams(e.normalize(e.loaded))
	if err != nil {
		return nil, explicit, err
	}
	tools, err := manifest.ExpandDeps(normalized, ids)
	if err != nil {
		return nil, explicit, err
	}
	out := make([]manifest.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Disabled {
			continue
		}
		// 整工具级的 when 不匹配 → 本机不适用，直接排除。
		if matched, _ := t.When.Matches(matchEnv(e.env)); !matched {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, explicit, fmt.Errorf("no applicable tools for %v on %s/%s", ids, e.env.OS(), e.env.Arch())
	}
	return out, explicit, nil
}

// ViaLabel 返回给人和脚本看的方式名（via=manager 时带上管理器名）。
func (s Step) ViaLabel() string {
	if s.Manager != "" {
		return s.Via + ":" + s.Manager
	}
	return s.Via
}

func dryRunStatus(s StepStatus) StepStatus {
	switch s {
	case StepInstall, StepUpgrade, StepUninstall:
		return StepStatus("planned")
	}
	return s
}

// Recorder 落盘安装记录（`<state>/installed.yaml`）。
//
// 只记录"装过什么、什么时候、用什么方式"，**不**作为"是否已安装"的依据
// （ADR-0011：判断一律实时探测）。它的用途是 `bs tool info` 展示历史、
// 以及将来做升级提示。
type Recorder struct {
	Path string
}

type installedFile struct {
	Version int                      `yaml:"version"`
	Tools   map[string]installedTool `yaml:"tools"`
}

type installedTool struct {
	Via       string `yaml:"via"`
	Version   string `yaml:"version,omitempty"`
	Installed string `yaml:"installed_at"`
}

// Record 把成功的步骤写入记录文件（内容不变时不写盘）。
func (r *Recorder) Record(report Report, now time.Time) error {
	if r == nil || r.Path == "" {
		return nil
	}
	file := installedFile{Version: 1, Tools: map[string]installedTool{}}
	if existing, err := r.load(); err == nil && existing != nil {
		file = *existing
	}
	if file.Tools == nil {
		file.Tools = map[string]installedTool{}
	}
	changed := false
	for _, s := range report.Steps {
		if s.Status != StepDone {
			continue
		}
		entry := installedTool{Via: s.Via, Installed: now.Format(time.RFC3339)}
		if s.Detection != nil {
			entry.Version = s.Detection.Version
		}
		if old, ok := file.Tools[s.ToolID]; ok && old.Via == entry.Via && old.Version == entry.Version {
			continue
		}
		file.Tools[s.ToolID] = entry
		changed = true
	}
	if !changed {
		return nil
	}
	if err := fsx.EnsureDir(filepath.Dir(r.Path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(file)
	if err != nil {
		return err
	}
	return fsx.AtomicWriteFile(r.Path, data, 0o644)
}

func (r *Recorder) load() (*installedFile, error) {
	data, err := os.ReadFile(r.Path)
	if err != nil {
		return nil, err
	}
	var file installedFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	return &file, nil
}

// LogPath 返回某次安装的日志文件路径（`<state>/logs/<时间>-<工具>.log`）。
func LogPath(stateDir string, toolID string, now time.Time) string {
	name := fmt.Sprintf("%s-%s.log", now.Format("20060102-150405"), strings.ReplaceAll(toolID, "/", "_"))
	return filepath.Join(stateDir, "logs", name)
}
