package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// initTestConfig：非交互回退用 base 套装；清单只从用户目录加载（自包含）。
const initTestConfig = `version: 1
log:
  level: error
proxy:
  default_profile: local
  profiles:
    local:
      http_proxy: http://127.0.0.1:7890
      https_proxy: http://127.0.0.1:7890
tool:
  default_profile: base
  manifest:
    builtin: false
    user_dirs:
      - %USERDIR%
`

// initManifestCLI：全部走假管理器安装，执行阶段不联网；
// 探测命令名用 bs-init-probe-*，宿主上不可能存在，因此装机状态完全可控。
const initManifestCLI = `
version: 1
tools:
  - id: g
    stack: golang
    kind: versionmgr
    description: 假管理器
    verify: {command: g}
    install:
      - via: manager
        params: {manager: g, action: install, version: g}
  - id: runtime-a
    stack: golang
    kind: runtime
    description: 需要管理器安装的运行时
    depends_on: [g]
    verify:
      command: bs-init-probe-a
      version_args: [version]
      version_regex: '(?P<v>\d+\.\d+(?:\.\d+)?)'
      min_version: "1.22"
    install:
      - via: manager
        params: {manager: g, action: install, version: "1.24.1"}
  - id: cli-b
    stack: base
    kind: cli
    description: CLI 工具
    verify: {command: bs-init-probe-b}
    install:
      - via: manager
        params: {manager: g, action: install, version: cli-b}
profiles:
  base:
    tools: [g, runtime-a]
  tiny:
    tools: [cli-b]
`

// sandboxForInit 准备一个"配置已存在 + 假管理器可用"的 CLI 沙箱，
// 返回 Runtime、各输出缓冲区与沙箱根目录。
func sandboxForInit(t *testing.T) (*Runtime, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	rt, out, errBuf, root := sandbox(t)
	userDir := filepath.Join(root, "config", "tools")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "tools.yaml"), []byte(initManifestCLI), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	writeConfig(t, root, strings.ReplaceAll(initTestConfig, "%USERDIR%", userDir))

	// 假管理器：`g install <x>` 把对应命令"装"出来。
	// PATH 里保留 /usr/bin:/bin 是因为脚本需要 chmod（shell 内建改不了权限位）。
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeFakeBin(t, binDir, "g", `#!/bin/sh
dir="${0%/*}"
case "$1.$2" in
  install.1.24.1)
    printf '#!/bin/sh\necho "bs-init-probe-a 1.24.1"\n' > "$dir/bs-init-probe-a"; chmod +x "$dir/bs-init-probe-a" ;;
  install.cli-b)
    printf '#!/bin/sh\necho "bs-init-probe-b 0.8.1"\n' > "$dir/bs-init-probe-b"; chmod +x "$dir/bs-init-probe-b" ;;
esac
echo "$@" >> "$FAKE_G_LOG"
exit 0
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("FAKE_G_LOG", filepath.Join(root, "fake-g.log"))
	return rt, out, errBuf, root
}

// TestInitNonInteractiveNeedsYes：非交互 + 没有 --yes，必须停在"要确认"这一步
// （退出码 5），既不能问、也不能自己动手。
func TestInitNonInteractiveNeedsYes(t *testing.T) {
	rt, _, _, root := sandboxForInit(t)
	code, stdout, stderr := run(t, rt, "init")
	if code != ExitNeedsInteraction {
		t.Fatalf("exit = %d, want %d\nstdout=%s\nstderr=%s", code, ExitNeedsInteraction, stdout, stderr)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("错误信息要告诉用户怎么办：\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "fake-g.log")); err == nil {
		t.Error("没有授权就不该执行任何安装命令")
	}
	// 概览必须先打出来（新机器上第一条命令要能看清环境）。
	if !strings.Contains(stdout, "配置文件") || !strings.Contains(stdout, "工具") {
		t.Errorf("应当先展示环境概览：\n%s", stdout)
	}
}

// TestInitYesInstallsProfile：--yes 全自动跑通"计划 → 执行 → 报告"。
func TestInitYesInstallsProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假管理器 g 是 POSIX 脚本：它要用 sh 把新命令写出来并 chmod +x，" +
			"Windows 上没有 sh 也没有可执行位（真机行为待补，见 AGENTS §10）")
	}
	rt, _, _, root := sandboxForInit(t)
	code, stdout, stderr := run(t, rt, "init", "--yes", "--no-shell")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	log, err := os.ReadFile(filepath.Join(root, "fake-g.log"))
	if err != nil {
		t.Fatalf("假管理器没有被调用：%v\nstdout=%s", err, stdout)
	}
	if !strings.Contains(string(log), "install 1.24.1") {
		t.Errorf("应当委托管理器安装 runtime-a：\n%s", log)
	}
	if !strings.Contains(stdout, "完成") {
		t.Errorf("应当给出安装结果：\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(root, "home", ".bashrc")); err == nil {
		t.Error("--no-shell 不该动 rc 文件")
	}
}

// TestInitDryRunTouchesNothing：--dry-run 只说不做——配置文件、状态、缓存之外
// 不能有任何新增；这里用目录快照严格比对。
func TestInitDryRunTouchesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假管理器 g 是 POSIX 脚本：它要用 sh 把新命令写出来并 chmod +x，" +
			"Windows 上没有 sh 也没有可执行位（真机行为待补，见 AGENTS §10）")
	}
	rt, _, _, root := sandboxForInit(t)
	before := snapshotTree(t, root)

	code, stdout, stderr := run(t, rt, "init", "--yes", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "dry-run") {
		t.Errorf("dry-run 应当明确标注：\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(root, "fake-g.log")); err == nil {
		t.Error("--dry-run 绝不能执行安装命令")
	}
	if diff := diffTree(t, before, root); diff != "" {
		t.Errorf("--dry-run 必须零写入，但有变化：\n%s", diff)
	}
}

// TestInitConfigBootstrap：配置文件缺失时先生成（--yes 表示同意），
// 然后继续用默认套装。
func TestInitConfigBootstrap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假管理器 g 是 POSIX 脚本：它要用 sh 把新命令写出来并 chmod +x，" +
			"Windows 上没有 sh 也没有可执行位（真机行为待补，见 AGENTS §10）")
	}
	rt, _, _, root := sandboxForInit(t)
	if err := os.Remove(filepath.Join(root, "config", "config.yaml")); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	code, stdout, stderr := run(t, rt, "init", "--yes", "--tools", "cli-b", "--no-shell")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "config.yaml")); err != nil {
		t.Errorf("应当生成默认配置：%v", err)
	}
	if !strings.Contains(stdout, "已生成配置文件") {
		t.Errorf("应当说明生成了配置：\n%s", stdout)
	}
}

// TestInitInteractiveConfirmThenInstall：交互式下问一句，回答 y 才动手。
func TestInitInteractiveConfirmThenInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假管理器 g 是 POSIX 脚本：它要用 sh 把新命令写出来并 chmod +x，" +
			"Windows 上没有 sh 也没有可执行位（真机行为待补，见 AGENTS §10）")
	}
	rt, _, _, root := sandboxForInit(t)
	yes := true
	rt.interactiveOverride = &yes
	// tiny 只有一个工具 → 没有选择提问，只有"要执行吗"。
	rt.Opts.In = strings.NewReader("y\n")

	code, stdout, stderr := run(t, rt, "init", "--profile", "tiny")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "继续？") && !strings.Contains(stderr, "要执行") {
		t.Errorf("交互式下应当先问一句：\n%s", stderr)
	}
	log, err := os.ReadFile(filepath.Join(root, "fake-g.log"))
	if err != nil || !strings.Contains(string(log), "install cli-b") {
		t.Errorf("回答 y 后应当执行安装：%v\n%s\nstdout=%s", err, log, stdout)
	}
}

// TestInitInteractiveCancelIsSuccess：交互式下回答 n 是"主动取消"，
// 退出码 0 且什么都没做（取消不是失败）。
func TestInitInteractiveCancelIsSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假管理器 g 是 POSIX 脚本：它要用 sh 把新命令写出来并 chmod +x，" +
			"Windows 上没有 sh 也没有可执行位（真机行为待补，见 AGENTS §10）")
	}
	rt, _, _, root := sandboxForInit(t)
	yes := true
	rt.interactiveOverride = &yes
	rt.Opts.In = strings.NewReader("n\n")

	code, stdout, stderr := run(t, rt, "init", "--profile", "tiny")
	if code != ExitOK {
		t.Fatalf("取消应当是退出码 0，实际 %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "已取消") {
		t.Errorf("应当明确说明已取消：\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(root, "fake-g.log")); err == nil {
		t.Error("取消后不该执行安装")
	}
}

// TestInitJSONIsMachineReadable：--json 下不提问、不阻塞，输出结构化结果。
func TestInitJSONIsMachineReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假管理器 g 是 POSIX 脚本：它要用 sh 把新命令写出来并 chmod +x，" +
			"Windows 上没有 sh 也没有可执行位（真机行为待补，见 AGENTS §10）")
	}
	rt, _, _, _ := sandboxForInit(t)
	code, stdout, stderr := run(t, rt, "init", "--json", "--yes", "--no-shell")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	var res struct {
		Overview struct {
			OS           string `json:"os"`
			ConfigExists bool   `json:"config_exists"`
			Tools        struct {
				Applicable int `json:"applicable"`
				Missing    int `json:"missing"`
			} `json:"tools"`
		} `json:"overview"`
		Mode    string   `json:"selection_mode"`
		Profile string   `json:"profile"`
		Tools   []string `json:"tools"`
		Plan    struct {
			Steps []struct {
				ToolID string `json:"tool_id"`
			} `json:"steps"`
		} `json:"plan"`
		Report *struct {
			Steps []struct {
				ToolID string `json:"tool_id"`
				Status string `json:"status"`
			} `json:"steps"`
		} `json:"report"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("--json 输出不是合法 JSON：%v\n%s", err, stdout)
	}
	if res.Overview.OS == "" || !res.Overview.ConfigExists {
		t.Errorf("概览字段缺失：%+v", res.Overview)
	}
	if res.Overview.Tools.Applicable != 3 || res.Overview.Tools.Missing != 2 {
		t.Errorf("装机统计不对：%+v", res.Overview.Tools)
	}
	if res.Mode != "profile" || res.Profile != "base" {
		t.Errorf("选择结果不对：mode=%s profile=%s", res.Mode, res.Profile)
	}
	if res.Report == nil || len(res.Report.Steps) == 0 {
		t.Fatalf("应当有执行报告：%+v", res.Report)
	}
	for _, s := range res.Report.Steps {
		if s.Status == "failed" {
			t.Errorf("不该有失败步骤：%+v", s)
		}
	}
}

// TestInitFlagConflicts：互相矛盾的参数要在用法层面就拒绝（退出码 2）。
func TestInitFlagConflicts(t *testing.T) {
	cases := [][]string{
		{"init", "--with-shell", "--no-shell"},
		{"init", "--missing-only", "--profile", "base"},
		{"init", "--missing-only", "--tools", "g"},
		{"init", "--interactive", "--non-interactive"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			rt, _, _, _ := sandboxForInit(t)
			code, _, stderr := run(t, rt, args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d（stderr=%s）", code, ExitUsage, stderr)
			}
		})
	}
}

// TestInitUnknownProfileIsUsageError：套装写错属于用法错误（退出码 2）。
func TestInitUnknownProfileIsUsageError(t *testing.T) {
	rt, _, _, _ := sandboxForInit(t)
	code, _, stderr := run(t, rt, "init", "--profile", "nosuch", "--yes")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d（stderr=%s）", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "nosuch") {
		t.Errorf("错误信息应当包含写错的套装名：\n%s", stderr)
	}
}

// TestInitMissingOnlyInstallsOnlyMissing：已有机器上"补缺"只处理缺失的。
func TestInitMissingOnlyInstallsOnlyMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假管理器 g 是 POSIX 脚本：它要用 sh 把新命令写出来并 chmod +x，" +
			"Windows 上没有 sh 也没有可执行位（真机行为待补，见 AGENTS §10）")
	}
	rt, _, _, root := sandboxForInit(t)
	writeFakeBin(t, filepath.Join(root, "bin"), "bs-init-probe-a", "#!/bin/sh\necho 'bs-init-probe-a 1.24.1'\n")

	code, stdout, stderr := run(t, rt, "init", "--missing-only", "--yes", "--no-shell", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	var res struct {
		Mode  string   `json:"selection_mode"`
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("bad json: %v\n%s", err, stdout)
	}
	if res.Mode != "missing" || len(res.Tools) != 1 || res.Tools[0] != "cli-b" {
		t.Errorf("应当只补缺失的 cli-b：mode=%s tools=%v", res.Mode, res.Tools)
	}
}
