package detect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
)

// fakeExec 记录被调用的命令并按需返回输出/错误，让探测测试完全离线。
type fakeExec struct {
	mu      sync.Mutex
	calls   []cmdx.Command
	outputs map[string]cmdx.Result
	errs    map[string]error
}

func newFakeExec() *fakeExec {
	return &fakeExec{outputs: map[string]cmdx.Result{}, errs: map[string]error{}}
}

func (f *fakeExec) set(name string, res cmdx.Result) { f.outputs[name] = res }

func (f *fakeExec) Run(_ context.Context, c cmdx.Command) (cmdx.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	// 探测会用 LookPath 解析出的完整路径执行命令（新机器上工具可能还没进 PATH），
	// 因此按 basename 匹配，才不会被"绝对路径 vs 命令名"这种无关差异绊住。
	key := c.Name
	if base := filepath.Base(c.Name); f.has(base) {
		key = base
	}
	if err, ok := f.errs[key]; ok {
		return f.outputs[key], err
	}
	if res, ok := f.outputs[key]; ok {
		return res, nil
	}
	return cmdx.Result{}, errors.New("command not found: " + c.Name)
}

// has 报告是否登记过这个名字（含绝对路径的 basename 形式）。
func (f *fakeExec) has(name string) bool {
	if _, ok := f.outputs[name]; ok {
		return true
	}
	_, ok := f.errs[name]
	return ok
}

func (f *fakeExec) called(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Name == name || filepath.Base(c.Name) == name {
			n++
		}
	}
	return n
}

type fakePath map[string]string

func (f fakePath) LookPath(name string) (string, bool) {
	p, ok := f[name]
	return p, ok
}

func testDetector(exec *fakeExec, paths fakePath, home string) *Detector {
	return &Detector{Executor: exec, LookPath: paths, HomeDir: home, Timeout: time.Second, Concurrency: 4}
}

func tool(id string, verify manifest.Verify, provides ...string) manifest.Tool {
	return manifest.Tool{ID: id, Stack: manifest.StackBase, Verify: verify, Provides: provides}
}

func compile(t *testing.T, tl manifest.Tool) manifest.Tool {
	t.Helper()
	// 复用清单加载器的编译逻辑：直接构造一个只含该条目的清单文件。
	dir := t.TempDir()
	path := filepath.Join(dir, "m.yaml")
	body := "version: 1\ntools:\n  - id: " + tl.ID + "\n    stack: base\n    kind: cli\n    verify:\n"
	if tl.Verify.Command != "" {
		body += "      command: " + tl.Verify.Command + "\n"
	}
	if len(tl.Verify.VersionArgs) > 0 {
		body += "      version_args: [" + strings.Join(tl.Verify.VersionArgs, ", ") + "]\n"
	}
	if tl.Verify.VersionRegex != "" {
		body += "      version_regex: '" + tl.Verify.VersionRegex + "'\n"
	}
	if tl.Verify.MinVersion != "" {
		body += "      min_version: \"" + tl.Verify.MinVersion + "\"\n"
	}
	if tl.Verify.ExactVersion != "" {
		body += "      exact_version: \"" + tl.Verify.ExactVersion + "\"\n"
	}
	for _, p := range tl.Provides {
		body += "    provides: [" + p + "]\n"
	}
	body += "    install:\n      - via: apt\n        params: {packages: [x]}\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := manifest.Load(manifest.LoadOptions{Builtin: false, UserDirs: []string{dir}})
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	out, ok := loaded.ByID(tl.ID)
	if !ok {
		t.Fatalf("fixture tool %q missing", tl.ID)
	}
	return out
}

func TestProbeInstalledParsesVersion(t *testing.T) {
	exec := newFakeExec()
	exec.set("go", cmdx.Result{Stdout: "go version go1.24.1 linux/arm64\n"})
	d := testDetector(exec, fakePath{"go": "/usr/local/go/bin/go"}, "")
	tl := compile(t, tool("golang", manifest.Verify{
		Command: "go", VersionArgs: []string{"version"},
		VersionRegex: `go version go(?P<v>\d+\.\d+(\.\d+)?)`,
	}))

	res := d.Probe(context.Background(), tl)
	if res.Status != StatusInstalled || res.Version != "1.24.1" {
		t.Fatalf("res = %+v", res)
	}
	if res.Command != "/usr/local/go/bin/go" {
		t.Errorf("command = %q", res.Command)
	}
	if res.Cached {
		t.Error("fresh probe must not be marked cached")
	}
	if !res.Present() || !res.Installed() {
		t.Error("Present/Installed helpers disagree")
	}
}

func TestProbeOutdated(t *testing.T) {
	tests := []struct {
		name   string
		verify manifest.Verify
		output string
		want   Status
	}{
		{
			name:   "below min",
			verify: manifest.Verify{Command: "go", VersionArgs: []string{"version"}, VersionRegex: `go(?P<v>\d+\.\d+)`, MinVersion: "1.22"},
			output: "go1.21.5",
			want:   StatusOutdated,
		},
		{
			name:   "meets min",
			verify: manifest.Verify{Command: "go", VersionArgs: []string{"version"}, VersionRegex: `go(?P<v>\d+\.\d+)`, MinVersion: "1.22"},
			output: "go1.24",
			want:   StatusInstalled,
		},
		{
			name:   "exact mismatch",
			verify: manifest.Verify{Command: "node", VersionArgs: []string{"--version"}, VersionRegex: `v(?P<v>\d+\.\d+\.\d+)`, ExactVersion: "20.11.0"},
			output: "v22.1.0",
			want:   StatusOutdated,
		},
		{
			name:   "exact match",
			verify: manifest.Verify{Command: "node", VersionArgs: []string{"--version"}, VersionRegex: `v(?P<v>\d+\.\d+\.\d+)`, ExactVersion: "20.11.0"},
			output: "v20.11.0",
			want:   StatusInstalled,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exec := newFakeExec()
			exec.set(tc.verify.Command, cmdx.Result{Stdout: tc.output})
			d := testDetector(exec, fakePath{tc.verify.Command: "/bin/" + tc.verify.Command}, "")
			res := d.Probe(context.Background(), compile(t, tool("x", tc.verify)))
			if res.Status != tc.want {
				t.Fatalf("status = %s (%s), want %s", res.Status, res.Detail, tc.want)
			}
			if res.Status == StatusOutdated && res.Detail == "" {
				t.Error("outdated must explain why")
			}
		})
	}
}

func TestProbeUnknownCases(t *testing.T) {
	tests := []struct {
		name    string
		exec    *fakeExec
		verify  manifest.Verify
		wantSub string
	}{
		{
			name:    "regex does not match",
			exec:    withOutput("x", cmdx.Result{Stdout: "hello world"}),
			verify:  manifest.Verify{Command: "x", VersionArgs: []string{"--version"}, VersionRegex: `v(?P<v>\d+\.\d+)`},
			wantSub: "无法用 version_regex 解析",
		},
		{
			name:    "command fails with no output",
			exec:    withErr("x", cmdx.Result{}, errors.New("exit status 1")),
			verify:  manifest.Verify{Command: "x", VersionArgs: []string{"--version"}, VersionRegex: `v(?P<v>\d+\.\d+)`},
			wantSub: "失败",
		},
		{
			name:    "empty output",
			exec:    withOutput("x", cmdx.Result{}),
			verify:  manifest.Verify{Command: "x", VersionArgs: []string{"--version"}, VersionRegex: `v(?P<v>\d+\.\d+)`},
			wantSub: "没有输出",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := testDetector(tc.exec, fakePath{"x": "/bin/x"}, "")
			res := d.Probe(context.Background(), compile(t, tool("x", tc.verify)))
			if res.Status != StatusUnknown {
				t.Fatalf("status = %s, want unknown", res.Status)
			}
			if !strings.Contains(res.Detail, tc.wantSub) {
				t.Errorf("detail = %q, want it to contain %q", res.Detail, tc.wantSub)
			}
			if !res.Present() {
				t.Error("Unknown 表示命令存在，不应被视为 Missing")
			}
		})
	}
}

// TestProbeVersionFromStderr 覆盖"版本打到 stderr 且退出码非 0"的常见情况。
func TestProbeVersionFromStderr(t *testing.T) {
	exec := newFakeExec()
	exec.outputs["x"] = cmdx.Result{Stderr: "x version v1.2.3\n"}
	exec.errs["x"] = errors.New("exit status 1")
	d := testDetector(exec, fakePath{"x": "/bin/x"}, "")
	res := d.Probe(context.Background(), compile(t, tool("x", manifest.Verify{
		Command: "x", VersionArgs: []string{"--version"}, VersionRegex: `x version v(?P<v>\d+\.\d+\.\d+)`,
	})))
	if res.Status != StatusInstalled || res.Version != "1.2.3" {
		t.Fatalf("res = %+v", res)
	}
}

func TestProbeMissing(t *testing.T) {
	d := testDetector(newFakeExec(), fakePath{}, "")
	res := d.Probe(context.Background(), compile(t, tool("rg", manifest.Verify{Command: "rg"})))
	if res.Status != StatusMissing {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Detail, "rg") {
		t.Errorf("detail = %q", res.Detail)
	}
	if res.Present() {
		t.Error("Missing must not be Present")
	}
}

// TestProbeProvidesFallback 覆盖 release/source 类工具：
// 命令不在 PATH，但 provides 声明的路径上确实有它。
func TestProbeProvidesFallback(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(binDir, "g")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	// provides 指向目录时会按 executableName(command) 找文件：Windows 上加 .exe
	// （release 装出来的就是 .exe），因此再建一个同名文件。它不会被真正执行——
	// 命中 provides 后的状态就是 Unknown（版本未知），内容无关紧要。
	dirTarget := target
	if runtime.GOOS == "windows" {
		dirTarget = filepath.Join(binDir, "g.exe")
		if err := os.WriteFile(dirTarget, []byte("placeholder\n"), 0o755); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	d := testDetector(newFakeExec(), fakePath{}, home)

	// provides 指向具体文件
	res := d.Probe(context.Background(), compile(t, tool("g", manifest.Verify{Command: "g"}, "~/.local/bin/g")))
	if res.Status != StatusUnknown || res.Command != target {
		t.Fatalf("res = %+v", res)
	}
	// provides 指向目录
	res = d.Probe(context.Background(), compile(t, tool("g", manifest.Verify{Command: "g"}, "~/.local/bin")))
	if res.Status != StatusUnknown || res.Command != dirTarget {
		t.Fatalf("dir provides: %+v", res)
	}
	ro := filepath.Join(binDir, "ro")
	if err := os.WriteFile(ro, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := d.Probe(context.Background(), compile(t, tool("ro", manifest.Verify{Command: "ro"}, "~/.local/bin/ro")))
	if runtime.GOOS == "windows" {
		// Windows 没有可执行位：普通文件即视为可执行（见 fsx.IsExecutable），
		// 所以 ro 会被当成"装上了但版本未知"，而不是 Missing。
		if got.Status != StatusUnknown {
			t.Errorf("on windows a plain file is executable, want Unknown: %+v", got)
		}
	} else if got.Status != StatusMissing {
		t.Errorf("non-executable file must not count as installed: %+v", got)
	}
}

func TestDetectUsesCacheAndRefresh(t *testing.T) {
	exec := newFakeExec()
	exec.set("go", cmdx.Result{Stdout: "go version go1.24.1"})
	d := testDetector(exec, fakePath{"go": "/bin/go"}, "")
	d.Cache = &Cache{Path: filepath.Join(t.TempDir(), "probe.json")}
	tl := compile(t, tool("go", manifest.Verify{
		Command: "go", VersionArgs: []string{"version"}, VersionRegex: `go version go(?P<v>\d+\.\d+\.\d+)`,
	}))

	first, err := d.Detect(context.Background(), []manifest.Tool{tl}, Options{UseCache: true})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if first["go"].Cached {
		t.Error("first call must actually probe")
	}
	if exec.called("go") != 1 {
		t.Fatalf("expected 1 probe, got %d", exec.called("go"))
	}

	second, err := d.Detect(context.Background(), []manifest.Tool{tl}, Options{UseCache: true})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !second["go"].Cached {
		t.Error("second call should come from cache")
	}
	if exec.called("go") != 1 {
		t.Errorf("cached call must not re-run the command: %d", exec.called("go"))
	}

	// 要据此跳过安装时必须强制重探
	if _, err := d.Detect(context.Background(), []manifest.Tool{tl}, Options{UseCache: true, Refresh: true}); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if exec.called("go") != 2 {
		t.Errorf("Refresh must re-probe: %d", exec.called("go"))
	}
}

func TestDetectWithoutCacheWritesNothing(t *testing.T) {
	exec := newFakeExec()
	exec.set("go", cmdx.Result{Stdout: "go version go1.24.1"})
	root := t.TempDir()
	d := testDetector(exec, fakePath{"go": "/bin/go"}, "")
	d.Cache = &Cache{Path: filepath.Join(root, "probe.json")}
	tl := compile(t, tool("go", manifest.Verify{Command: "go", VersionArgs: []string{"version"}, VersionRegex: `go(?P<v>\d+\.\d+\.\d+)`}))

	if _, err := d.Detect(context.Background(), []manifest.Tool{tl}, Options{UseCache: false}); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "probe.json")); !os.IsNotExist(err) {
		t.Fatalf("UseCache=false must not write a cache file (%v)", err)
	}
}

func TestCacheTTLAndIdempotentWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "probe.json")
	now := time.Now()
	c := &Cache{Path: path, TTL: 10 * time.Minute, Now: func() time.Time { return now }}
	r := Result{ToolID: "go", Status: StatusInstalled, Version: "1.24.1", CheckedAt: now}
	if err := c.Merge(map[string]Result{"go": r}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got, err := c.Load()
	if err != nil || len(got) != 1 {
		t.Fatalf("Load = %v, %v", got, err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := c.Merge(map[string]Result{"go": r}); err != nil {
		t.Fatalf("Merge again: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("identical cache content must not touch the file")
	}

	// TTL 过期后不再命中
	now = now.Add(11 * time.Minute)
	c.Now = func() time.Time { return now }
	if got, _ := c.Load(); len(got) != 0 {
		t.Errorf("expired entries must be dropped: %v", got)
	}

	// 损坏的缓存文件不应导致错误蔓延
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, err := c.Load(); err == nil && got != nil {
		t.Errorf("corrupt cache should yield an error or empty map, got %v", got)
	}
	if err := c.Invalidate(); err != nil {
		t.Errorf("Invalidate: %v", err)
	}
}

func TestDetectOnlyAndConcurrency(t *testing.T) {
	exec := newFakeExec()
	var tools []manifest.Tool
	for _, id := range []string{"a", "b", "c"} {
		exec.set(id, cmdx.Result{Stdout: "1.0.0"})
		tools = append(tools, compile(t, tool(id, manifest.Verify{
			Command: id, VersionArgs: []string{"--version"}, VersionRegex: `(?P<v>\d+\.\d+\.\d+)`,
		})))
	}
	d := testDetector(exec, fakePath{"a": "/a", "b": "/b", "c": "/c"}, "")
	got, err := d.Detect(context.Background(), tools, Options{Only: map[string]bool{"a": true}})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Only filter ignored: %v", got)
	}
	if exec.called("b") != 0 || exec.called("c") != 0 {
		t.Error("filtered tools must not be probed")
	}

	// 并发路径：全部探测且每个只跑一次（以本次调用前后的增量为准）
	before := map[string]int{}
	for _, id := range []string{"a", "b", "c"} {
		before[id] = exec.called(id)
	}
	got, err = d.Detect(context.Background(), tools, Options{})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results", len(got))
	}
	for _, id := range []string{"a", "b", "c"} {
		if got[id].Status != StatusInstalled {
			t.Errorf("%s = %+v", id, got[id])
		}
		if exec.called(id)-before[id] != 1 {
			t.Errorf("%s probed %d times in this call", id, exec.called(id)-before[id])
		}
	}
}

func TestDetectWithoutExecutor(t *testing.T) {
	d := &Detector{}
	if _, err := d.Detect(context.Background(), nil, Options{}); !errors.Is(err, ErrNoExecutor) {
		t.Fatalf("err = %v, want ErrNoExecutor", err)
	}
}

func TestResultLabel(t *testing.T) {
	tests := []struct {
		r    Result
		want string
	}{
		{Result{Status: StatusInstalled, Version: "1.2.3"}, "已安装 1.2.3"},
		{Result{Status: StatusInstalled}, "已安装"},
		{Result{Status: StatusOutdated, Version: "1.0"}, "版本过旧 1.0"},
		{Result{Status: StatusOutdated}, "版本过旧"},
		{Result{Status: StatusMissing}, "未安装"},
		{Result{Status: StatusUnknown, Version: "x"}, "版本未知 x"},
		{Result{Status: StatusUnknown}, "版本未知"},
	}
	for _, tc := range tests {
		if got := tc.r.Label(); got != tc.want {
			t.Errorf("Label(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

func withOutput(name string, res cmdx.Result) *fakeExec {
	f := newFakeExec()
	f.set(name, res)
	return f
}

func withErr(name string, res cmdx.Result, err error) *fakeExec {
	f := newFakeExec()
	f.outputs[name] = res
	f.errs[name] = err
	return f
}

// TestProbeFileProbe：nvm 这类"shell 函数型"工具没有可执行文件，
// 只能靠 verify.file 判断装没装。
func TestProbeFileProbe(t *testing.T) {
	home := t.TempDir()
	tool := manifest.Tool{ID: "nvm", Verify: manifest.Verify{File: "~/.nvm/nvm.sh"}}
	d := &Detector{HomeDir: home, Executor: newFakeExec(), LookPath: fakePath{}}
	// 文件不存在 → missing
	if res := d.Probe(context.Background(), tool); res.Status != StatusMissing {
		t.Fatalf("status = %s, want missing (%s)", res.Status, res.Detail)
	}
	if err := os.MkdirAll(filepath.Join(home, ".nvm"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".nvm", "nvm.sh"), []byte("# nvm\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res := d.Probe(context.Background(), tool)
	if res.Status != StatusInstalled {
		t.Fatalf("status = %s (%s)", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "nvm.sh") {
		t.Errorf("detail = %s", res.Detail)
	}
}

// TestCacheInvalidateTools：只丢指定工具的缓存项，别把整份缓存删了。
//
// 这是"装完立刻可见"的关键：装/升级后若不更新缓存，
// `bs tool list --installed` 会在整个 TTL 周期里继续显示 missing。
func TestCacheInvalidateTools(t *testing.T) {
	now := time.Now()
	c := &Cache{Path: filepath.Join(t.TempDir(), "probe.json"), TTL: time.Hour,
		Now: func() time.Time { return now }}
	seed := map[string]Result{
		"go": {ToolID: "go", Status: StatusInstalled, Version: "1.24.1", CheckedAt: now},
		"rg": {ToolID: "rg", Status: StatusInstalled, Version: "14.1.1", CheckedAt: now},
		"jq": {ToolID: "jq", Status: StatusMissing, CheckedAt: now},
	}
	if err := c.Merge(seed); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if err := c.InvalidateTools("jq"); err != nil {
		t.Fatalf("InvalidateTools: %v", err)
	}
	got, err := c.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := got["jq"]; ok {
		t.Error("jq 应当被丢掉")
	}
	if len(got) != 2 || got["go"].Version != "1.24.1" || got["rg"].Version != "14.1.1" {
		t.Errorf("其他条目不该被动：%v", got)
	}
	// 不在缓存里的 id 不该改动文件（保持"内容不变就不写盘"的既有性质）。
	before, err := os.Stat(c.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := c.InvalidateTools("nope"); err != nil {
		t.Fatalf("InvalidateTools(unknown): %v", err)
	}
	after, err := os.Stat(c.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("没有命中任何 id 时不该写文件")
	}
	// 空 id / nil 缓存都不该 panic。
	var nilCache *Cache
	if err := nilCache.InvalidateTools("go"); err != nil {
		t.Errorf("nil cache: %v", err)
	}
	if err := c.InvalidateTools(); err != nil {
		t.Errorf("no ids: %v", err)
	}
}
