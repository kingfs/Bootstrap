package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// 本文件是 `bs doctor --fix` 的用例层：把体检结论翻译成"能安全自动修的动作"。
//
// 两条硬规矩：
//  1. **计划与执行分离**：PlanFixes 只读，ApplyFixes 才动手；`--dry-run` 只走到 PlanFixes。
//  2. **不猜**：只修"结论明确、修法唯一"的问题（建配置、建目录、装 shell 受管块）。
//     缺 git、清单写错、代理没开这类要用户拿主意的事只给提示，绝不代劳。

// FixAction 是一项可自动修复的问题。
type FixAction struct {
	// ID 为稳定标识：config | dirs | repo-root | shell。
	ID    string `json:"id"`
	Title string `json:"title"`
	// Detail 说清"要改哪个文件的什么"，让人一眼能判断该不该同意。
	Detail  string `json:"detail"`
	Applied bool   `json:"applied"`
	Message string `json:"message,omitempty"`
	Err     string `json:"err,omitempty"`
	// Blocked 为真表示"这次修不了"（例如父目录不可写），Message 里写原因。
	Blocked bool `json:"blocked,omitempty"`
}

// DoctorFixPlan 是一次修复计划。
type DoctorFixPlan struct {
	Actions []FixAction `json:"actions"`
}

// Fixable 返回真正会执行的动作数（Blocked 的不算）。
func (p DoctorFixPlan) Fixable() int {
	n := 0
	for _, a := range p.Actions {
		if !a.Blocked {
			n++
		}
	}
	return n
}

// PlanFixes 依据体检结论算出可自动修的动作（只读，不创建任何文件）。
func (a *App) PlanFixes(checks []Check) DoctorFixPlan {
	var plan DoctorFixPlan
	byName := make(map[string]Check, len(checks))
	for _, c := range checks {
		byName[c.Name] = c
	}

	// 1. 缺配置文件 → 写默认配置（与 `bs config init` 同一条代码路径，已存在则不覆盖）。
	if c, ok := byName["config"]; ok && c.Status == StatusWarn {
		plan.Actions = append(plan.Actions, FixAction{
			ID: "config", Title: "生成默认配置文件",
			Detail: a.Paths.ConfigFile() + "（已存在时不覆盖）",
		})
	}

	// 2. bs 自己的目录**不作为修复动作**：体检把"不存在但父目录可写"判为 OK（那本来就不是问题），
	// 而 config/state/cache 目录在第一次真正需要时会被创建（生成配置、写缓存、存探测结果）。
	// 这里刻意不制造"为了体检好看而预建空目录"的动作。

	// 3. 仓库根目录不存在 → 建出来（`bs repo get` 本来也会建，先建好省一次困惑）。
	if c, ok := byName["repo root"]; ok && c.Status == StatusWarn {
		if root, _ := a.RepoRoot(); root != "" && !fsx.IsDir(root) {
			plan.Actions = append(plan.Actions, FixAction{
				ID: "repo-root", Title: "创建仓库根目录", Detail: root,
			})
		}
	}

	// 4. shell 受管块缺失/过期 → 装一次（写 rc 前会备份，块外字节不动）。
	if c, ok := byName["shell rc"]; ok && c.Status == StatusWarn {
		shell := a.Platform.Shell
		if shell == "" {
			shell = "bash"
		}
		rc, err := a.ResolveRCFile(shell, "")
		switch {
		case err != nil:
			plan.Actions = append(plan.Actions, FixAction{
				ID: "shell", Title: "安装 shell 受管块", Blocked: true,
				Detail: "无法确定 rc 文件", Message: err.Error(),
			})
		default:
			plan.Actions = append(plan.Actions, FixAction{
				ID: "shell", Title: "安装/更新 shell 受管块",
				Detail: rc + "（写前备份，块外内容字节级不动）",
			})
		}
	}

	return plan
}

// ApplyFixes 执行修复计划，返回每个动作的结果。
//
// 单个动作失败不会中断后面的动作：修好一件算一件，失败原因逐条回报
// （这也是"体检"这个场景的预期——用户想知道"还剩什么没解决"）。
func (a *App) ApplyFixes(ctx context.Context, plan DoctorFixPlan) DoctorFixPlan {
	out := DoctorFixPlan{Actions: make([]FixAction, 0, len(plan.Actions))}
	for _, act := range plan.Actions {
		if act.Blocked {
			out.Actions = append(out.Actions, act)
			continue
		}
		act.Applied = true
		msg, err := a.applyFix(ctx, act)
		act.Message = msg
		if act.Message == "" {
			act.Message = "已处理"
		}
		if err != nil {
			act.Err = err.Error()
		}
		out.Actions = append(out.Actions, act)
	}
	return out
}

// applyFix 执行单个动作，返回一句人话结果 + 可能的错误。
//
// 返回错误不是"抛给上层中断"：体检场景要的是"还剩什么没解决"，所以每条各自记账。
func (a *App) applyFix(_ context.Context, act FixAction) (string, error) {
	switch act.ID {
	case "config":
		path, created, err := a.EnsureConfig(false)
		switch {
		case err != nil:
			return "", err
		case created:
			return "已生成 " + path, nil
		default:
			return "已存在，未改动", nil
		}
	case "dirs":
		var done, failed []string
		for _, dir := range strings.Split(act.Detail, ", ") {
			dir = strings.TrimSpace(dir)
			if dir == "" {
				continue
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				failed = append(failed, dir+"："+err.Error())
				continue
			}
			done = append(done, dir)
		}
		if len(failed) > 0 {
			return fmt.Sprintf("已创建 %d 个", len(done)), errors.New(strings.Join(failed, "；"))
		}
		return fmt.Sprintf("已创建 %d 个目录", len(done)), nil
	case "repo-root":
		if err := os.MkdirAll(act.Detail, 0o755); err != nil {
			return "", err
		}
		return "已创建 " + act.Detail, nil
	case "shell":
		shell := a.Platform.Shell
		if shell == "" {
			shell = "bash"
		}
		res, err := a.ShellInstall(shell, "")
		if err != nil {
			return "", err
		}
		if !res.Changed {
			return res.RCFile + " 已是最新，未改动", nil
		}
		if res.Backup != "" {
			return "已写入 " + res.RCFile + "（原文件备份：" + res.Backup + "）", nil
		}
		return "已写入 " + res.RCFile, nil
	default:
		return "未知动作，已跳过", nil
	}
}

// DoctorFixReport 是 `bs doctor --fix` 的结果：修复前后的体检 + 修复明细。
type DoctorFixReport struct {
	// Before / After 是修复前后的体检结论（After 只有在真的修过之后才有意义）。
	Before []Check       `json:"before"`
	After  []Check       `json:"after,omitempty"`
	Fixes  DoctorFixPlan `json:"fixes"`
	// Summary 是修复后的计数（没有 After 时是修复前的）。
	Summary map[string]int `json:"summary"`
}

// DoctorFixSummary 汇总一次体检的四个计数。
func DoctorFixSummary(checks []Check) map[string]int {
	ok, warn, fail, skip := CheckSummary(checks)
	return map[string]int{"ok": ok, "warn": warn, "fail": fail, "skip": skip}
}
