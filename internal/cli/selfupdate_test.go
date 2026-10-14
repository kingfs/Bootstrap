package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kingfs/Bootstrap/internal/buildinfo"
	"github.com/kingfs/Bootstrap/pkg/ghrelease"
)

// cliFakeRelease 是一个"假 GitHub"：API 给 tag，资产给归档，另有 checksums.txt。
type cliFakeRelease struct {
	server   *httptest.Server
	asset    string
	checksum string
	requests []string
}

func startCLIFakeRelease(t *testing.T, tag, asset string, archive []byte) *cliFakeRelease {
	t.Helper()
	f := &cliFakeRelease{asset: asset, checksum: hashHex(archive)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":"` + tag + `"}`))
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			if f.checksum == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(f.checksum + "  " + f.asset + "\n"))
		case strings.HasSuffix(r.URL.Path, "/"+f.asset):
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

func (f *cliFakeRelease) downloaded() bool {
	for _, p := range f.requests {
		if strings.HasSuffix(p, "/"+f.asset) {
			return true
		}
	}
	return false
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// tarGz 造一个 tar.gz 归档。
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
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

// withVersion 临时把"当前版本"改掉（自更新的版本比较依赖它）。
func withVersion(t *testing.T, v string) {
	t.Helper()
	old := buildinfo.Version
	buildinfo.Version = v
	t.Cleanup(func() { buildinfo.Version = old })
}

// sandboxForSelfUpdate 准备沙箱 + 一个假的"当前二进制"，返回 exe 路径。
func sandboxForSelfUpdate(t *testing.T) (*Runtime, *bytes.Buffer, *bytes.Buffer, string, string) {
	t.Helper()
	rt, out, errBuf, root := sandbox(t)
	exe := filepath.Join(root, "bin", "bs")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(exe, []byte("old-bs\n"), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	t.Setenv("HOME", root)
	return rt, out, errBuf, root, exe
}

// TestSelfUpdateCheckUpToDate：--check 在已是最新时只说一句，退出码 0。
func TestSelfUpdateCheckUpToDate(t *testing.T) {
	withVersion(t, "v0.3.0")
	rt, _, _, _, exe := sandboxForSelfUpdate(t)
	f := startCLIFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", nil)

	code, stdout, stderr := run(t, rt, "self-update", "--check", "--exe", exe)
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "已是最新") {
		t.Errorf("应当说明已是最新：\n%s", stdout)
	}
	if f.downloaded() {
		t.Error("--check 不该下载归档")
	}
}

// TestSelfUpdateCheckOutdated：有新版本时报告版本、归档与可执行文件，但不下载不替换。
func TestSelfUpdateCheckOutdated(t *testing.T) {
	withVersion(t, "v0.2.0")
	rt, _, _, root, exe := sandboxForSelfUpdate(t)
	before := snapshotTree(t, root)
	f := startCLIFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", nil)

	code, stdout, stderr := run(t, rt, "self-update", "--check", "--exe", exe)
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	for _, want := range []string{"v0.2.0", "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", exe} {
		if !strings.Contains(stdout, want) {
			t.Errorf("输出里缺少 %q：\n%s", want, stdout)
		}
	}
	if f.downloaded() {
		t.Error("--check 不该下载归档")
	}
	if diff := diffTree(t, before, root); diff != "" {
		t.Errorf("--check 必须零写入：\n%s", diff)
	}
}

// TestSelfUpdateDryRunTouchesNothing：--dry-run 连校验和都查出来给你看，但不下载不替换。
func TestSelfUpdateDryRunTouchesNothing(t *testing.T) {
	withVersion(t, "v0.2.0")
	rt, _, _, root, exe := sandboxForSelfUpdate(t)
	before := snapshotTree(t, root)
	f := startCLIFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", tarGz(t, map[string]string{"bs": "new\n"}))

	code, stdout, stderr := run(t, rt, "self-update", "--dry-run", "--exe", exe)
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "sha256") || !strings.Contains(stdout, "dry-run") {
		t.Errorf("dry-run 应当展示校验和与「不会执行」的标注：\n%s", stdout)
	}
	if f.downloaded() {
		t.Error("--dry-run 不该下载归档")
	}
	if diff := diffTree(t, before, root); diff != "" {
		t.Errorf("--dry-run 必须零写入：\n%s", diff)
	}
	if data, _ := os.ReadFile(exe); string(data) != "old-bs\n" {
		t.Errorf("二进制被改动了：%q", data)
	}
}

// TestSelfUpdateNeedsYesInNonInteractive：非交互 + 没有 --yes 必须以退出码 5 结束，
// 且不下载归档、不替换任何东西。
func TestSelfUpdateNeedsYesInNonInteractive(t *testing.T) {
	withVersion(t, "v0.2.0")
	rt, _, _, _, exe := sandboxForSelfUpdate(t)
	f := startCLIFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", tarGz(t, map[string]string{"bs": "new\n"}))

	code, _, stderr := run(t, rt, "self-update", "--exe", exe)
	if code != ExitNeedsInteraction {
		t.Fatalf("exit = %d, want %d（stderr=%s）", code, ExitNeedsInteraction, stderr)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("错误信息要告诉用户怎么办：\n%s", stderr)
	}
	if len(f.requests) != 0 {
		t.Errorf("没有授权就不该发起任何网络请求：%v", f.requests)
	}
	if data, _ := os.ReadFile(exe); string(data) != "old-bs\n" {
		t.Errorf("二进制被改动了：%q", data)
	}
}

// TestSelfUpdateInteractiveConfirm：交互式回答 y 才替换（回答 n 什么都不做）。
func TestSelfUpdateInteractiveConfirm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("归档里的假二进制是 shell 脚本")
	}
	for _, tc := range []struct {
		name     string
		answer   string
		replaced bool
	}{
		{"回答 y 则替换", "y\n", true},
		{"回答 n 则不动", "n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withVersion(t, "v0.2.0")
			rt, _, _, _, exe := sandboxForSelfUpdate(t)
			startCLIFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz",
				tarGz(t, map[string]string{"bs": "#!/bin/sh\necho bs v0.3.0\n"}))
			yes := true
			rt.interactiveOverride = &yes
			rt.Opts.In = strings.NewReader(tc.answer)

			code, stdout, stderr := run(t, rt, "self-update", "--exe", exe)
			if code != ExitOK {
				t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
			}
			data, _ := os.ReadFile(exe)
			got := strings.Contains(string(data), "v0.3.0")
			if got != tc.replaced {
				t.Errorf("替换 = %v, want %v（内容 %q）\nstdout=%s", got, tc.replaced, data, stdout)
			}
			if !tc.replaced && !strings.Contains(stdout, "已取消") {
				t.Errorf("拒绝后应说明已取消：\n%s", stdout)
			}
			if tc.replaced && !strings.Contains(stdout, "已更新") {
				t.Errorf("替换后应报告结果：\n%s", stdout)
			}
		})
	}
}

// TestSelfUpdateYesReplacesAndBacksUp：--yes 全自动替换，旧版本留备份。
func TestSelfUpdateYesReplacesAndBacksUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("归档里的假二进制是 shell 脚本")
	}
	withVersion(t, "v0.2.0")
	rt, _, _, _, exe := sandboxForSelfUpdate(t)
	startCLIFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz",
		tarGz(t, map[string]string{"bs": "#!/bin/sh\necho bs v0.3.0\n"}))

	code, stdout, stderr := run(t, rt, "self-update", "--yes", "--exe", exe)
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read exe: %v", err)
	}
	if !strings.Contains(string(data), "v0.3.0") {
		t.Errorf("没有被替换：%q", data)
	}
	if !strings.Contains(stdout, "已更新") || !strings.Contains(stdout, "sha256 校验通过") {
		t.Errorf("结果说明不完整：\n%s", stdout)
	}
	backups, _ := filepath.Glob(exe + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("应当正好留一个备份，实际 %v", backups)
	}
	old, _ := os.ReadFile(backups[0])
	if string(old) != "old-bs\n" {
		t.Errorf("备份内容不对：%q", old)
	}
}

// TestSelfUpdateWithoutChecksumsAborts：拿不到校验和就中止（安全红线）。
func TestSelfUpdateWithoutChecksumsAborts(t *testing.T) {
	withVersion(t, "v0.2.0")
	rt, _, _, _, exe := sandboxForSelfUpdate(t)
	f := startCLIFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", tarGz(t, map[string]string{"bs": "new\n"}))
	f.checksum = ""

	code, _, stderr := run(t, rt, "self-update", "--yes", "--exe", exe)
	if code != ExitFail {
		t.Fatalf("exit = %d, want %d（stderr=%s）", code, ExitFail, stderr)
	}
	if !strings.Contains(stderr, "校验和") {
		t.Errorf("错误要说清原因：\n%s", stderr)
	}
	if f.downloaded() {
		t.Error("没有校验和时不该下载归档")
	}
	if data, _ := os.ReadFile(exe); string(data) != "old-bs\n" {
		t.Errorf("二进制被改动了：%q", data)
	}
}

// TestSelfUpdateJSON：--json 输出结构化结果，且不含任何人类可读的杂讯。
func TestSelfUpdateJSON(t *testing.T) {
	withVersion(t, "v0.2.0")
	rt, _, _, _, exe := sandboxForSelfUpdate(t)
	startCLIFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz", tarGz(t, map[string]string{"bs": "new\n"}))

	code, stdout, stderr := run(t, rt, "self-update", "--json", "--dry-run", "--exe", exe)
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json 输出不是合法 JSON：%v\n%s", err, stdout)
	}
	if res["latest"] != "v0.3.0" || res["check_only"] != false || res["dry_run"] != true {
		t.Errorf("JSON 字段不对：%v", res)
	}
	if res["sha256"] == "" || res["sha256"] == nil {
		t.Errorf("dry-run 应当带上校验和：%v", res)
	}

	// --check 只报版本
	code, stdout, _ = run(t, rt, "self-update", "--json", "--check", "--exe", exe)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("bad json: %v\n%s", err, stdout)
	}
	if res["check_only"] != true {
		t.Errorf("check_only 应当为 true：%v", res)
	}
}

// TestSelfUpdateVersionFlagPinsTag：--version 指定目标版本时不去查 latest。
func TestSelfUpdateVersionFlagPinsTag(t *testing.T) {
	withVersion(t, "v0.2.0")
	rt, _, _, _, exe := sandboxForSelfUpdate(t)
	f := startCLIFakeRelease(t, "v0.9.9", "bootstrap_0.3.0_linux_arm64.tar.gz", tarGz(t, map[string]string{"bs": "new\n"}))

	code, stdout, stderr := run(t, rt, "self-update", "--check", "--version", "0.3.0", "--exe", exe)
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "v0.3.0") {
		t.Errorf("应当报告指定版本：\n%s", stdout)
	}
	for _, p := range f.requests {
		if strings.Contains(p, "releases/latest") {
			t.Errorf("指定了版本就不该查 latest：%v", f.requests)
		}
	}
}

// TestSelfUpdateForceReinstallsSameVersion：--force 让"已是最新"也重装一遍。
func TestSelfUpdateForceReinstallsSameVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("归档里的假二进制是 shell 脚本")
	}
	withVersion(t, "v0.3.0")
	rt, _, _, _, exe := sandboxForSelfUpdate(t)
	startCLIFakeRelease(t, "v0.3.0", "bootstrap_0.3.0_linux_arm64.tar.gz",
		tarGz(t, map[string]string{"bs": "#!/bin/sh\necho bs v0.3.0-reinstalled\n"}))

	code, stdout, stderr := run(t, rt, "self-update", "--yes", "--force", "--exe", exe)
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	data, _ := os.ReadFile(exe)
	if !strings.Contains(string(data), "reinstalled") {
		t.Errorf("--force 应当重装：%q", data)
	}
	if !strings.Contains(stdout, "已更新") {
		t.Errorf("应当报告结果：\n%s", stdout)
	}
}
