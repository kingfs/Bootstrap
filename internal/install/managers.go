package install

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/version"
)

// ManagerSpec 是"版本管理器"的命令模板表（docs/design.md §6.2）。
//
// 每个管理器只是一组命令模板：**没有**任何下载、解压、切 PATH 的逻辑——
// 那些都是管理器自己的事。bs 只负责"确保管理器在、把命令发出去、验证结果"。
type ManagerSpec struct {
	Name string
	// Bin 是探测用的可执行文件名。
	Bin string
	// NeedsShell 为真表示它是 shell 函数（如 nvm），需要先 source 脚本。
	NeedsShell bool
	// Source 是需要 source 的脚本路径（可含 ~）。
	Source string
	// Actions 是 action → 命令模板（模板可含 {{version}} 等变量）。
	Actions map[string]string
	// Stack 是该管理器的默认归属 stack（配置 managers.<stack> 缺省时用）。
	Stack string
}

// SupportedActions 返回支持的动作（稳定顺序）。
func (m ManagerSpec) SupportedActions() []string {
	var out []string
	for _, a := range managerActions {
		if _, ok := m.Actions[a]; ok {
			out = append(out, a)
		}
	}
	return out
}

// managerActions 是动作的稳定展示顺序。
var managerActions = []string{"install", "use", "upgrade", "uninstall"}

// managerOrder 是管理器的展示与查找顺序。
var managerOrder = []string{"g", "fnm", "nvm", "uv", "rustup", "mise", "pyenv"}

func builtinManagers() map[string]ManagerSpec {
	specs := []ManagerSpec{
		{
			Name: "g", Bin: "g", Stack: "golang",
			Actions: map[string]string{
				"install":     "g install {{version}}",
				"use":         "g use {{version}}",
				"upgrade":     "g upgrade",
				"uninstall":   "g uninstall {{version}}",
				"list-remote": "g ls-remote",
			},
		},
		{
			Name: "fnm", Bin: "fnm", Stack: "nodejs",
			Actions: map[string]string{
				"install":     "fnm install {{version}}",
				"use":         "fnm use {{version}}",
				"upgrade":     "fnm install {{version}}",
				"uninstall":   "fnm uninstall {{version}}",
				"list-remote": "fnm ls-remote",
			},
		},
		{
			// nvm 是 shell 函数而不是二进制：必须 source 之后才能调用。
			Name: "nvm", Bin: "nvm", NeedsShell: true, Source: "~/.nvm/nvm.sh", Stack: "nodejs",
			Actions: map[string]string{
				"install":     "nvm install {{version}}",
				"use":         "nvm use {{version}}",
				"upgrade":     "nvm install {{version}}",
				"uninstall":   "nvm uninstall {{version}}",
				"list-remote": "nvm ls-remote",
			},
		},
		{
			Name: "uv", Bin: "uv", Stack: "python",
			Actions: map[string]string{
				"install":     "uv python install {{version}}",
				"use":         "uv python pin {{version}}",
				"upgrade":     "uv python install --reinstall {{version}}",
				"uninstall":   "uv python uninstall {{version}}",
				"list-remote": "uv python list --all-versions",
			},
		},
		{
			Name: "rustup", Bin: "rustup", Stack: "rust",
			Actions: map[string]string{
				"install":     "rustup toolchain install {{version}}",
				"use":         "rustup default {{version}}",
				"upgrade":     "rustup update",
				"uninstall":   "rustup toolchain uninstall {{version}}",
				"list-remote": "rustup toolchain list",
			},
		},
		{
			Name: "mise", Bin: "mise", Stack: "base",
			Actions: map[string]string{
				"install":     "mise install {{version}}",
				"use":         "mise use -g {{version}}",
				"upgrade":     "mise upgrade",
				"uninstall":   "mise uninstall {{version}}",
				"list-remote": "mise ls-remote",
			},
		},
		{
			Name: "pyenv", Bin: "pyenv", Stack: "python",
			Actions: map[string]string{
				"install":     "pyenv install -s {{version}}",
				"use":         "pyenv global {{version}}",
				"upgrade":     "pyenv install -s {{version}}",
				"uninstall":   "pyenv uninstall -f {{version}}",
				"list-remote": "pyenv install -l",
			},
		},
	}
	out := make(map[string]ManagerSpec, len(specs))
	for _, s := range specs {
		out[s.Name] = s
	}
	return out
}

// managerInstaller 把 `via: manager` 落到具体管理器的命令模板上。
type managerInstaller struct {
	reg *Registry
}

func newManagerInstaller(r *Registry) *managerInstaller { return &managerInstaller{reg: r} }

// Name 实现 Installer。
func (m *managerInstaller) Name() string { return "manager" }

// Validate 实现 ValidateParams：manager/action 必须存在，install/uninstall 需要 version。
func (m *managerInstaller) Validate(p manifest.Params) error {
	action := p.Action
	if action == "" {
		return fmt.Errorf("via=manager requires params.action")
	}
	if _, ok := m.reg.Manager(p.Manager); !ok && p.Manager != "" {
		return &UnknownManagerError{Manager: p.Manager}
	}
	for _, a := range managerActions {
		if a == action {
			if (action == "install" || action == "uninstall") && strings.TrimSpace(p.Version) == "" {
				return fmt.Errorf("via=manager action=%s requires params.version", action)
			}
			return nil
		}
	}
	return fmt.Errorf("via=manager: unknown action %q (want one of %s)", action, joinStrings(managerActions))
}

// Available 报告该管理器是否可用。
func (m *managerInstaller) Available(_ context.Context, e ExecEnv, _ manifest.Tool, method manifest.Method) bool {
	spec, ok := m.reg.Manager(method.Params.Manager)
	if !ok {
		return false
	}
	return managerRunnable(e, spec)
}

// managerRunnable 报告管理器**现在**就能跑。
//
// 注意它和 `Plan` 里那个"稍后会就绪"的判断是两件事：`Available` 允许被 planEnv
// 覆盖（管理器可能就在本次计划的更早一步），而 Plan 只看真实环境。
func managerRunnable(e ExecEnv, spec ManagerSpec) bool {
	if spec.NeedsShell {
		// shell 函数型管理器：要求 source 脚本存在，且有 shell 可用。
		if _, ok := e.LookPath("sh"); !ok {
			return false
		}
		return fsx.Exists(fsx.ExpandPath(spec.Source, e.HomeDir()))
	}
	if spec.Bin == "" {
		return false
	}
	_, ok := e.LookPath(spec.Bin)
	return ok
}

// Plan 渲染命令模板。
func (m *managerInstaller) Plan(ctx context.Context, e ExecEnv, t manifest.Tool, method manifest.Method, op Op) (Action, error) {
	return m.plan(ctx, e, t, method, op, true)
}

// plan 是 Plan 的实现；allowDefer 允许把"解析 latest"推迟到执行前。
//
// 为什么要推迟：依赖是"先装管理器、再用管理器装 runtime"，而 `g ls-remote` 这样的
// 解析命令在计划期根本跑不了（g 还没装）。以前这里直接失败，于是新机器上
// `bs init` 里 golang/nodejs/python 全都会掉到发行版包或干脆报"没有可用方式"。
func (m *managerInstaller) plan(ctx context.Context, e ExecEnv, t manifest.Tool, method manifest.Method, op Op, allowDefer bool) (Action, error) {
	spec, ok := m.reg.Manager(method.Params.Manager)
	if !ok {
		return Action{}, fmt.Errorf("tool %q: via=manager needs params.manager (or tool.managers.%s)", t.ID, t.Stack)
	}
	template, ok := spec.Actions[string(op)]
	if !ok {
		return Action{}, &ErrUnsupported{Via: "manager:" + spec.Name, Op: op}
	}

	ver := method.Params.Version
	if ver == "" {
		ver = "latest"
	}
	deferResolve := false
	if (op == OpInstall || op == OpUpgrade) && (ver == "latest" || strings.EqualFold(ver, "lts")) {
		switch {
		case allowDefer && !e.DryRun() && !managerRunnable(realEnv(e), spec):
			// 管理器还没就绪（本次计划的更早一步会装它）。现在去跑 list-remote 只会
			// 拿到"命令不存在"，所以保留 latest 占位，执行到这一步时再解析。
			deferResolve = true
		default:
			// 只有真正要装的时候才去解析 latest；干跑时留占位符，避免"预览也打网络"。
			if resolved, err := m.resolveLatest(ctx, e, spec, method, ver); err == nil && resolved != "" {
				ver = resolved
			} else if !e.DryRun() && err != nil {
				return Action{}, err
			}
		}
	}
	data := RenderData{Version: ver, Tag: ver, OS: e.OS(), Arch: e.Arch(), Versions: e.Versions()}
	line, err := Render(template, data)
	if err != nil {
		return Action{}, fmt.Errorf("tool %q: %w", t.ID, err)
	}
	cmd, err := m.commandFor(spec, line, e)
	if err != nil {
		return Action{}, fmt.Errorf("tool %q: %w", t.ID, err)
	}

	notes := []string{"委托 " + spec.Name}
	if ver != "latest" && ver != "" {
		notes = append(notes, ver)
	}
	action := Action{
		ToolID: t.ID, Via: "manager", Op: op,
		Commands: []Command{cmd},
		Network:  true,
		Summary:  strings.Join(notes, " "),
		Reason:   fmt.Sprintf("via=manager: %s %s", spec.Name, op),
	}
	if deferResolve {
		action.Resolve = func(ctx context.Context, e ExecEnv) (Action, error) {
			return m.plan(ctx, e, t, method, op, false)
		}
	}
	return action, nil
}

// Execute 执行计划里的命令。
func (m *managerInstaller) Execute(ctx context.Context, a Action, e ExecEnv) error {
	return runCommands(ctx, a, e)
}

// versionRe 从管理器的 list-remote 输出里抽版本号。
//
// 用非捕获组：pkg/version.Extract 在存在捕获组时会返回第一个组，
// 而这里我们要的是完整版本号。
var versionRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)

// resolveLatest 用管理器的 list-remote 把 latest 解析成具体版本。
func (m *managerInstaller) resolveLatest(ctx context.Context, e ExecEnv, spec ManagerSpec, method manifest.Method, want string) (string, error) {
	listRemote, ok := spec.Actions["list-remote"]
	if !ok {
		return "", fmt.Errorf("manager %s cannot resolve %q (no list-remote)", spec.Name, want)
	}
	cmd, err := m.commandFor(spec, listRemote, e)
	if err != nil {
		return "", err
	}
	res, err := e.Run(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("resolve latest version via %s: %w", spec.Name, err)
	}
	// 取输出里最后一个版本号：g/fnm/nvm 的 list-remote 都是升序输出，
	// 最后一行即最新。清单里写了明确版本时不会走到这里（推荐写明确版本）。
	matches := versionRe.FindAllString(res.Stdout+"\n"+res.Stderr, -1)
	if len(matches) == 0 {
		return "", fmt.Errorf("resolve latest version via %s: no version found in output", spec.Name)
	}
	ver := matches[len(matches)-1]
	if _, perr := version.Parse(ver); perr != nil {
		return "", fmt.Errorf("resolve latest version via %s: parsed %q: %w", spec.Name, ver, perr)
	}
	return ver, nil
}

// commandFor 把渲染后的命令行变成可执行命令（nvm 这类需要 source 脚本）。
func (m *managerInstaller) commandFor(spec ManagerSpec, line string, e ExecEnv) (Command, error) {
	args, err := splitArgs(line)
	if err != nil {
		return Command{}, err
	}
	if len(args) == 0 {
		return Command{}, fmt.Errorf("empty command template")
	}
	if !spec.NeedsShell {
		return Command{Name: args[0], Args: args[1:]}, nil
	}
	source := fsx.ExpandPath(spec.Source, e.HomeDir())
	if source == "" {
		return Command{}, fmt.Errorf("manager %s needs a source script", spec.Name)
	}
	script := ". " + shellQuote(source) + " && " + line
	return Command{Name: "sh", Args: []string{"-c", script}}, nil
}
