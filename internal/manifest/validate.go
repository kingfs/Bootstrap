package manifest

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/kingfs/Bootstrap/internal/config"
)

// MethodValidator 由安装引擎提供，供清单校验期检查 `via` 与 `params`。
//
// 反向依赖的方式（manifest 定义接口、install 实现）让清单模型不必知道
// 任何一个具体 Installer 的存在。
type MethodValidator interface {
	// KnownVias 返回全部已注册的 via 名（用于"可用方法"提示与拼写错误检查）。
	KnownVias() []string
	// ValidateMethod 检查某个 via 的参数是否符合该 Installer 的 schema。
	ValidateMethod(via string, p Params) error
	// ValidateManagerAction 检查版本管理器是否支持该 action。
	ValidateManagerAction(manager, action string) error
	// IsSystemVia 报告该 via 是否属于"系统包管理器"（受护栏约束）。
	IsSystemVia(via string) bool
}

// ValidateOptions 控制校验行为。
type ValidateOptions struct {
	Vias MethodValidator
	// AllowSystemFor 是允许用系统包管理器安装的 kind 列表（配置 tool.allow_system_for）。
	AllowSystemFor []config.Kind
	// Managers 是 stack → 版本管理器名 的映射，用于给出更准确的提示。
	Managers map[string]string
	// RequireManagerForRuntime 为 true 时，runtime 条目必须有一条 via: manager 路径
	// （除非声明 manager_optional: true）。见 docs/design.md §6.6。
	RequireManagerForRuntime bool
	// Versions 是配置 tool.versions；模板校验时用它渲染 {{versions.*}}，
	// 否则清单里写 {{versions.rust}} 会被误判成"没定义"。
	Versions map[string]string
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// profile 解析的两类问题：缺失由"引用检查"负责报告，环由本函数负责报告，
// 分开是为了不在输出里重复同一条消息。
var (
	errProfileMissing = errors.New("profile not found")
	errProfileCycle   = errors.New("profile cycle")
)

// Validate 按 §6.6 校验合并后的清单，并一次性返回全部问题。
func Validate(l *Loaded, opts ValidateOptions) error {
	p := &Problems{}
	known := map[string]bool{}

	for i := range l.Tools {
		t := &l.Tools[i]
		where := fmt.Sprintf("tool %q", t.ID)
		if t.Origin != "" {
			where = fmt.Sprintf("tool %q (%s)", t.ID, t.Origin)
		}

		if t.ID == "" {
			p.Add("tool #%d: id is required", i+1)
			continue
		}
		if !idPattern.MatchString(t.ID) {
			p.Add("%s: id must match %s", where, idPattern)
		}
		known[t.ID] = true

		if t.Stack == "" {
			p.Add("%s: stack is required", where)
		} else if !ValidStack(t.Stack) {
			p.Add("%s: unknown stack %q (want one of %s)", where, t.Stack, stackList())
		}
		if t.Kind == "" {
			p.Add("%s: kind is required", where)
		} else if !config.IsKind(string(t.Kind)) {
			p.Add("%s: unknown kind %q (want one of %s)", where, t.Kind, kindList())
		}

		if t.Disabled {
			continue // 禁用条目只做 id/枚举检查，其余允许"半成品"
		}

		// verify：单条规则（绝大多数）或多条按 when 挑选的规则。
		rules := t.VerifyRules
		if len(rules) == 0 {
			// 程序化构造的条目只填了单条 Verify，按同一条规则校验。
			rules = VerifySet{{Verify: t.Verify}}
		}
		hasProbe := false
		for i, r := range rules {
			ruleWhere := where + " verify"
			if len(rules) > 1 {
				ruleWhere = fmt.Sprintf("%s verify[%d]", where, i)
			}
			if r.Command == "" && r.File == "" && r.VersionRegex == "" {
				p.Add("%s: verify.command, verify.file or verify.version_regex is required", ruleWhere)
			}
			if r.File != "" && r.Command != "" {
				p.Add("%s: verify.file and verify.command are mutually exclusive (command wins)", ruleWhere)
			}
			if r.Command != "" || r.File != "" {
				hasProbe = true
			}
			// 多条规则靠 when 分流：中间那条不能不带 when（否则后面的规则永远匹配不到），
			// 最后一条必须不带 when（兜底），否则某些机器上会挑不出任何规则。
			if len(rules) > 1 {
				switch {
				case i < len(rules)-1 && r.When.IsZero():
					p.Add("%s: only the last verify rule may omit `when` "+
						"(an earlier catch-all shadows the rest)", ruleWhere)
				case i == len(rules)-1 && !r.When.IsZero():
					p.Add("%s: the last verify rule must omit `when` "+
						"(it is the fallback for machines no other rule matches)", ruleWhere)
				}
			}
		}
		if t.Kind == config.KindVersionMgr && !hasProbe {
			p.Add("%s: a versionmgr must declare verify.command or verify.file "+
				"(depends_on/manager 委托都靠它探测)", where)
		}

		// install 链
		if len(t.Install) == 0 {
			p.Add("%s: install must not be empty", where)
		}
		for j, m := range t.Install {
			validateMethod(p, opts, fmt.Sprintf("%s install[%d]", where, j), t, m)
		}
		for j, m := range t.Uninstall {
			validateMethod(p, opts, fmt.Sprintf("%s uninstall[%d]", where, j), t, m)
		}
		if t.Update != nil {
			validateMethod(p, opts, where+" update", t, *t.Update)
		}

		// 管理器优先策略的护栏
		if t.Kind == config.KindRuntime && opts.RequireManagerForRuntime && !t.ManagerOptional {
			if !hasManagerMethod(t) {
				p.Add("%s: kind=runtime must have a `via: manager` path (or set manager_optional: true)", where)
			}
		}

		// 系统包管理器护栏
		if opts.Vias != nil && len(opts.AllowSystemFor) > 0 {
			for j, m := range t.Install {
				if !m.IsEnabled() || !opts.Vias.IsSystemVia(m.Via) {
					continue
				}
				if allowSystem(t.Kind, opts.AllowSystemFor) || boolValue(m.Params.AllowSystem) {
					continue
				}
				p.Add("%s install[%d]: via=%s is a system package manager; kind=%s needs tool.allow_system_for or params.allow_system: true",
					where, j, m.Via, t.Kind)
			}
		}
	}

	// depends_on 引用与跨 stack 误指
	for i := range l.Tools {
		t := l.Tools[i]
		for _, dep := range t.DependsOn {
			if !known[dep] {
				p.Add("tool %q: depends_on %q not found", t.ID, dep)
				continue
			}
			d, _ := l.ByID(dep)
			if d.Kind == config.KindVersionMgr && t.Kind == config.KindRuntime && d.Stack != t.Stack {
				p.Add("tool %q (stack=%s): depends_on versionmgr %q (stack=%s) crosses stacks",
					t.ID, t.Stack, dep, d.Stack)
			}
		}
	}
	if _, err := TopoOrder(l.Tools); err != nil {
		p.Add("%v", err)
	}

	// profiles
	for _, name := range l.ProfileNames() {
		prof := l.Profiles[name]
		for _, id := range prof.Tools {
			if !known[id] {
				p.Add("profile %q: tool %q not found", name, id)
			}
		}
		for _, ext := range prof.Extends {
			if _, ok := l.Profiles[ext]; !ok {
				p.Add("profile %q: extends %q not found", name, ext)
			}
		}
	}
	if _, err := resolveProfiles(l); err != nil && !errors.Is(err, errProfileMissing) {
		p.Add("%v", err)
	}

	return p.Err()
}

func validateMethod(p *Problems, opts ValidateOptions, where string, t *Tool, m Method) {
	_ = opts.Versions
	if m.Via == "" {
		p.Add("%s: via is required", where)
		return
	}
	if opts.Vias == nil {
		return
	}
	if err := opts.Vias.ValidateMethod(m.Via, m.Params); err != nil {
		p.Add("%s: %v", where, err)
		return
	}
	if m.Via != "manager" {
		return
	}
	// via: manager —— 管理器必须在注册表里，且支持声明的 action。
	manager := m.Params.Manager
	if manager == "" {
		// 允许只写 `via: manager`：落地目标由配置 managers.<stack> 决定，
		// 这样把 nodejs 从 fnm 换成 nvm 不需要改清单（§6.2）。
		manager = opts.Managers[string(t.Stack)]
		if manager == "" {
			p.Add("%s: via=manager needs params.manager (no tool.managers entry for stack %q)", where, t.Stack)
			return
		}
	}
	if m.Params.Action == "" {
		p.Add("%s: via=manager requires params.action", where)
		return
	}
	if err := opts.Vias.ValidateManagerAction(manager, m.Params.Action); err != nil {
		p.Add("%s: %v", where, err)
	}
}

func hasManagerMethod(t *Tool) bool {
	for _, m := range t.Install {
		if m.Via == "manager" && m.IsEnabled() {
			return true
		}
	}
	return false
}

func allowSystem(kind config.Kind, allowed []config.Kind) bool {
	for _, k := range allowed {
		if k == kind {
			return true
		}
	}
	return false
}

func boolValue(b *bool) bool { return b != nil && *b }

// resolveProfiles 展开每个 profile 的 extends 链，返回 name → 工具 id 列表。
//
// 同时检测 extends 环——profile 之间互相继承是最容易写错的地方。
func resolveProfiles(l *Loaded) (map[string][]string, error) {
	out := make(map[string][]string, len(l.Profiles))
	state := map[string]int{}

	var expand func(name string, path []string) ([]string, error)
	expand = func(name string, path []string) ([]string, error) {
		if ids, ok := out[name]; ok {
			return ids, nil
		}
		prof, ok := l.Profiles[name]
		if !ok {
			return nil, fmt.Errorf("%w: %q", errProfileMissing, name)
		}
		switch state[name] {
		case 1:
			return nil, fmt.Errorf("%w: %s", errProfileCycle, strings.Join(append(path, name), " → "))
		case 2:
			return out[name], nil
		}
		state[name] = 1
		seen := map[string]bool{}
		var ids []string
		for _, ext := range prof.Extends {
			sub, err := expand(ext, append(path, name))
			if err != nil {
				return nil, err
			}
			for _, id := range sub {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
		for _, id := range prof.Tools {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		state[name] = 2
		out[name] = ids
		return ids, nil
	}

	for _, name := range l.ProfileNames() {
		if _, err := expand(name, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ResolveProfile 返回某个 profile 展开后的工具 id 列表（含 extends）。
func (l *Loaded) ResolveProfile(name string) ([]string, error) {
	all, err := resolveProfiles(l)
	if err != nil {
		return nil, err
	}
	ids, ok := all[name]
	if !ok {
		return nil, &UnknownProfileError{Name: name, Known: l.ProfileNames()}
	}
	return ids, nil
}

// UnknownProfileError 表示请求的 profile 不在清单里（属于用法错误，退出码 2）。
type UnknownProfileError struct {
	Name  string
	Known []string
}

func (e *UnknownProfileError) Error() string {
	return fmt.Sprintf("profile %q not found (available: %s)", e.Name, strings.Join(e.Known, ", "))
}

func stackList() string {
	names := make([]string, 0, len(AllStacks))
	for _, s := range AllStacks {
		names = append(names, string(s))
	}
	return strings.Join(names, "|")
}

func kindList() string {
	names := make([]string, 0, len(config.AllKinds))
	for _, k := range config.AllKinds {
		names = append(names, string(k))
	}
	return strings.Join(names, "|")
}
