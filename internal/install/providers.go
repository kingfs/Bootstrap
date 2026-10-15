package install

import (
	"context"

	"github.com/kingfs/Bootstrap/internal/manifest"
)

// 本文件解决"依赖会先装、但计划期还看不到它"这一类问题：
//
//	gopls 的 goinstall 要 go   → 得先有 golang
//	golang 的 via: manager 要 g → 得先有 g
//
// 计划期的链解析与 `when.has` 都按**当前** PATH 判断可用性，而被依赖的工具要到
// 拓扑序更早的步骤才装出来。于是分三层处理：
//
//  1. providersForMissing：某工具在本机一条可用方式都没有、卡点只是缺一个命令时，
//     把清单里"正是靠这个命令验收"的工具补进本次集合（gopls → golang → g）；
//  2. planEnv：把"本次计划里更早的步骤会装出来的命令"当作已存在，链解析因此选得中
//     `via: manager` 这类还没就绪的方式；
//  3. 真正执行时按拓扑序，命令已经在 PATH 上（`g` 装完才轮到 golang）。

// maxProviderRounds 限制"补提供者 → 重新展开依赖"的轮数。
//
// 每轮至少补进一个尚未在集合里的工具，而集合大小有清单上界，所以这只是防御性兜底
// （真有环也会被 TopoOrder 抓住）。
const maxProviderRounds = 8

// planEnv 在计划期把"本次计划里更早的步骤会装出来的命令"当成已存在。
//
// 只影响"存不存在"的判断：真正执行永远走原始环境，拿到的也是真实路径。
type planEnv struct {
	ExecEnv
	// provided 是 命令名 → 提供它的工具 id。
	provided map[string]string
}

// LookPath 先问真实环境，再问"稍后会有谁"。
//
// 命中 provided 时返回的是占位路径（那个文件此刻并不存在），调用方只该拿它做
// 存在性判断，不要拿去执行。
func (p planEnv) LookPath(name string) (string, bool) {
	if path, ok := p.ExecEnv.LookPath(name); ok {
		return path, true
	}
	if _, ok := p.provided[name]; ok {
		return "planned:" + name, true
	}
	return "", false
}

// realEnv 剥掉计划期的"稍后会有"覆盖层。
//
// 判断"管理器现在就绪了吗"必须看真实环境：planEnv 会诚实地说"g 会有"，
// 因为更早的步骤确实会把它装出来，但此刻去跑 `g ls-remote` 一定是找不到命令。
func realEnv(e ExecEnv) ExecEnv {
	if p, ok := e.(planEnv); ok {
		return p.ExecEnv
	}
	return e
}

// providerIndex 建立"命令名 → 提供它的工具 id"索引（按 verify.command）。
//
// 同一个命令被多个工具验收时取清单里靠前的那个：顺序稳定，结果可复现。
func providerIndex(l *manifest.Loaded, env manifest.MatchEnv) map[string]string {
	out := map[string]string{}
	for _, t := range l.Tools {
		if t.Disabled || t.Verify.Command == "" {
			continue
		}
		// 本机不适用的工具不能当提供者：补进来也装不上。
		if matched, _ := t.When.Matches(env); !matched {
			continue
		}
		if _, ok := out[t.Verify.Command]; !ok {
			out[t.Verify.Command] = t.ID
		}
	}
	return out
}

// providersForMissing 找出"为了把这批工具装下去还必须补上的提供者"。
//
// 返回值是 被卡住的工具 id → 提供者工具 id（可能多个）。两条约束：
//
//  1. 只对**一条可用方式都没有**的工具做推断：有可用方式的工具不缺依赖，硬补会把作者
//     刻意留的偏好分支（例如 golang 的 `when: {has: [mise]}`）变成必装；
//  2. 只认**同一个 stack** 的提供者。gopls(golang) 缺 go → golang(golang) 是"这个工具
//     的语言运行时"，而 fnm(nodejs) 缺 cargo → rust(rust) 属于跨栈的工具链：跨栈需求是
//     清单作者该显式写 depends_on 的事，否则 `bs tool install nodejs` 会为了一个版本
//     管理器把整条 Rust 工具链（还是 apt 系统包）拉进来，太意外了。
func (e *Engine) providersForMissing(ctx context.Context, l *manifest.Loaded, tools []manifest.Tool, explicit map[string]bool) map[string][]string {
	byCmd := providerIndex(l, matchEnv(e.env))
	out := map[string][]string{}
	for _, t := range tools {
		// 与 PlanTools 同一套规则：--via 只约束用户显式点名的工具。
		via := ""
		if explicit[t.ID] {
			via = e.opt.DefaultVia
		}
		if _, err := ResolveChain(ctx, e.reg, e.env, t, ChainOptions{Via: via, Managers: e.opt.Managers}); err == nil {
			continue
		}
		seen := map[string]bool{}
		for _, cmd := range blockingCommands(t, matchEnv(e.env)) {
			id, ok := byCmd[cmd]
			if !ok || id == t.ID || seen[id] {
				continue
			}
			if p, ok := l.ByID(id); !ok || p.Stack != t.Stack {
				continue
			}
			seen[id] = true
			out[t.ID] = append(out[t.ID], id)
		}
	}
	return out
}

// addInferredDeps 返回一份"补上推断依赖"的清单副本，并报告是否真的补了边。
//
// 推断出的提供者必须变成真正的依赖边：只有 ExpandDeps/TopoOrder 知道先装谁，
// 否则 gopls 可能排在 golang 前面，计划期照样解析不到 go。
func addInferredDeps(l *manifest.Loaded, extra map[string][]string) (*manifest.Loaded, bool) {
	if len(extra) == 0 {
		return l, false
	}
	out := &manifest.Loaded{Tools: make([]manifest.Tool, len(l.Tools)), Profiles: l.Profiles, Sources: l.Sources}
	copy(out.Tools, l.Tools)
	index := make(map[string]int, len(out.Tools))
	for i, t := range out.Tools {
		index[t.ID] = i
	}
	changed := false
	for toolID, providers := range extra {
		i, ok := index[toolID]
		if !ok {
			continue
		}
		for _, p := range providers {
			if containsString(out.Tools[i].DependsOn, p) {
				continue
			}
			// 先把切片拷一份：out.Tools[i] 与 l.Tools[i] 共享底层数组，
			// 直接 append 可能写到原清单的容量里。
			deps := append([]string{}, out.Tools[i].DependsOn...)
			out.Tools[i].DependsOn = append(deps, p)
			changed = true
		}
	}
	return out, changed
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// blockingCommands 汇总该工具"只差这些命令"的 when.has 命令（其余条件都匹配）。
func blockingCommands(t manifest.Tool, env manifest.MatchEnv) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range t.Install {
		if !m.IsEnabled() {
			continue
		}
		for _, cmd := range m.When.MissingHas(env) {
			if !seen[cmd] {
				seen[cmd] = true
				out = append(out, cmd)
			}
		}
	}
	return out
}
