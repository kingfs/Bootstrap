package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kingfs/Bootstrap/internal/detect"
	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
)

// ── 假执行环境：所有 installer / engine 测试都跑在它上面，绝不碰真实系统 ──

type fakeEnv struct {
	mu          sync.Mutex
	os          string
	arch        string
	distro      string
	home        string
	work        string
	downloadDir string
	dry         bool
	root        bool
	env         map[string]string
	bins        map[string]string // 命令名 → 路径（等价于 PATH 里有它）
	files       map[string]string // URL → 内容（FetchText/Download 用）
	tags        map[string]string // repo → latest tag
	commands    []Command
	downloads   []Download
	runErr      map[string]error // 命令名 → 故意失败
	runOut      map[string]string
	versions    map[string]string
	log         *slog.Logger
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	work := t.TempDir()
	return &fakeEnv{
		os: "linux", arch: "amd64", distro: "ubuntu",
		home: filepath.Join(work, "home"), work: work,
		env:  map[string]string{"PATH": "/usr/bin:/bin", "HOME": filepath.Join(work, "home")},
		bins: map[string]string{}, files: map[string]string{}, tags: map[string]string{},
		runErr: map[string]error{}, runOut: map[string]string{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (f *fakeEnv) has(name string) *fakeEnv {
	f.bins[name] = "/usr/bin/" + name
	return f
}

func (f *fakeEnv) Run(_ context.Context, c Command) (Result, error) {
	// 记录时剥掉 sudo 前缀，"实际跑了什么"才是测试关心的
	// （真实 ExecEnv 会把 sudo 交给系统去提权）。
	rec := c
	if c.Name == "sudo" && len(c.Args) > 0 {
		rec = Command{Name: c.Args[0], Args: c.Args[1:], Dir: c.Dir, Env: c.Env}
	}
	f.mu.Lock()
	f.commands = append(f.commands, rec)
	f.mu.Unlock()
	if err, ok := f.runErr[c.Name]; ok {
		return Result{}, err
	}
	if out, ok := f.runOut[c.Name]; ok {
		return Result{Stdout: out}, nil
	}
	return Result{}, nil
}

func (f *fakeEnv) Download(_ context.Context, d Download) (string, error) {
	f.mu.Lock()
	f.downloads = append(f.downloads, d)
	body, ok := f.files[d.URL]
	f.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("fake download: no content for %s", d.URL)
	}
	dest := d.Dest
	if dest == "" {
		dest = filepath.Join(f.work, filepath.Base(d.URL))
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	mode := d.Mode
	if mode == 0 {
		mode = 0o644
	}
	if err := os.WriteFile(dest, []byte(body), mode); err != nil {
		return "", err
	}
	return dest, nil
}

func (f *fakeEnv) FetchText(_ context.Context, url string) (string, error) {
	if body, ok := f.files[url]; ok {
		return body, nil
	}
	return "", fmt.Errorf("fake fetch: 404 %s", url)
}

func (f *fakeEnv) LatestRelease(_ context.Context, repo string) (string, error) {
	if tag, ok := f.tags[repo]; ok {
		return tag, nil
	}
	return "", fmt.Errorf("fake release: no tag for %s", repo)
}

func (f *fakeEnv) LookPath(name string) (string, bool) {
	p, ok := f.bins[name]
	return p, ok
}

func (f *fakeEnv) Env() map[string]string { return f.env }
func (f *fakeEnv) DryRun() bool           { return f.dry }
func (f *fakeEnv) IsRoot() bool           { return f.root }
func (f *fakeEnv) OS() string             { return f.os }
func (f *fakeEnv) Arch() string           { return f.arch }
func (f *fakeEnv) Distro() string         { return f.distro }
func (f *fakeEnv) HomeDir() string        { return f.home }
func (f *fakeEnv) Versions() map[string]string {
	if f.versions == nil {
		f.versions = map[string]string{}
	}
	return f.versions
}
func (f *fakeEnv) WorkDir() string { return f.work }
func (f *fakeEnv) DownloadDir() string {
	if f.downloadDir == "" {
		f.downloadDir = filepath.Join(f.work, "downloads")
	}
	return f.downloadDir
}
func (f *fakeEnv) Log() *slog.Logger { return f.log }

func (f *fakeEnv) ran(name string) []Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Command
	for _, c := range f.commands {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeEnv) commandLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.commands {
		out = append(out, c.String())
	}
	return out
}

func (f *fakeEnv) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = nil
	f.downloads = nil
}

// ── 清单与探测器装配 ──

func loadManifest(t *testing.T, body string) *manifest.Loaded {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tools.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	loaded, err := manifest.Load(manifest.LoadOptions{Builtin: false, UserDirs: []string{dir}})
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return loaded
}

// fakeDetector 让探测结果完全可控（不执行任何命令）。
type fakeDetector struct {
	status  map[string]detect.Status
	version map[string]string
	probes  int
}

// LookPath 实现 detect.PathLookup：只有非 missing 的命令才"在 PATH 里"。
func (f *fakeDetector) LookPath(name string) (string, bool) {
	if st, ok := f.status[name]; ok && st != detect.StatusMissing {
		return "/usr/bin/" + name, true
	}
	return "", false
}

func (f *fakeDetector) executor() detect.Executor { return f }
func (f *fakeDetector) Run(_ context.Context, c cmdx.Command) (cmdx.Result, error) {
	f.probes++
	// 探测用 LookPath 解析出的完整路径执行（工具可能还没进 PATH），
	// 因此按 basename 匹配登记名。
	name := c.Name
	if _, ok := f.status[filepath.Base(name)]; ok {
		name = filepath.Base(name)
	}
	st, ok := f.status[name]
	if !ok || st == detect.StatusMissing {
		return cmdx.Result{}, errors.New("not found")
	}
	v := f.version[name]
	if v == "" {
		v = "1.0.0"
	}
	return cmdx.Result{Stdout: v}, nil
}

// newFakeDetector 构造一个探测结果完全可控的探测器。
func newFakeDetector(status map[string]detect.Status) *fakeDetector {
	return &fakeDetector{status: status, version: map[string]string{}}
}

// newDetector 把假探测器接进真实 Detector。
// fakeDetector 同时充当 Executor 和 PathLookup，并且是**动态**的：
// 测试可以在 plan 之后改 status 来模拟"装上了"，这正是验证阶段要观察的。
func newDetector(t *testing.T, f *fakeDetector) *detect.Detector {
	t.Helper()
	return &detect.Detector{
		Executor: f, LookPath: f,
		HomeDir: t.TempDir(), Timeout: 1e9, Concurrency: 2,
	}
}

// tarGz 构造 tar.gz 字节，用于 release 安装器的真实解压测试。
func tarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func mustContain(t *testing.T, got string, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
}
