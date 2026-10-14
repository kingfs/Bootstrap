package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
//
// 两种产物来源二选一：
//   - `params.repo` + `params.asset`：GitHub Release 资产（绝大多数工具走这条）；
//   - `params.url`：直链（产物不在 GitHub Release 上，或 Release 里只有签名）。
//
// 直链模式的**版本来源**必须能确定：`params.version_source` 写固定版本或一个能读出
// 版本的 URL（kubectl 的 `stable.txt`、gradle 的 `versions/current`），
// 或者给 `params.repo` 让引擎去问 GitHub 的 releases/latest（helm：Release 里只有
// `.asc` 签名，但 tag 仍然是最可靠的版本来源）。两者都没有就报错——
// 没有版本就没法渲染 `{{version}}`，也没法判断"当前是不是最新"。
func (r *releaseInstaller) Validate(p manifest.Params) error {
	hasURL := strings.TrimSpace(p.URL) != ""
	hasRepo := strings.TrimSpace(p.Repo) != ""
	hasAsset := strings.TrimSpace(p.Asset) != ""
	switch {
	case hasURL && hasAsset:
		return fmt.Errorf("via=release: params.asset cannot be combined with params.url (the file name comes from the URL)")
	case hasURL:
		src := strings.TrimSpace(p.VersionSource)
		if (src == "" || src == "latest-release") && !hasRepo {
			return fmt.Errorf("via=release: params.url needs params.version_source " +
				"(a fixed version, or a URL to read the version from) or params.repo for releases/latest")
		}
	case hasRepo && hasAsset:
	default:
		return fmt.Errorf("via=release requires params.repo+params.asset, or params.url")
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

// releaseSource 是一次 release 安装的产物来源。
//
// 两种形态共用后面的下载/解压/复核逻辑，差别只在"去哪儿拿产物、去哪儿拿校验和"：
// repo 模式是 GitHub Release 资产，直链模式是清单里写死的 URL（kubectl/helm/gradle 这类
// 官方产物不在 GitHub Release 上、或 Release 里只有签名）。
type releaseSource struct {
	repo  string
	tag   string
	asset string
	url   string
}

// direct 表示这是直链模式（已渲染成完整 URL）。
func (s releaseSource) direct() bool { return s.url != "" }

// downloadURL 返回产物下载地址。
func (s releaseSource) downloadURL() string {
	if s.direct() {
		return s.url
	}
	return ghrelease.AssetURL(s.repo, s.tag, s.asset)
}

// label 是给用户看的来源名：直链只显示 host/最后一段，免得摘要长到看不清 dest。
func (s releaseSource) label() string {
	if s.direct() {
		return shortURL(s.url)
	}
	return s.repo
}

// assetFromURL 从直链推出产物文件名：去掉查询串与末尾斜杠，取最后一段。
//
// 只有 host（`https://x/`）时返回空——那不是产物名，交给调用方报错，
// 免得下载缓存里出现一个叫 "x" 的文件。
func assetFromURL(raw string) string {
	s := raw
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, "/")
	i := strings.LastIndex(s, "/")
	if i < 0 {
		return ""
	}
	if scheme := strings.Index(s, "//"); scheme >= 0 && i == scheme+1 {
		return ""
	}
	name := s[i+1:]
	if name == "." || name == ".." {
		return ""
	}
	return name
}

// defaultBinary 在清单没写 params.binary 时猜一个可执行文件名。
//
// repo 模式沿用"仓库名就是二进制名"的老约定；直链模式没有仓库名可用，
// 只能从产物名去掉归档后缀（kubectl → kubectl，gradle-9.0-bin.zip → gradle-9.0-bin，
// 后者必须显式写 params.binary）。
func defaultBinary(src releaseSource) string {
	if !src.direct() {
		return filepath.Base(strings.Trim(src.repo, "/"))
	}
	name := src.asset
	for _, suffix := range []string{".tar.gz", ".tgz", ".tar.xz", ".tar.bz2", ".tar.zst", ".zip", ".gz", ".xz", ".bz2"} {
		if trimmed := strings.TrimSuffix(name, suffix); trimmed != name {
			return trimmed
		}
	}
	return name
}

// renderSymlinks 渲染软链的 from/to。
//
// 目录型发布物（gradle 解压出 `gradle-9.0/`）要在路径里插 `{{version}}`，
// 用的是与 asset/dest 同一份 RenderData，规则一致才不会出现"dest 渲染了、symlink 没渲染"。
func renderSymlinks(p manifest.Params, data RenderData) ([]manifest.Symlink, error) {
	in := releaseSymlinks(p)
	out := make([]manifest.Symlink, 0, len(in))
	for _, l := range in {
		from, err := Render(l.From, data)
		if err != nil {
			return nil, fmt.Errorf("params.symlink.from: %w", err)
		}
		to, err := Render(l.To, data)
		if err != nil {
			return nil, fmt.Errorf("params.symlink.to: %w", err)
		}
		l.From, l.To = from, to
		out = append(out, l)
	}
	return out, nil
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
	// Remove 是卸载计划要删掉的产物路径（仅 OpUninstall 用；只删文件，不删目录）。
	Remove []string
	// RemoveLinks 是卸载计划要删掉的软链（仅 OpUninstall 用）。
	//
	// 与 Remove 分开是因为校验方式不同：软链路径上如果站着一个**真实文件**，
	// 那多半是用户自己放的（安装时 replaceSymlink 就不会去覆盖它），卸载不能删。
	RemoveLinks []string
}

// planReleaseUninstall 规划"删掉当初装出去的那些文件"。
//
// 发布物是 bs 自己写进 Dest 的（单个可执行文件 + 可选软链），所以卸载的**唯一**依据就是
// 清单里那几个路径：不需要联网、也不该联网——"卸载先要能连上 GitHub"是荒唐的。
//
// 两种情形只能报"不支持"，而且必须把"手工删哪里"讲清楚：
//   - `params.dest`/`params.binary`/软链里含 `{{version}}`（例如 Gradle 的
//     `~/.local/share/gradle/gradle-{{version}}/bin/gradle`）：没有"已安装的那个版本"
//     就渲染不出具体路径，猜一个路径去删是不可接受的；
//   - `params.extract: true`（整包解压型，如 uv/gradle）：解出来的是一整棵目录树，
//     bs 没有记录都写了哪些文件，递归删 Dest 可能连累用户放在同一目录下的东西。
//     要根治得记录"安装时写了哪些路径"（见 design §18.3 的待办），当前先明确拒绝。
func planReleaseUninstall(e ExecEnv, t manifest.Tool, m manifest.Method) (Action, error) {
	data := RenderData{OS: e.OS(), Arch: e.Arch(), Versions: e.Versions()}
	dest := fsx.ExpandPath(renderOr(m.Params.Dest, data), e.HomeDir())
	binary := m.Params.Binary
	if binary == "" {
		// 与安装时保持同一套推导规则：直链模式看产物名，repo 模式看仓库名。
		binary = defaultBinary(releaseSource{repo: m.Params.Repo, asset: assetFromURL(strings.TrimSpace(m.Params.URL))})
	}
	if m.Params.Extract != nil && *m.Params.Extract {
		return Action{}, &ErrUnsupported{Via: "release", Op: OpUninstall,
			Detail: "整包解压型发布物（params.extract）暂不支持自动卸载；请手工删除 " + dest}
	}
	// 模板要在**渲染之前**查：`{{version}}` 在 RenderData 没有版本时会被渲染成空串，
	// 于是"渲染后不含 {{" 并不代表路径确定——那只会悄悄指向一个错误的路径。
	//
	// 只查"决定文件在哪"的字段：dest / 产物名 / 软链。`params.url` 里有模板是常态
	// （kubectl 的 `v{{version}}/bin/{{os}}/{{arch}}/kubectl`），它的文件名部分照样是
	// 确定的，不该因此拒绝卸载。
	templated := []string{m.Params.Dest, binary}
	for _, l := range releaseSymlinks(m.Params) {
		templated = append(templated, l.From, l.To)
	}
	for _, raw := range templated {
		if strings.Contains(raw, "{{") {
			return Action{}, &ErrUnsupported{Via: "release", Op: OpUninstall,
				Detail: "清单里的路径含 {{...}} 模板（" + raw + "），没有已安装版本就无法确定具体文件；请手工删除 " + dest}
		}
	}
	// 共享目录（~/.local/bin）只删文件本身；软链同样是 bs 建的，一并删。
	remove := []string{filepath.Join(dest, executableName(binary, e.OS()))}
	var links []string
	for _, l := range releaseSymlinks(m.Params) {
		links = append(links, fsx.ExpandPath(l.To, e.HomeDir()))
	}
	summary := "删除 " + strings.Join(append(append([]string{}, remove...), links...), "、")
	return Action{
		ToolID: t.ID, Via: "release", Op: OpUninstall,
		Summary: summary,
		Reason:  "via=release（删除当初写入的产物与软链）",
	}.WithResolved(releasePlan{Remove: remove, RemoveLinks: links}), nil
}

// removeReleaseArtifacts 执行卸载。
//
// 只删文件与软链，**绝不**递归删目录：Dest 通常是 ~/.local/bin 这类共享目录，
// 删目录等于毁掉别人的东西。路径不存在直接跳过，所以卸载是幂等的。
//
// 软链路径上站着真实文件时不删：那是用户自己的东西（安装时 replaceSymlink
// 也会拒绝覆盖它），卸载更不能越权。§8.1"绝不删除用户已有内容"。
func removeReleaseArtifacts(plan releasePlan, e ExecEnv) error {
	for _, p := range plan.Remove {
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("uninstall %s: %w", p, err)
		}
		if info.IsDir() {
			return fmt.Errorf("uninstall %s: refusing to remove a directory", p)
		}
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("uninstall %s: %w", p, err)
		}
		e.Log().Info("removed", "path", p)
	}
	for _, p := range plan.RemoveLinks {
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("uninstall symlink %s: %w", p, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			e.Log().Warn("uninstall: not a symlink, leaving it alone", "path", p)
			continue
		}
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("uninstall symlink %s: %w", p, err)
		}
		e.Log().Info("removed symlink", "path", p)
	}
	return nil
}

// Plan 实现 Installer。
func (r *releaseInstaller) Plan(ctx context.Context, e ExecEnv, t manifest.Tool, m manifest.Method, op Op) (Action, error) {
	if op == OpUninstall {
		return planReleaseUninstall(e, t, m)
	}
	tag, version, err := r.resolveVersion(ctx, e, m)
	if err != nil {
		return Action{}, fmt.Errorf("tool %q: %w", t.ID, err)
	}

	data := RenderData{Version: version, Tag: tag, OS: e.OS(), Arch: e.Arch(), Versions: e.Versions()}
	src := releaseSource{repo: m.Params.Repo, tag: tag}
	if direct := strings.TrimSpace(m.Params.URL); direct != "" {
		// 直链模式：URL 里的 `{{version}}` 由 installer 在版本确定后渲染，
		// 产物名从 URL 推出来（下载缓存的文件名、校验和里的条目名都用它）。
		u, rerr := Render(direct, data)
		if rerr != nil {
			return Action{}, fmt.Errorf("tool %q: params.url: %w", t.ID, rerr)
		}
		src.url, src.asset = u, assetFromURL(u)
		if src.asset == "" {
			return Action{}, fmt.Errorf("tool %q: params.url %q has no file name", t.ID, u)
		}
	} else {
		asset, aerr := Render(m.Params.Asset, data)
		if aerr != nil {
			return Action{}, fmt.Errorf("tool %q: params.asset: %w", t.ID, aerr)
		}
		src.asset = asset
	}
	asset := src.asset
	url := src.downloadURL()
	dest := fsx.ExpandPath(renderOr(m.Params.Dest, data), e.HomeDir())

	sha, err := r.checksumFor(ctx, e, t, m, src)
	if err != nil {
		return Action{}, err
	}
	symlinks, err := renderSymlinks(m.Params, data)
	if err != nil {
		return Action{}, fmt.Errorf("tool %q: %w", t.ID, err)
	}

	binary := m.Params.Binary
	if binary == "" {
		binary = defaultBinary(src)
	}
	plan := releasePlan{
		// 归档放在跨次运行稳定的下载缓存里：断了能续，装过了能直接复用（见 httpx.Download）。
		Download: Download{URL: url, SHA256: sha, Dest: filepath.Join(e.DownloadDir(), asset), Mode: 0o644},
		Dest:     dest,
		Strip:    m.Params.StripComponents,
		Binary:   binary,
		Symlinks: symlinks,
		Archive:  archive.IsArchive(asset),
		Tree:     m.Params.Extract != nil && *m.Params.Extract,
	}
	if plan.Tree && !plan.Archive {
		return Action{}, fmt.Errorf("tool %q: params.extract needs an archive asset, got %q", t.ID, asset)
	}
	summary := fmt.Sprintf("release %s@%s → %s", src.label(), tag, dest)
	if plan.Tree {
		summary = fmt.Sprintf("release %s@%s 解压到 %s", src.label(), tag, dest)
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
func (r *releaseInstaller) checksumFor(ctx context.Context, e ExecEnv, t manifest.Tool, m manifest.Method, src releaseSource) (string, error) {
	if m.Params.Checksum == "none" {
		return "", nil
	}
	if p := strings.TrimSpace(m.Params.SHA256); p != "" {
		return validateSHA256(t.ID, p)
	}
	if src.tag == latestPlaceholder {
		// 干跑且无法解析最新版本时，不做校验和查询，避免预览阶段报错。
		return "", nil
	}
	data := RenderData{Version: strings.TrimPrefix(src.tag, "v"), Tag: src.tag, OS: e.OS(), Arch: e.Arch(),
		Versions: e.Versions()}
	if src.direct() {
		return r.checksumForURL(ctx, e, t, m, src, data)
	}
	asset, tag := src.asset, src.tag
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

// checksumForURL 处理直链产物的 sha256。
//
// 直链没有 GitHub 的"资产名"概念，因此按 URL 的兄弟文件找：
//   - `params.checksum_file` 给了绝对 URL 就用它；给的是名字（如 `sha256sum`）
//     就当作 `<url>.<名字>`（helm 的 `helm-v4.3.0-linux-arm64.tar.gz.sha256sum` 正是这个形态）；
//   - 没给就依次试 `<url>.sha256`（kubectl、gradle）与 `<url>.sha256sum`（helm）。
//
// 解析两种内容：带文件名的列表，以及只有一个裸哈希的文件。
func (r *releaseInstaller) checksumForURL(ctx context.Context, e ExecEnv, t manifest.Tool,
	m manifest.Method, src releaseSource, data RenderData) (string, error) {
	var candidates []string
	if name := strings.TrimSpace(m.Params.ChecksumFile); name != "" {
		n, rerr := Render(name, data)
		if rerr != nil {
			return "", fmt.Errorf("tool %q: params.checksum_file: %w", t.ID, rerr)
		}
		if strings.Contains(n, "://") {
			candidates = append(candidates, n)
		} else {
			candidates = append(candidates, src.url+"."+strings.TrimPrefix(n, "."))
		}
	} else {
		candidates = append(candidates, src.url+".sha256", src.url+".sha256sum")
	}
	var tried []string
	for _, u := range candidates {
		text, ferr := e.FetchText(ctx, u)
		if ferr != nil {
			tried = append(tried, fmt.Sprintf("%s (fetch failed: %v)", u, firstLineOf(ferr.Error())))
			continue
		}
		if sha, cerr := ghrelease.ChecksumFor(text, src.asset); cerr == nil {
			return validateSHA256(t.ID, sha)
		}
		if sha, ok := ghrelease.BareChecksum(text); ok {
			return validateSHA256(t.ID, sha)
		}
		tried = append(tried, u+"(无 "+src.asset+" 条目)")
	}
	return "", fmt.Errorf("tool %q: cannot find a sha256 for %s next to %s (tried %s); "+
		"set params.checksum_file, params.sha256, or params.checksum: none to opt out",
		t.ID, src.asset, shortURL(src.url), strings.Join(tried, ", "))
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
	if a.Op == OpUninstall {
		return removeReleaseArtifacts(plan, e)
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
		if strings.TrimSpace(m.Params.Repo) == "" {
			return "", "", fmt.Errorf("params.version_source is empty and params.repo is missing: " +
				"cannot tell which version to use")
		}
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
		if e.DryRun() {
			// 预览阶段不联网（`bs tool plan` 的承诺）：版本等 Apply 时再读。
			return latestPlaceholder, latestPlaceholder, nil
		}
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
