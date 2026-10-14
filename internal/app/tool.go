package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/internal/detect"
	"github.com/kingfs/Bootstrap/internal/install"
	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/internal/ui"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/tools"
)

// 本文件是 M2 的用例层：清单装载、工具清单/详情/搜索、计划与执行。
//
// 这里只做编排与"取数据"，不解析任何 YAML 细节（那是 internal/manifest 的事），
// 也不打印任何东西（那是 internal/cli 的事）。

// ToolListOptions 是 `bs tool list` 的过滤条件。
type ToolListOptions struct {
	Profile string
	Stack   string
	Kind    string
	Tags    []string
	Search  string
	// Installed / Missing 二选一，用于只看某种状态。
	Installed bool
	Missing   bool
	// All 为真时包含 disabled / when 不匹配本机的工具。
	All bool
	// Refresh 为真时忽略探测缓存。
	Refresh bool
	// NoProbe 为真时不做实时探测（只列清单，快）。
	NoProbe bool
}

// ToolRow 是列表里的一行。
type ToolRow struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Stack       string   `json:"stack"`
	Kind        string   `json:"kind"`
	Tags        []string `json:"tags,omitempty"`
	Status      string   `json:"status"`
	Version     string   `json:"version,omitempty"`
	Detail      string   `json:"detail,omitempty"`
	ViaDisplay  string   `json:"via,omitempty"`
	Disabled    bool     `json:"disabled,omitempty"`
	NotMatching bool     `json:"not_matching,omitempty"`
	Description string   `json:"description,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	Provides    []string `json:"provides,omitempty"`
	Source      string   `json:"source,omitempty"`
}

// LoadedManifest 装载清单（进程内缓存一次）。
func (a *App) LoadedManifest() (*manifest.Loaded, error) {
	if a.loaded != nil {
		return a.loaded, nil
	}
	loaded, err := manifest.Load(a.manifestOptions())
	if err != nil {
		return nil, err
	}
	a.loaded = loaded
	return loaded, nil
}

func (a *App) manifestOptions() manifest.LoadOptions {
	opt := manifest.LoadOptions{
		Builtin:   a.Cfg.Tool.Manifest.Builtin,
		BuiltinFS: tools.Manifests,
		GOOS:      a.manifestGOOS(),
	}
	for _, dir := range a.Cfg.Tool.Manifest.UserDirs {
		opt.UserDirs = append(opt.UserDirs, fsx.ExpandPath(dir, a.Paths.HomeDir))
	}
	return opt
}

// ToolRegistry 返回安装器注册表（同时实现清单校验用的 schema）。
func (a *App) ToolRegistry() *install.Registry {
	reg := install.NewRegistry()
	// 模板校验期要用配置里的 tool.versions 渲染 {{versions.*}}。
	reg.SetVersions(a.Cfg.Tool.Versions)
	return reg
}

// ValidateManifest 校验清单，并把安装引擎的 schema 一起接上。
func (a *App) ValidateManifest() (*manifest.Loaded, error) {
	loaded, err := a.LoadedManifest()
	if err != nil {
		return nil, err
	}
	return loaded, a.ValidateManifestChecked(loaded)
}

// ValidateManifestChecked 校验一份已经装载好的清单。
func (a *App) ValidateManifestChecked(loaded *manifest.Loaded) error {
	opt := manifest.ValidateOptions{
		Vias:                     a.ToolRegistry(),
		AllowSystemFor:           allowedKinds(a.Cfg.Tool.AllowSystemFor),
		Managers:                 a.Cfg.Tool.Managers,
		RequireManagerForRuntime: true,
		Versions:                 a.Cfg.Tool.Versions,
	}
	return manifest.Validate(loaded, opt)
}

// detectOptions 构造探测器。
//
// 探测命令、PATH 查找、超时、并发都在这里统一装配，命令层不需要知道细节。
func (a *App) detector() *detect.Detector {
	// 探测**始终**真实执行，包括 --dry-run：它是只读的版本查询，
	// 而"计划"必须反映机器真实状态，否则预览会把已装的工具全列成"要安装"。
	probe := *a.Runner
	probe.DryRun = false
	// 查找规则必须与安装期一致（`extra_path` + 清单 `path_dirs` + 家目录下的 bin）：
	// 否则新机器上"刚装进 ~/.local/bin 的工具"会因为还没进 shell PATH 而复核失败——
	// 明明装好了却报 failed，或者 `bs tool list` 把它显示成 missing。
	lookup, _ := a.installEnv().(*install.OSEnv)
	return &detect.Detector{
		Executor:    &probe,
		LookPath:    lookup,
		HomeDir:     a.Paths.HomeDir,
		Timeout:     withDefault(a.timeouts().Probe, detect.ProbeTimeout),
		Concurrency: detect.MaxConcurrency,
		Cache:       &detect.Cache{Path: filepath.Join(a.Paths.CacheDir, "probe.json")},
	}
}

// ToolRows 返回经过过滤的工具列表。
func (a *App) ToolRows(ctx context.Context, opt ToolListOptions) ([]ToolRow, error) {
	loaded, err := a.LoadedManifest()
	if err != nil {
		return nil, err
	}
	selected, err := a.selectByProfile(loaded, opt.Profile)
	if err != nil {
		return nil, err
	}

	env := a.installEnv()
	rows := make([]ToolRow, 0, len(selected))
	for _, t := range selected {
		row := ToolRow{
			ID: t.ID, Name: t.Name, Stack: t.Stack.String(), Kind: string(t.Kind),
			Tags: t.Tags, Disabled: t.Disabled, Description: t.Description,
			Homepage: t.Homepage, Provides: t.Provides, Source: t.Origin,
			ViaDisplay: preferredVia(t, manifestEnv(env), a.Cfg.Tool.Managers),
		}
		if matched, reason := t.When.Matches(manifestEnv(env)); !matched {
			row.NotMatching = true
			row.Detail = reason
		}
		rows = append(rows, row)
	}

	if !opt.NoProbe {
		var probeTools []manifest.Tool
		for _, t := range selected {
			if t.Disabled {
				continue
			}
			probeTools = append(probeTools, t)
		}
		if len(probeTools) > 0 {
			results, derr := a.detector().Detect(ctx, probeTools, detect.Options{
				// 干跑不写缓存：--dry-run 的语义是"磁盘上什么都不留下"。
				UseCache: !a.DryRun, Refresh: opt.Refresh && !a.DryRun,
			})
			if derr != nil {
				return nil, derr
			}
			for i := range rows {
				if r, ok := results[rows[i].ID]; ok {
					rows[i].Status = string(r.Status)
					rows[i].Version = r.Version
					if rows[i].Detail == "" {
						rows[i].Detail = r.Detail
					}
				}
			}
		}
	}

	out := rows[:0]
	for _, row := range rows {
		if !opt.All && (row.Disabled || row.NotMatching) {
			continue
		}
		if opt.Stack != "" && row.Stack != opt.Stack {
			continue
		}
		if opt.Kind != "" && row.Kind != opt.Kind {
			continue
		}
		if len(opt.Tags) > 0 && !hasAllTags(row.Tags, opt.Tags) {
			continue
		}
		if opt.Search != "" && !matchesSearch(row, opt.Search) {
			continue
		}
		switch {
		case opt.Installed && row.Status != string(detect.StatusInstalled):
			continue
		case opt.Missing && row.Status != string(detect.StatusMissing):
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// ToolInfo 返回单个工具的详细信息（含方法链与探测结果）。
func (a *App) ToolInfo(ctx context.Context, id string, refresh bool) (*ToolDetail, error) {
	loaded, err := a.LoadedManifest()
	if err != nil {
		return nil, err
	}
	tool, ok := loaded.ByID(id)
	if !ok {
		return nil, &UnknownToolError{ID: id, Known: loaded.IDs()}
	}
	env := a.installEnv()
	reg := a.ToolRegistry()
	detail := &ToolDetail{
		ToolRow: ToolRow{
			ID: tool.ID, Name: tool.Name, Stack: tool.Stack.String(), Kind: string(tool.Kind),
			Tags: tool.Tags, Disabled: tool.Disabled, Description: tool.Description,
			Homepage: tool.Homepage, Provides: tool.Provides, Source: tool.Origin,
			ViaDisplay: preferredVia(tool, manifestEnv(env), a.Cfg.Tool.Managers),
		},
		Verify: verifySummary(tool),
	}
	chain, cerr := install.ResolveChain(ctx, reg, env, tool, install.ChainOptions{
		Via: a.defaultVia(), Managers: a.Cfg.Tool.Managers,
	})
	for i, m := range tool.Install {
		entry := MethodEntry{
			Index: i, Via: m.Via, Display: displayVia(m, tool.Stack.String(), a.Cfg.Tool.Managers), Enabled: m.IsEnabled(),
			Params: summarizeParams(m.Params),
		}
		if cerr == nil {
			for _, cand := range chain.Candidates {
				if cand.Index == i {
					entry.Available = cand.Available
					entry.Skip = cand.Skip
					entry.Note = cand.Note
				}
			}
			// ToolInfo 展示的是"这个工具实际会走哪条路"，因此用链的选择结果标注。
			entry.Preferred = chain.Selected == i
		}
		detail.Methods = append(detail.Methods, entry)
	}
	if !tool.Disabled {
		res := a.detector().Probe(ctx, tool)
		detail.Status = string(res.Status)
		detail.Version = res.Version
		if detail.Detail == "" {
			detail.Detail = res.Detail
		}
		detail.CheckedAt = res.CheckedAt
	}
	detail.DependsOn = tool.DependsOn
	detail.Dependents = loaded.Dependents(tool.ID)
	return detail, nil
}

// ToolDetail 是 `bs tool info` 的结构化结果。
type ToolDetail struct {
	ToolRow
	Verify     VerifySummary `json:"verify"`
	Methods    []MethodEntry `json:"methods"`
	DependsOn  []string      `json:"depends_on,omitempty"`
	Dependents []string      `json:"dependents,omitempty"`
	CheckedAt  time.Time     `json:"checked_at,omitempty"`
}

// VerifySummary 描述"怎么判断装没装"。
type VerifySummary struct {
	Command      string   `json:"command,omitempty"`
	VersionArgs  []string `json:"version_args,omitempty"`
	VersionRegex string   `json:"version_regex,omitempty"`
	MinVersion   string   `json:"min_version,omitempty"`
	ExactVersion string   `json:"exact_version,omitempty"`
}

// MethodEntry 是方法链里的一条。
type MethodEntry struct {
	Index int    `json:"index"`
	Via   string `json:"via"`
	// Display 是给人看的名字（via=manager 时带上管理器名，如 manager:g）。
	Display   string            `json:"display"`
	Enabled   bool              `json:"enabled"`
	Available bool              `json:"available"`
	Preferred bool              `json:"preferred"`
	Skip      string            `json:"skip,omitempty"`
	Note      string            `json:"note,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
}

// UnknownToolError 在工具 id 不存在时返回，并列出可用 id。
type UnknownToolError struct {
	ID    string
	Known []string
}

func (e *UnknownToolError) Error() string {
	if len(e.Known) == 0 {
		return "unknown tool " + e.ID
	}
	return "unknown tool " + e.ID + " (known: " + strings.Join(e.Known, ", ") + ")"
}

// PickOptions 控制"让用户勾选工具"的展示方式。
type PickOptions struct {
	// Title 是提问标题；留空时由 PickTools 依据候选数量生成。
	Title string
	// Question 是纯文本实现里的提示行（huh 界面下不显示）。
	Question string
	// PreSelected 决定默认勾选规则；为 nil 时"非已安装"默认勾选。
	PreSelected func(ToolRow) bool
}

// PickTools 让用户从候选工具里勾选，返回选中的 id（`bs init` 与 `tool install -i` 共用）。
//
// 只做"取候选行 → 组装选项 → 提问"三件事：候选过滤复用 ToolRows，
// 默认勾选规则可注入，交互通道由调用方给（非交互环境下 Prompter 会返回 ErrNotInteractive）。
// disabled / 本机不适用的条目不进候选——让用户选一个"选了也不能装"的东西只会添乱。
func (a *App) PickTools(ctx context.Context, opt ToolListOptions, p ui.Prompter, pick PickOptions) ([]string, error) {
	if p == nil {
		p = ui.NonInteractive{}
	}
	rows, err := a.ToolRows(ctx, opt)
	if err != nil {
		return nil, err
	}
	rows = sortToolRows(rows)
	options := make([]ui.Option, 0, len(rows))
	for _, r := range rows {
		if r.Disabled || r.NotMatching {
			continue
		}
		preselect := r.Status != string(detect.StatusInstalled)
		if pick.PreSelected != nil {
			preselect = pick.PreSelected(r)
		}
		options = append(options, ui.Option{
			Value:    r.ID,
			Label:    r.ID,
			Hint:     toolRowHint(r),
			Selected: preselect,
		})
	}
	if len(options) == 0 {
		return nil, errors.New("no applicable tool found in manifest")
	}
	title := pick.Title
	if title == "" {
		title = fmt.Sprintf("选择要处理的工具（%d 个候选）", len(options))
	}
	question := pick.Question
	if question == "" {
		question = "缺失/可升级的已默认勾选；编号用逗号/空格分隔，a=全选，回车=用默认，q=取消"
	}
	return p.MultiSelect(ui.MultiSelect{
		Title:       title,
		Description: question,
		Options:     options,
		MinSelected: 1,
	})
}

// sortToolRows 让勾选列表稳定可读：先按技术栈，再按 id。
func sortToolRows(rows []ToolRow) []ToolRow {
	out := append([]ToolRow(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Stack != out[j].Stack {
			return out[i].Stack < out[j].Stack
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// toolRowHint 给一行工具配一句人话（装了/可升级时带上版本）。
func toolRowHint(r ToolRow) string {
	switch {
	case r.Status == string(detect.StatusInstalled) && r.Version != "":
		return "已装 " + r.Version
	case r.Status == string(detect.StatusOutdated) && r.Version != "":
		return "可升级 " + r.Version
	default:
		return r.Stack
	}
}

// ToolPlan 计算安装/升级/卸载计划。
func (a *App) ToolPlan(ctx context.Context, ids []string, profile string, op install.Op) (install.Plan, error) {
	loaded, err := a.LoadedManifest()
	if err != nil {
		return install.Plan{}, err
	}
	ids, err = a.resolveIDs(loaded, ids, profile)
	if err != nil {
		return install.Plan{}, err
	}
	eng, err := a.engine(loaded)
	if err != nil {
		return install.Plan{}, err
	}
	return eng.PlanTools(ctx, ids, op)
}

// ToolApply 执行计划。
func (a *App) ToolApply(ctx context.Context, plan install.Plan, op install.Op) (install.Report, error) {
	loaded, err := a.LoadedManifest()
	if err != nil {
		return install.Report{}, err
	}
	eng, err := a.engine(loaded)
	if err != nil {
		return install.Report{}, err
	}
	return eng.Apply(ctx, plan, op)
}

// engine 装配安装引擎：ExecEnv 是唯一接触系统的出口，代理环境在这里注入。
func (a *App) engine(loaded *manifest.Loaded) (*install.Engine, error) {
	env := a.installEnv()
	var rec *install.Recorder
	logDir := ""
	if !a.DryRun {
		rec = &install.Recorder{Path: filepath.Join(a.Paths.StateDir, "installed.yaml")}
		// 只有真执行时才准备日志目录（--dry-run 必须零写入）。
		logDir = a.Paths.StateDir
	}
	return install.NewEngine(install.EngineOptions{
		Registry:   a.ToolRegistry(),
		Env:        env,
		Loaded:     loaded,
		Detector:   a.detector(),
		Managers:   a.Cfg.Tool.Managers,
		DefaultVia: a.defaultVia(),
		Fallback:   a.fallbackEnabled(),
		Versions:   a.Cfg.Tool.Versions,
		Recorder:   rec,
		LogDir:     logDir,
		OnStep:     a.Progress,
		Now:        Now,
	})
}

// installEnv 构造真实执行环境。
func (a *App) installEnv() install.ExecEnv {
	return install.NewOSEnv(install.OSEnvOptions{
		Env:       mergeMaps(a.Runner.Env, a.NetworkEnv()),
		ExtraPath: a.extraPathDirs(),
		HomeDir:   a.Paths.HomeDir,
		GOOS:      a.manifestGOOS(),
		Arch:      a.Platform.Arch,
		Distro:    a.Platform.Distro,
		DryRun:    a.DryRun,
		WorkRoot:  a.Paths.CacheDir,
		CacheDir:  a.Paths.CacheDir,
		Versions:  a.Cfg.Tool.Versions,
		Log:       a.Log,
		Runner:    a.Runner,
		// 时限留空 = 用各环节内置默认值（见 config.TimeoutConfig 的注释）。
		LookupTimeout:   a.timeouts().Lookup,
		DownloadTimeout: a.timeouts().Download,
	})
}

// timeouts 返回解析后的时限配置（已在 config.Validate 阶段校验过，这里解析失败就退回默认值）。
func (a *App) timeouts() config.Timeouts {
	t, err := a.Cfg.ResolvedTimeouts()
	if err != nil {
		a.Log.Warn("timeouts config ignored", "err", err)
		return config.Timeouts{}
	}
	return t
}

// withDefault 让"0 = 用内置默认值"这条约定只写一次。
func withDefault(override, fallback time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return fallback
}

// extraPathDirs 是参与 LookPath 的额外目录（配置 + 约定俗成的用户 bin 目录）。
func (a *App) extraPathDirs() []string {
	var out []string
	// 与 `bs env path` 用同一份来源：配置 extra_path + 清单里的 path_dirs。
	// 两边不一致会导致"PATH 里有它、但安装期查找看不到"这类灵异现象。
	for _, e := range a.PathEntries() {
		out = append(out, e.Path)
	}
	if home := a.Paths.HomeDir; home != "" {
		out = append(out,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
			filepath.Join(home, ".g", "versions", "current", "bin"),
		)
	}
	return out
}

// fallbackEnabled 决定失败后是否允许换安装方式（ADR-0013）。
//
// 交互式终端默认开（能自救就自救），非交互 / CI / --json 默认关
// （绝不静默换方式，避免"本想用 bun 装，结果悄悄用 apt 装了个旧版本"）。
func (a *App) fallbackEnabled() bool {
	if a.Fallback != nil {
		return *a.Fallback
	}
	if a.JSON || a.Yes || a.NonInteractive {
		return false
	}
	return a.DryRun || isTerminal(a.Out)
}

func (a *App) defaultVia() string {
	if a.DefaultViaOverride != "" {
		return a.DefaultViaOverride
	}
	if len(a.Cfg.Tool.DefaultVia) > 0 {
		return a.Cfg.Tool.DefaultVia[0]
	}
	return ""
}

// selectByProfile 按 profile 过滤。
//
// 只认显式传入的 profile：`bs tool list` 默认应该让人看到全部条目（这是
// 查询类命令），default_profile 只影响"不指定任何选择条件时的安装行为"。
func (a *App) selectByProfile(loaded *manifest.Loaded, profile string) ([]manifest.Tool, error) {
	if profile == "" {
		return loaded.Tools, nil
	}
	ids, err := loaded.ResolveProfile(profile)
	if err != nil {
		return nil, err
	}
	out := make([]manifest.Tool, 0, len(ids))
	for _, id := range ids {
		if tool, ok := loaded.ByID(id); ok {
			out = append(out, tool)
		}
	}
	return out, nil
}

// resolveIDs 把"用户给的选择"补全成完整的 id 列表（含依赖与 profile）。
func (a *App) resolveIDs(loaded *manifest.Loaded, ids []string, profile string) ([]string, error) {
	if len(ids) == 0 {
		if profile == "" {
			profile = a.Cfg.Tool.DefaultProfile
		}
		if profile != "" {
			return loaded.ResolveProfile(profile)
		}
		return nil, errors.New("no tool selected: pass tool ids or --profile")
	}
	// 支持 `bs tool install <前缀>/*` 与模糊名：前缀唯一时自动补全。
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		resolved, err := a.expandID(loaded, id)
		if err != nil {
			return nil, err
		}
		for _, r := range resolved {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func (a *App) expandID(loaded *manifest.Loaded, id string) ([]string, error) {
	if _, ok := loaded.ByID(id); ok {
		return []string{id}, nil
	}
	if strings.HasSuffix(id, "/*") || strings.HasSuffix(id, "*") {
		prefix := strings.TrimSuffix(strings.TrimSuffix(id, "*"), "/")
		var out []string
		for _, t := range loaded.Tools {
			if strings.HasPrefix(t.ID, prefix) {
				out = append(out, t.ID)
			}
		}
		if len(out) == 0 {
			return nil, &UnknownToolError{ID: id, Known: loaded.IDs()}
		}
		return out, nil
	}
	return nil, &UnknownToolError{ID: id, Known: loaded.IDs()}
}

// ToolIDs 返回清单里的全部 id（按 stack 再按 id 排序，输出稳定）。
func (a *App) ToolIDs() ([]string, error) {
	loaded, err := a.LoadedManifest()
	if err != nil {
		return nil, err
	}
	ids := loaded.IDs()
	sort.Strings(ids)
	return ids, nil
}

func manifestEnv(env install.ExecEnv) manifest.MatchEnv {
	return manifest.MatchEnv{
		OS:     env.OS(),
		Arch:   env.Arch(),
		Distro: env.Distro(),
		Has:    func(name string) bool { _, ok := env.LookPath(name); return ok },
	}
}

func displayVia(m manifest.Method, stack string, managers map[string]string) string {
	if m.Via == "manager" {
		name := m.Params.Manager
		if name == "" && managers != nil {
			// 清单没写 manager 时由配置决定（§6.2），展示时也要说清楚是哪个管理器。
			name = managers[stack]
		}
		if name != "" {
			return "manager:" + name
		}
	}
	return m.Via
}

// preferredVia 返回"本机上会走的那种方式"：
// 跳过禁用方法与 when 不匹配的方法（否则 Linux 上会显示 macOS 才用的 brew）。
func preferredVia(t manifest.Tool, env manifest.MatchEnv, managers map[string]string) string {
	for _, m := range t.Install {
		if !m.IsEnabled() {
			continue
		}
		if matched, _ := m.When.Matches(env); !matched {
			continue
		}
		return displayVia(m, t.Stack.String(), managers)
	}
	if len(t.Install) > 0 {
		return t.Install[0].Via
	}
	return ""
}

func verifySummary(t manifest.Tool) VerifySummary {
	return VerifySummary{
		Command:      t.Verify.Command,
		VersionArgs:  t.Verify.VersionArgs,
		VersionRegex: t.Verify.VersionRegex,
		MinVersion:   t.Verify.MinVersion,
		ExactVersion: t.Verify.ExactVersion,
	}
}

// FormatParams 把参数渲染成稳定的 "k=v k=v" 文本（键排序，便于比较与展示）。
func FormatParams(params map[string]string) string {
	keys := slices.Sorted(maps.Keys(params))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, " ")
}

// summarizeParams 把参数压成"人看得懂"的键值对（脱敏与截断）。
func summarizeParams(p manifest.Params) map[string]string {
	out := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	set("manager", p.Manager)
	set("action", p.Action)
	set("version", p.Version)
	set("package", p.Package)
	set("crate", p.Crate)
	set("module", p.Module)
	set("formula", p.Formula)
	set("cask", p.Cask)
	set("tap", p.Tap)
	if len(p.Packages) > 0 {
		set("packages", strings.Join(p.Packages, " "))
	}
	set("repo", p.Repo)
	set("asset", p.Asset)
	set("dest", p.Dest)
	set("id", p.ID)
	set("url", p.URL)
	set("shell", p.Shell)
	set("build", p.Build)
	set("install", p.Install)
	if len(p.Features) > 0 {
		set("features", strings.Join(p.Features, ","))
	}
	if p.AllowSystem != nil && *p.AllowSystem {
		set("allow_system", "true")
	}
	return out
}

func hasAllTags(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if strings.EqualFold(h, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func matchesSearch(row ToolRow, q string) bool {
	q = strings.ToLower(q)
	for _, field := range []string{row.ID, row.Name, row.Description, row.Stack, row.Kind, strings.Join(row.Tags, " ")} {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	return false
}

// ManifestUserDirs 返回实际参与加载的用户覆盖目录（已展开 ~）。

// manifestGOOS 返回清单加载时使用的 GOOS（--os 覆盖优先）。
func (a *App) manifestGOOS() string {
	if a.GOOSOverride != "" {
		return a.GOOSOverride
	}
	return a.Platform.OS
}

func (a *App) ManifestUserDirs() []string {
	var out []string
	for _, dir := range a.Cfg.Tool.Manifest.UserDirs {
		out = append(out, fsx.ExpandPath(dir, a.Paths.HomeDir))
	}
	if len(out) == 0 {
		out = append(out, a.Paths.ToolsDir())
	}
	return out
}

// ManifestDiff 对比某个 YAML 文件与当前合并结果的差异。
//
// 用途：把本地草稿清单和"实际生效"的清单比一比，确认覆盖是否按预期合并。
func (a *App) ManifestDiff(_ context.Context, path string) (*ManifestDiffResult, error) {
	filePath := fsx.ExpandPath(path, a.Paths.HomeDir)
	single, err := manifest.LoadFile(filePath)
	if err != nil {
		return nil, err
	}
	merged, err := a.LoadedManifest()
	if err != nil {
		return nil, err
	}
	out := &ManifestDiffResult{File: filePath}
	for _, t := range single.Tools {
		other, ok := merged.ByID(t.ID)
		if !ok {
			out.Added = append(out.Added, t.ID)
			continue
		}
		if detail := describeToolDiff(other, t); detail != "" {
			out.Changed = append(out.Changed, ToolChange{ID: t.ID, Detail: detail})
		}
	}
	sort.Strings(out.Added)
	return out, nil
}

// ManifestDiffResult 描述两个清单的差异。
type ManifestDiffResult struct {
	File    string       `json:"file"`
	Added   []string     `json:"added,omitempty"`
	Changed []ToolChange `json:"changed,omitempty"`
}

// ToolChange 是一个工具的关键字段差异。
type ToolChange struct {
	ID     string `json:"id"`
	Detail string `json:"detail"`
}

// describeToolDiff 只比较"会影响安装结果"的字段，避免无意义的噪声。
func describeToolDiff(a, b manifest.Tool) string {
	var diffs []string
	if a.Stack != b.Stack {
		diffs = append(diffs, fmt.Sprintf("stack %s → %s", a.Stack, b.Stack))
	}
	if a.Kind != b.Kind {
		diffs = append(diffs, fmt.Sprintf("kind %s → %s", a.Kind, b.Kind))
	}
	if a.Verify.Command != b.Verify.Command {
		diffs = append(diffs, fmt.Sprintf("verify.command %q → %q", a.Verify.Command, b.Verify.Command))
	}
	if a.Disabled != b.Disabled {
		diffs = append(diffs, fmt.Sprintf("disabled %v → %v", a.Disabled, b.Disabled))
	}
	if len(a.Install) != len(b.Install) {
		diffs = append(diffs, fmt.Sprintf("install 方法数 %d → %d", len(a.Install), len(b.Install)))
	} else {
		for i := range a.Install {
			if a.Install[i].Via != b.Install[i].Via {
				diffs = append(diffs, fmt.Sprintf("install[%d].via %s → %s", i, a.Install[i].Via, b.Install[i].Via))
				continue
			}
			// via 相同但参数不同（比如版本从 1.24.1 变成 1.80）同样会改变安装结果。
			pa, pb := summarizeParams(a.Install[i].Params), summarizeParams(b.Install[i].Params)
			if !maps.Equal(pa, pb) {
				diffs = append(diffs, fmt.Sprintf("install[%d].params %s → %s", i, FormatParams(pa), FormatParams(pb)))
			}
		}
	}
	if strings.Join(a.DependsOn, ",") != strings.Join(b.DependsOn, ",") {
		diffs = append(diffs, "depends_on 不同")
	}
	return strings.Join(diffs, "; ")
}
