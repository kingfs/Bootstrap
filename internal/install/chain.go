package install

import (
	"context"
	"fmt"
	"strings"

	"github.com/kingfs/Bootstrap/internal/manifest"
)

// Candidate 是某个工具的一条可选安装方法及其评估结果。
type Candidate struct {
	Index int
	Via   string
	// Method 是（可能已补全 manager 默认值的）方法定义。
	Method manifest.Method
	// Usable 表示"按声明就是可用的候选"（未被 enabled: false 或 when 排除）。
	Usable bool
	// Available 表示该机制在当前机器上确实可用。
	Available bool
	// Skip 非空表示被排除的原因（enabled: false / when 不匹配）。
	Skip string
	// Note 是"可用性"方面的补充说明。
	Note string
}

// Chain 是一个工具的方法链解析结果。
type Chain struct {
	ToolID     string
	Candidates []Candidate
	// Selected 是最终选中的候选下标，-1 表示没有可用方法。
	Selected int
	// Explicit 表示通过 --via / tool.default_via 显式指定。
	Explicit bool
}

// SelectedCandidate 返回选中的候选。
func (c Chain) SelectedCandidate() (Candidate, bool) {
	if c.Selected < 0 || c.Selected >= len(c.Candidates) {
		return Candidate{}, false
	}
	return c.Candidates[c.Selected], true
}

// Fallbacks 返回"选中方法失败后按顺序可以再试"的候选。
//
// 只包含按声明本来就合法的候选（`enabled: false` 与 `when` 不匹配的不算），
// 因为那两种排除是**作者的明确意图**，不该由降级悄悄推翻。
func (c Chain) Fallbacks() []Candidate {
	var out []Candidate
	for i, cand := range c.Candidates {
		if i == c.Selected || !cand.Usable {
			continue
		}
		out = append(out, cand)
	}
	return out
}

// Skipped 返回被排除的候选及其原因，供 plan -v 展示。
func (c Chain) Skipped() []Candidate {
	var out []Candidate
	for _, cand := range c.Candidates {
		if cand.Skip != "" {
			out = append(out, cand)
		}
	}
	return out
}

// ChainOptions 影响链的解析。
type ChainOptions struct {
	// Via 是显式指定的方法名（--via 或 tool.default_via）；空表示按清单顺序。
	Via string
	// Managers 是 stack → 默认版本管理器的映射，用于补全 `via: manager`。
	Managers map[string]string
}

// ErrMethodNotFound 表示 --via 指定的方法不在链里。
type ErrMethodNotFound struct {
	ToolID string
	Via    string
	Known  []string
}

func (e *ErrMethodNotFound) Error() string {
	return fmt.Sprintf("tool %s has no method via=%s (available: %s)", e.ToolID, e.Via, joinStrings(e.Known))
}

// ErrViaUnavailable 表示 `--via` 指定的方法在当前机器上不可用
// （Declared 为真时是"清单里就写明了不启用/条件不匹配"）。
type ErrViaUnavailable struct {
	ToolID   string
	Via      string
	Reason   string
	Declared bool
}

func (e *ErrViaUnavailable) Error() string {
	return fmt.Sprintf("tool %s: via=%s is not available (%s)", e.ToolID, e.Via, e.Reason)
}

// ErrNoMethod 表示链里没有任何可用方法。
type ErrNoMethod struct {
	ToolID  string
	Reasons []string
}

func (e *ErrNoMethod) Error() string {
	return fmt.Sprintf("tool %s: no usable install method (%s)", e.ToolID, joinStrings(e.Reasons))
}

// ResolveChain 按 docs/design.md §7.2 的算法选择安装方法。
//
// 顺序即策略：清单里的书写顺序就是优先级，代码里没有任何硬编码偏好。
func ResolveChain(ctx context.Context, reg *Registry, e ExecEnv, t manifest.Tool, opt ChainOptions) (Chain, error) {
	tool := FillManagerDefaults(t, opt.Managers)
	chain := Chain{ToolID: tool.ID, Selected: -1, Explicit: opt.Via != ""}

	for i, method := range tool.Install {
		cand := Candidate{Index: i, Via: method.Via, Method: method, Usable: true}
		switch {
		case !method.IsEnabled():
			cand.Usable = false
			cand.Skip = "enabled: false"
		default:
			if matched, reason := method.When.Matches(matchEnv(e)); !matched {
				cand.Usable = false
				cand.Skip = "when: " + reason
			}
		}
		if cand.Usable {
			in, ok := reg.Get(method.Via)
			switch {
			case !ok:
				cand.Available = false
				cand.Note = "unknown via"
			case in.Available(ctx, e, tool, method):
				cand.Available = true
			default:
				cand.Note = unavailableNote(method.Via, reg)
			}
		}
		chain.Candidates = append(chain.Candidates, cand)
	}

	if opt.Via != "" {
		for i, cand := range chain.Candidates {
			if cand.Via != opt.Via {
				continue
			}
			if !cand.Usable {
				return chain, &ErrViaUnavailable{ToolID: tool.ID, Via: opt.Via, Reason: cand.Skip, Declared: true}
			}
			if !cand.Available {
				return chain, &ErrViaUnavailable{ToolID: tool.ID, Via: opt.Via, Reason: cand.Note}
			}
			chain.Selected = i
			return chain, nil
		}
		var known []string
		seen := map[string]bool{}
		for _, cand := range chain.Candidates {
			if !seen[cand.Via] {
				seen[cand.Via] = true
				known = append(known, cand.Via)
			}
		}
		return chain, &ErrMethodNotFound{ToolID: tool.ID, Via: opt.Via, Known: known}
	}

	for i, cand := range chain.Candidates {
		if cand.Usable && cand.Available {
			chain.Selected = i
			return chain, nil
		}
	}

	var reasons []string
	for _, cand := range chain.Candidates {
		switch {
		case cand.Skip != "":
			reasons = append(reasons, cand.Via+" ("+cand.Skip+")")
		case cand.Note != "":
			reasons = append(reasons, cand.Via+" ("+cand.Note+")")
		default:
			reasons = append(reasons, cand.Via+" (unavailable)")
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "no methods declared")
	}
	return chain, &ErrNoMethod{ToolID: tool.ID, Reasons: reasons}
}

// FillManagerDefaults 给 `via: manager` 且没写明 manager 的方法补上配置里的默认管理器。
//
// 这样清单可以写"python 用 uv、nodejs 用 fnm"，而不用在每条工具上重复一遍。
func FillManagerDefaults(t manifest.Tool, managers map[string]string) manifest.Tool {
	if len(t.Install) == 0 {
		return t
	}
	out := t
	out.Install = append([]manifest.Method{}, t.Install...)
	for i := range out.Install {
		m := out.Install[i]
		if m.Via != "manager" || m.Params.Manager != "" {
			continue
		}
		if name, ok := managers[t.Stack.String()]; ok {
			m.Params.Manager = name
		}
		out.Install[i] = m
	}
	return out
}

func matchEnv(e ExecEnv) manifest.MatchEnv {
	return manifest.MatchEnv{
		OS:     e.OS(),
		Arch:   e.Arch(),
		Distro: e.Distro(),
		Has:    func(name string) bool { _, ok := e.LookPath(name); return ok },
	}
}

// unavailableNote 给"机制不可用"一个可执行的原因说明。
func unavailableNote(via string, reg *Registry) string {
	switch via {
	case "manager":
		return "版本管理器未安装"
	case "brew":
		return "brew 未安装"
	case "apt", "dnf", "yum", "pacman", "zypper", "apk":
		return via + " 不存在"
	case "winget", "scoop", "choco":
		return via + " 不存在"
	case "bun", "npm", "pnpm", "pipx", "uvpm", "cargo", "goinstall":
		return via + " 不可用"
	case "script", "release", "source":
		return "缺少所需命令"
	}
	if reg != nil {
		if _, ok := reg.Get(via); !ok {
			return "未知方式 " + via
		}
	}
	return "不可用"
}

// ManagerName 抽取 `via: manager` 的方法所使用的管理器名（供提示与日志）。
func ManagerName(m manifest.Method) string { return m.Params.Manager }

// viaString 只用于日志。
func viaString(m manifest.Method) string {
	if m.Via == "manager" && m.Params.Manager != "" {
		return "manager:" + m.Params.Manager
	}
	return strings.TrimSpace(m.Via)
}
