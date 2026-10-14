package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/pkg/ghrelease"
	"github.com/kingfs/Bootstrap/pkg/httpx"
)

// fakeGitHub 用一个 httptest 服务器冒充 GitHub（API + 资产下载）。
//
// 只注册需要的路径，其它一律 404——这样"少发/多发了一个请求"都能被断言出来。
type fakeGitHub struct {
	server   *httptest.Server
	requests []string
	tag      string
	asset    string
	archive  []byte
	checksum string // 空 = 不提供 checksums.txt
}

func newFakeGitHub(t *testing.T, tag, asset string, archive []byte, checksum string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{tag: tag, asset: asset, archive: archive, checksum: checksum}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":"` + tag + `"}`))
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			if checksum == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(checksum + "  " + asset + "\n"))
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)

	oldAPI, oldAsset := ghrelease.APIBaseURL, ghrelease.AssetBaseURL
	ghrelease.APIBaseURL, ghrelease.AssetBaseURL = f.server.URL, f.server.URL
	t.Cleanup(func() { ghrelease.APIBaseURL, ghrelease.AssetBaseURL = oldAPI, oldAsset })
	return f
}

func (f *fakeGitHub) fetchedAsset() bool {
	for _, p := range f.requests {
		if strings.HasSuffix(p, "/"+f.asset) {
			return true
		}
	}
	return false
}

// buildTarGz 造一个只含指定文件的 tar.gz。
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, n := range names {
		body := files[n]
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// newFakeExe 造一个"当前二进制"（一个假的可执行脚本），返回路径。
func newFakeExe(t *testing.T, root, body string) string {
	t.Helper()
	path := filepath.Join(root, "bs")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	return path
}

// baseOptions 返回一份指向假 GitHub 的输入；用例只需要覆盖关心的字段。
func baseOptions(t *testing.T, exe, current string) Options {
	t.Helper()
	// 让"是否系统包管理器装的"判定确定：把宿主家目录指到 exe 所在目录。
	// Windows 上 os.UserHomeDir() 读的是 USERPROFILE（不看 HOME），只设 HOME 的话
	// 它仍返回 runner 的真实家目录，exe 就落在"家目录之外"被误判成系统装的。
	home := filepath.Dir(exe)
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	return Options{
		Repo:    "kingfs/Bootstrap",
		GOOS:    "linux",
		GOARCH:  "arm64",
		Current: current,
		Client:  httpx.New(httpx.DefaultTimeout, map[string]string{"NO_PROXY": "*"}),
		Exe:     exe,
	}
}

func TestAssetName(t *testing.T) {
	cases := []struct {
		ver, goos, goarch, want string
	}{
		{"0.3.0", "linux", "amd64", "bootstrap_0.3.0_linux_amd64.tar.gz"},
		{"v0.3.0", "darwin", "arm64", "bootstrap_0.3.0_darwin_arm64.tar.gz"},
		{"v1.0.0", "windows", "amd64", "bootstrap_1.0.0_windows_amd64.zip"},
	}
	for _, tc := range cases {
		if got := AssetName(tc.ver, tc.goos, tc.goarch); got != tc.want {
			t.Errorf("AssetName(%q,%q,%q) = %q, want %q", tc.ver, tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// TestExePathRefusesTestBinary：护栏——把测试二进制当 bs 替换必须被拒。
func TestExePathRefusesTestBinary(t *testing.T) {
	_, err := exePath(Options{Exe: filepath.Join(t.TempDir(), "cli.test")})
	if !errors.Is(err, ErrRefuseTestBinary) {
		t.Fatalf("err = %v, want ErrRefuseTestBinary", err)
	}
	_, err = exePath(Options{Exe: filepath.Join(t.TempDir(), "cli.test.exe")})
	if !errors.Is(err, ErrRefuseTestBinary) {
		t.Fatalf("windows 的 .test.exe 也要拦住：%v", err)
	}
	got, err := exePath(Options{Exe: filepath.Join(t.TempDir(), "bs")})
	if err != nil || !strings.HasSuffix(got, "bs") {
		t.Fatalf("正常的 bs 不该被拦：%q %v", got, err)
	}
}

func TestUpToDate(t *testing.T) {
	cases := []struct {
		current, tag string
		want         bool
	}{
		{"v0.3.0", "v0.3.0", true},
		{"0.3.0", "v0.3.0", true},
		{"v0.4.0", "v0.3.0", true},  // 比发布版还新（本地自编译）
		{"v0.2.0", "v0.3.0", false}, // 该更新
		{"dev", "v0.3.0", false},    // dev 构建一律认为要更新
		{"", "v0.3.0", false},
		{"unknown", "v0.3.0", false},
		{"8d0e150", "v0.3.0", false}, // git hash 解析不出语义版本
	}
	for _, tc := range cases {
		if got := upToDate(tc.current, tc.tag); got != tc.want {
			t.Errorf("upToDate(%q,%q) = %v, want %v", tc.current, tc.tag, got, tc.want)
		}
	}
}

func TestResolveFindsLatestAndChecksum(t *testing.T) {
	root := t.TempDir()
	exe := newFakeExe(t, root, "old-bs")
	archive := buildTarGz(t, map[string]string{"bs": "new-bs"})
	f := newFakeGitHub(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", archive, sha256Hex(archive))

	plan, err := Resolve(context.Background(), baseOptions(t, exe, "v0.2.0"), false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.Tag != "v0.3.0" || plan.Version != "0.3.0" {
		t.Errorf("版本解析不对：%+v", plan)
	}
	if plan.Asset != "bootstrap_0.3.0_linux_arm64.tar.gz" || plan.SHA256 != sha256Hex(archive) {
		t.Errorf("归档/校验和不对：%+v", plan)
	}
	if plan.UpToDate {
		t.Error("v0.2.0 → v0.3.0 应当需要更新")
	}
	if plan.SystemManaged {
		t.Error("二进制在 HOME 下，不该标记为系统包管理器装的")
	}
	if f.fetchedAsset() {
		t.Errorf("Resolve 不该下载归档（只取校验和），实际请求：%v", f.requests)
	}
}

func TestResolveUpToDateSkipsEverything(t *testing.T) {
	root := t.TempDir()
	exe := newFakeExe(t, root, "bs")
	f := newFakeGitHub(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", nil, "")

	plan, err := Resolve(context.Background(), baseOptions(t, exe, "v0.3.0"), false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !plan.UpToDate || plan.SHA256 != "" {
		t.Errorf("已是最新时不该取校验和：%+v", plan)
	}
	for _, p := range f.requests {
		if strings.Contains(p, "checksums.txt") {
			t.Errorf("已是最新却请求了 checksums.txt：%v", f.requests)
		}
	}
}

func TestResolveCheckOnlyDoesNotFetchChecksums(t *testing.T) {
	root := t.TempDir()
	exe := newFakeExe(t, root, "bs")
	f := newFakeGitHub(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", nil, "")

	plan, err := Resolve(context.Background(), baseOptions(t, exe, "v0.2.0"), true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.SHA256 != "" {
		t.Errorf("--check 不该取校验和：%q", plan.SHA256)
	}
	for _, p := range f.requests {
		if strings.Contains(p, "checksums.txt") {
			t.Errorf("--check 却请求了 checksums.txt：%v", f.requests)
		}
	}
}

func TestResolveWithoutChecksumsAborts(t *testing.T) {
	root := t.TempDir()
	exe := newFakeExe(t, root, "bs")
	newFakeGitHub(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", nil, "") // 不提供 checksums.txt

	_, err := Resolve(context.Background(), baseOptions(t, exe, "v0.2.0"), false)
	if !errors.Is(err, ErrNoChecksum) {
		t.Fatalf("err = %v, want ErrNoChecksum（拿不到校验和必须中止）", err)
	}
}

func TestResolveChecksumWithoutOurAssetAborts(t *testing.T) {
	root := t.TempDir()
	exe := newFakeExe(t, root, "bs")
	newFakeGitHub(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", nil, "deadbeef")

	_, err := Resolve(context.Background(), baseOptions(t, exe, "v0.2.0"), false)
	if !errors.Is(err, ErrNoChecksum) {
		t.Fatalf("err = %v, want ErrNoChecksum", err)
	}
}

func TestResolveNoReleaseAtAll(t *testing.T) {
	root := t.TempDir()
	exe := newFakeExe(t, root, "bs")
	opt := baseOptions(t, exe, "v0.2.0")
	// 指向一个不存在的仓库路径：latest 解析必然 404。
	f := newFakeGitHub(t, "", "x", nil, "")
	f.tag = ""

	_, err := Resolve(context.Background(), opt, true)
	if !errors.Is(err, ErrNoRelease) {
		t.Fatalf("err = %v, want ErrNoRelease", err)
	}
}

func TestApplyReplacesBinaryAndKeepsBackup(t *testing.T) {
	root := t.TempDir()
	exe := newFakeExe(t, root, "old-bs")
	archive := buildTarGz(t, map[string]string{"bs": "new-bs"})
	newFakeGitHub(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", archive, sha256Hex(archive))

	opt := baseOptions(t, exe, "v0.2.0")
	var verified string
	opt.Verify = func(_ context.Context, path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		verified = string(data)
		return nil
	}
	plan, err := Resolve(context.Background(), opt, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	res, err := Apply(context.Background(), opt, plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Updated {
		t.Fatal("应当报告已更新")
	}
	if verified != "new-bs" {
		t.Errorf("自检应当针对解压出来的新二进制，实际 %q", verified)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read exe: %v", err)
	}
	if string(data) != "new-bs" {
		t.Errorf("可执行文件内容 = %q, want %q", data, "new-bs")
	}
	requireExecBit(t, exe)
	backup, err := os.ReadFile(res.Backup)
	if err != nil {
		t.Fatalf("备份不存在：%v", err)
	}
	if string(backup) != "old-bs" {
		t.Errorf("备份内容 = %q, want %q", backup, "old-bs")
	}
	if !strings.Contains(res.Backup, ".bak-") || !strings.HasPrefix(res.Backup, exe) {
		t.Errorf("备份命名不对：%s", res.Backup)
	}
}

// TestApplyBadChecksumKeepsCurrent：校验和不匹配时绝不替换，也不留备份。
func TestApplyBadChecksumKeepsCurrent(t *testing.T) {
	root := t.TempDir()
	exe := newFakeExe(t, root, "old-bs")
	archive := buildTarGz(t, map[string]string{"bs": "tampered"})
	// 清单里给的校验和与真实内容不一致（模拟被中间人改了）。
	newFakeGitHub(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", archive, strings.Repeat("ab", 32))

	opt := baseOptions(t, exe, "v0.2.0")
	plan, err := Resolve(context.Background(), opt, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := Apply(context.Background(), opt, plan); err == nil {
		t.Fatal("校验和不匹配时必须失败")
	}
	assertUnchanged(t, root, exe, "old-bs")
}

// TestApplyVerifyFailureKeepsCurrent：新二进制跑不起来时保留旧的。
func TestApplyVerifyFailureKeepsCurrent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("自检失败用 shell 脚本模拟")
	}
	root := t.TempDir()
	exe := newFakeExe(t, root, "old-bs")
	archive := buildTarGz(t, map[string]string{"bs": "broken"})
	newFakeGitHub(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", archive, sha256Hex(archive))

	opt := baseOptions(t, exe, "v0.2.0")
	opt.Verify = func(context.Context, string) error { return errors.New("exec format error") }
	plan, _ := Resolve(context.Background(), opt, false)
	_, err := Apply(context.Background(), opt, plan)
	if err == nil || !strings.Contains(err.Error(), "self-check") {
		t.Fatalf("err = %v, want 自检失败", err)
	}
	assertUnchanged(t, root, exe, "old-bs")
}

// TestApplyArchiveWithoutBinary：归档里没有 bs 时给出手上有什么，且不替换。
func TestApplyArchiveWithoutBinary(t *testing.T) {
	root := t.TempDir()
	exe := newFakeExe(t, root, "old-bs")
	archive := buildTarGz(t, map[string]string{"README.md": "hi", "LICENSE": "x"})
	newFakeGitHub(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", archive, sha256Hex(archive))

	opt := baseOptions(t, exe, "v0.2.0")
	plan, _ := Resolve(context.Background(), opt, false)
	_, err := Apply(context.Background(), opt, plan)
	if err == nil || !strings.Contains(err.Error(), "does not contain bs") {
		t.Fatalf("err = %v, want 归档缺少 bs", err)
	}
	if !strings.Contains(err.Error(), "README.md") {
		t.Errorf("错误里应当列出归档内容便于排查：%v", err)
	}
	assertUnchanged(t, root, exe, "old-bs")
}

// TestApplyPrunesOnlyOurOwnStaleFiles：只清理我们自己命名的旧备份/临时文件，
// 用户放在同目录里的东西一个都不能动。
func TestApplyPrunesOnlyOurOwnStaleFiles(t *testing.T) {
	root := t.TempDir()
	exe := newFakeExe(t, root, "old-bs")
	staleBackup := exe + ".bak-19700101-000000"
	staleTmp := exe + ".tmp-19700101-000000"
	userFile := filepath.Join(root, "notes.txt")
	for _, p := range []string{staleBackup, staleTmp, userFile} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	// 名字里带 .bak- 但前缀不是我们的：也不能删。
	otherTool := filepath.Join(root, "other-tool.bak-19700101-000000")
	if err := os.WriteFile(otherTool, []byte("x"), 0o644); err != nil {
		t.Fatalf("write other: %v", err)
	}

	archive := buildTarGz(t, map[string]string{"bs": "new-bs"})
	newFakeGitHub(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", archive, sha256Hex(archive))
	opt := baseOptions(t, exe, "v0.2.0")
	plan, _ := Resolve(context.Background(), opt, false)
	res, err := Apply(context.Background(), opt, plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, gone := range []string{staleBackup, staleTmp} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("应当清理掉旧文件：%s", gone)
		}
	}
	for _, keep := range []string{userFile, otherTool, res.Backup} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("不该动别人的文件：%s（%v）", keep, err)
		}
	}
	if strings.Join(res.Pruned, ",") == "" {
		t.Error("应当报告清理了哪些文件")
	}
}

// TestSwapWindowsRollsBack：Windows 上"先改名让位、再放新的"，
// 如果第二步失败必须把原来的二进制改回去，绝不能留下"没有 bs"的状态。
func TestSwapWindowsRollsBack(t *testing.T) {
	root := t.TempDir()
	exe := filepath.Join(root, "bs")
	backup := exe + ".bak-x"
	staged := exe + ".tmp-x"
	for _, p := range []string{exe, staged} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	// 第一次改名（让位）成功，第二次（放新的）失败，第三次（回滚）用真实现。
	calls := 0
	rename := func(oldpath, newpath string) error {
		calls++
		if calls == 2 {
			return errors.New("injected failure")
		}
		return os.Rename(oldpath, newpath)
	}
	err := swapWith(rename, staged, exe, backup, "windows")
	if err == nil {
		t.Fatal("第二步 rename 失败时必须报错")
	}
	if calls != 3 {
		t.Errorf("应当尝试回滚（共 3 次 rename），实际 %d 次", calls)
	}
	if _, statErr := os.Stat(exe); statErr != nil {
		t.Errorf("回滚后原来的二进制应当还在原位：%v", statErr)
	}
	if _, statErr := os.Stat(backup); statErr == nil {
		t.Error("回滚成功后不该留下备份（它已经改回原位了）")
	}
	if _, statErr := os.Stat(staged); statErr != nil {
		t.Errorf("staged 文件应当还在（失败路径由调用方 defer 清理）：%v", statErr)
	}
}

// TestSwapWindowsRollbackFailureIsLoud：连回滚都失败时，错误里必须写清"原件在哪"。
func TestSwapWindowsRollbackFailureIsLoud(t *testing.T) {
	root := t.TempDir()
	exe := filepath.Join(root, "bs")
	backup := exe + ".bak-x"
	staged := exe + ".tmp-x"
	for _, p := range []string{exe, staged} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	// 让位成功，之后（放新的 + 回滚）都失败。
	calls := 0
	rename := func(oldpath, newpath string) error {
		calls++
		if calls == 1 {
			return os.Rename(oldpath, newpath)
		}
		return errors.New("injected failure")
	}
	err := swapWith(rename, staged, exe, backup, "windows")
	if err == nil {
		t.Fatal("必须报错")
	}
	if !strings.Contains(err.Error(), backup) {
		t.Errorf("错误信息要告诉用户原件还在哪：%v", err)
	}
	if _, statErr := os.Stat(backup); statErr != nil {
		t.Errorf("回滚失败时原件应当留在备份路径：%v", statErr)
	}
}

// TestApplyDryRunPlanIsUntouched：Resolve 出来的计划在 SHA256 为空时绝不下载/替换。
func TestApplySkipsWhenNoChecksumInPlan(t *testing.T) {
	root := t.TempDir()
	exe := newFakeExe(t, root, "old-bs")
	f := newFakeGitHub(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", nil, "")
	opt := baseOptions(t, exe, "v0.2.0")
	plan, err := Resolve(context.Background(), opt, true) // --check：没有校验和
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	res, err := Apply(context.Background(), opt, plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Updated {
		t.Error("没有校验和时不该更新")
	}
	if f.fetchedAsset() {
		t.Error("没有校验和时不该下载归档")
	}
	assertUnchanged(t, root, exe, "old-bs")
}

// requireExecBit 断言 path 带 POSIX 可执行位。
//
// Windows 没有 POSIX 权限位：os.FileMode 的 0o111 读出来恒为 0，
// 产品里"可执行"由 fsx.IsExecutable 表达为"普通文件即视为可执行"，
// 所以这一条断言在 Windows 上不适用。注意这里刻意**不用 t.Skip**：
// t.Skip 会中止整个用例，把后面"内容未变/备份存在/没有残留备份"等断言一起丢掉；
// 直接返回才能只跳过这一条，符合"Windows 上保留其它断言"的要求。
func requireExecBit(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("可执行位丢了：%s（mode=%v）", path, info.Mode().Perm())
	}
}

// assertUnchanged 断言目标二进制字节级未变、可执行位还在，且没有留下备份。
func assertUnchanged(t *testing.T, root, exe, want string) {
	t.Helper()
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read exe: %v", err)
	}
	if string(data) != want {
		t.Errorf("可执行文件被改动了：%q, want %q", data, want)
	}
	requireExecBit(t, exe)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak-") {
			t.Errorf("失败路径不该留下备份：%s", e.Name())
		}
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("失败路径不该留下临时文件：%s", e.Name())
		}
	}
}
