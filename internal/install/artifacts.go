package install

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/pkg/archive"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/ghrelease"
)

// ── script / release / source：三种"bs 自己动手"的安装方式 ──
//
// 它们与前两族的区别：需要下载、解压、落盘，因此必须严格保证
//   - sha256 校验（拿不到校验文件就中止）；
//   - 干跑不产生任何写操作；
//   - 只写用户目录，绝不静默 sudo。

// ── via: script ──

type scriptInstaller struct{}

func newScriptInstaller() *scriptInstaller { return &scriptInstaller{} }

// Name 实现 Installer。
func (s *scriptInstaller) Name() string { return "script" }

// Validate 实现 ValidateParams。官方脚本必须固化 sha256，除非显式 insecure。
func (s *scriptInstaller) Validate(p manifest.Params) error {
	if strings.TrimSpace(p.URL) == "" {
		return fmt.Errorf("via=script requires params.url")
	}
	if strings.TrimSpace(p.SHA256) == "" && !boolValue(p.Insecure) {
		return fmt.Errorf("via=script requires params.sha256 (or params.insecure: true to opt out)")
	}
	return nil
}

// Available 实现 Installer。
func (s *scriptInstaller) Available(_ context.Context, e ExecEnv, _ manifest.Tool, m manifest.Method) bool {
	shell := scriptShell(m)
	_, ok := e.LookPath(shell)
	return ok
}

// Plan 实现 Installer。
func (s *scriptInstaller) Plan(_ context.Context, e ExecEnv, t manifest.Tool, m manifest.Method, op Op) (Action, error) {
	if op == OpUninstall {
		return Action{}, &ErrUnsupported{Via: "script", Op: op}
	}
	shell := scriptShell(m)
	dest := filepath.Join(e.WorkDir(), t.ID+"-install.sh")
	args := append([]string{dest}, m.Params.Args...)
	dl := Download{URL: renderOr(m.Params.URL, RenderData{OS: e.OS(), Arch: e.Arch()}), SHA256: m.Params.SHA256, Dest: dest, Mode: 0o700}
	return Action{
		ToolID: t.ID, Via: "script", Op: op,
		Commands:  []Command{{Name: shell, Args: args}},
		Downloads: []Download{dl},
		Network:   true,
		Summary:   "官方脚本 " + shortURL(dl.URL),
		Reason:    "via=script（sha256 已固化）",
	}.WithResolved(dl), nil
}

// Execute 实现 Installer。
func (s *scriptInstaller) Execute(ctx context.Context, a Action, e ExecEnv) error {
	if e.DryRun() {
		return nil
	}
	dl, _ := a.Resolved().(Download)
	if dl.URL != "" {
		if _, err := e.Download(ctx, dl); err != nil {
			return err
		}
	}
	return runCommands(ctx, a, e)
}

func scriptShell(m manifest.Method) string {
	if m.Params.Shell != "" {
		return m.Params.Shell
	}
	return "sh"
}

// ── via: release ──

type releaseInstaller struct{}

func newReleaseInstaller() *releaseInstaller { return &releaseInstaller{} }

// Name 实现 Installer。
func (r *releaseInstaller) Name() string { return "release" }

// Validate 实现 ValidateParams。
func (r *releaseInstaller) Validate(p manifest.Params) error {
	if strings.TrimSpace(p.Repo) == "" {
		return fmt.Errorf("via=release requires params.repo (owner/name)")
	}
	if strings.TrimSpace(p.Asset) == "" {
		return fmt.Errorf("via=release requires params.asset (a template like g{{version}}-{{os}}.tar.gz)")
	}
	if strings.TrimSpace(p.Dest) == "" {
		return fmt.Errorf("via=release requires params.dest")
	}
	switch p.Checksum {
	case "", "sha256", "none":
	default:
		return fmt.Errorf("via=release: params.checksum %q must be sha256 or none", p.Checksum)
	}
	return nil
}

// Available 实现 Installer：release 只需要网络能力。
func (r *releaseInstaller) Available(context.Context, ExecEnv, manifest.Tool, manifest.Method) bool {
	return true
}

type releasePlan struct {
	Download Download
	Dest     string
	Strip    int
	Binary   string
	Symlink  *manifest.Symlink
	Archive  bool
}

// Plan 实现 Installer。
func (r *releaseInstaller) Plan(ctx context.Context, e ExecEnv, t manifest.Tool, m manifest.Method, op Op) (Action, error) {
	if op == OpUninstall {
		return Action{}, &ErrUnsupported{Via: "release", Op: op}
	}
	tag, version, err := r.resolveVersion(ctx, e, m)
	if err != nil {
		return Action{}, fmt.Errorf("tool %q: %w", t.ID, err)
	}

	data := RenderData{Version: version, Tag: tag, OS: e.OS(), Arch: e.Arch(), Versions: map[string]string{}}
	asset, err := Render(m.Params.Asset, data)
	if err != nil {
		return Action{}, fmt.Errorf("tool %q: params.asset: %w", t.ID, err)
	}
	url := ghrelease.AssetURL(m.Params.Repo, tag, asset)
	dest := fsx.ExpandPath(renderOr(m.Params.Dest, data), e.HomeDir())

	sha := ""
	if m.Params.Checksum != "none" {
		if tag == latestPlaceholder {
			// 干跑且无法解析最新版本时，不做校验和查询，避免预览阶段报错。
			sha = ""
		} else {
			text, ferr := e.FetchText(ctx, ghrelease.ChecksumsURL(m.Params.Repo, tag))
			if ferr != nil {
				return Action{}, fmt.Errorf("tool %q: cannot fetch checksums.txt for %s@%s: %w", t.ID, m.Params.Repo, tag, ferr)
			}
			sha, ferr = ghrelease.ChecksumFor(text, asset)
			if ferr != nil {
				return Action{}, fmt.Errorf("tool %q: %w", t.ID, ferr)
			}
		}
	}

	binary := m.Params.Binary
	if binary == "" {
		binary = filepath.Base(strings.Trim(m.Params.Repo, "/"))
	}
	plan := releasePlan{
		Download: Download{URL: url, SHA256: sha, Dest: filepath.Join(e.WorkDir(), asset), Mode: 0o644},
		Dest:     dest,
		Strip:    m.Params.StripComponents,
		Binary:   binary,
		Symlink:  m.Params.Symlink,
		Archive:  archive.IsArchive(asset),
	}
	return Action{
		ToolID: t.ID, Via: "release", Op: op,
		Downloads: []Download{plan.Download},
		Network:   true,
		Summary:   fmt.Sprintf("release %s@%s → %s", m.Params.Repo, tag, dest),
		Reason:    "via=release（GitHub Release 二进制）",
	}.WithResolved(plan), nil
}

// Execute 实现 Installer。
func (r *releaseInstaller) Execute(ctx context.Context, a Action, e ExecEnv) error {
	plan, ok := a.Resolved().(releasePlan)
	if !ok {
		return fmt.Errorf("release installer: missing plan data")
	}
	if e.DryRun() {
		return nil
	}
	local, err := e.Download(ctx, plan.Download)
	if err != nil {
		return err
	}
	if err := fsx.EnsureDir(plan.Dest, 0o755); err != nil {
		return err
	}

	source := local
	if plan.Archive {
		extractDir := filepath.Join(e.WorkDir(), "extract-"+a.ToolID)
		files, err := archive.Extract(local, extractDir, plan.Strip)
		if err != nil {
			return fmt.Errorf("extract %s: %w", filepath.Base(local), err)
		}
		found, ok := archive.FindBinary(files, plan.Binary)
		if !ok {
			return fmt.Errorf("%s 中没有找到可执行文件 %s", filepath.Base(local), plan.Binary)
		}
		source = found
	}

	target := filepath.Join(plan.Dest, executableName(plan.Binary, e.OS()))
	if err := copyExecutable(source, target); err != nil {
		return err
	}
	if plan.Symlink != nil {
		from := fsx.ExpandPath(plan.Symlink.From, e.HomeDir())
		to := fsx.ExpandPath(plan.Symlink.To, e.HomeDir())
		if err := replaceSymlink(from, to); err != nil {
			return err
		}
	}
	e.Log().Info("installed", "tool", a.ToolID, "path", target)
	return nil
}

// resolveVersion 解析 release 的版本来源。
func (r *releaseInstaller) resolveVersion(ctx context.Context, e ExecEnv, m manifest.Method) (tag, version string, err error) {
	src := strings.TrimSpace(m.Params.VersionSource)
	switch {
	case src == "" || src == "latest-release":
		if e.DryRun() {
			// 预览阶段不打网络：给出占位符，真正的解析在 Apply 时发生。
			if v := m.Params.Version; v != "" {
				return normalizeTag(v)
			}
			return latestPlaceholder, latestPlaceholder, nil
		}
		t, terr := e.LatestRelease(ctx, m.Params.Repo)
		if terr != nil {
			return "", "", fmt.Errorf("resolve latest release: %w", terr)
		}
		return normalizeTag(t)
	case strings.Contains(src, "://"):
		text, ferr := e.FetchText(ctx, src)
		if ferr != nil {
			return "", "", fmt.Errorf("read version from %s: %w", src, ferr)
		}
		v, ok := versionFromText(text)
		if !ok {
			return "", "", fmt.Errorf("no version found in %s", src)
		}
		return normalizeTag(v)
	default:
		return normalizeTag(src)
	}
}

// latestPlaceholder 是干跑时无法确定最新版本时的占位符。
const latestPlaceholder = "<latest>"

func normalizeTag(v string) (tag, version string, err error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", "", fmt.Errorf("empty version")
	}
	if v == latestPlaceholder {
		return latestPlaceholder, latestPlaceholder, nil
	}
	return v, strings.TrimPrefix(v, "v"), nil
}

func versionFromText(text string) (string, bool) {
	if m := versionRe.FindString(text); m != "" {
		return m, true
	}
	return "", false
}

// ── via: source ──

type sourceInstaller struct{}

func newSourceInstaller() *sourceInstaller { return &sourceInstaller{} }

// Name 实现 Installer。
func (s *sourceInstaller) Name() string { return "source" }

// Validate 实现 ValidateParams。
func (s *sourceInstaller) Validate(p manifest.Params) error {
	if p.Repo == "" && p.URL == "" {
		return fmt.Errorf("via=source requires params.repo or params.url")
	}
	if p.Build == "" && p.Install == "" && p.Command == "" {
		return fmt.Errorf("via=source requires params.build, params.install or params.command")
	}
	return nil
}

// Available 实现 Installer。
func (s *sourceInstaller) Available(_ context.Context, e ExecEnv, _ manifest.Tool, _ manifest.Method) bool {
	_, ok := e.LookPath("git")
	return ok
}

// Plan 实现 Installer。
func (s *sourceInstaller) Plan(_ context.Context, e ExecEnv, t manifest.Tool, m manifest.Method, op Op) (Action, error) {
	if op == OpUninstall {
		return Action{}, &ErrUnsupported{Via: "source", Op: op}
	}
	dir := filepath.Join(e.WorkDir(), "src-"+t.ID)
	url := m.Params.URL
	if url == "" {
		url = "https://github.com/" + strings.Trim(m.Params.Repo, "/") + ".git"
	}
	commands := []Command{{Name: "git", Args: []string{"clone", "--depth", "1", url, dir}}}
	if m.Params.Build != "" {
		commands = append(commands, Command{Name: "sh", Args: []string{"-c", m.Params.Build}, Dir: dir})
	}
	if m.Params.Install != "" {
		commands = append(commands, Command{Name: "sh", Args: []string{"-c", m.Params.Install}, Dir: dir})
	}
	if m.Params.Command != "" {
		commands = append(commands, Command{Name: "sh", Args: []string{"-c", renderOr(m.Params.Command, RenderData{OS: e.OS(), Arch: e.Arch()})}, Dir: dir})
	}
	return Action{
		ToolID: t.ID, Via: "source", Op: op,
		Commands: commands,
		Network:  true,
		Summary:  "源码编译 " + shortURL(url),
		Reason:   "via=source（最后兜底）",
	}, nil
}

// Execute 实现 Installer。
func (s *sourceInstaller) Execute(ctx context.Context, a Action, e ExecEnv) error {
	return runCommands(ctx, a, e)
}

// shortURL 把 URL 缩短到 host/最后一段，便于表格显示。
func shortURL(raw string) string {
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, "https://"), "/")
	if i := strings.Index(raw, "?"); i >= 0 {
		raw = raw[:i]
	}
	parts := strings.Split(raw, "/")
	if len(parts) <= 3 {
		return raw
	}
	return parts[0] + "/…/" + parts[len(parts)-1]
}

func boolValue(b *bool) bool { return b != nil && *b }

func executableName(name, goos string) string {
	if goos != "windows" {
		return name
	}
	if filepath.Ext(name) != "" {
		return name
	}
	return name + ".exe"
}
