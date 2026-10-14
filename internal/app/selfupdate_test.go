package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/internal/selfupdate"
	"github.com/kingfs/Bootstrap/pkg/ghrelease"
)

// fakeRelease 用 httptest 冒充 GitHub，把 release 路径整条跑通（不联网）。
type fakeRelease struct {
	tag      string
	asset    string
	archive  []byte
	checksum string
	url      string
}

// startFakeRelease 起一个假 GitHub（API + 资产 + checksums.txt），
// 并把 ghrelease 的地址前缀指过去；测试结束自动还原。
func startFakeRelease(t *testing.T, tag, asset string, archive []byte) *fakeRelease {
	t.Helper()
	f := &fakeRelease{tag: tag, asset: asset, archive: archive, checksum: sha256Hex(archive)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":"` + f.tag + `"}`))
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			if f.checksum == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(f.checksum + "  " + f.asset + "\n"))
		case strings.HasSuffix(r.URL.Path, "/"+f.asset):
			_, _ = w.Write(f.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.url = server.URL

	oldAPI, oldAsset := ghrelease.APIBaseURL, ghrelease.AssetBaseURL
	ghrelease.APIBaseURL, ghrelease.AssetBaseURL = server.URL, server.URL
	t.Cleanup(func() { ghrelease.APIBaseURL, ghrelease.AssetBaseURL = oldAPI, oldAsset })
	return f
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// buildReleaseTarGz 造一个只含指定文件的 tar.gz（模拟发布归档）。
func buildReleaseTarGz(t *testing.T, files map[string]string) []byte {
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

// selfUpdateApp 准备一个"自更新专用"的 App：平台固定、二进制路径注入到临时目录。
func selfUpdateApp(t *testing.T, exeBody string) (*App, string) {
	t.Helper()
	a, root := newTestApp(t)
	a.Platform.OS = "linux"
	a.Platform.Arch = "arm64"
	a.CurrentVersion = "v0.2.0"
	exe := filepath.Join(root, "bin", "bs")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(exe, []byte(exeBody), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	a.ExePath = exe
	if err := os.MkdirAll(a.Paths.HomeDir, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", root)
	return a, exe
}

// TestSelfUpdateReplacesBinary：app 层把"下载 → 校验 → 自检 → 替换"串起来，
// 新的"二进制"是一个会打印版本号的脚本，因此自检也真的执行了。
func TestSelfUpdateReplacesBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("自检要执行 shell 脚本")
	}
	a, exe := selfUpdateApp(t, "#!/bin/sh\necho bs v0.2.0\n")
	archive := buildReleaseTarGz(t, map[string]string{"bs": "#!/bin/sh\necho bs v0.3.0\n"})
	startFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", archive)

	plan, err := a.SelfUpdatePlan(context.Background(), SelfUpdateOptions{Repo: "kingfs/Bootstrap"})
	if err != nil {
		t.Fatalf("SelfUpdatePlan: %v", err)
	}
	if plan.UpToDate || plan.Tag != "v0.3.0" || plan.SHA256 == "" {
		t.Fatalf("计划不对：%+v", plan)
	}
	res, err := a.SelfUpdate(context.Background(), SelfUpdateOptions{Repo: "kingfs/Bootstrap"})
	if err != nil {
		t.Fatalf("SelfUpdate: %v", err)
	}
	if !res.Updated {
		t.Fatal("应当报告已更新")
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read exe: %v", err)
	}
	if !strings.Contains(string(data), "v0.3.0") {
		t.Errorf("可执行文件没有被替换：%q", data)
	}
	if _, err := os.Stat(res.Backup); err != nil {
		t.Errorf("应当留下备份：%v", err)
	}
}

// TestSelfUpdateDryRunMakesNoChanges：dry-run 下连归档都不下载，二进制原样不动。
func TestSelfUpdateDryRunMakesNoChanges(t *testing.T) {
	a, exe := selfUpdateApp(t, "old")
	a.DryRun = true
	a.Runner.DryRun = true
	archive := buildReleaseTarGz(t, map[string]string{"bs": "new"})
	startFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", archive)

	before := snapshotSet(snapshot(t, a.Paths.HomeDir))
	res, err := a.SelfUpdate(context.Background(), SelfUpdateOptions{Repo: "kingfs/Bootstrap"})
	if err != nil {
		t.Fatalf("SelfUpdate: %v", err)
	}
	if res.Updated {
		t.Error("dry-run 不该替换")
	}
	data, _ := os.ReadFile(exe)
	if string(data) != "old" {
		t.Errorf("dry-run 改动了二进制：%q", data)
	}
	if diff := diffSnapshot(t, before, a.Paths.HomeDir); diff != "" {
		t.Errorf("dry-run 不该留下任何文件：\n%s", diff)
	}
}

// TestSelfUpdateUpToDateIsNoOp：当前版本不比发布版旧时什么都不做。
func TestSelfUpdateUpToDateIsNoOp(t *testing.T) {
	a, exe := selfUpdateApp(t, "old")
	a.CurrentVersion = "v0.3.0"
	startFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", buildReleaseTarGz(t, map[string]string{"bs": "new"}))

	plan, err := a.SelfUpdatePlan(context.Background(), SelfUpdateOptions{Repo: "kingfs/Bootstrap"})
	if err != nil {
		t.Fatalf("SelfUpdatePlan: %v", err)
	}
	if !plan.UpToDate {
		t.Fatalf("v0.3.0 对比 v0.3.0 应当是最新：%+v", plan)
	}
	res, err := a.SelfUpdate(context.Background(), SelfUpdateOptions{Repo: "kingfs/Bootstrap"})
	if err != nil {
		t.Fatalf("SelfUpdate: %v", err)
	}
	if res.Updated {
		t.Error("已是最新时不该替换")
	}
	if data, _ := os.ReadFile(exe); string(data) != "old" {
		t.Errorf("二进制被改动了：%q", data)
	}
}

// TestSelfUpdateVerifyRejectsBrokenArchive：归档里的新二进制跑不起来时保留旧的
// （这里是"新二进制"根本不打印版本号的情况）。
func TestSelfUpdateVerifyRejectsBrokenArchive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("自检要执行 shell 脚本")
	}
	a, exe := selfUpdateApp(t, "old")
	// 新二进制会成功执行但什么都不输出 → 自检应当判它可疑。
	archive := buildReleaseTarGz(t, map[string]string{"bs": "#!/bin/sh\nexit 0\n"})
	startFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", archive)

	_, err := a.SelfUpdate(context.Background(), SelfUpdateOptions{Repo: "kingfs/Bootstrap"})
	if err == nil {
		t.Fatal("自检不通过时必须失败")
	}
	if data, _ := os.ReadFile(exe); string(data) != "old" {
		t.Errorf("自检失败时不能替换：%q", data)
	}
}

// TestSelfUpdateNoChecksumAborts：仓库没给校验和时按安全红线中止。
func TestSelfUpdateNoChecksumAborts(t *testing.T) {
	a, _ := selfUpdateApp(t, "old")
	f := startFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", buildReleaseTarGz(t, map[string]string{"bs": "new"}))
	f.checksum = "" // 关掉 checksums.txt

	_, err := a.SelfUpdatePlan(context.Background(), SelfUpdateOptions{Repo: "kingfs/Bootstrap"})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want 校验和缺失", err)
	}
}

// TestSelfUpdateRepoEnv：BS_REPO 可以覆盖默认仓库。
func TestSelfUpdateRepoEnv(t *testing.T) {
	t.Setenv("BS_REPO", "someone/fork")
	if got := SelfUpdateRepo(); got != "someone/fork" {
		t.Errorf("SelfUpdateRepo() = %q", got)
	}
	t.Setenv("BS_REPO", "")
	if got := SelfUpdateRepo(); got != selfupdate.DefaultRepo {
		t.Errorf("缺省应为 %q，实际 %q", selfupdate.DefaultRepo, got)
	}
}

// TestVerifyBinaryRunsVersion：自检确实执行了新二进制（而不是只看文件存在）。
func TestVerifyBinaryRunsVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本当被测二进制")
	}
	a, root := newTestApp(t)
	script := filepath.Join(root, "fake-bs")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho bs v9.9.9\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := a.verifyBinary(context.Background(), script); err != nil {
		t.Errorf("应当通过：%v", err)
	}
	silent := filepath.Join(root, "silent")
	if err := os.WriteFile(silent, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := a.verifyBinary(context.Background(), silent); err == nil {
		t.Error("没有版本输出时应当报错")
	}
	if err := a.verifyBinary(context.Background(), filepath.Join(root, "missing")); err == nil {
		t.Error("文件不存在时应当报错")
	}
}
