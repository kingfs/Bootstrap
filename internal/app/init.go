package app

import (
	"context"
	"fmt"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/internal/detect"
	"github.com/kingfs/Bootstrap/internal/install"
	"github.com/kingfs/Bootstrap/internal/ui"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/hostinfo"
)

// 本文件是 M3 的"新机器一把梭"用例：环境概览 → 选工具 → 计划 → 执行 → 可选 shell 集成。
//
// 分层约定（AGENTS.md §2）：
//   - 交互全部通过注入的 ui.Prompter，绝不直接读 os.Stdin；
//   - 本文件不打印任何东西：需要渲染时用 OnOverview / OnPlan 回调把数据交给 cli；
//   - "要不要确认"的策略在 cli（--yes / ui.confirm / 非交互退出码 5），这里只认 AssumeYes。

// InitOverview 是 `bs init` 开头展示的环境概览。
//
// 全部来自只读探测：不写配置文件、不发网络请求；`--dry-run` 下连探测缓存都不写。
type InitOverview struct {
	OS            string        `json:"os"`
	Arch          string        `json:"arch"`
	Distro        string        `json:"distro,omitempty"`
	Shell         string        `json:"shell,omitempty"`
	HomeDir       string        `json:"home"`
	ConfigFile    string        `json:"config_file"`
	ConfigExists  bool          `json:"config_exists"`
	ConfigSources []string      `json:"config_sources,omitempty"`
	ConfigDir     string        `json:"config_dir"`
	StateDir      string        `json:"state_dir"`
	CacheDir      string        `json:"cache_dir"`
	RepoRoot      string        `json:"repo_root"`
	RepoRootFrom  string        `json:"repo_root_source,omitempty"`
	Proxy         string        `json:"proxy,omitempty"`
	ProxyDetail   string        `json:"proxy_detail,omitempty"`
	Tools         InitToolStats `json:"tools"`
}

// InitToolStats 统计本机适用工具的装机情况。
type InitToolStats struct {
	Applicable int `json:"applicable"`
	Installed  int `json:"installed"`
	Outdated   int `json:"outdated"`
	Missing    int `json:"missing"`
	Unknown    int `json:"unknown"`
	// Excluded 是"清单里有、但不适用于本机或被禁用"的数量，只用于解释总数。
	Excluded int `json:"excluded"`
}

// InitSelectionMode 说明这次的工具是怎么选出来的。
type InitSelectionMode string

// 工具选择的来源。
const (
	SelectionGiven   InitSelectionMode = "given"   // 命令行显式给出 id
	SelectionProfile InitSelectionMode = "profile" // 按套装
	SelectionManual  InitSelectionMode = "manual"  // 交互式手工勾选
	SelectionMissing InitSelectionMode = "missing" // 只补缺失
	SelectionNone    InitSelectionMode = "none"    // 没有需要处理的工具
	SelectionSkipped InitSelectionMode = "skipped" // 明确要求这次不装工具（--no-tools / 交互选"先不装"）
)

// InitOptions 控制 `bs init` 的行为。
type InitOptions struct {
	// Profile 显式指定套装；为空时交互式向导会问、非交互式回退到配置的默认套装。
	Profile string
	// Tools 显式指定工具 id（优先于 Profile，也不再提问选工具）。
	Tools []string
	// MissingOnly 为真时自动只选"当前缺失"的工具（给已有机器"补缺"用）。
	MissingOnly bool
	// NoTools 为真时只做"环境准备"（生成配置、可选 shell 受管块），一个工具都不装：
	// 新机器上想先看清环境、或者本来就有自己的装法时用得上。
	NoTools bool
	// Prompter 是交互通道；非交互会话请传 ui.NonInteractive{}。
	Prompter ui.Prompter
	// AssumeYes 表示已获得授权（--yes 或 ui.confirm=false）：不再询问确认。
	AssumeYes bool
	// ShellInstall 为 nil 表示"由向导决定"（交互时问一句，非交互时不装）；
	// 非 nil 表示明确要求装/不装 rc 受管块。
	ShellInstall *bool
	// OnOverview 在概览算完后、任何提问之前调用（cli 负责渲染）。
	OnOverview func(InitOverview) error
	// OnPlan 在计划算完后、执行之前调用（cli 负责渲染）。计划为空时也会调用一次。
	OnPlan func(install.Plan) error
}

// InitResult 是 `bs init` 的结构化结果。
type InitResult struct {
	Overview InitOverview        `json:"overview"`
	Mode     InitSelectionMode   `json:"selection_mode"`
	Profile  string              `json:"profile,omitempty"`
	Tools    []string            `json:"tools"`
	Plan     install.Plan        `json:"plan"`
	Report   *install.Report     `json:"report,omitempty"`
	Shell    *ShellInstallResult `json:"shell,omitempty"`
	// ConfigCreated 为真表示这次生成了默认配置文件（dry-run 下恒为 false）。
	ConfigCreated bool `json:"config_created"`
	// DryRunPlannedConfig 表示"如果没开 dry-run 就会生成配置"。
	DryRunPlannedConfig bool `json:"dry_run_planned_config,omitempty"`
	// SkippedShell 表示按用户/配置的决定没有动 shell 启动文件。
	SkippedShell bool `json:"skipped_shell,omitempty"`
	// SkipTools 表示这次没有处理任何工具（--no-tools，或交互式里选了"先不装工具"）。
	// 概览里的探测结果照旧有效：调用方据此告诉用户"缺什么、想装时敲哪条命令"。
	SkipTools bool `json:"skip_tools,omitempty"`
}

// EnsureConfig 生成默认配置文件（已存在且未 force 时什么都不做）。
//
// 放在 app 层是为了让 `bs config init` 与 `bs init` 共用一份"写配置"的逻辑：
// cli 不该直接碰配置文件（AGENTS.md §2）。
func (a *App) EnsureConfig(force bool) (path string, created bool, err error) {
	if a.DryRun {
		// dry-run 必须零写入：把"会生成哪份文件"作为结果返回，由 cli 提示。
		return a.Paths.ConfigFile(), false, nil
	}
	return config.WriteDefault(a.Paths, force)
}

// InitOverview 采集环境概览（只读，含一次并发工具探测）。
func (a *App) InitOverview(ctx context.Context) (InitOverview, error) {
	ov := InitOverview{
		OS:            a.Platform.OS,
		Arch:          a.Platform.Arch,
		Distro:        distroLabel(a.Platform),
		Shell:         a.Platform.Shell,
		HomeDir:       a.Paths.HomeDir,
		ConfigFile:    a.Paths.ConfigFile(),
		ConfigExists:  fsx.IsFile(a.Paths.ConfigFile()),
		ConfigSources: a.ConfigSources,
		ConfigDir:     a.Paths.ConfigDir,
		StateDir:      a.Paths.StateDir,
		CacheDir:      a.Paths.CacheDir,
	}
	ov.RepoRoot, ov.RepoRootFrom = a.RepoRoot()
	// 代理状态拿不到不算致命：概览只是展示，后面的安装自己会再解析一次。
	if res, err := a.ProxyService().Active(); err == nil && res.Enabled() {
		ov.Proxy = res.Name
		if ov.Proxy == "" {
			ov.Proxy = string(res.Source)
		}
		ov.ProxyDetail = res.Detail
	}

	// 适用工具（含探测状态）与被排除的工具分开统计。
	applicable, err := a.ToolRows(ctx, ToolListOptions{})
	if err != nil {
		return InitOverview{}, err
	}
	all, err := a.ToolRows(ctx, ToolListOptions{All: true, NoProbe: true})
	if err != nil {
		return InitOverview{}, err
	}
	ov.Tools.Applicable = len(applicable)
	for _, r := range applicable {
		switch detect.Status(r.Status) {
		case detect.StatusInstalled:
			ov.Tools.Installed++
		case detect.StatusOutdated:
			ov.Tools.Outdated++
		case detect.StatusMissing:
			ov.Tools.Missing++
		default:
			ov.Tools.Unknown++
		}
	}
	ov.Tools.Excluded = len(all) - len(applicable)
	return ov, nil
}

// Init 执行"新机器一把梭"：概览 → 选工具 → 计划 →（确认）→ 执行 → 可选 shell 集成。
//
// 用户在确认时说不 → 返回 ui.ErrCancelled，调用方视为成功（退出码 0）。
// 需要确认而环境问不了 → 返回 ui.ErrNotInteractive，调用方映射成退出码 5。
func (a *App) Init(ctx context.Context, opt InitOptions) (InitResult, error) {
	if opt.Prompter == nil {
		opt.Prompter = ui.NonInteractive{}
	}
	res := InitResult{}

	overview, err := a.InitOverview(ctx)
	if err != nil {
		return res, err
	}
	res.Overview = overview
	if opt.OnOverview != nil {
		if err := opt.OnOverview(overview); err != nil {
			return res, err
		}
	}

	// 配置文件不存在时先补上：后面的清单/配置读取都依赖它。
	if !overview.ConfigExists {
		ok, err := a.askYes(opt, fmt.Sprintf("未找到配置文件 %s，现在生成一份默认配置？", overview.ConfigFile))
		if err != nil {
			return res, err
		}
		if ok {
			path, created, err := a.EnsureConfig(false)
			if err != nil {
				return res, err
			}
			res.ConfigCreated = created
			res.DryRunPlannedConfig = a.DryRun
			// 用户可见的提示由 cli 打（"已生成配置文件 …"）；这里只留调试信息，
			// 否则 `level=INFO msg=config …` 会混进正常输出里。
			a.Log.Debug("config", "file", path, "created", created, "dry_run", a.DryRun)
		}
	}

	if opt.NoTools {
		// 只做准备：概览（含"缺什么"的只读探测）已经给过，计划与执行整段跳过。
		res.Mode, res.SkipTools = SelectionSkipped, true
		return res, a.finishInit(opt, &res)
	}

	ids, profile, mode, err := a.chooseInitTools(ctx, opt, overview)
	if err != nil {
		return res, err
	}
	res.Mode, res.Profile, res.Tools = mode, profile, ids
	if mode == SelectionSkipped {
		res.SkipTools = true
		return res, a.finishInit(opt, &res)
	}

	if len(ids) == 0 && profile == "" {
		// 没有任何要处理的工具（例如"只补缺失"时一个都不缺）：不是错误。
		res.Mode = SelectionNone
		return res, a.finishInit(opt, &res)
	}

	plan, err := a.ToolPlan(ctx, ids, profile, install.OpInstall)
	if err != nil {
		return res, err
	}
	res.Plan = plan
	if opt.OnPlan != nil {
		if err := opt.OnPlan(plan); err != nil {
			return res, err
		}
	}
	if len(plan.Steps) == 0 {
		return res, a.finishInit(opt, &res)
	}
	if !a.DryRun {
		ok, err := a.askYes(opt, fmt.Sprintf("要执行上面的计划吗？（%d 个工具）", len(plan.Steps)))
		if err != nil {
			return res, err
		}
		if !ok {
			return res, ui.ErrCancelled
		}
	}

	report, err := a.ToolApply(ctx, plan, install.OpInstall)
	res.Report = &report
	if err != nil {
		// 部分失败也把结果交回（cli 按步骤渲染 + 退出码 1/3）。
		return res, err
	}
	if a.DryRun {
		// 干跑到这里结束：rc 文件一行都不动。
		return res, nil
	}
	return res, a.finishInit(opt, &res)
}

// finishInit 处理收尾：可选的 shell 受管块。
func (a *App) finishInit(opt InitOptions, res *InitResult) error {
	var installShell bool
	switch {
	case opt.ShellInstall != nil:
		installShell = *opt.ShellInstall
	case a.DryRun:
		// dry-run 不碰 rc 文件。
		return nil
	case opt.Prompter.Interactive() && !opt.AssumeYes:
		ok, err := a.askYes(opt, "要把 bs 的受管块写进 shell 启动文件吗？（代理命令即时生效 + 自动装配 PATH）")
		if err != nil {
			return err
		}
		installShell = ok
	default:
		return nil
	}
	if !installShell {
		res.SkippedShell = true
		return nil
	}
	result, err := a.ShellInstall(a.Platform.Shell, "")
	if err != nil {
		return err
	}
	res.Shell = &result
	return nil
}

// chooseInitTools 决定这次要处理哪些工具。
//
// 优先级：显式 id > 显式套装 > MissingOnly > 交互式向导 > 配置里的默认套装。
// 只在"真的能问"时才调用 Prompter，非交互环境下绝不阻塞。
func (a *App) chooseInitTools(ctx context.Context, opt InitOptions, overview InitOverview) (ids []string, profile string, mode InitSelectionMode, err error) {
	if len(opt.Tools) > 0 {
		return opt.Tools, "", SelectionGiven, nil
	}
	if opt.Profile != "" {
		return nil, opt.Profile, SelectionProfile, nil
	}
	if opt.MissingOnly {
		missing, err := a.missingIDs(ctx)
		if err != nil {
			return nil, "", "", err
		}
		return missing, "", SelectionMissing, nil
	}
	if !opt.Prompter.Interactive() {
		// 非交互：用配置的默认套装，行为可预测（脚本里加 --yes 即可全自动）。
		return nil, a.Cfg.Tool.DefaultProfile, SelectionProfile, nil
	}

	names, err := a.ProfileNames()
	if err != nil {
		return nil, "", "", err
	}
	entry, err := opt.Prompter.Select(ui.Question{
		Title: fmt.Sprintf("本机 %s/%s，适用工具 %d 个（已装 %d，缺失 %d）。想怎么装？",
			overview.OS, overview.Arch, overview.Tools.Applicable, overview.Tools.Installed, overview.Tools.Missing),
		Default: "profile",
	}, []ui.Option{
		{Value: "profile", Label: "按套装快速开始", Hint: "推荐；默认套装 " + a.Cfg.Tool.DefaultProfile},
		{Value: "missing", Label: "只补缺失的工具", Hint: fmt.Sprintf("%d 个", overview.Tools.Missing)},
		{Value: "manual", Label: "手工挑选", Hint: "逐个勾选"},
		{Value: "none", Label: "先不装工具", Hint: "只配好环境，之后随时 bs tool install"},
	})
	if err != nil {
		return nil, "", "", err
	}
	switch entry {
	case "none":
		return nil, "", SelectionSkipped, nil
	case "missing":
		missing, err := a.missingIDs(ctx)
		if err != nil {
			return nil, "", "", err
		}
		return missing, "", SelectionMissing, nil
	case "manual":
		ids, err := a.pickTools(ctx, opt, overview)
		return ids, "", SelectionManual, err
	default:
		profile, err := opt.Prompter.Select(ui.Question{
			Title:   "选一个套装",
			Default: a.Cfg.Tool.DefaultProfile,
		}, profileOptions(names))
		if err != nil {
			return nil, "", "", err
		}
		return nil, profile, SelectionProfile, nil
	}
}

// pickTools 让用户逐个勾选：缺失的默认勾上，已装的默认不勾（"补缺"而不是"全都重来"）。
func (a *App) pickTools(ctx context.Context, opt InitOptions, overview InitOverview) ([]string, error) {
	return a.PickTools(ctx, ToolListOptions{}, opt.Prompter, PickOptions{
		Title: fmt.Sprintf("选择要安装/修复的工具（%d 个适用，%d 个缺失）",
			overview.Tools.Applicable, overview.Tools.Missing),
		Question: "缺失的已默认勾选；编号用逗号/空格分隔，a=全选，回车=用默认，q=取消",
	})
}

// missingIDs 返回当前缺失（未安装）的适用工具 id。
func (a *App) missingIDs(ctx context.Context) ([]string, error) {
	rows, err := a.ToolRows(ctx, ToolListOptions{Missing: true})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// ProfileNames 返回清单里的套装名（已排序）。
func (a *App) ProfileNames() ([]string, error) {
	loaded, err := a.LoadedManifest()
	if err != nil {
		return nil, err
	}
	return loaded.ProfileNames(), nil
}

func profileOptions(names []string) []ui.Option {
	out := make([]ui.Option, 0, len(names))
	for _, n := range names {
		out = append(out, ui.Option{Value: n, Label: n})
	}
	return out
}

// askYes 统一"要不要"的问法：AssumeYes 直接放行；问不了时把 ui.ErrNotInteractive
// 交给 cli 映射成退出码 5。
func (a *App) askYes(opt InitOptions, title string) (bool, error) {
	if opt.AssumeYes {
		return true, nil
	}
	return opt.Prompter.Confirm(ui.Question{Title: title})
}

func distroLabel(p hostinfo.Platform) string {
	if p.Distro == "" {
		return ""
	}
	if p.DistroVer == "" {
		return p.Distro
	}
	return p.Distro + " " + p.DistroVer
}
