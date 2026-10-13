package install

import (
	"context"
	"fmt"
	"strings"

	"github.com/kingfs/Bootstrap/internal/manifest"
)

// ── 系统包管理器（apt / dnf / pacman / zypper / apk，含 winget/scoop/choco）──
//
// 这一族都是"调用系统包管理器装包"，差异集中在命令与是否需要 root，
// 因此共用一份实现（设计文档 §7.3 的"共享基类"）。

type sysManager struct {
	via     string
	bin     string
	install []string
	update  []string
	remove  []string
	root    bool
	label   string
	// windows 标记 Windows 专有包管理器（用于提示）。
	windows bool
}

type systemInstaller struct {
	specs map[string]sysManager
	order []string
}

func newSystemInstaller() *systemInstaller {
	list := []sysManager{
		{via: "apt", bin: "apt-get", install: []string{"install", "-y"}, update: []string{"update"}, remove: []string{"remove", "-y"}, root: true, label: "apt-get install"},
		{via: "dnf", bin: "dnf", install: []string{"install", "-y"}, remove: []string{"remove", "-y"}, root: true, label: "dnf install"},
		{via: "yum", bin: "yum", install: []string{"install", "-y"}, remove: []string{"remove", "-y"}, root: true, label: "yum install"},
		{via: "pacman", bin: "pacman", install: []string{"-S", "--noconfirm", "--needed"}, remove: []string{"-R", "--noconfirm"}, root: true, label: "pacman -S"},
		{via: "zypper", bin: "zypper", install: []string{"--non-interactive", "install"}, remove: []string{"--non-interactive", "remove"}, root: true, label: "zypper install"},
		{via: "apk", bin: "apk", install: []string{"add"}, remove: []string{"del"}, root: true, label: "apk add"},
		{via: "winget", bin: "winget", install: []string{"install", "--exact", "--silent", "--accept-source-agreements", "--accept-package-agreements"}, remove: []string{"uninstall", "--exact", "--silent"}, label: "winget install", windows: true},
		{via: "scoop", bin: "scoop", install: []string{"install"}, remove: []string{"uninstall"}, label: "scoop install", windows: true},
		{via: "choco", bin: "choco", install: []string{"install", "-y"}, remove: []string{"uninstall", "-y"}, label: "choco install", windows: true},
	}
	s := &systemInstaller{specs: map[string]sysManager{}}
	for _, sp := range list {
		s.specs[sp.via] = sp
		s.order = append(s.order, sp.via)
	}
	return s
}

// Name 实现 Installer。系统包管理器是多个 via，这里返回一个占位名，
// 真正的分派按 method.Via 进行（见 Registry 的注册表结构）。
func (s *systemInstaller) Name() string { return "system" }

// Handles 报告该实现是否处理某个 via。
func (s *systemInstaller) Handles(via string) bool {
	_, ok := s.specs[via]
	return ok
}

// Validate 实现 ValidateParams（由 Registry 按 via 转发）。
func (s *systemInstaller) ValidateFor(via string, p manifest.Params) error {
	if _, ok := s.specs[via]; !ok {
		return &UnknownViaError{Via: via}
	}
	if p.ID != "" && via != "winget" && via != "scoop" && via != "choco" {
		return fmt.Errorf("via=%s: use params.packages instead of params.id", via)
	}
	if len(p.Packages) == 0 && p.ID == "" {
		return fmt.Errorf("via=%s requires params.packages", via)
	}
	for _, pkg := range p.Packages {
		if strings.TrimSpace(pkg) == "" {
			return fmt.Errorf("via=%s: params.packages contains an empty entry", via)
		}
		if strings.ContainsAny(pkg, " \t") {
			return fmt.Errorf("via=%s: package %q must not contain spaces", via, pkg)
		}
	}
	if p.Repo != "" {
		return fmt.Errorf("via=%s: params.repo is not supported yet", via)
	}
	return nil
}

// Available 实现 Installer。
func (s *systemInstaller) Available(_ context.Context, e ExecEnv, _ manifest.Tool, m manifest.Method) bool {
	spec, ok := s.specs[m.Via]
	if !ok {
		return false
	}
	_, ok = e.LookPath(spec.bin)
	return ok
}

// Plan 实现 Installer。
func (s *systemInstaller) Plan(_ context.Context, e ExecEnv, t manifest.Tool, m manifest.Method, op Op) (Action, error) {
	spec, ok := s.specs[m.Via]
	if !ok {
		return Action{}, &UnknownViaError{Via: m.Via}
	}
	var verb []string
	switch op {
	case OpInstall:
		verb = spec.install
	case OpUninstall:
		if m.Via == "pacman" || m.Via == "winget" || len(spec.remove) == 0 {
			// 卸载系统包不在 bs 的职责内（§7.6）：提示交给原生包管理器。
			return Action{}, &ErrUnsupported{Via: m.Via, Op: op}
		}
		verb = spec.remove
	default:
		// 升级系统包默认不做（"不需要及时升级"的定位，§7.6）。
		return Action{}, &ErrUnsupported{Via: m.Via, Op: op}
	}

	args := append([]string{}, verb...)
	targets := m.Params.Packages
	if m.Via == "winget" && m.Params.ID != "" {
		args = append(args, "--id", m.Params.ID)
	} else {
		args = append(args, targets...)
	}

	var commands []Command
	if op == OpInstall && len(spec.update) > 0 {
		// 只有 apt 需要显式 update；其余包管理器自己会刷新索引。
		update := Command{Name: spec.bin, Args: append([]string{}, spec.update...)}
		commands = append(commands, rootCommand(update, needsRootParam(m.Params.NeedsRoot, spec.root), e))
	}
	// plain 是不带 sudo 的形态：摘要按**实际动作**生成（卸载要显示 remove，
	// 不能永远显示 install 的 label），但 sudo 只在 JSON/-v 里可见（§16）。
	plain := Command{Name: spec.bin, Args: args}
	commands = append(commands, rootCommand(plain, needsRootParam(m.Params.NeedsRoot, spec.root), e))

	return Action{
		ToolID: t.ID, Via: m.Via, Op: op,
		Commands:  commands,
		NeedsRoot: needsRootParam(m.Params.NeedsRoot, spec.root) && !e.IsRoot(),
		Network:   true,
		Summary:   plain.String(),
		Reason:    fmt.Sprintf("via=%s（系统包管理器）", m.Via),
	}, nil
}

// Execute 实现 Installer。
func (s *systemInstaller) Execute(ctx context.Context, a Action, e ExecEnv) error {
	return runCommands(ctx, a, e)
}
