// Package manifest 定义工具清单的数据模型、分层加载与校验。
//
// 设计依据：docs/design.md §6。三条不可动摇的约定：
//   - 清单是**声明式**的：条目只说"要什么"，不说"怎么解压、怎么切 PATH"；
//   - 合并规则可预测：按 id 覆盖、数组整体替换（需要追加时用 `+install` 等显式语法）；
//   - 校验期把错误全部找出来（一次报全），而不是运行到一半才发现拼错字段。
package manifest

import (
	"regexp"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/pkg/version"
)

// Stack 是工具的技术栈归属（docs/design.md §6.1）。
type Stack string

// 技术栈枚举。与 config 里的 Kind 正交。
const (
	StackBase       Stack = "base"
	StackGolang     Stack = "golang"
	StackPython     Stack = "python"
	StackNodeJS     Stack = "nodejs"
	StackRust       Stack = "rust"
	StackCPP        Stack = "cpp"
	StackJava       Stack = "java"
	StackShell      Stack = "shell"
	StackContainers Stack = "containers"
	StackCloud      Stack = "cloud"
	StackInfra      Stack = "infra"
	StackEditor     Stack = "editor"
	StackAgent      Stack = "agent"
)

// AllStacks 是全部合法的 stack，用于校验。
var AllStacks = []Stack{
	StackBase, StackGolang, StackPython, StackNodeJS, StackRust, StackCPP,
	StackJava, StackShell, StackContainers, StackCloud, StackInfra, StackEditor, StackAgent,
}

// String 实现 fmt.Stringer（便于把它当作配置里的键）。
func (s Stack) String() string { return string(s) }

// ValidStack 报告 s 是否是合法 stack。
func ValidStack(s Stack) bool {
	for _, k := range AllStacks {
		if k == s {
			return true
		}
	}
	return false
}

// Manifest 是一个清单文件的内容。
type Manifest struct {
	Version  int                `yaml:"version"`
	Tools    []Tool             `yaml:"tools"`
	Profiles map[string]Profile `yaml:"profiles"`

	// Source 记录文件来源（便于报错与 `bs tool manifest diff`）。
	Source string `yaml:"-"`
}

// Tool 是一个工具条目。
type Tool struct {
	ID          string      `yaml:"id"`
	Name        string      `yaml:"name"`
	Stack       Stack       `yaml:"stack"`
	Kind        config.Kind `yaml:"kind"`
	Description string      `yaml:"description"`
	Homepage    string      `yaml:"homepage"`
	Tags        []string    `yaml:"tags"`

	// DependsOn 声明依赖的工具 id；安装按拓扑序执行。
	DependsOn []string `yaml:"depends_on"`
	// When 声明适用范围（不匹配则从当前平台的可见清单中隐藏）。
	When When `yaml:"when"`
	// Provides 是安装后应纳入 PATH 的路径（可含 ~）。
	Provides []string `yaml:"provides"`

	Verify Verify `yaml:"verify"`

	Install   []Method `yaml:"install"`
	Update    *Method  `yaml:"update"`
	Uninstall []Method `yaml:"uninstall"`

	// Disabled 为 true 表示整条禁用（用户覆盖时最常用的手段）。
	Disabled bool `yaml:"disabled"`
	// ManagerOptional 允许 runtime 条目没有 `via: manager` 路径（默认不允许，见 §6.4）。
	ManagerOptional bool `yaml:"manager_optional"`

	// ── 追加语法：只在用户覆盖文件里有意义 ──
	AppendInstall   []Method `yaml:"+install"`
	AppendUninstall []Method `yaml:"+uninstall"`
	AppendTags      []string `yaml:"+tags"`
	AppendProvides  []string `yaml:"+provides"`

	// Origin 记录这条工具最终由哪个文件定义/覆盖。
	Origin string `yaml:"-"`
}

// Enabled 报告该工具是否参与选择与安装。
func (t Tool) Enabled() bool { return !t.Disabled }

// Method 是一种安装方式（install/update/uninstall 的链上的一环）。
type Method struct {
	Via     string `yaml:"via"`
	When    When   `yaml:"when"`
	Enabled *bool  `yaml:"enabled"`
	Params  Params `yaml:"params"`
}

// IsEnabled 报告该方式是否可用（未显式声明即启用）。
func (m Method) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// When 是适用条件。空条件表示"总是适用"。
//
// `has` 是工具存在性条件（例如"机器上已有 bun 才用 bun 装"），
// 需要调用方提供 LookPath 能力，因此匹配逻辑放在 Matches 里由 MatchEnv 注入。
type When struct {
	OS        []string `yaml:"os"`
	OSNot     []string `yaml:"os_not"`
	Arch      []string `yaml:"arch"`
	Distro    []string `yaml:"distro"`
	DistroNot []string `yaml:"distro_not"`
	Has       []string `yaml:"has"`
	HasNot    []string `yaml:"has_not"`
}

// IsZero 报告条件是否为空（空条件总是匹配）。
func (w When) IsZero() bool {
	return len(w.OS) == 0 && len(w.OSNot) == 0 && len(w.Arch) == 0 &&
		len(w.Distro) == 0 && len(w.DistroNot) == 0 && len(w.Has) == 0 && len(w.HasNot) == 0
}

// MatchEnv 是匹配 When 所需的环境事实。
type MatchEnv struct {
	OS     string
	Arch   string
	Distro string
	// Has 报告某个可执行文件是否存在；nil 表示无法判断（此时 has 条件视为不匹配）。
	Has func(string) bool
}

// Matches 报告条件是否满足。不满足时返回的 reason 用于 plan/verbose 输出。
func (w When) Matches(env MatchEnv) (bool, string) {
	if len(w.OS) > 0 && !contains(w.OS, env.OS) {
		return false, "os=" + env.OS + " 不在 " + join(w.OS)
	}
	if contains(w.OSNot, env.OS) {
		return false, "os=" + env.OS + " 被排除"
	}
	if len(w.Arch) > 0 && !contains(w.Arch, env.Arch) {
		return false, "arch=" + env.Arch + " 不在 " + join(w.Arch)
	}
	if len(w.Distro) > 0 && !containsFold(w.Distro, env.Distro) {
		return false, "distro=" + env.Distro + " 不在 " + join(w.Distro)
	}
	if containsFold(w.DistroNot, env.Distro) && env.Distro != "" {
		return false, "distro=" + env.Distro + " 被排除"
	}
	for _, bin := range w.Has {
		if env.Has == nil || !env.Has(bin) {
			return false, "缺少依赖命令 " + bin
		}
	}
	for _, bin := range w.HasNot {
		if env.Has != nil && env.Has(bin) {
			return false, "已存在 " + bin + "（该路径不适用）"
		}
	}
	return true, ""
}

// Verify 描述"如何判断这个工具装好了、版本是多少"。
type Verify struct {
	Command      string   `yaml:"command"`
	VersionArgs  []string `yaml:"version_args"`
	VersionRegex string   `yaml:"version_regex"`
	MinVersion   string   `yaml:"min_version"`
	ExactVersion string   `yaml:"exact_version"`

	// 编译期产物（Load 时填充）。
	re    *regexp.Regexp
	min   *version.Version
	exact *version.Version
}

// Regex 返回编译好的版本正则（未声明则为 nil）。
func (v Verify) Regex() *regexp.Regexp { return v.re }

// Min 返回解析好的最低版本（未声明则为 nil）。
func (v Verify) Min() *version.Version { return v.min }

// Exact 返回解析好的精确版本（未声明则为 nil）。
func (v Verify) Exact() *version.Version { return v.exact }

// Params 是方法参数。不同 via 用到的字段不同，
// 每个 Installer 负责校验自己关心的字段（见 install.Installer.Validate）。
//
// 之所以用强类型而不是 map[string]any：拼错字段名必须在加载期就报错，
// 而不是在真正安装时才悄悄忽略。
type Params struct {
	// via: manager
	Manager string `yaml:"manager"`
	Action  string `yaml:"action"`
	Version string `yaml:"version"`
	Source  string `yaml:"source"`
	Global  *bool  `yaml:"global"`

	// via: bun / npm / pnpm / pipx / uvpm / cargo / goinstall
	Package  string   `yaml:"package"`
	Prefix   string   `yaml:"prefix"`
	Crate    string   `yaml:"crate"`
	Features []string `yaml:"features"`
	Git      string   `yaml:"git"`
	Module   string   `yaml:"module"`
	Python   string   `yaml:"python"`

	// via: brew
	Formula string `yaml:"formula"`
	Cask    string `yaml:"cask"`
	Tap     string `yaml:"tap"`

	// via: apt / dnf / pacman / winget / scoop / choco
	Packages []string `yaml:"packages"`
	// AllowSystem 显式放行"用系统包管理器装 runtime/versionmgr"（默认护栏会拒绝）。
	AllowSystem *bool  `yaml:"allow_system"`
	Repo        string `yaml:"repo"`
	NeedsRoot   *bool  `yaml:"needs_root"`
	ID          string `yaml:"id"`

	// via: script
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
	// Insecure 显式放弃 sha256 校验（官方脚本没有稳定校验和时才允许）。
	Insecure *bool    `yaml:"insecure"`
	Args     []string `yaml:"args"`
	Shell    string   `yaml:"shell"`

	// via: release
	Asset           string   `yaml:"asset"`
	VersionSource   string   `yaml:"version_source"`
	Dest            string   `yaml:"dest"`
	StripComponents int      `yaml:"strip_components"`
	Symlink         *Symlink `yaml:"symlink"`
	Checksum        string   `yaml:"checksum"`
	Binary          string   `yaml:"binary"`

	// via: source
	Build   string `yaml:"build"`
	Install string `yaml:"install"`

	// 逃生口：清单表达不了的怪命令（仍属于该方法的语义范围）。
	Command string `yaml:"command"`
}

// TemplateField 是一个可能含模板的参数字段。
type TemplateField struct {
	// Name 用 YAML 路径命名（例如 install[0].params.asset），便于定位问题。
	Name  string
	Value string
}

// TemplateFields 返回可能含模板的参数值。
//
// 安装引擎在清单校验期用它预渲染一遍，从而把"{{verison}} 这种拼写错误"
// 挡在 validate 阶段，而不是等到真正安装时才失败。
func (p Params) TemplateFields() []TemplateField {
	return []TemplateField{
		{"params.asset", p.Asset},
		{"params.version_source", p.VersionSource},
		{"params.dest", p.Dest},
		{"params.binary", p.Binary},
		{"params.url", p.URL},
		{"params.sha256", p.SHA256},
		{"params.package", p.Package},
		{"params.crate", p.Crate},
		{"params.module", p.Module},
		{"params.formula", p.Formula},
		{"params.cask", p.Cask},
		{"params.id", p.ID},
		{"params.version", p.Version},
		{"params.command", p.Command},
		{"params.build", p.Build},
		{"params.install", p.Install},
		{"params.prefix", p.Prefix},
		{"params.repo", p.Repo},
		{"params.source", p.Source},
		{"params.python", p.Python},
	}
}

// Symlink 描述安装后的软链：from 是真实路径，to 是暴露到 PATH 的路径。
type Symlink struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// Profile 是一个"套装"：一组工具 id，可继承其他 profile。
type Profile struct {
	Description string   `yaml:"description"`
	Extends     []string `yaml:"extends"`
	Tools       []string `yaml:"tools"`
}

// UnmarshalYAML 允许 profile 写成两种形式：
//
//	base: [git, curl]                       # 简写：纯列表
//	base: {description: ..., tools: [...]}  # 完整形式
func (p *Profile) UnmarshalYAML(unmarshal func(any) error) error {
	var list []string
	if err := unmarshal(&list); err == nil {
		p.Tools = list
		return nil
	}
	type raw Profile
	var r raw
	if err := unmarshal(&r); err != nil {
		return err
	}
	*p = Profile(r)
	return nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if equalFold(s, v) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func join(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += "/"
		}
		out += s
	}
	return out
}
