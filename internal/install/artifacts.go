package install

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
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
	// 落到下载缓存（而不是本次运行的临时目录）：脚本很小，但"同一个脚本重复下载"
	// 完全没必要，而且校验和一致时可以直接复用。
	dest := filepath.Join(e.DownloadDir(), t.ID+"-install.sh")
	args := append([]string{dest}, m.Params.Args...)
	dl := Download{URL: renderOr(m.Params.URL, RenderData{OS: e.OS(), Arch: e.Arch(), Versions: e.Versions()}), SHA256: m.Params.SHA256, Dest: dest, Mode: 0o700}
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
	Symlinks []manifest.Symlink
	Archive  bool
	// Tree 为真时整包解压到 Dest（目录型发布物），否则只取 Binary。
	Tree bool
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

	data := RenderData{Version: version, Tag: tag, OS: e.OS(), Arch: e.Arch(), Versions: e.Versions()}
	asset, err := Render(m.Params.Asset, data)
	if err != nil {
		return Action{}, fmt.Errorf("tool %q: params.asset: %w", t.ID, err)
	}
	url := ghrelease.AssetURL(m.Params.Repo, tag, asset)
	dest := fsx.ExpandPath(renderOr(m.Params.Dest, data), e.HomeDir())

	sha, err := r.checksumFor(ctx, e, t, m, tag, asset)
	if err != nil {
		return Action{}, err
	}

	binary := m.Params.Binary
	if binary == "" {
		binary = filepath.Base(strings.Trim(m.Params.Repo, "/"))
	}
	plan := releasePlan{
		// 归档放在跨次运行稳定的下载缓存里：断了能续，装过了能直接复用（见 httpx.Download）。
		Download: Download{URL: url, SHA256: sha, Dest: filepath.Join(e.DownloadDir(), asset), Mode: 0o644},
		Dest:     dest,
		Strip:    m.Params.StripComponents,
		Binary:   binary,
		Symlinks: releaseSymlinks(m.Params),
		Archive:  archive.IsArchive(asset),
		Tree:     m.Params.Extract != nil && *m.Params.Extract,
	}
	if plan.Tree && !plan.Archive {
		return Action{}, fmt.Errorf("tool %q: params.extract needs an archive asset, got %q", t.ID, asset)
	}
	summary := fmt.Sprintf("release %s@%s → %s", m.Params.Repo, tag, dest)
	if plan.Tree {
		summary = fmt.Sprintf("release %s@%s 解压到 %s", m.Params.Repo, tag, dest)
	}
	return Action{
		ToolID: t.ID, Via: "release", Op: op,
		Downloads: []Download{plan.Download},
		Network:   true,
		Summary:   summary,
		Reason:    "via=release（GitHub Release 二进制）",
	}.WithResolved(plan), nil
}

// checksumFor 解析产物的 sha256。
//
// 顺序：显式固化 → 显式文件名 → 常见文件名 → `<asset>.sha256`。
// 拿不到校验和就报错（AGENTS.md §8.3：宁可失败，也不装一个来路不明的东西）。
func (r *releaseInstaller) checksumFor(ctx context.Context, e ExecEnv, t manifest.Tool, m manifest.Method, tag, asset string) (string, error) {
	if m.Params.Checksum == "none" {
		return "", nil
	}
	if p := strings.TrimSpace(m.Params.SHA256); p != "" {
		return validateSHA256(t.ID, p)
	}
	if tag == latestPlaceholder {
		// 干跑且无法解析最新版本时，不做校验和查询，避免预览阶段报错。
		return "", nil
	}
	data := RenderData{Version: strings.TrimPrefix(tag, "v"), Tag: tag, OS: e.OS(), Arch: e.Arch(),
		Versions: e.Versions()}
	var candidates []string
	if m.Params.ChecksumFile != "" {
		name, rerr := Render(m.Params.ChecksumFile, data)
		if rerr != nil {
			return "", fmt.Errorf("tool %q: params.checksum_file: %w", t.ID, rerr)
		}
		candidates = append(candidates, name)
	} else {
		candidates = append(candidates, ghrelease.ChecksumFileNames...)
	}
	var tried []string
	for _, name := range candidates {
		text, ferr := e.FetchText(ctx, ghrelease.AssetURL(m.Params.Repo, tag, name))
		if ferr != nil {
			// 区分"这个文件不存在"与"取不到这个文件"：
			// 网络不通时如果说成"找不到校验和"，会把人引向错误的排查方向。
			tried = append(tried, fmt.Sprintf("%s (fetch failed: %v)", name, firstLineOf(ferr.Error())))
			continue
		}
		sha, cerr := ghrelease.ChecksumFor(text, asset)
		if cerr != nil {
			tried = append(tried, name+"(无 "+asset+" 条目)")
			continue
		}
		return validateSHA256(t.ID, sha)
	}
	// 最后试 Rust 生态的 `<asset>.sha256`（文件里通常只有一个裸哈希）
	if !slices.Contains(candidates, asset+".sha256") {
		if text, ferr := e.FetchText(ctx, ghrelease.AssetURL(m.Params.Repo, tag, asset+".sha256")); ferr == nil {
			if sha, ok := ghrelease.BareChecksum(text); ok {
				return validateSHA256(t.ID, sha)
			}
		}
	}
	return "", fmt.Errorf("tool %q: cannot find a sha256 for %s in %s@%s (tried %s); "+
		"set params.checksum_file, params.sha256, or params.checksum: none to opt out",
		t.ID, asset, m.Params.Repo, tag, strings.Join(tried, ", "))
}

func validateSHA256(toolID, sha string) (string, error) {
	sha = strings.ToLower(strings.TrimSpace(sha))
	if len(sha) != 64 || !hexSHA256.MatchString(sha) {
		return "", fmt.Errorf("tool %q: invalid sha256 %q", toolID, sha)
	}
	return sha, nil
}

var hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

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
		// 目录型发布物（Go 工具链、nvm）直接落到 dest，不再去挑单个可执行文件。
		if plan.Tree {
			extractDir = plan.Dest
		}
		files, err := archive.Extract(local, extractDir, plan.Strip)
		if err != nil {
			return fmt.Errorf("extract %s: %w", filepath.Base(local), err)
		}
		if len(files) == 0 {
			// strip_components 写大了会把整个归档"剥"没（归档本身没有外层目录时最常见）。
			// 这时 dest 会是空的，必须当场说清楚，别让它变成"装了个寂寞"。
			return fmt.Errorf("extract %s: nothing left after stripping %d path component(s); "+
				"the archive probably has no wrapping directory", filepath.Base(local), plan.Strip)
		}
		if plan.Tree {
			e.Log().Info("extracted", "tool", a.ToolID, "files", len(files), "dest", plan.Dest)
			return applySymlink(plan, e)
		}
		found, ok := archive.FindBinary(files, plan.Binary)
		if !ok {
			// 把归档里真实存在的顶层条目报出来：清单里 strip_components/binary
			// 写错时，作者一眼就能看出该怎么改。
			return fmt.Errorf("%s 中没有找到可执行文件 %s（解出 %d 个文件，顶层：%s）",
				filepath.Base(local), plan.Binary, len(files), topLevelList(files))
		}
		source = found
	}

	target := filepath.Join(plan.Dest, executableName(plan.Binary, e.OS()))
	if err := copyExecutable(source, target); err != nil {
		return err
	}
	if err := applySymlink(plan, e); err != nil {
		return err
	}
	e.Log().Info("installed", "tool", a.ToolID, "path", target)
	return nil
}

// releaseSymlinks 合并单数/复数两种写法（单数保留是为了兼容早期清单）。
func releaseSymlinks(p manifest.Params) []manifest.Symlink {
	out := append([]manifest.Symlink{}, p.Symlinks...)
	if p.Symlink != nil {
		out = append(out, *p.Symlink)
	}
	return out
}

// applySymlink 建立"真实路径 → PATH 里的名字"的软链（可选）。
func applySymlink(plan releasePlan, e ExecEnv) error {
	for _, l := range plan.Symlinks {
		from := fsx.ExpandPath(l.From, e.HomeDir())
		to := fsx.ExpandPath(l.To, e.HomeDir())
		if err := replaceSymlink(from, to); err != nil {
			return err
		}
	}
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
	if p.Build == "" && p.Install == "" && p.Command == "" && p.Dest == "" {
		return fmt.Errorf("via=source requires params.build, params.install, params.command or params.dest")
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
	cloneArgs := []string{"clone", "--depth", "1"}
	if m.Params.Ref != "" {
		// 固定到 tag/commit：源码编译同样要可复现（比跟 HEAD 安全）。
		cloneArgs = append(cloneArgs, "--branch", m.Params.Ref)
	}
	if m.Params.Dest != "" {
		// 直接克隆到目标目录（nvm 这类"仓库即安装物"的工具）。
		dir = fsx.ExpandPath(renderOr(m.Params.Dest, RenderData{OS: e.OS(), Arch: e.Arch(), Versions: e.Versions()}), e.HomeDir())
	}
	cloneArgs = append(cloneArgs, url, dir)
	commands := []Command{{Name: "git", Args: cloneArgs}}
	if m.Params.Build != "" {
		commands = append(commands, Command{Name: "sh", Args: []string{"-c", m.Params.Build}, Dir: dir})
	}
	if m.Params.Install != "" {
		commands = append(commands, Command{Name: "sh", Args: []string{"-c", m.Params.Install}, Dir: dir})
	}
	if m.Params.Command != "" {
		commands = append(commands, Command{Name: "sh", Args: []string{"-c", renderOr(m.Params.Command, RenderData{OS: e.OS(), Arch: e.Arch(), Versions: e.Versions()})}, Dir: dir})
	}
	summary := "源码编译 " + shortURL(url)
	if m.Params.Dest != "" && m.Params.Build == "" && m.Params.Install == "" {
		summary = "git 克隆 " + shortURL(url) + " → " + dir
	}
	return Action{
		ToolID: t.ID, Via: "source", Op: op,
		Commands: commands,
		Network:  true,
		Summary:  summary,
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

// firstLineOf 只取错误的第一行：终端表格与 JSON 里都不需要多行栈式错误。
// topLevelList 汇总解出来的顶层条目（目录名或文件名），用于错误提示。
func topLevelList(files []string) string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		rel := strings.TrimPrefix(filepath.ToSlash(f), "./")
		name := rel
		if i := strings.Index(rel, "/"); i >= 0 {
			name = rel[:i] + "/"
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) == 8 {
			break
		}
	}
	if len(out) == 0 {
		return "(空)"
	}
	return strings.Join(out, ", ")
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
