// Package selfupdate 实现 `bs self-update`：从 GitHub Release 取下当前平台的归档，
// 校验 sha256 后替换正在运行的二进制。
//
// 三条硬约束（ADR-0009、AGENTS.md §8）：
//  1. **只认 sha256**：拿不到 checksums.txt 或里面没有对应归档就中止，绝不"跳过校验"；
//     签名校验（cosign/sigstore）明确不做。
//  2. **改用户文件前先备份**：旧二进制留一份带时间戳的备份，替换失败要能回滚。
//  3. **替换必须是原子的**：新二进制先落到目标同目录再 rename，
//     避免"复制到一半断电"留下半个可执行文件，也避免跨设备 rename。
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kingfs/Bootstrap/pkg/archive"
	"github.com/kingfs/Bootstrap/pkg/fsx"
	"github.com/kingfs/Bootstrap/pkg/ghrelease"
	"github.com/kingfs/Bootstrap/pkg/httpx"
	"github.com/kingfs/Bootstrap/pkg/version"
)

// DefaultRepo 是 bs 自己的发布仓库。自更新只认它，除非显式覆盖（`--repo` / `BS_REPO`）。
const DefaultRepo = "kingfs/Bootstrap"

// Binary 是归档里的可执行文件名（不含 Windows 的 .exe）。
const Binary = "bs"

// ErrNoChecksum 表示拿不到校验和：按安全红线直接中止，而不是"降级为不校验"。
var ErrNoChecksum = errors.New("no checksum available for the release asset")

// ErrNoRelease 表示仓库还没有发布任何 Release（自更新暂时无从下手）。
var ErrNoRelease = errors.New("no release published")

// ErrAssetMissing 表示这个平台没有对应的归档产物。
var ErrAssetMissing = errors.New("release asset not found for this platform")

// ErrRefuseTestBinary 表示目标看起来是 Go 测试二进制：拒绝替换（防止误伤）。
var ErrRefuseTestBinary = errors.New("refusing to self-update a test binary")

// Options 是一次自更新的输入。
type Options struct {
	Repo    string // owner/repo，空则用 DefaultRepo
	GOOS    string // 目标平台（默认 runtime.GOOS，测试可注入）
	GOARCH  string
	Current string // 当前版本（buildinfo.Version）
	Client  *httpx.Client
	Exe     string // 被替换的二进制路径（默认 os.Executable()）
	TempDir string // 下载/解压的临时目录（默认在 Exe 同目录下）

	Tag   string // 目标 tag；空 = latest
	Force bool   // 同版本也重装
	// Verify 在替换**之前**自检新二进制（默认跑 `<新二进制> version`）；
	// 返回错误则中止替换，当前二进制保持不动。
	Verify func(ctx context.Context, path string) error

	Now func() time.Time // 注入时钟（测试用）
}

// Plan 描述"要不要更新、更新到哪个归档"。
type Plan struct {
	Repo    string `json:"repo"`
	Current string `json:"current"`
	Tag     string `json:"tag"`
	Version string `json:"version"`
	Asset   string `json:"asset"`
	// AssetURL / ChecksumsURL 是给用户看的下载地址（日志里也用它排查）。
	AssetURL     string `json:"asset_url"`
	ChecksumsURL string `json:"checksums_url"`
	// SHA256 是归档的期望校验和；`--check`/`--dry-run` 时可能为空（那时不联网取它）。
	SHA256 string `json:"sha256,omitempty"`
	// UpToDate 为真表示当前版本已经不旧于目标版本，此时什么都不做。
	UpToDate bool   `json:"up_to_date"`
	Exe      string `json:"exe"`
	// SystemManaged 提示二进制可能由系统包管理器安装（不在 $HOME 下），
	// 自更新会与包管理器打架，只提醒不阻止。
	SystemManaged bool `json:"system_managed,omitempty"`
}

// Result 是一次自更新的结果。
type Result struct {
	Plan
	Updated bool   `json:"updated"`
	Backup  string `json:"backup,omitempty"`
	// Pruned 是被清理掉的旧备份/临时文件（只清理我们自己造的命名）。
	Pruned []string `json:"pruned,omitempty"`
}

// AssetName 返回归档文件名，与 .goreleaser.yaml 的 name_template 保持一致。
func AssetName(ver, goos, goarch string) string {
	name := fmt.Sprintf("bootstrap_%s_%s_%s", strings.TrimPrefix(ver, "v"), goos, goarch)
	if goos == "windows" {
		return name + ".zip"
	}
	return name + ".tar.gz"
}

// normalizeTag 统一成 vX.Y.Z 形式（GitHub 上 bs 的 tag 一律带 v）。
func normalizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return tag
	}
	if !strings.HasPrefix(tag, "v") {
		return "v" + tag
	}
	return tag
}

// Resolve 解析目标版本与归档，并在需要真正更新时取回期望的 sha256。
//
// `checkOnly` 为真时完全不联网取校验和（`bs self-update --check` 只想知道版本号）。
func Resolve(ctx context.Context, opt Options, checkOnly bool) (Plan, error) {
	if opt.Client == nil {
		return Plan{}, errors.New("selfupdate: nil http client")
	}
	repo := opt.Repo
	if repo == "" {
		repo = DefaultRepo
	}
	goos, goarch := opt.GOOS, opt.GOARCH
	if goos == "" || goarch == "" {
		return Plan{}, errors.New("selfupdate: GOOS/GOARCH required")
	}
	exe, err := exePath(opt)
	if err != nil {
		return Plan{}, err
	}

	tag := normalizeTag(opt.Tag)
	if tag == "" {
		tag, err = ghrelease.LatestTag(ctx, opt.Client, repo)
		if err != nil {
			return Plan{}, fmt.Errorf("%w: %v", ErrNoRelease, err)
		}
		tag = normalizeTag(tag)
	}
	if tag == "" {
		return Plan{}, fmt.Errorf("cannot determine latest release of %s", repo)
	}
	ver := strings.TrimPrefix(tag, "v")

	plan := Plan{
		Repo:          repo,
		Current:       opt.Current,
		Tag:           tag,
		Version:       ver,
		Asset:         AssetName(ver, goos, goarch),
		AssetURL:      ghrelease.AssetURL(repo, tag, AssetName(ver, goos, goarch)),
		ChecksumsURL:  ghrelease.ChecksumsURL(repo, tag),
		Exe:           exe,
		SystemManaged: !underHome(exe),
	}
	if upToDate(opt.Current, tag) && !opt.Force {
		plan.UpToDate = true
		return plan, nil
	}

	if checkOnly {
		// `--check` 只想知道"有没有新版本"：不下载校验和，也不碰文件。
		return plan, nil
	}

	// 校验和只在这里取一次：拿不到就中止（ErrNoChecksum），绝不降级为不校验。
	text, err := opt.Client.GetString(ctx, plan.ChecksumsURL)
	if err != nil {
		return plan, fmt.Errorf("%w: fetch %s: %v", ErrNoChecksum, plan.ChecksumsURL, err)
	}
	sum, err := ghrelease.ChecksumFor(text, plan.Asset)
	if err != nil {
		return plan, fmt.Errorf("%w: %v", ErrNoChecksum, err)
	}
	plan.SHA256 = sum
	return plan, nil
}

// Apply 执行替换：下载 → 校验 sha256 → 解压 → 自检 → 备份 → 原子替换。
//
// plan.UpToDate 或 plan.SHA256 为空时直接返回（前者无需更新，后者说明是 check/dry-run）。
func Apply(ctx context.Context, opt Options, plan Plan) (Result, error) {
	res := Result{Plan: plan}
	if plan.UpToDate || plan.SHA256 == "" {
		return res, nil
	}
	if opt.Client == nil {
		return res, errors.New("selfupdate: nil http client")
	}
	now := time.Now
	if opt.Now != nil {
		now = opt.Now
	}
	stamp := now().UTC().Format("20060102-150405")

	tmpRoot := opt.TempDir
	if tmpRoot == "" {
		// 临时目录放在目标同目录下：rename 不会跨设备，且权限/配额与目标一致。
		dir, err := os.MkdirTemp(filepath.Dir(plan.Exe), ".bs-selfupdate-")
		if err != nil {
			return res, fmt.Errorf("create temp dir next to %s: %w", plan.Exe, err)
		}
		tmpRoot = dir
		defer os.RemoveAll(dir)
	}
	if err := fsx.EnsureDir(tmpRoot, 0o755); err != nil {
		return res, err
	}

	archivePath := filepath.Join(tmpRoot, plan.Asset)
	if _, err := opt.Client.Download(ctx, plan.AssetURL, archivePath, plan.SHA256, 0o644); err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) && se.Status == 404 {
			return res, fmt.Errorf("%w: %s: %v", ErrAssetMissing, plan.Asset, err)
		}
		return res, fmt.Errorf("download %s: %w", plan.Asset, err)
	}

	extractDir := filepath.Join(tmpRoot, "extract")
	if err := fsx.EnsureDir(extractDir, 0o755); err != nil {
		return res, err
	}
	files, err := archive.Extract(archivePath, extractDir, 0)
	if err != nil {
		return res, fmt.Errorf("extract %s: %w", plan.Asset, err)
	}
	binName := Binary
	if opt.GOOS == "windows" {
		binName += ".exe"
	}
	candidate, ok := archive.FindBinary(files, binName)
	if !ok {
		return res, fmt.Errorf("archive %s does not contain %s (%s)", plan.Asset, binName, topLevelList(files, extractDir))
	}

	// 自检：宁可"新版本跑不起来"停在这里，也不要替换掉能用的旧版本。
	if opt.Verify != nil {
		if err := opt.Verify(ctx, candidate); err != nil {
			return res, fmt.Errorf("new binary failed self-check, current binary kept: %w", err)
		}
	}

	mode := os.FileMode(0o755)
	if info, err := os.Stat(plan.Exe); err == nil {
		mode = info.Mode().Perm()
	}
	// 1) 先把新二进制放到目标同目录（保证 rename 是同设备原子操作）。
	staged := fmt.Sprintf("%s.tmp-%s", plan.Exe, stamp)
	if err := copyExecutable(candidate, staged, mode); err != nil {
		return res, err
	}
	defer os.Remove(staged)

	// 2) 再备份当前二进制（安全红线：改用户文件前先备份）。
	backup := fmt.Sprintf("%s.bak-%s", plan.Exe, stamp)
	if err := copyExecutable(plan.Exe, backup, mode); err != nil {
		return res, fmt.Errorf("backup %s: %w", plan.Exe, err)
	}
	res.Backup = backup

	// 3) 原子替换；失败则回滚到备份。
	if err := swap(staged, plan.Exe, backup, opt.GOOS); err != nil {
		return res, err
	}
	res.Updated = true
	res.Pruned = pruneStale(plan.Exe, backup, stamp)
	return res, nil
}

// swap 把 staged 换到 exe 的位置：POSIX 上 rename 覆盖是原子的；
// Windows 上正在运行的 exe 不能被覆盖、但能被改名，因此先"改名让位"，
// 再放新的进去，任何一步失败都把备份改回去。
func swap(staged, exe, backup, goos string) error {
	return swapWith(os.Rename, staged, exe, backup, goos)
}

// swapWith 是 swap 的实现，rename 可注入（测试要覆盖"第二步失败"这条回滚分支，
// 而现实里很难稳定造出 rename 失败）。
func swapWith(rename func(oldpath, newpath string) error, staged, exe, backup, goos string) error {
	if goos == "windows" {
		if err := rename(exe, backup); err != nil {
			return fmt.Errorf("move running binary aside: %w", err)
		}
		if err := rename(staged, exe); err != nil {
			// 回滚：把原来的二进制放回去，避免留下"没有 bs"的状态。
			if rerr := rename(backup, exe); rerr != nil {
				return fmt.Errorf("replace binary: %w (rollback also failed: %v, original kept at %s)", err, rerr, backup)
			}
			return fmt.Errorf("replace binary: %w", err)
		}
		return nil
	}
	if err := rename(staged, exe); err != nil {
		return fmt.Errorf("replace binary: %w (backup kept at %s)", err, backup)
	}
	return nil
}

// pruneStale 清理我们自己造的旧备份/临时文件，只保留本次的备份。
//
// 只删 `<exe>.bak-*` / `<exe>.tmp-*` 这两种我们自己命名的文件，别的一律不碰。
func pruneStale(exe, keepBackup, stamp string) []string {
	dir, base := filepath.Split(exe)
	prefixes := []string{base + ".bak-", base + ".tmp-"}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var pruned []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefixes[0]) && !strings.HasPrefix(name, prefixes[1]) {
			continue
		}
		full := filepath.Join(dir, name)
		if full == keepBackup || strings.HasSuffix(name, stamp) {
			continue
		}
		if err := os.Remove(full); err == nil {
			pruned = append(pruned, full)
		}
	}
	return pruned
}

// copyExecutable 复制文件并落权限，先写临时文件再原子改名。
func copyExecutable(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if len(data) == 0 {
		return fmt.Errorf("read %s: empty file", src)
	}
	if err := fsx.AtomicWriteFile(dst, data, mode); err != nil {
		return err
	}
	// AtomicWriteFile 可能因为 umask 而丢掉执行位，这里显式兜底。
	if err := os.Chmod(dst, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}
	return nil
}

// upToDate 判断当前版本是否已经不旧于目标 tag。
//
// "dev"/git hash 这类解析不出语义版本的当前版本一律视为需要更新——
// 用户跑 `bs self-update` 的意图就是"换成正式发布版"。
func upToDate(current, tag string) bool {
	current = strings.TrimSpace(current)
	if current == "" || current == "dev" || current == "unknown" {
		return false
	}
	if current == tag {
		return true
	}
	// 只有"看起来像版本号"的当前版本才做语义比较。
	// git hash 这种十六进制串（例如 8d0e150）会被 version.Parse 当成"数字 8 + 预发布"，
	// 于是"永远比 v0.3.0 新"——那显然是错的。
	if !looksLikeVersion(current) {
		return false
	}
	return version.Compare(tag, current) <= 0
}

// versionLike 匹配 `0.3.0` / `v0.3.0` / `0.3` 这类版本号（至少要有一个点）。
var versionLike = regexp.MustCompile(`^v?\d+(\.\d+)+`)

// looksLikeVersion 报告字符串是否像语义版本号。
func looksLikeVersion(s string) bool { return versionLike.MatchString(s) }

// exePath 解析被替换的二进制路径。
func exePath(opt Options) (string, error) {
	exe := opt.Exe
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate current executable: %w", err)
		}
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", exe, err)
	}
	// 护栏：绝不把 Go 测试二进制当成 bs 替换。
	// 忘了注入 `--exe` 时，这是最危险的一种误伤（会把正在跑的测试可执行文件换掉）。
	if base := strings.ToLower(filepath.Base(abs)); strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
		return "", fmt.Errorf("%w: %s（请用 --exe 指定真正的 bs）", ErrRefuseTestBinary, abs)
	}
	// os.Executable 在 Linux 上给的是 /proc/self/exe 的真实路径（已解开符号链接），
	// 这里不再额外 EvalSymlinks：替换真实文件比替换链接更符合"更新这个程序"的语义。
	return abs, nil
}

// underHome 报告路径是否在用户主目录下（否则可能是系统包管理器装的）。
func underHome(path string) bool {
	home := fsx.HomeDir()
	if home == "" {
		return true
	}
	rel, err := filepath.Rel(home, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// topLevelList 列出归档里的条目，便于排查"为什么找不到二进制"。
func topLevelList(files []string, root string) string {
	seen := map[string]bool{}
	var names []string
	for _, f := range files {
		rel, err := filepath.Rel(root, f)
		if err != nil {
			continue
		}
		top := rel
		if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
			top = rel[:i] + "/…"
		}
		if !seen[top] {
			seen[top] = true
			names = append(names, top)
		}
	}
	if len(names) == 0 {
		return "archive is empty"
	}
	return "contains " + strings.Join(names, ", ")
}
