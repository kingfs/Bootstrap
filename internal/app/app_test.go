package app

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kingfs/Bootstrap/internal/config"
	"github.com/kingfs/Bootstrap/pkg/hostinfo"
	"github.com/kingfs/Bootstrap/pkg/proxyenv"
)

// 测试用别名：让用例读起来更接近业务语言。
type proxyProfile = proxyenv.Profile

const (
	shellBash = proxyenv.ShellBash
	shellFish = proxyenv.ShellFish
	shellPwsh = proxyenv.ShellPwsh
)

// newTestApp 构造一个完全跑在临时目录里的 App（等价于"注入了 FakeExecEnv"）：
// 路径、平台信息、输出目标全部可控，不会触碰真实 $HOME 或系统状态。
func newTestApp(t *testing.T) (*App, string) {
	t.Helper()
	root := t.TempDir()
	paths := config.Paths{
		ConfigDir: filepath.Join(root, "config"),
		StateDir:  filepath.Join(root, "state"),
		CacheDir:  filepath.Join(root, "cache"),
		HomeDir:   filepath.Join(root, "home"),
	}
	cfg := config.Defaults()
	cfg.Repo.Root = filepath.Join(paths.HomeDir, "src")
	cfg.Shell.SnippetsDir = filepath.Join(paths.ConfigDir, "shell")
	cfg.Tool.ExtraPath = []string{filepath.Join(paths.HomeDir, ".local", "bin")}
	cfg.Proxy.Profiles = map[string]proxyProfile{
		"local": {
			HTTPProxy:  "http://127.0.0.1:7890",
			HTTPSProxy: "http://127.0.0.1:7890",
			NoProxy:    "127.0.0.1,localhost",
		},
	}
	platform := hostinfo.Platform{
		OS:              "linux",
		Arch:            "arm64",
		Distro:          "ubuntu",
		DistroVer:       "24.04",
		Shell:           "bash",
		HomeDir:         paths.HomeDir,
		PackageManagers: []string{"apt-get"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(paths, &cfg, nil, platform, logger, io.Discard, io.Discard)
	return a, root
}

// snapshot 记录目录树（路径 + 大小 + mtime），用于断言"某操作没有产生持久化改动"。
func snapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// 目录自身的 mtime 会因子项的创建/删除而变化，因此目录只记录存在性；
		// 文件记录大小与 mtime，"没有持久化改动"才是有意义的断言。
		if d.IsDir() {
			out = append(out, path+"|dir")
			return nil
		}
		out = append(out, path+"|"+d.Type().String()+"|"+itoa(info.Size())+"|"+info.ModTime().UTC().Format(time.RFC3339Nano))
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func TestRenderPathPerShell(t *testing.T) {
	a, _ := newTestApp(t)

	got, err := a.RenderPath(shellBash)
	if err != nil {
		t.Fatalf("RenderPath: %v", err)
	}
	// 目录形状按宿主平台算：Windows 上是 `…\.local\bin`，断言不能写死 `.local/bin`。
	if !strings.HasPrefix(got, "export PATH=") || !strings.Contains(got, filepath.Join(".local", "bin")) {
		t.Errorf("bash PATH = %q", got)
	}

	fish, err := a.RenderPath(shellFish)
	if err != nil {
		t.Fatalf("RenderPath fish: %v", err)
	}
	if !strings.HasPrefix(fish, "set -gx PATH ") || !strings.HasSuffix(fish, " $PATH") {
		t.Errorf("fish PATH = %q", fish)
	}

	pwsh, err := a.RenderPath(shellPwsh)
	if err != nil {
		t.Fatalf("RenderPath pwsh: %v", err)
	}
	if !strings.Contains(pwsh, `$env:PATH = `) {
		t.Errorf("pwsh PATH = %q", pwsh)
	}
}

// TestRenderPathIsIdempotent 断言已在 PATH 里的目录不会被重复加入：
// 这让"每次开 shell 都 eval"这件事天然安全。
func TestRenderPathIsIdempotent(t *testing.T) {
	a, root := newTestApp(t)
	// 只看 extra_path 的行为：内置清单里也有 path_dirs，那部分由
	// TestPathEntriesIncludeManifestPathDirs 覆盖。
	a.Cfg.Tool.Manifest.Builtin = false
	already := filepath.Join(root, "already")
	if err := os.MkdirAll(already, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("PATH", already+string(os.PathListSeparator)+os.Getenv("PATH"))
	a.Cfg.Tool.ExtraPath = []string{already}

	got, err := a.RenderPath(shellBash)
	if err != nil {
		t.Fatalf("RenderPath: %v", err)
	}
	if got != "" {
		t.Errorf("already-present entry must produce no output, got %q", got)
	}
}

// TestPathEntriesIncludeManifestPathDirs：清单里显式声明的 path_dirs 要进 PATH，
// 而 provides（可能是 nvm.sh 这种文件）不能被当成目录塞进 PATH。
func TestPathEntriesIncludeManifestPathDirs(t *testing.T) {
	a, root := newTestApp(t)
	userDir := filepath.Join(root, "config", "tools")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `
version: 1
tools:
  - id: demo
    stack: base
    kind: cli
    verify: {command: demo}
    provides: ["~/.demo/rc.sh"]
    path_dirs: ["~/.demo/bin"]
    install:
      - via: brew
        params: {formula: demo}
`
	if err := os.WriteFile(filepath.Join(userDir, "demo.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	a.Cfg.Tool.Manifest = config.ManifestConfig{Builtin: false, UserDirs: []string{userDir}}

	var got []string
	for _, e := range a.PathEntries() {
		got = append(got, e.Path)
		if strings.HasSuffix(e.Path, "rc.sh") {
			t.Errorf("provides 是文件，不应进 PATH: %v", e)
		}
	}
	want := filepath.Join(a.Paths.HomeDir, ".demo", "bin")
	if !slices.Contains(got, want) {
		t.Errorf("path_dirs 缺失: got %v, want %s", got, want)
	}
}

func TestPathEntriesReportExistence(t *testing.T) {
	a, root := newTestApp(t)
	a.Cfg.Tool.Manifest.Builtin = false
	existing := filepath.Join(root, "exists")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	a.Cfg.Tool.ExtraPath = []string{existing, filepath.Join(root, "missing")}

	entries := a.PathEntries()
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
	if !entries[0].Exists || entries[1].Exists {
		t.Errorf("existence detection wrong: %+v", entries)
	}
}

func TestShellInitUsesBuiltinAndUserSnippets(t *testing.T) {
	a, _ := newTestApp(t)

	init, err := a.ShellInit("bash")
	if err != nil {
		t.Fatalf("ShellInit: %v", err)
	}
	if init.Family != "posix" {
		t.Errorf("Family = %q, want posix", init.Family)
	}
	if !strings.Contains(init.Content, "command bs") {
		t.Error("builtin proxy wrapper missing")
	}
	if !strings.Contains(init.Content, "startproxy()") {
		t.Error("startproxy helper missing")
	}
	if len(init.Snippets) < 3 {
		t.Errorf("expected >= 3 builtin snippets, got %v", init.Snippets)
	}

	// 用户片段同名覆盖内置片段。
	userDir := filepath.Join(a.Cfg.Shell.SnippetsDir, "posix")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "10-proxy.sh"), []byte("# 我的代理包装\nalias myproxy='echo hi'\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	init, err = a.ShellInit("bash")
	if err != nil {
		t.Fatalf("ShellInit: %v", err)
	}
	if !strings.Contains(init.Content, "我的代理包装") {
		t.Error("user snippet should override the builtin one")
	}

	if _, err := a.ShellInit("cmd"); err == nil {
		t.Error("cmd.exe should be rejected")
	}
}

// TestShellInitHonorsAutoPathAndAutoProxy：shell.auto_path / shell.auto_proxy 必须真的起作用。
//
// 这两个开关不写进 rc（受管块只有一行 eval），而是在每次开 shell 时由
// `bs shell init <shell>` 现算，因此改配置立刻生效、无需重写 rc 文件。
func TestShellInitHonorsAutoPathAndAutoProxy(t *testing.T) {
	a, _ := newTestApp(t)

	// 默认：装配 PATH，不自动应用代理。
	init, err := a.ShellInit("bash")
	if err != nil {
		t.Fatalf("ShellInit: %v", err)
	}
	if !hasSnippet(init.Content, "20-path") || !strings.Contains(init.Content, "bs env path") {
		t.Error("默认应当包含 PATH 装配片段")
	}
	if strings.Contains(init.Content, "99-auto-proxy") {
		t.Error("默认不该自动应用代理（会污染新 shell）")
	}

	// 关掉 PATH 装配、打开自动应用代理。
	a.Cfg.Shell.AutoPath = false
	a.Cfg.Shell.AutoProxy = true
	init, err = a.ShellInit("bash")
	if err != nil {
		t.Fatalf("ShellInit: %v", err)
	}
	if hasSnippet(init.Content, "20-path") || strings.Contains(init.Content, "bs env path") {
		t.Errorf("auto_path=false 时不该出现 PATH 装配片段：\n%s", init.Content)
	}
	if !strings.Contains(init.Content, "99-auto-proxy") ||
		!strings.Contains(init.Content, "proxy env --apply") {
		t.Errorf("auto_proxy=true 时应当追加上自动应用片段：\n%s", init.Content)
	}
	// 头部要说明为什么少了片段，否则会被当成 bug。
	if !strings.Contains(init.Content, "shell.auto_path=false") {
		t.Error("应当在头部说明省略了 PATH 装配片段")
	}
	for _, sn := range init.Snippets {
		if strings.HasPrefix(sn.Name, "20-path") {
			t.Errorf("片段清单里也不该有 20-path：%+v", init.Snippets)
		}
	}
	// 用户自己覆盖的 PATH 片段同样按名字被过滤（避免"开关失效"的错觉）。
	userDir := filepath.Join(a.Cfg.Shell.SnippetsDir, "posix")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "20-path.sh"), []byte("export PATH=my-way:$PATH\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	init, err = a.ShellInit("bash")
	if err != nil {
		t.Fatalf("ShellInit: %v", err)
	}
	if strings.Contains(init.Content, "my-way") {
		t.Error("用户覆盖的 20-path 片段也该被 auto_path=false 过滤掉")
	}

	// fish / pwsh 的自动应用语法必须各自正确（不能把 posix 语法塞给 fish）。
	a.Cfg.Shell.AutoPath = true
	for _, tc := range []struct{ shell, want string }{
		{"fish", "command bs proxy env --apply --shell fish | source"},
		{"pwsh", "proxy env --apply --shell pwsh | Out-String | Invoke-Expression"},
	} {
		init, err := a.ShellInit(tc.shell)
		if err != nil {
			t.Fatalf("ShellInit(%s): %v", tc.shell, err)
		}
		if !strings.Contains(init.Content, tc.want) {
			t.Errorf("%s 的自动应用语法不对，应当包含 %q：\n%s", tc.shell, tc.want, init.Content)
		}
	}
}

// hasSnippet 报告渲染结果里是否包含某个片段（按 Render 的标题行判断，
// 避免"头部说明里提到过片段名"被误当成片段本身）。
func hasSnippet(content, name string) bool {
	return strings.Contains(content, "# ─── "+name)
}

// TestShellInstallIsIdempotentAndReversible 覆盖 rc 受管块的三条硬要求：
// 幂等、可精确卸载、块外内容不变。
func TestShellInstallIsIdempotentAndReversible(t *testing.T) {
	a, _ := newTestApp(t)
	rc := filepath.Join(a.Paths.HomeDir, ".bashrc")
	original := "# 我自己的配置\nexport EDITOR=vim\nalias ll='ls -alh'\n"
	if err := os.MkdirAll(filepath.Dir(rc), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := a.ShellInstall("bash", rc)
	if err != nil {
		t.Fatalf("ShellInstall: %v", err)
	}
	if !res.Changed || !strings.Contains(readFile(t, rc), ">>> bootstrap") {
		t.Fatalf("managed block not installed: %+v", res)
	}

	before, err := os.Stat(rc)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	res, err = a.ShellInstall("bash", rc)
	if err != nil {
		t.Fatalf("ShellInstall again: %v", err)
	}
	if res.Changed {
		t.Error("repeated install must be a no-op")
	}
	after, err := os.Stat(rc)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("no-op install touched the rc file")
	}

	st, err := a.ShellStatusOf("bash", rc)
	if err != nil {
		t.Fatalf("ShellStatusOf: %v", err)
	}
	if !st.HasBlock || !st.UpToDate {
		t.Errorf("status = %+v, want installed and up to date", st)
	}

	out, err := a.ShellUninstall("bash", rc)
	if err != nil {
		t.Fatalf("ShellUninstall: %v", err)
	}
	if !out.Removed {
		t.Error("uninstall should report removal")
	}
	if got := readFile(t, rc); got != original {
		t.Errorf("rc content changed:\n%q\nwant\n%q", got, original)
	}
}

// TestShellInstallDryRunWritesNothing 是 dry-run 的核心断言。
func TestShellInstallDryRunWritesNothing(t *testing.T) {
	a, _ := newTestApp(t)
	rc := filepath.Join(a.Paths.HomeDir, ".bashrc")
	a.DryRun = true

	res, err := a.ShellInstall("bash", rc)
	if err != nil {
		t.Fatalf("ShellInstall: %v", err)
	}
	if !res.Changed {
		t.Error("dry-run should still report what would change")
	}
	if _, err := os.Stat(rc); !os.IsNotExist(err) {
		t.Fatalf("dry-run created the rc file (%v)", err)
	}
}

func TestShellUninstallMissingRCIsNoOp(t *testing.T) {
	a, root := newTestApp(t)
	before := snapshot(t, root)
	rc := filepath.Join(a.Paths.HomeDir, "nope", ".bashrc")

	res, err := a.ShellUninstall("bash", rc)
	if err != nil {
		t.Fatalf("ShellUninstall: %v", err)
	}
	if res.Removed {
		t.Error("nothing to remove")
	}
	if got := snapshot(t, root); strings.Join(before, "\n") != strings.Join(got, "\n") {
		t.Error("uninstall on a missing rc must not create anything")
	}
}

// TestRepoGetDryRunCreatesNothing 断言应用层的 dry-run 也不落盘。
func TestRepoGetDryRunCreatesNothing(t *testing.T) {
	a, root := newTestApp(t)
	a.DryRun = true
	a.Runner.DryRun = true
	before := snapshot(t, root)

	results, err := a.RepoGet(context.Background(), []string{"kingfs/Bootstrap", "https://gitlab.com/group/sub/proj"}, RepoOptions{})
	if err != nil {
		t.Fatalf("RepoGet: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v", results)
	}
	for _, r := range results {
		if r.Action != "clone" {
			t.Errorf("action = %q, want clone", r.Action)
		}
		if len(r.Commands) == 0 {
			t.Errorf("dry-run should report the commands it would run: %+v", r)
		}
	}
	if got := snapshot(t, root); strings.Join(before, "\n") != strings.Join(got, "\n") {
		t.Error("dry-run modified the filesystem")
	}
	if _, err := os.Stat(a.Cfg.Repo.Root); !os.IsNotExist(err) {
		t.Error("dry-run created the repo root")
	}
}

func TestRepoListOnMissingRootIsEmpty(t *testing.T) {
	a, _ := newTestApp(t)
	views, root, err := a.RepoList(context.Background())
	if err != nil {
		t.Fatalf("RepoList: %v", err)
	}
	if len(views) != 0 {
		t.Errorf("views = %+v, want empty", views)
	}
	if root != a.Cfg.Repo.Root {
		t.Errorf("root = %q, want %q", root, a.Cfg.Repo.Root)
	}
}

func TestProxyLifecycleThroughApp(t *testing.T) {
	a, _ := newTestApp(t)

	res, err := a.ProxyUse(context.Background(), "local")
	if err != nil {
		t.Fatalf("ProxyUse: %v", err)
	}
	if res.Name != "local" {
		t.Errorf("Name = %q", res.Name)
	}
	text, err := a.RenderProxyEnv("", true, "bash")
	if err != nil {
		t.Fatalf("RenderProxyEnv: %v", err)
	}
	if !strings.Contains(text, "export HTTPS_PROXY='http://127.0.0.1:7890'") {
		t.Errorf("apply output = %q", text)
	}

	if err := a.ProxyOff(context.Background()); err != nil {
		t.Fatalf("ProxyOff: %v", err)
	}
	text, err = a.RenderProxyEnv("", true, "bash")
	if err != nil {
		t.Fatalf("RenderProxyEnv after off: %v", err)
	}
	if !strings.HasPrefix(text, "unset ") || !strings.Contains(text, "HTTPS_PROXY") {
		t.Errorf("off output = %q", text)
	}

	// off 会被持久化：重新构造 App（同一个 state 目录）仍然是关闭状态。
	cfg := a.Cfg
	reopened := New(a.Paths, cfg, nil, a.Platform, a.Log, io.Discard, io.Discard)
	text, err = reopened.RenderProxyEnv("", true, "bash")
	if err != nil {
		t.Fatalf("RenderProxyEnv reopened: %v", err)
	}
	if !strings.HasPrefix(text, "unset ") || !strings.Contains(text, "HTTPS_PROXY") {
		t.Errorf("off state did not survive reopen: %q", text)
	}
}

func TestProxyUseUnknownProfile(t *testing.T) {
	a, _ := newTestApp(t)
	if _, err := a.ProxyUse(context.Background(), "nope"); err == nil {
		t.Error("unknown profile should fail")
	}
}

func TestProxyStatusMasksCredentials(t *testing.T) {
	a, _ := newTestApp(t)
	a.Cfg.Proxy.Profiles["corp"] = proxyProfile{
		HTTPProxy:  "http://user:pw@proxy.corp:8123",
		HTTPSProxy: "http://user:pw@proxy.corp:8123",
	}
	if _, err := a.ProxyUse(context.Background(), "corp"); err != nil {
		t.Fatalf("ProxyUse: %v", err)
	}
	summary, err := a.ProxyStatus()
	if err != nil {
		t.Fatalf("ProxyStatus: %v", err)
	}
	if strings.Contains(summary.HTTPProxy, "user:pw") {
		t.Errorf("credentials leaked into status: %+v", summary)
	}
	if !strings.Contains(summary.HTTPSProxy, "***@proxy.corp:8123") {
		t.Errorf("masked value = %q", summary.HTTPSProxy)
	}
}

func TestProxyAddPrintsSnippetWithoutWriting(t *testing.T) {
	a, root := newTestApp(t)
	before := snapshot(t, root)

	res, err := a.ProxyAdd("smoke", "http://127.0.0.1:7890", "", "localhost", false)
	if err != nil {
		t.Fatalf("ProxyAdd: %v", err)
	}
	if !strings.Contains(res.Snippet, "http_proxy: http://127.0.0.1:7890") {
		t.Errorf("snippet = %q", res.Snippet)
	}
	if res.Written {
		t.Error("without --write, nothing should be written")
	}
	if got := snapshot(t, root); strings.Join(before, "\n") != strings.Join(got, "\n") {
		t.Error("ProxyAdd without --write modified the filesystem")
	}

	if _, err := a.ProxyAdd("bad", "", "", "", false); err == nil {
		t.Error("adding a profile without any proxy URL should fail")
	}
}

func TestProxyAddWriteCreatesConfig(t *testing.T) {
	a, _ := newTestApp(t)
	res, err := a.ProxyAdd("smoke", "http://127.0.0.1:7890", "http://127.0.0.1:7890", "localhost", true)
	if err != nil {
		t.Fatalf("ProxyAdd: %v", err)
	}
	if !res.Written {
		t.Fatal("--write should write the config file")
	}
	data := readFile(t, a.Paths.ConfigFile())
	if !strings.Contains(data, "smoke") || !strings.Contains(data, "http://127.0.0.1:7890") {
		t.Errorf("config content = %q", data)
	}
	// 写出来的配置必须能被自己的装载器读回并通过校验。
	loaded, err := config.Load(config.LoadOptions{Paths: a.Paths, GOOS: "linux", Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("the config written by ProxyAdd must be loadable: %v", err)
	}
	if _, ok := loaded.Config.Proxy.Profiles["smoke"]; !ok {
		t.Error("profile missing after reload")
	}
}

// TestDoctorIsReadOnly 断言 doctor 不留下任何持久化改动。
//
// 唯一的例外是"目录可写性探测"：它会在最近的已存在祖先目录里创建一个临时探针文件
// 并立刻删除。这里正是要证明这个探测不会留下痕迹。
func TestDoctorIsReadOnly(t *testing.T) {
	a, root := newTestApp(t)
	before := snapshot(t, root)

	checks := a.Doctor(context.Background())
	if len(checks) == 0 {
		t.Fatal("doctor returned no checks")
	}

	after := snapshot(t, root)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("doctor modified the filesystem:\nbefore=%v\nafter=%v", before, after)
	}
	for _, c := range checks {
		if c.Status == StatusFail {
			t.Errorf("unexpected failing check: %+v", c)
		}
	}
}

func TestDoctorFlagsMissingConfigAndRC(t *testing.T) {
	a, _ := newTestApp(t)
	byName := map[string]Check{}
	for _, c := range a.Doctor(context.Background()) {
		byName[c.Name] = c
	}
	if got := byName["config"].Status; got != StatusWarn {
		t.Errorf("config check = %q, want warn (no config file yet)", got)
	}
	if got := byName["shell rc"].Status; got != StatusWarn {
		t.Errorf("shell rc check = %q, want warn (block not installed)", got)
	}
	if got := byName["git"].Status; got != StatusOK {
		t.Errorf("git check = %q, want ok", got)
	}
	ok, warn, fail, _ := CheckSummary(a.Doctor(context.Background()))
	if fail != 0 || ok == 0 || warn == 0 {
		t.Errorf("summary = ok:%d warn:%d fail:%d", ok, warn, fail)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestShellInitHonorsCompletion：shell.completion 与 auto_path/auto_proxy 一样，
// 只在渲染阶段起作用（默认关），并且按**具体 shell** 生成加载语句。
func TestShellInitHonorsCompletion(t *testing.T) {
	a, _ := newTestApp(t)

	init, err := a.ShellInit("bash")
	if err != nil {
		t.Fatalf("ShellInit: %v", err)
	}
	if hasSnippet(init.Content, "99-completion") || strings.Contains(init.Content, "bs completion") {
		t.Errorf("默认不该加载补全（会改变每个新 shell 的行为）：\n%s", init.Content)
	}

	a.Cfg.Shell.Completion = true
	for _, tc := range []struct{ shell, want string }{
		{"bash", `eval "$(command bs completion bash)"`},
		{"zsh", `eval "$(command bs completion zsh)"`},
		{"fish", "command bs completion fish | source"},
		{"pwsh", "completion powershell"},
	} {
		init, err := a.ShellInit(tc.shell)
		if err != nil {
			t.Fatalf("ShellInit(%s): %v", tc.shell, err)
		}
		if !hasSnippet(init.Content, "99-completion") || !strings.Contains(init.Content, tc.want) {
			t.Errorf("%s 的补全片段不对，应当包含 %q：\n%s", tc.shell, tc.want, init.Content)
		}
		// 片段必须出现在最后（放在别名/代理包装之后），且 Fish 语法不能被塞进 posix。
		if !strings.HasSuffix(strings.TrimRight(init.Content, "\n"), "fi") &&
			!strings.HasSuffix(strings.TrimRight(init.Content, "\n"), "end") &&
			!strings.HasSuffix(strings.TrimRight(init.Content, "\n"), "}") {
			t.Errorf("%s 的补全片段应当在末尾：\n%s", tc.shell, init.Content)
		}
	}
}

// TestCompletionInstallTurnsOnSwitchAndWritesRC 覆盖 install 的两件事：
// 打开 shell.completion + 幂等写入 rc 受管块，且第二次执行不碰任何文件。
func TestCompletionInstallTurnsOnSwitchAndWritesRC(t *testing.T) {
	a, _ := newTestApp(t)
	rc := filepath.Join(a.Paths.HomeDir, ".bashrc")
	original := "# 我自己的配置\nexport EDITOR=vim\n"
	if err := os.MkdirAll(filepath.Dir(rc), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := a.CompletionInstall("bash", rc)
	if err != nil {
		t.Fatalf("CompletionInstall: %v", err)
	}
	if !res.ConfigSet || !res.RCChanged {
		t.Fatalf("第一次应当同时改配置与 rc：%+v", res)
	}
	cfg := readFile(t, a.Paths.ConfigFile())
	if !strings.Contains(cfg, "completion: true") {
		t.Errorf("配置里没有打开开关：\n%s", cfg)
	}
	installed := readFile(t, rc)
	if !strings.Contains(installed, ">>> bootstrap") || !strings.Contains(installed, original) {
		t.Errorf("rc 受管块没装好或破坏了原有内容：\n%s", installed)
	}
	// 打开开关后，渲染结果里就该出现补全片段（"装完即生效"的实质）。
	init, err := a.ShellInit("bash")
	if err != nil {
		t.Fatalf("ShellInit: %v", err)
	}
	if !strings.Contains(init.Content, "bs completion bash") {
		t.Errorf("install 之后 shell init 仍未渲染补全：\n%s", init.Content)
	}

	// 第二次：两处都不该再动（mtime 不变是"没有无谓写盘"的证据）。
	cfgBefore, err := os.Stat(a.Paths.ConfigFile())
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	rcBefore, err := os.Stat(rc)
	if err != nil {
		t.Fatalf("stat rc: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	res, err = a.CompletionInstall("bash", rc)
	if err != nil {
		t.Fatalf("CompletionInstall again: %v", err)
	}
	if res.ConfigSet || res.RCChanged {
		t.Errorf("重复执行应当什么都不改：%+v", res)
	}
	cfgAfter, _ := os.Stat(a.Paths.ConfigFile())
	rcAfter, _ := os.Stat(rc)
	if !cfgBefore.ModTime().Equal(cfgAfter.ModTime()) || !rcBefore.ModTime().Equal(rcAfter.ModTime()) {
		t.Error("重复执行改动了文件（mtime 变了）")
	}
}

// TestCompletionInstallDryRunWritesNothing 是应用层的 dry-run 断言。
func TestCompletionInstallDryRunWritesNothing(t *testing.T) {
	a, root := newTestApp(t)
	a.DryRun = true
	rc := filepath.Join(a.Paths.HomeDir, ".bashrc")
	before := snapshot(t, root)

	res, err := a.CompletionInstall("bash", rc)
	if err != nil {
		t.Fatalf("CompletionInstall: %v", err)
	}
	if !res.ConfigSet || !res.RCChanged {
		t.Errorf("dry-run 仍要如实汇报会改什么：%+v", res)
	}
	if _, err := os.Stat(rc); !os.IsNotExist(err) {
		t.Fatalf("dry-run 创建了 rc 文件（%v）", err)
	}
	if _, err := os.Stat(a.Paths.ConfigFile()); !os.IsNotExist(err) {
		t.Fatalf("dry-run 创建了配置文件（%v）", err)
	}
	if got := snapshot(t, root); strings.Join(before, "\n") != strings.Join(got, "\n") {
		t.Error("dry-run 修改了文件系统")
	}
}

// TestCompletionInstallRejectsCmd：cmd.exe 没有 rc，必须在使用前就报错，
// 而不是先改了配置才发现写不了 rc。
func TestCompletionInstallRejectsCmd(t *testing.T) {
	a, root := newTestApp(t)
	before := snapshot(t, root)
	if _, err := a.CompletionInstall("cmd", ""); err == nil {
		t.Fatal("cmd.exe 应当被拒绝")
	}
	if got := snapshot(t, root); strings.Join(before, "\n") != strings.Join(got, "\n") {
		t.Error("被拒绝的 shell 不该留下任何写入")
	}
}

// TestBashCompletionInstalled 覆盖"bash 补全还依赖 bash-completion 包"的探测。
func TestBashCompletionInstalled(t *testing.T) {
	none := func(string) bool { return false }
	if bashCompletionInstalled(none) {
		t.Error("什么都找不到时应当报 false")
	}
	found := func(p string) bool { return p == "/usr/share/bash-completion/completions" }
	if !bashCompletionInstalled(found) {
		t.Error("命中已知落点时应当报 true")
	}
}
