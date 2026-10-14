package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/kingfs/Bootstrap/internal/detect"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
	"github.com/kingfs/Bootstrap/pkg/ghrelease"
	"github.com/kingfs/Bootstrap/pkg/httpx"
)

// TestReleaseInstallEndToEndOffline 是本仓库最"真"的一条无网络端到端测试：
// 用 httptest 冒充 GitHub（API + 资产地址），把 release 路径整条跑通——
//
//	解析最新 tag → 用 {{os}}/{{arch}}/{{version}} 渲染 asset 名 → 拉 checksums.txt
//	→ 下载 tar.gz → 校验 sha256 → 解压 → 放到 dest → 复核（真的执行 --version）
//
// 换掉的只有网络端点：httpx、archive、文件系统、探测全是真代码。
func TestReleaseInstallEndToEndOffline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假二进制是 POSIX shell 脚本")
	}

	// 1. 造一个真实的 tar.gz：里面是 mytool-1.2.3/mytool（可执行脚本）。
	const (
		repo  = "acme/mytool"
		tag   = "v1.2.3"
		ver   = "1.2.3"
		asset = "mytool_1.2.3_linux_amd64.tar.gz"
	)
	payload := buildTarGz(t, map[string]tarEntry{
		"mytool-1.2.3/mytool": {Body: "#!/bin/sh\necho \"mytool " + ver + "\"\n", Mode: 0o755},
		"mytool-1.2.3/README": {Body: "docs\n", Mode: 0o644},
	})
	sum := sha256.Sum256(payload)

	// 2. 冒充 GitHub。
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + repo + "/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
		case "/" + repo + "/releases/download/" + tag + "/checksums.txt":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
		case "/" + repo + "/releases/download/" + tag + "/" + asset:
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	oldAPI, oldAsset := ghrelease.APIBaseURL, ghrelease.AssetBaseURL
	ghrelease.APIBaseURL, ghrelease.AssetBaseURL = api.URL, api.URL
	defer func() { ghrelease.APIBaseURL, ghrelease.AssetBaseURL = oldAPI, oldAsset }()

	// 3. 真实的 ExecEnv（真 httpx 客户端 + 真 runner），但家目录/工作目录都在 temp 里。
	home := t.TempDir()
	dest := filepath.Join(home, "bin")
	env := NewOSEnv(OSEnvOptions{
		Env:       map[string]string{"PATH": os.Getenv("PATH"), "HOME": home},
		ExtraPath: []string{dest},
		HomeDir:   home,
		GOOS:      "linux",
		Arch:      "amd64",
		Client:    httpx.New(30e9, nil),
		WorkRoot:  t.TempDir(),
	})
	runner := cmdx.New(nil)
	runner.DryRun = false

	loaded := loadManifest(t, `
version: 1
tools:
  - id: mytool
    stack: base
    kind: cli
    verify:
      command: mytool
      version_args: [--version]
      version_regex: 'mytool (?P<v>\d+\.\d+\.\d+)'
      min_version: "1.0"
    install:
      - via: release
        params:
          repo: acme/mytool
          asset: "mytool_{{version}}_{{os}}_{{arch}}.tar.gz"
          strip_components: 1
          binary: mytool
          dest: `+dest+`
          checksum_file: checksums.txt
`)
	// 与 app.detector() 的装配一致：查找规则用安装期那一套（含 extra_path），
	// 否则"刚装进 ~/.local/bin 的工具"会因为还没进 shell PATH 而被判成没装上。
	det := &detect.Detector{
		Executor: runner, LookPath: env, HomeDir: home,
		Timeout: detect.ProbeTimeout, Concurrency: 2,
	}
	eng, err := NewEngine(EngineOptions{
		Env: env, Loaded: loaded, Detector: det, LogDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// 4. 计划：应当是 release，且资产名已经渲染成真实版本。
	plan, err := eng.PlanTools(context.Background(), []string{"mytool"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Status != StepInstall {
		t.Fatalf("plan = %+v", plan.Steps)
	}
	if got := plan.Steps[0].Summary; !strings.Contains(got, tag) {
		t.Errorf("summary = %q, want it to contain %s", got, tag)
	}

	// 5. 执行：下载 + 校验 + 解压 + 复核，全程真实。
	report, err := eng.Apply(context.Background(), plan, OpInstall)
	if err != nil {
		t.Fatalf("Apply: %v (steps=%+v)", err, report.Steps)
	}
	if report.Steps[0].Status != StepDone {
		t.Fatalf("step = %+v", report.Steps[0])
	}
	bin := filepath.Join(dest, "mytool")
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	// strip_components 生效：README 不该被摊到 dest 根目录
	if _, err := os.Stat(filepath.Join(dest, "README")); err == nil {
		t.Error("strip_components 没生效：README 被解到了 dest")
	}
	// 探测复核绑定了真实版本（引擎已经跑过），再手动执行一次确认二进制可用。
	res, err := runner.Run(context.Background(), cmdx.Command{Name: bin, Args: []string{"--version"}})
	if err != nil || !strings.Contains(res.Stdout, ver) {
		t.Fatalf("run installed binary: %v / %q", err, res.Stdout)
	}
	// 纯下载型安装没有命令可跑，因此不该留下空日志文件（惰性创建）。
	if report.Steps[0].LogPath != "" {
		t.Errorf("release 安装没有命令，不该产生日志文件：%q", report.Steps[0].LogPath)
	}

	// 6. 幂等：再规划一次应当是 skip（真的探测到了刚装的二进制）。
	plan2, err := eng.PlanTools(context.Background(), []string{"mytool"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools#2: %v", err)
	}
	if plan2.Steps[0].Status != StepSkip {
		t.Errorf("second plan = %+v det=%+v, want skip", plan2.Steps[0].Status, plan2.Steps[0].Detection)
	}
}

// TestReleaseInstallRejectsBadChecksum：校验和对不上时必须中止，
// 并且不能把半个文件留在 dest（安全红线 §8.3）。
func TestReleaseInstallRejectsBadChecksum(t *testing.T) {
	const (
		repo  = "acme/bad"
		tag   = "v1.0.0"
		asset = "bad_1.0.0_linux_amd64.tar.gz"
	)
	payload := buildTarGz(t, map[string]tarEntry{"bad/bad": {Body: "#!/bin/sh\necho bad\n", Mode: 0o755}})

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + repo + "/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
		case "/" + repo + "/releases/download/" + tag + "/checksums.txt":
			// 故意给一个错误的 sha256
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), asset)
		case "/" + repo + "/releases/download/" + tag + "/" + asset:
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	oldAPI, oldAsset := ghrelease.APIBaseURL, ghrelease.AssetBaseURL
	ghrelease.APIBaseURL, ghrelease.AssetBaseURL = api.URL, api.URL
	defer func() { ghrelease.APIBaseURL, ghrelease.AssetBaseURL = oldAPI, oldAsset }()

	home := t.TempDir()
	dest := filepath.Join(home, "bin")
	env := NewOSEnv(OSEnvOptions{
		Env: map[string]string{"HOME": home}, ExtraPath: []string{dest}, HomeDir: home,
		GOOS: "linux", Arch: "amd64", Client: httpx.New(30e9, nil), WorkRoot: t.TempDir(),
	})
	loaded := loadManifest(t, `
version: 1
tools:
  - id: bad
    stack: base
    kind: cli
    verify: {command: bad}
    install:
      - via: release
        params:
          repo: acme/bad
          asset: "bad_{{version}}_{{os}}_{{arch}}.tar.gz"
          strip_components: 1
          binary: bad
          dest: `+dest+`
          checksum_file: checksums.txt
`)
	eng, err := NewEngine(EngineOptions{Env: env, Loaded: loaded})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	plan, err := eng.PlanTools(context.Background(), []string{"bad"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	report, err := eng.Apply(context.Background(), plan, OpInstall)
	if err == nil {
		t.Fatalf("checksum mismatch must fail, steps=%+v", report.Steps)
	}
	if !strings.Contains(report.Steps[0].Error, "sha256") && !strings.Contains(report.Steps[0].Error, "checksum") {
		t.Errorf("error = %q", report.Steps[0].Error)
	}
	if _, serr := os.Stat(filepath.Join(dest, "bad")); serr == nil {
		t.Error("校验失败的产物不能留在 dest")
	}
	// 也不该留下临时文件
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bs-download-") {
			t.Errorf("残留临时文件 %s", e.Name())
		}
	}
}

type tarEntry struct {
	Body string
	Mode int64
}

// buildTarGz 在内存里造一个 tar.gz（测试用）。
func buildTarGz(t *testing.T, files map[string]tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// 固定的写入顺序，便于排查
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, name := range names {
		f := files[name]
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: f.Mode, Size: int64(len(f.Body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(f.Body)); err != nil {
			t.Fatalf("tar write: %v", err)
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

// TestReleaseInstallWithoutStripComponents：归档根目录直接放二进制（lazygit/k9s/g 就是这样）
// 时必须能装成功。曾经这三个清单都误写了 strip_components: 1，把文件本身剥没了——
// 而这一点直到真的下载安装才会暴露（离线假归档太"规整"，反而测不出来）。
func TestReleaseInstallWithoutStripComponents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假二进制是 POSIX shell 脚本")
	}
	const (
		repo  = "acme/flat"
		tag   = "v0.1.0"
		asset = "flat_0.1.0_linux_amd64.tar.gz"
	)
	// 关键：没有外层目录，条目就是根下的 flat 本身。
	payload := buildTarGz(t, map[string]tarEntry{
		"flat":   {Body: "#!/bin/sh\necho flat 0.1.0\n", Mode: 0o755},
		"README": {Body: "x\n", Mode: 0o644},
	})
	sum := sha256.Sum256(payload)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + repo + "/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
		case "/" + repo + "/releases/download/" + tag + "/checksums.txt":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
		default:
			_, _ = w.Write(payload)
		}
	}))
	defer api.Close()
	oldAPI, oldAsset := ghrelease.APIBaseURL, ghrelease.AssetBaseURL
	ghrelease.APIBaseURL, ghrelease.AssetBaseURL = api.URL, api.URL
	defer func() { ghrelease.APIBaseURL, ghrelease.AssetBaseURL = oldAPI, oldAsset }()

	home := t.TempDir()
	dest := filepath.Join(home, "bin")
	env := NewOSEnv(OSEnvOptions{
		Env: map[string]string{"HOME": home}, ExtraPath: []string{dest}, HomeDir: home,
		GOOS: "linux", Arch: "amd64", Client: httpx.New(30e9, nil), WorkRoot: t.TempDir(),
	})
	loaded := loadManifest(t, `
version: 1
tools:
  - id: flat
    stack: base
    kind: cli
    verify: {command: flat, version_args: [--version], version_regex: 'flat (?P<v>\d+\.\d+\.\d+)'}
    install:
      - via: release
        params:
          repo: acme/flat
          asset: "flat_{{version}}_{{os}}_{{arch}}.tar.gz"
          binary: flat
          dest: `+dest+`
`)
	eng, err := NewEngine(EngineOptions{Env: env, Loaded: loaded})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	plan, err := eng.PlanTools(context.Background(), []string{"flat"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	report, err := eng.Apply(context.Background(), plan, OpInstall)
	if err != nil {
		t.Fatalf("Apply: %v (steps=%+v)", err, report.Steps)
	}
	if _, err := os.Stat(filepath.Join(dest, "flat")); err != nil {
		t.Fatalf("flat 没装上：%v", err)
	}
}

// TestReleaseInstallStripTooManyIsLoud：strip_components 写大了必须当场报错，
// 而不是"解出 0 个文件"然后当作装好了。
func TestReleaseInstallStripTooManyIsLoud(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假二进制是 POSIX shell 脚本")
	}
	const (
		repo  = "acme/over"
		tag   = "v0.2.0"
		asset = "over_0.2.0_linux_amd64.tar.gz"
	)
	payload := buildTarGz(t, map[string]tarEntry{"over": {Body: "#!/bin/sh\n", Mode: 0o755}})
	sum := sha256.Sum256(payload)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/latest") {
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
			return
		}
		_, _ = w.Write(payload)
	}))
	defer api.Close()
	oldAPI, oldAsset := ghrelease.APIBaseURL, ghrelease.AssetBaseURL
	ghrelease.APIBaseURL, ghrelease.AssetBaseURL = api.URL, api.URL
	defer func() { ghrelease.APIBaseURL, ghrelease.AssetBaseURL = oldAPI, oldAsset }()

	home := t.TempDir()
	env := NewOSEnv(OSEnvOptions{
		Env: map[string]string{"HOME": home}, HomeDir: home,
		GOOS: "linux", Arch: "amd64", Client: httpx.New(30e9, nil), WorkRoot: t.TempDir(),
	})
	loaded := loadManifest(t, `
version: 1
tools:
  - id: over
    stack: base
    kind: cli
    verify: {command: over}
    install:
      - via: release
        params:
          repo: acme/over
          asset: "over_{{version}}_{{os}}_{{arch}}.tar.gz"
          strip_components: 1
          binary: over
          dest: `+filepath.Join(home, "bin")+`
`)
	eng, err := NewEngine(EngineOptions{Env: env, Loaded: loaded})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	plan, err := eng.PlanTools(context.Background(), []string{"over"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	report, _ := eng.Apply(context.Background(), plan, OpInstall)
	if report.Steps[0].Status == StepDone {
		t.Fatalf("strip 过大不该算成功：%+v", report.Steps[0])
	}
	if !strings.Contains(report.Steps[0].Error, "strip") {
		t.Errorf("error = %q, want it to mention stripping", report.Steps[0].Error)
	}
}

// TestReleaseInstallResumesInterruptedDownload 是本仓库对"断点续传"最真的一条断言：
// 第一次下载被服务端掐断（真实 TCP 断开），半成品留在下载缓存里；第二次重跑必须
// 带 Range 从断点续上，最终校验和通过、解压出可用的二进制。
//
// 之所以放在 artifacts_e2e_test.go：它和 TestReleaseInstallEndToEndOffline 走同一条
// release 路径（真 httpx + 真解压 + 真校验），能顺带证明"续传不会破坏 sha256 校验"。
func TestReleaseInstallResumesInterruptedDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假二进制是 POSIX shell 脚本")
	}
	const (
		repo  = "acme/resumetool"
		tag   = "v2.0.0"
		ver   = "2.0.0"
		asset = "resumetool_2.0.0_linux_amd64.tar.gz"
	)
	// 造一个足够大的归档，保证"掐断点"在中间而不是刚好在末尾。
	big := strings.Repeat("#!/bin/sh\necho padding\n", 4000)
	payload := buildTarGz(t, map[string]tarEntry{
		"resumetool-2.0.0/resumetool": {Body: "#!/bin/sh\necho \"resumetool " + ver + "\"\n", Mode: 0o755},
		"resumetool-2.0.0/pad.txt":    {Body: big, Mode: 0o644},
	})
	sum := sha256.Sum256(payload)

	var (
		mu        sync.Mutex
		rangeHdrs []string
		attempts  int
	)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/"+repo+"/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
		case r.URL.Path == "/"+repo+"/releases/download/"+tag+"/checksums.txt":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
		case r.URL.Path == "/"+repo+"/releases/download/"+tag+"/"+asset:
			mu.Lock()
			attempts++
			rangeHdrs = append(rangeHdrs, r.Header.Get("Range"))
			n := attempts
			mu.Unlock()
			if rng := r.Header.Get("Range"); rng != "" {
				var start int64
				if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err == nil && start < int64(len(payload)) {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write(payload[start:])
					return
				}
			}
			if n == 1 {
				// 第一次：只写一半，然后把连接掐掉（不设 Content-Length，避免客户端等齐）。
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(payload[:len(payload)/2])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						_ = conn.Close()
					}
				}
				return
			}
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	oldAPI, oldAsset := ghrelease.APIBaseURL, ghrelease.AssetBaseURL
	ghrelease.APIBaseURL, ghrelease.AssetBaseURL = api.URL, api.URL
	defer func() { ghrelease.APIBaseURL, ghrelease.AssetBaseURL = oldAPI, oldAsset }()

	home := t.TempDir()
	cache := t.TempDir()
	dest := filepath.Join(home, "bin")
	loaded := loadManifest(t, `
version: 1
tools:
  - id: resumetool
    stack: base
    kind: cli
    verify:
      command: resumetool
      version_args: [--version]
      version_regex: 'resumetool (?P<v>\d+\.\d+\.\d+)'
    install:
      - via: release
        params:
          repo: `+repo+`
          asset: "resumetool_{{version}}_{{os}}_{{arch}}.tar.gz"
          strip_components: 1
          binary: resumetool
          dest: `+dest+`
          checksum_file: checksums.txt
`)
	runner := cmdx.New(nil)
	runner.DryRun = false

	// 每次"运行一次 bs"都构造一套新的 ExecEnv（同一 cache）——模拟两次命令调用。
	newEngine := func() *Engine {
		env := NewOSEnv(OSEnvOptions{
			Env:       map[string]string{"PATH": os.Getenv("PATH"), "HOME": home},
			ExtraPath: []string{dest},
			HomeDir:   home, GOOS: "linux", Arch: "amd64",
			Client:   httpx.New(30e9, nil),
			WorkRoot: cache, CacheDir: cache,
		})
		det := &detect.Detector{Executor: runner, LookPath: env, HomeDir: home,
			Timeout: detect.ProbeTimeout, Concurrency: 2}
		eng, err := NewEngine(EngineOptions{Env: env, Loaded: loaded, Detector: det, LogDir: t.TempDir()})
		if err != nil {
			t.Fatalf("NewEngine: %v", err)
		}
		return eng
	}

	// 第一次：被掐断 → 失败，但半成品要留在下载缓存里。
	eng := newEngine()
	plan, err := eng.PlanTools(context.Background(), []string{"resumetool"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools: %v", err)
	}
	if _, err := eng.Apply(context.Background(), plan, OpInstall); err == nil {
		t.Fatal("被掐断的下载应当让这次安装失败")
	}
	if _, err := os.Stat(filepath.Join(dest, "resumetool")); err == nil {
		t.Fatal("没下完就出现了二进制")
	}
	parts, _ := filepath.Glob(filepath.Join(cache, "downloads", ".*.part"))
	if len(parts) != 1 {
		t.Fatalf("应当留下一个半成品，实际 %v", parts)
	}
	if info, err := os.Stat(parts[0]); err != nil || info.Size() == 0 {
		t.Fatalf("半成品应当是部分内容：%v", err)
	}

	// 第二次：带 Range 续上 → 校验和通过 → 解压出可用的二进制。
	eng = newEngine()
	plan2, err := eng.PlanTools(context.Background(), []string{"resumetool"}, OpInstall)
	if err != nil {
		t.Fatalf("PlanTools#2: %v", err)
	}
	report, err := eng.Apply(context.Background(), plan2, OpInstall)
	if err != nil {
		t.Fatalf("续传安装失败：%v (steps=%+v)", err, report.Steps)
	}
	mu.Lock()
	hdrs := append([]string(nil), rangeHdrs...)
	mu.Unlock()
	if len(hdrs) < 2 || hdrs[len(hdrs)-1] == "" {
		t.Errorf("第二次下载没有带 Range：%v", hdrs)
	}
	bin := filepath.Join(dest, "resumetool")
	res, err := runner.Run(context.Background(), cmdx.Command{Name: bin, Args: []string{"--version"}})
	if err != nil || !strings.Contains(res.Stdout, ver) {
		t.Fatalf("续传装出来的二进制不可用：%v / %q", err, res.Stdout)
	}
	if parts, _ := filepath.Glob(filepath.Join(cache, "downloads", ".*.part")); len(parts) != 0 {
		t.Errorf("成功后不该留下半成品：%v", parts)
	}
}
