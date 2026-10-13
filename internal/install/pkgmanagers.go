package install

import (
	"context"
	"fmt"
	"strings"

	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// ── 包管理器族的通用实现 ──
//
// 这一族都是"某个 CLI 存在 → 用固定动词把包名交给它"，差异只在动词，
// 所以用一张表 + 一个结构体覆盖 bun/npm/pnpm/pipx/uv/cargo/goinstall/brew，
// 而不是每个写一个文件（"薄实现"的直接体现）。

type pkgManager struct {
	via            string
	bin            string
	installVerb    string
	useVerb        string
	upgradeVerb    string
	uninstallVerb  string
	supportsRemove bool
	// kind 用于生成人话摘要。
	label string
	// needsOneOf 是"包名参数"字段（package/crate/module/formula）。
	paramName string
	// network 标记该方式需要网络。
	network bool
}

// newNPMInstaller 覆盖 bun / npm / pnpm。
func newNPMInstaller(via, bin, installVerb, uninstallVerb, upgradeVerb string) *pkgManager {
	return &pkgManager{
		via: via, bin: bin,
		installVerb: installVerb, uninstallVerb: uninstallVerb, upgradeVerb: upgradeVerb,
		supportsRemove: true, paramName: "package", network: true, label: via,
	}
}

// newPipInstaller 覆盖 pipx 与 uv tool（Python CLI 的隔离安装）。
func newPipInstaller(via, bin, installVerb, uninstallVerb, upgradeVerb string) *pkgManager {
	m := newNPMInstaller(via, bin, installVerb, uninstallVerb, upgradeVerb)
	m.paramName = "package"
	return m
}

func newCargoInstaller() *pkgManager {
	return &pkgManager{
		via: "cargo", bin: "cargo",
		installVerb: "install", uninstallVerb: "uninstall", upgradeVerb: "install --force",
		supportsRemove: true, paramName: "crate", network: true, label: "cargo install",
	}
}

func newGoInstallInstaller() *pkgManager {
	return &pkgManager{
		via: "goinstall", bin: "go",
		installVerb: "install", upgradeVerb: "install",
		supportsRemove: false, paramName: "module", network: true, label: "go install",
	}
}

func newBrewInstaller() *pkgManager {
	return &pkgManager{
		via: "brew", bin: "brew",
		installVerb: "install", uninstallVerb: "uninstall", upgradeVerb: "upgrade",
		supportsRemove: true, paramName: "formula", network: true, label: "brew",
	}
}

// Name 实现 Installer。
func (p *pkgManager) Name() string { return p.via }

// Validate 实现 ValidateParams。
func (p *pkgManager) Validate(params manifest.Params) error {
	switch p.paramName {
	case "package":
		if strings.TrimSpace(params.Package) == "" {
			return fmt.Errorf("via=%s requires params.package", p.via)
		}
	case "crate":
		if strings.TrimSpace(params.Crate) == "" {
			return fmt.Errorf("via=%s requires params.crate", p.via)
		}
	case "module":
		if strings.TrimSpace(params.Module) == "" {
			return fmt.Errorf("via=%s requires params.module", p.via)
		}
	case "formula", "cask":
		if strings.TrimSpace(params.Formula) == "" && strings.TrimSpace(params.Cask) == "" {
			return fmt.Errorf("via=%s requires params.formula or params.cask", p.via)
		}
	}
	if p.via == "goinstall" && strings.Contains(params.Module, " ") {
		return fmt.Errorf("via=goinstall: params.module must not contain spaces")
	}
	return nil
}

// Available 实现 Installer。
func (p *pkgManager) Available(_ context.Context, e ExecEnv, _ manifest.Tool, _ manifest.Method) bool {
	_, ok := e.LookPath(p.bin)
	return ok
}

// Plan 实现 Installer。
func (p *pkgManager) Plan(_ context.Context, e ExecEnv, t manifest.Tool, m manifest.Method, op Op) (Action, error) {
	data := RenderData{Version: m.Params.Version, Tag: m.Params.Version, OS: e.OS(), Arch: e.Arch(), Versions: e.Versions()}
	global := m.Params.Global == nil || *m.Params.Global

	var verb string
	switch op {
	case OpInstall:
		verb = p.installVerb
	case OpUpgrade:
		verb = p.upgradeVerb
	case OpUninstall:
		if !p.supportsRemove {
			return Action{}, &ErrUnsupported{Via: p.via, Op: op}
		}
		verb = p.uninstallVerb
	default:
		return Action{}, &ErrUnsupported{Via: p.via, Op: op}
	}
	args := strings.Fields(verb)
	var commands []Command

	switch p.via {
	case "npm", "pnpm", "bun":
		if global {
			args = append(args, "-g")
		}
		if m.Params.Prefix != "" && op != OpUninstall {
			args = append(args, "--prefix", fsx.ExpandPath(renderOr(m.Params.Prefix, data), e.HomeDir()))
		}
		args = append(args, renderOr(m.Params.Package, data))
	case "pipx", "uvpm":
		// pipx install / uv tool install 天生是"用户级全局"，不需要 -g。
		if op != OpUninstall && m.Params.Python != "" {
			args = append(args, "--python", m.Params.Python)
		}
		args = append(args, renderOr(m.Params.Package, data))
	case "cargo":
		if len(m.Params.Features) > 0 && op != OpUninstall {
			args = append(args, "--features", strings.Join(m.Params.Features, ","))
		}
		if m.Params.Git != "" && op != OpUninstall {
			args = append(args, "--git", m.Params.Git)
		}
		if op == OpUpgrade {
			args = append(args, "--force")
		}
		args = append(args, renderOr(m.Params.Crate, data))
	case "goinstall":
		module := renderOr(m.Params.Module, data)
		ver := m.Params.Version
		if ver == "" || op == OpUpgrade {
			ver = "latest"
		}
		if strings.Contains(module, "@") {
			args = append(args, module)
		} else {
			args = append(args, module+"@"+ver)
		}
	case "brew":
		formula := renderOr(m.Params.Formula, data)
		if m.Params.Cask != "" {
			formula = renderOr(m.Params.Cask, data)
			if op != OpUninstall {
				args = append(args, "--cask")
			}
		}
		args = append(args, formula)
		if m.Params.Tap != "" && op == OpInstall {
			// tap 必须先于 install 执行。
			commands = append(commands, Command{Name: "brew", Args: []string{"tap", m.Params.Tap}})
		}
	default:
		return Action{}, &ErrUnsupported{Via: p.via, Op: op}
	}

	args = compact(args)
	commands = append(commands, Command{Name: p.bin, Args: args})
	return Action{
		ToolID: t.ID, Via: p.via, Op: op,
		Commands: commands,
		Network:  p.network,
		Summary:  p.label + " " + strings.Join(args, " "),
		Reason:   fmt.Sprintf("via=%s", p.via),
	}, nil
}

// Execute 实现 Installer。brew tap 需要先执行，这里统一处理。
func (p *pkgManager) Execute(ctx context.Context, a Action, e ExecEnv) error {
	return runCommands(ctx, a, e)
}

func renderOr(v string, d RenderData) string {
	if !ContainsTemplate(v) {
		return v
	}
	out, err := Render(v, d)
	if err != nil {
		return v
	}
	return out
}

func compact(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if strings.TrimSpace(a) == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}
