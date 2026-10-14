// Package install 是安装引擎：把清单里的声明变成"可预览的行动"并执行它。
//
// 结构（对应 docs/design.md §7）：
//
//	registry  —— via 名 → Installer 实现，同时为清单校验提供 schema
//	chain     —— 按清单顺序挑出第一个可用方法，并按策略决定是否降级
//	engine    —— Preflight → Select → Plan → Apply（拓扑序）→ Verify → Report
//
// 一条硬规则：**Installer 只描述做什么，执行只能通过 ExecEnv**。
// 这样所有 installer 的单测都能用假 ExecEnv 覆盖，不需要网络、sudo 或真实安装。
package install

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
)

// Command / Result 直接复用 cmdx，避免多一层无意义的类型转换。
type (
	Command = cmdx.Command
	Result  = cmdx.Result
)

// Op 是一次安装动作的类型。
type Op string

// 动作类型枚举。
// OpLabel 返回操作的中文名（用于确认提示等面向用户的文案）。
func OpLabel(op Op) string {
	switch op {
	case OpUpgrade:
		return "升级"
	case OpUninstall:
		return "卸载"
	default:
		return "安装"
	}
}

const (
	OpInstall   Op = "install"
	OpUpgrade   Op = "upgrade"
	OpUninstall Op = "uninstall"
)

// Download 描述一次下载。
type Download struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
	// Dest 为空时由 ExecEnv 在工作目录里决定落点。
	Dest string      `json:"dest,omitempty"`
	Mode os.FileMode `json:"-"`
}

// Action 是"将要发生什么"的完整描述，既能给人看（plan 输出），也能被执行。
type Action struct {
	ToolID    string     `json:"tool_id"`
	Via       string     `json:"via"`
	Op        Op         `json:"op"`
	Reason    string     `json:"reason,omitempty"`
	Commands  []Command  `json:"commands,omitempty"`
	Downloads []Download `json:"downloads,omitempty"`
	NeedsRoot bool       `json:"needs_root,omitempty"`
	Network   bool       `json:"network,omitempty"`
	// Summary 是一行人类可读的"将用方式"（表格里显示）。
	Summary string `json:"summary,omitempty"`

	// resolved 保存 Installer 私有数据（解析出的版本、解压参数等）。
	// 它不参与序列化：Action 的导出字段才是对外的稳定契约。
	resolved any
}

// WithResolved 附带 Installer 私有数据。
func (a Action) WithResolved(v any) Action {
	a.resolved = v
	return a
}

// Resolved 取出 Installer 私有数据。
func (a Action) Resolved() any { return a.resolved }

// ExecEnv 是安装过程中**唯一**允许接触系统的出口。
type ExecEnv interface {
	// Run 执行一条命令。
	Run(ctx context.Context, c Command) (Result, error)
	// Download 下载并进行 sha256 校验（若指定），返回落地路径。
	Download(ctx context.Context, d Download) (string, error)
	// FetchText 取回一个小文本（checksums.txt、VERSION 等）。
	FetchText(ctx context.Context, url string) (string, error)
	// LatestRelease 解析 GitHub 仓库最新 release tag。
	LatestRelease(ctx context.Context, repo string) (string, error)
	// LookPath 在"已装配的 PATH"里查找可执行文件。
	LookPath(name string) (string, bool)
	// Env 是注入给子进程的环境（已含代理）。
	Env() map[string]string
	// DryRun 为真时任何写操作都不允许发生。
	DryRun() bool
	// IsRoot 报告当前是否以 root 运行（决定是否加 sudo）。
	IsRoot() bool
	// OS 与 Arch 用于模板渲染（{{os}} / {{arch}}）。
	OS() string
	Arch() string
	// Distro 用于 when.distro 判断（非 Linux 为空）。
	Distro() string
	// HomeDir 用于展开 `~`。
	HomeDir() string
	// Versions 是配置 tool.versions 的映射（清单模板 `{{versions.golang}}` 用它）。
	Versions() map[string]string

	// WorkDir 是本次安装可用的临时目录（惰性：只算路径，不创建目录）。
	WorkDir() string
	// DownloadDir 是**跨次运行稳定**的下载缓存目录（惰性：只算路径，不创建目录）。
	//
	// 与 WorkDir 的关键区别：WorkDir 每次运行都不一样，下载到那里等于
	// 每次都从头再来；下载缓存要能"这次断了下次接着下"，所以必须稳定。
	DownloadDir() string
	// Log 用于结构化日志。
	Log() *slog.Logger
}

// Installer 是一种安装机制（manager / bun / apt / release / script ...）。
//
// 与设计文档 §7.1 的差异：不再用 Uninstaller/Upgrader 两个可选接口，
// 而是给 Plan 一个 Op 参数——三种动作的"计划"形态完全一致，
// 执行也共用一条路径，拆成三个接口只会让每个 installer 重复三份样板。
type Installer interface {
	// Name 与清单里的 via 对应。
	Name() string
	// Available 判断该机制对"这个工具的这条方法"是否可用
	// （可执行文件存在、包管理器存在、when 条件满足等）。
	Available(ctx context.Context, e ExecEnv, t manifest.Tool, m manifest.Method) bool
	// Plan 只做静态计算，不产生任何副作用。
	Plan(ctx context.Context, e ExecEnv, t manifest.Tool, m manifest.Method, op Op) (Action, error)
	// Execute 执行 Action。
	Execute(ctx context.Context, a Action, e ExecEnv) error
}

// ValidateParams 是可选的参数 schema 校验（Installer 可实现它）。
type ValidateParams interface {
	// Validate 校验 params 是否合法；返回的错误会出现在清单校验输出里。
	Validate(p manifest.Params) error
}

// ValidateParamsFor 是"一个实现覆盖多个 via"时用的参数校验接口。
type ValidateParamsFor interface {
	ValidateFor(via string, p manifest.Params) error
}

// systemVias 是需要 root 的系统包管理器（受 §7.2 的护栏约束）。
//
// brew/scoop 不在其中：它们是用户态、无 sudo、可回退的包管理器，
// 用它们装 versionmgr（如 macOS 上的 fnm）是正常做法。
var systemVias = map[string]bool{
	"apt": true, "dnf": true, "yum": true, "pacman": true,
	"zypper": true, "apk": true, "winget": true, "choco": true,
}

// Registry 保存全部 Installer 与版本管理器定义。
type Registry struct {
	order    []string
	byName   map[string]Installer
	managers map[string]ManagerSpec
	// versions 来自配置 tool.versions，模板校验时用它渲染 {{versions.*}}。
	versions map[string]string
}

// NewRegistry 注册全部内置安装器。
func NewRegistry() *Registry {
	r := &Registry{byName: map[string]Installer{}, managers: builtinManagers()}
	for _, in := range []Installer{
		newManagerInstaller(r),
		newNPMInstaller("bun", "bun", "add", "remove", "update"),
		newNPMInstaller("npm", "npm", "install", "uninstall", "update"),
		newNPMInstaller("pnpm", "pnpm", "add", "remove", "update"),
		newPipInstaller("pipx", "pipx", "install", "uninstall", "upgrade"),
		newPipInstaller("uvpm", "uv", "tool install", "tool uninstall", "tool upgrade"),
		newCargoInstaller(),
		newGoInstallInstaller(),
		newBrewInstaller(),
		newScriptInstaller(),
		newReleaseInstaller(),
		newSourceInstaller(),
	} {
		r.Register(in)
	}
	// 系统包管理器是"一个实现、多个 via"，逐个名字注册。
	sys := newSystemInstaller()
	for _, via := range sys.order {
		r.RegisterAs(via, sys)
	}
	return r
}

// SetVersions 注入配置 tool.versions（只影响模板校验的可用变量）。
func (r *Registry) SetVersions(v map[string]string) { r.versions = v }

// Register 注册一个 Installer（后注册的覆盖同名）。
func (r *Registry) Register(in Installer) {
	if _, ok := r.byName[in.Name()]; !ok {
		r.order = append(r.order, in.Name())
	}
	r.byName[in.Name()] = in
}

// RegisterAs 用指定的 via 名注册实现（用于一个实现覆盖多个 via 的情况）。
func (r *Registry) RegisterAs(via string, in Installer) {
	if _, ok := r.byName[via]; !ok {
		r.order = append(r.order, via)
	}
	r.byName[via] = in
}

// Get 按 via 名取 Installer。
func (r *Registry) Get(via string) (Installer, bool) {
	in, ok := r.byName[via]
	return in, ok
}

// KnownVias 返回全部已注册的 via（注册顺序）。
func (r *Registry) KnownVias() []string { return append([]string{}, r.order...) }

// IsSystemVia 报告该 via 是否属于"系统包管理器"。
func (r *Registry) IsSystemVia(via string) bool { return systemVias[via] }

// Managers 返回全部版本管理器定义（按名字）。
func (r *Registry) Managers() []ManagerSpec {
	out := make([]ManagerSpec, 0, len(r.managers))
	for _, name := range managerOrder {
		if m, ok := r.managers[name]; ok {
			out = append(out, m)
		}
	}
	return out
}

// Manager 按名字取版本管理器定义。
func (r *Registry) Manager(name string) (ManagerSpec, bool) {
	m, ok := r.managers[name]
	return m, ok
}

// ValidateMethod 实现 manifest.MethodValidator：校验 via 与 params。
func (r *Registry) ValidateMethod(via string, p manifest.Params) error {
	in, ok := r.byName[via]
	if !ok {
		return &UnknownViaError{Via: via, Known: r.KnownVias()}
	}
	if v, ok := in.(ValidateParamsFor); ok {
		if err := v.ValidateFor(via, p); err != nil {
			return err
		}
	} else if v, ok := in.(ValidateParams); ok {
		if err := v.Validate(p); err != nil {
			return err
		}
	}
	return validateTemplates(p, r.versions)
}

// ValidateManagerAction 实现 manifest.MethodValidator。
func (r *Registry) ValidateManagerAction(manager, action string) error {
	spec, ok := r.managers[manager]
	if !ok {
		names := make([]string, 0, len(r.managers))
		for _, name := range managerOrder {
			if _, ok := r.managers[name]; ok {
				names = append(names, name)
			}
		}
		return &UnknownManagerError{Manager: manager, Known: names}
	}
	if _, ok := spec.Actions[action]; !ok {
		return &UnsupportedActionError{Manager: manager, Action: action, Supported: spec.SupportedActions()}
	}
	return nil
}

// UnknownViaError 在清单写了不存在的 via 时返回，并列出可用项。
type UnknownViaError struct {
	Via   string
	Known []string
}

func (e *UnknownViaError) Error() string {
	return "unknown via " + e.Via + " (known: " + joinStrings(e.Known) + ")"
}

// UnknownManagerError 在清单写了不存在的版本管理器时返回。
type UnknownManagerError struct {
	Manager string
	Known   []string
}

func (e *UnknownManagerError) Error() string {
	return "unknown manager " + e.Manager + " (known: " + joinStrings(e.Known) + ")"
}

// UnsupportedActionError 在管理器不支持某动作时返回。
type UnsupportedActionError struct {
	Manager   string
	Action    string
	Supported []string
}

func (e *UnsupportedActionError) Error() string {
	return "manager " + e.Manager + " does not support action " + e.Action +
		" (supported: " + joinStrings(e.Supported) + ")"
}

// ErrUnsupported 表示某个 installer 不支持该动作（例如系统包管理器不支持由 bs 卸载）。
//
// Detail 是给用户看的补充说明（"为什么"以及"那该怎么办"），因为这句话会直接出现在
// `bs tool uninstall` 的表格里——只说"不支持"等于把问题丢回给用户。
type ErrUnsupported struct {
	Via    string
	Op     Op
	Detail string
}

func (e *ErrUnsupported) Error() string {
	msg := "via=" + e.Via + " cannot be used for " + string(e.Op)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// ErrDryRun 由 ExecEnv 在干跑模式下返回给"必须真正执行才能继续"的步骤。
var ErrDryRun = errDryRun{}

type errDryRun struct{}

func (errDryRun) Error() string { return "dry-run: command not executed" }

// Duration 便于测试里注入更短的超时。
type Duration = time.Duration

func joinStrings(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
