package install

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kingfs/Bootstrap/internal/manifest"
	"github.com/kingfs/Bootstrap/pkg/httpx"
)

func TestRender(t *testing.T) {
	data := RenderData{
		Version: "1.24.1", Tag: "v1.24.1", OS: "darwin", Arch: "arm64",
		Package: "@scope/pkg", Manager: "g", Crate: "ripgrep", Module: "example.com/m",
		Python: "3.12", Versions: map[string]string{"golang": "1.24.1"},
	}
	tests := []struct {
		in   string
		want string
	}{
		{"g{{version}}-{{os}}.tar.gz", "g1.24.1-darwin.tar.gz"},
		{"{{tag}}", "v1.24.1"},
		{"v{{version|trimv}}", "v1.24.1"},
		{"go{{versions.golang}}.linux-{{arch}}", "go1.24.1.linux-arm64"},
		{"{{arch|archmap:amd64=x86_64,arm64=aarch64}}", "aarch64"},
		{"{{arch|archmap:amd64=x86_64}}", "arm64"},
		{"{{os|osmap:darwin=macos,linux=linux}}", "macos"},
		{"{{version|default:latest}}", "1.24.1"},
		{"{{package}}", "@scope/pkg"},
		{"no template here", "no template here"},
	}
	for _, tc := range tests {
		got, err := Render(tc.in, data)
		if err != nil {
			t.Errorf("Render(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Render(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	empty := RenderData{Versions: map[string]string{}}
	if got, err := Render("{{version|default:latest}}", empty); err != nil || got != "latest" {
		t.Errorf("default filter on empty value = %q, %v", got, err)
	}

	// 写错变量名必须报错，而不是渲染成空串
	if _, err := Render("{{verison}}", data); err == nil {
		t.Error("unknown variable must fail")
	}
	if _, err := Render("{{versions.nodejs}}", data); err == nil {
		t.Error("missing versions key must fail")
	}
	if _, err := Render("{{version|nope}}", data); err == nil {
		t.Error("unknown filter must fail")
	}
	if _, err := Render("{{arch|archmap}}", data); err == nil {
		t.Error("archmap without mapping must fail")
	}
	if !ContainsTemplate("a{{version}}") || ContainsTemplate("plain") {
		t.Error("ContainsTemplate is wrong")
	}
}

func TestSplitArgsAndQuote(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"g install 1.24.1", []string{"g", "install", "1.24.1"}},
		{"npm install -g 'a b'", []string{"npm", "install", "-g", "a b"}},
		{`sh -c "echo hi"`, []string{"sh", "-c", "echo hi"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
		{`a\ b`, []string{"a b"}},
		{`""`, []string{""}},
	}
	for _, tc := range tests {
		got, err := splitArgs(tc.in)
		if err != nil {
			t.Errorf("splitArgs(%q): %v", tc.in, err)
			continue
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("splitArgs(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if _, err := splitArgs(`unbalanced "quote`); err == nil {
		t.Error("unbalanced quote must fail")
	}
	if _, err := splitArgs(`trailing \`); err == nil {
		t.Error("trailing escape must fail")
	}

	if got := shellQuote("/home/a b/nvm.sh"); got != `'/home/a b/nvm.sh'` {
		t.Errorf("shellQuote = %q", got)
	}
	if got := shellQuote("/plain/path.sh"); got != "/plain/path.sh" {
		t.Errorf("shellQuote = %q", got)
	}
	if got := shellQuote(""); got != "''" {
		t.Errorf("shellQuote(empty) = %q", got)
	}
}

const golangManifest = `
version: 1
tools:
  - id: g
    stack: golang
    kind: versionmgr
    verify: {command: g}
    install:
      - via: release
        params:
          repo: voidint/g
          asset: g{{version}}-{{os}}-{{arch}}.tar.gz
          dest: ~/.local/bin
  - id: golang
    stack: golang
    kind: runtime
    verify:
      command: go
      version_args: [version]
      version_regex: 'go(?P<v>\d+\.\d+(\.\d+)?)'
    depends_on: [g]
    install:
      - via: manager
        params: {manager: g, action: install, version: "{{versions.golang}}"}
      - via: apt
        params: {packages: [golang-go]}
      - via: source
        params: {repo: golang/go, build: "cd src && ./make.bash", install: "true"}
`

func TestResolveChainFollowsManifestOrder(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["g"] = "/usr/bin/g"
	loaded := loadManifest(t, golangManifest)
	tool, _ := loaded.ByID("golang")
	reg := NewRegistry()

	chain, err := ResolveChain(context.Background(), reg, env, tool, ChainOptions{})
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	cand, ok := chain.SelectedCandidate()
	if !ok || cand.Via != "manager" {
		t.Fatalf("selected = %+v", cand)
	}
	// apt 在本机不存在的场景（fakeEnv 里没有 apt-get）
	if len(chain.Candidates) != 3 {
		t.Fatalf("candidates = %d", len(chain.Candidates))
	}
	if chain.Candidates[1].Available {
		t.Error("apt-get is not in PATH, so apt must be unavailable")
	}
	mustContain(t, chain.Candidates[1].Note, "apt")
}

func TestResolveChainSkipsDisabledAndWhen(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["npm"] = "/usr/bin/npm"
	env.bins["bun"] = "/usr/bin/bun"
	env.bins["brew"] = "/usr/bin/brew"
	env.os = "darwin"
	loaded := loadManifest(t, `
version: 1
tools:
  - id: t
    stack: base
    kind: cli
    verify: {command: t}
    install:
      - via: brew
        when: {os: [linux]}
        params: {formula: t}
      - via: bun
        enabled: false
        params: {package: t}
      - via: npm
        params: {package: t}
`)
	tool, _ := loaded.ByID("t")
	chain, err := ResolveChain(context.Background(), NewRegistry(), env, tool, ChainOptions{})
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	cand, _ := chain.SelectedCandidate()
	if cand.Via != "npm" {
		t.Fatalf("selected = %+v", cand)
	}
	if chain.Candidates[0].Skip == "" || !strings.Contains(chain.Candidates[0].Skip, "when") {
		t.Errorf("when mismatch must be recorded: %+v", chain.Candidates[0])
	}
	if chain.Candidates[1].Skip != "enabled: false" {
		t.Errorf("disabled must be recorded: %+v", chain.Candidates[1])
	}
	// 被 enabled/when 排除的不能出现在降级候选里
	if len(chain.Fallbacks()) != 0 {
		t.Errorf("fallbacks = %+v", chain.Fallbacks())
	}
	if len(chain.Skipped()) != 2 {
		t.Errorf("skipped = %+v", chain.Skipped())
	}
}

func TestResolveChainExplicitViaAndErrors(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["npm"] = "/usr/bin/npm"
	loaded := loadManifest(t, `
version: 1
tools:
  - id: t
    stack: base
    kind: cli
    verify: {command: t}
    install:
      - via: bun
        params: {package: t}
      - via: npm
        params: {package: t}
`)
	tool, _ := loaded.ByID("t")
	reg := NewRegistry()

	// 显式指定一个可用但排在后面的方法
	chain, err := ResolveChain(context.Background(), reg, env, tool, ChainOptions{Via: "npm"})
	if err != nil {
		t.Fatalf("explicit via: %v", err)
	}
	if cand, _ := chain.SelectedCandidate(); cand.Via != "npm" || !chain.Explicit {
		t.Fatalf("cand = %+v explicit=%v", cand, chain.Explicit)
	}

	// 显式指定一个不可用的方法 → 报错
	if _, err := ResolveChain(context.Background(), reg, env, tool, ChainOptions{Via: "bun"}); err == nil {
		t.Error("unavailable explicit via must fail")
	}
	// 显式指定不存在的方法 → 列出可用项
	_, err = ResolveChain(context.Background(), reg, env, tool, ChainOptions{Via: "cargo"})
	var notFound *ErrMethodNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want ErrMethodNotFound", err)
	}
	if len(notFound.Known) != 2 {
		t.Errorf("known = %v", notFound.Known)
	}

	// 全不可用 → ErrNoMethod 且每个方法都有原因
	env2 := newFakeEnv(t)
	_, err = ResolveChain(context.Background(), reg, env2, tool, ChainOptions{})
	var noMethod *ErrNoMethod
	if !errors.As(err, &noMethod) {
		t.Fatalf("err = %v, want ErrNoMethod", err)
	}
	if len(noMethod.Reasons) != 2 {
		t.Errorf("reasons = %v", noMethod.Reasons)
	}
}

func TestFillManagerDefaults(t *testing.T) {
	loaded := loadManifest(t, `
version: 1
tools:
  - id: python
    stack: python
    kind: runtime
    verify: {command: python3}
    install:
      - via: manager
        params: {action: install, version: "3.12"}
      - via: apt
        params: {packages: [python3]}
`)
	tool, _ := loaded.ByID("python")
	filled := FillManagerDefaults(tool, map[string]string{"python": "uv"})
	if filled.Install[0].Params.Manager != "uv" {
		t.Errorf("manager not filled: %+v", filled.Install[0].Params)
	}
	// 原工具不被修改（值语义）
	if tool.Install[0].Params.Manager != "" {
		t.Error("FillManagerDefaults must not mutate its input")
	}
	// 已经写明的 manager 不被覆盖
	loaded2 := loadManifest(t, `
version: 1
tools:
  - id: nodejs
    stack: nodejs
    kind: runtime
    verify: {command: node}
    install:
      - via: manager
        params: {manager: nvm, action: install, version: "22"}
`)
	tool2, _ := loaded2.ByID("nodejs")
	filled2 := FillManagerDefaults(tool2, map[string]string{"nodejs": "fnm"})
	if filled2.Install[0].Params.Manager != "nvm" {
		t.Errorf("explicit manager must win: %+v", filled2.Install[0].Params)
	}
}

func TestManagerInstallerPlan(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["g"] = "/usr/bin/g"
	loaded := loadManifest(t, golangManifest)
	tool, _ := loaded.ByID("golang")
	reg := NewRegistry()
	in, _ := reg.Get("manager")

	method := tool.Install[0]
	action, err := in.Plan(context.Background(), env, tool, method, OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(action.Commands) != 1 {
		t.Fatalf("commands = %v", action.Commands)
	}
	got := action.Commands[0].String()
	if !strings.HasPrefix(got, "g install ") {
		t.Errorf("command = %q", got)
	}
	if !action.Network {
		t.Error("manager install should be marked as needing network")
	}
}

func TestManagerInstallerResolvesLatest(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["g"] = "/usr/bin/g"
	env.runOut["g"] = "1.22.5\n1.24.1\n"
	loaded := loadManifest(t, `
version: 1
tools:
  - id: golang
    stack: golang
    kind: runtime
    verify: {command: go}
    install:
      - via: manager
        params: {manager: g, action: install, version: latest}
`)
	tool, _ := loaded.ByID("golang")
	in, _ := NewRegistry().Get("manager")
	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(env.ran("g")) != 1 || env.ran("g")[0].Args[0] != "ls-remote" {
		t.Fatalf("expected a list-remote call, got %v", env.commandLines())
	}
	mustContain(t, action.Commands[0].String(), "1.24.1")
}

func TestManagerInstallerShellFunction(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["sh"] = "/bin/sh"
	home := env.home
	if err := os.MkdirAll(filepath.Join(home, ".nvm"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".nvm", "nvm.sh"), []byte("# nvm\n"), 0o644); err != nil {
		t.Fatalf("write nvm.sh: %v", err)
	}
	loaded := loadManifest(t, `
version: 1
tools:
  - id: nodejs
    stack: nodejs
    kind: runtime
    verify: {command: node}
    install:
      - via: manager
        params: {manager: nvm, action: install, version: "22.1.0"}
`)
	tool, _ := loaded.ByID("nodejs")
	in, _ := NewRegistry().Get("manager")
	if !in.Available(context.Background(), env, tool, tool.Install[0]) {
		t.Fatal("nvm should be available when the source script exists")
	}
	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	cmd := action.Commands[0]
	if cmd.Name != "sh" || len(cmd.Args) != 2 || cmd.Args[0] != "-c" {
		t.Fatalf("command = %+v", cmd)
	}
	mustContain(t, cmd.Args[1], ". ")
	mustContain(t, cmd.Args[1], "nvm install 22.1.0")

	// 没有 nvm.sh 时不可用
	env2 := newFakeEnv(t)
	env2.bins["sh"] = "/bin/sh"
	if in.Available(context.Background(), env2, tool, tool.Install[0]) {
		t.Error("nvm without the source script must be unavailable")
	}
}

const pkgManifest = `
version: 1
tools:
  - id: rg
    stack: base
    kind: cli
    verify: {command: rg}
    install:
      - via: npm
        params: {package: "@scope/rg", global: true}
      - via: cargo
        params: {crate: ripgrep, features: [pcre2, simd]}
      - via: goinstall
        params: {module: github.com/x/tool, version: v1.2.3}
      - via: brew
        params: {formula: ripgrep, tap: homebrew/core}
      - via: brew
        params: {cask: ripgrep-app}
      - via: pipx
        params: {package: ripgrep, python: "3.12"}
`

func TestPackageManagerPlans(t *testing.T) {
	env := newFakeEnv(t)
	loaded := loadManifest(t, pkgManifest)
	tool, _ := loaded.ByID("rg")
	reg := NewRegistry()

	tests := []struct {
		index  int
		op     Op
		expect []string
	}{
		{0, OpInstall, []string{"npm install -g @scope/rg"}},
		{0, OpUninstall, []string{"npm uninstall -g @scope/rg"}},
		{0, OpUpgrade, []string{"npm update -g @scope/rg"}},
		{1, OpInstall, []string{"cargo install --features pcre2,simd ripgrep"}},
		{1, OpUninstall, []string{"cargo uninstall ripgrep"}},
		{2, OpInstall, []string{"go install github.com/x/tool@v1.2.3"}},
		{2, OpUpgrade, []string{"go install github.com/x/tool@latest"}},
		{3, OpInstall, []string{"brew tap homebrew/core", "brew install ripgrep"}},
		{4, OpInstall, []string{"brew install --cask ripgrep-app"}},
		{5, OpInstall, []string{"pipx install --python 3.12 ripgrep"}},
	}
	for _, tc := range tests {
		method := tool.Install[tc.index]
		in, ok := reg.Get(method.Via)
		if !ok {
			t.Fatalf("no installer for %s", method.Via)
		}
		action, err := in.Plan(context.Background(), env, tool, method, tc.op)
		if err != nil {
			t.Fatalf("%s %s: %v", method.Via, tc.op, err)
		}
		var lines []string
		for _, c := range action.Commands {
			lines = append(lines, c.String())
		}
		if strings.Join(lines, " ; ") != strings.Join(tc.expect, " ; ") {
			t.Errorf("%s %s = %v, want %v", method.Via, tc.op, lines, tc.expect)
		}
	}

	// goinstall 不支持卸载
	in, _ := reg.Get("goinstall")
	if _, err := in.Plan(context.Background(), env, tool, tool.Install[2], OpUninstall); err == nil {
		t.Error("goinstall uninstall must be unsupported")
	}
}

func TestPackageManagerGlobalAndPrefix(t *testing.T) {
	env := newFakeEnv(t)
	loaded := loadManifest(t, `
version: 1
tools:
  - id: t
    stack: base
    kind: cli
    verify: {command: t}
    install:
      - via: npm
        params: {package: t, global: false}
      - via: npm
        params: {package: t, prefix: ~/.npm-global}
      - via: bun
        params: {package: t}
`)
	tool, _ := loaded.ByID("t")
	reg := NewRegistry()

	in, _ := reg.Get("npm")
	a1, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if strings.Contains(a1.Commands[0].String(), "-g") {
		t.Errorf("global: false must drop -g: %q", a1.Commands[0].String())
	}
	a2, err := in.Plan(context.Background(), env, tool, tool.Install[1], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	mustContain(t, a2.Commands[0].String(), "--prefix "+filepath.Join(env.home, ".npm-global"))

	inBun, _ := reg.Get("bun")
	a3, err := inBun.Plan(context.Background(), env, tool, tool.Install[2], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	mustContain(t, a3.Commands[0].String(), "add -g t")
}

func TestSystemInstallerPlans(t *testing.T) {
	env := newFakeEnv(t)
	loaded := loadManifest(t, `
version: 1
tools:
  - id: btop
    stack: base
    kind: cli
    verify: {command: btop}
    install:
      - via: apt
        params: {packages: [btop, htop]}
      - via: winget
        params: {id: aristocratos.btop4win}
      - via: pacman
        params: {packages: [btop]}
`)
	tool, _ := loaded.ByID("btop")
	reg := NewRegistry()

	in, _ := reg.Get("apt")
	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(action.Commands) != 2 {
		t.Fatalf("apt install should refresh the index first: %v", action.Commands)
	}
	if !action.NeedsRoot {
		t.Error("apt install needs root")
	}
	mustContain(t, action.Commands[1].String(), "sudo apt-get install -y btop htop")

	// 已经是 root 时不再加 sudo
	env.root = true
	action, err = in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if action.NeedsRoot || strings.HasPrefix(action.Commands[1].String(), "sudo") {
		t.Errorf("root should not need sudo: %+v", action.Commands[1])
	}
	env.root = false

	win, _ := reg.Get("winget")
	action, err = win.Plan(context.Background(), env, tool, tool.Install[1], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	mustContain(t, action.Commands[0].String(), "--id aristocratos.btop4win")

	pac, _ := reg.Get("pacman")
	if _, err := pac.Plan(context.Background(), env, tool, tool.Install[2], OpUninstall); err == nil {
		t.Error("pacman uninstall must be refused (not bs's job)")
	}
	if _, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpUpgrade); err == nil {
		t.Error("upgrading system packages must be refused by default")
	}
}

const scriptManifest = `
version: 1
tools:
  - id: uv
    stack: python
    kind: versionmgr
    verify: {command: uv}
    install:
      - via: script
        params:
          url: https://astral.sh/uv/install.sh
          sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          args: ["--no-modify-path"]
`

func TestScriptInstaller(t *testing.T) {
	env := newFakeEnv(t)
	env.bins["sh"] = "/bin/sh"
	env.files["https://astral.sh/uv/install.sh"] = "#!/bin/sh\necho install\n"
	loaded := loadManifest(t, scriptManifest)
	tool, _ := loaded.ByID("uv")
	reg := NewRegistry()
	in, _ := reg.Get("script")

	action, err := in.Plan(context.Background(), env, tool, tool.Install[0], OpInstall)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(action.Downloads) != 1 || action.Downloads[0].SHA256 == "" {
		t.Fatalf("downloads = %+v", action.Downloads)
	}
	if !filepath.IsAbs(action.Downloads[0].Dest) {
		t.Errorf("download dest must be absolute: %q", action.Downloads[0].Dest)
	}
	cmd := action.Commands[0]
	if cmd.Name != "sh" || cmd.Args[len(cmd.Args)-1] != "--no-modify-path" {
		t.Fatalf("command = %+v", cmd)
	}

	if err := in.Execute(context.Background(), action, env); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(env.downloads) != 1 {
		t.Fatalf("downloads not performed: %+v", env.downloads)
	}
	if _, err := os.Stat(action.Downloads[0].Dest); err != nil {
		t.Errorf("script not written: %v", err)
	}
	ran := env.ran("sh")
	if len(ran) != 1 {
		t.Fatalf("script not executed: %v", env.commandLines())
	}

	// 干跑：不下载、不执行
	env2 := newFakeEnv(t)
	env2.dry = true
	env2.bins["sh"] = "/bin/sh"
	if err := in.Execute(context.Background(), action, env2); err != nil {
		t.Fatalf("dry-run Execute: %v", err)
	}
	if len(env2.downloads) != 0 || len(env2.commands) != 0 {
		t.Errorf("dry-run must not download or run: %+v %v", env2.downloads, env2.commandLines())
	}
}

func TestValidateParams(t *testing.T) {
	reg := NewRegistry()
	tests := []struct {
		name    string
		via     string
		params  manifest.Params
		wantErr string
	}{
		{"unknown via", "nope", manifest.Params{}, "unknown via nope"},
		{"windows-only param misuse", "apt", manifest.Params{ID: "x"}, "params.id"},
		{"apt without packages", "apt", manifest.Params{}, "packages"},
		{"apt with spaces in package", "apt", manifest.Params{Packages: []string{"a b"}}, "spaces"},
		{"apt repo unsupported", "apt", manifest.Params{Packages: []string{"a"}, Repo: "ppa:x"}, "repo"},
		{"npm without package", "npm", manifest.Params{}, "params.package"},
		{"cargo without crate", "cargo", manifest.Params{}, "params.crate"},
		{"goinstall module with space", "goinstall", manifest.Params{Module: "a b"}, "spaces"},
		{"brew without formula", "brew", manifest.Params{}, "formula"},
		{"script without url", "script", manifest.Params{}, "params.url"},
		{"script without sha256", "script", manifest.Params{URL: "https://x/y.sh"}, "params.sha256"},
		{"script insecure ok", "script", manifest.Params{URL: "https://x/y.sh", Insecure: boolPtr(true)}, ""},
		{"release without repo", "release", manifest.Params{}, "params.repo"},
		{"release without asset", "release", manifest.Params{Repo: "a/b"}, "params.asset"},
		{"release without dest", "release", manifest.Params{Repo: "a/b", Asset: "x.tgz"}, "params.dest"},
		{"release bad checksum", "release", manifest.Params{Repo: "a/b", Asset: "x", Dest: "~/bin", Checksum: "md5"}, "checksum"},
		{"release ok", "release", manifest.Params{Repo: "a/b", Asset: "x", Dest: "~/bin"}, ""},
		{"source without repo", "source", manifest.Params{}, "params.repo"},
		{"source without commands", "source", manifest.Params{Repo: "a/b"}, "params.build"},
		{"manager without action", "manager", manifest.Params{Manager: "g"}, "params.action"},
		{"manager install without version", "manager", manifest.Params{Manager: "g", Action: "install"}, "params.version"},
		{"manager unknown action", "manager", manifest.Params{Manager: "g", Action: "frobnicate"}, "unknown action"},
		{"template typo", "npm", manifest.Params{Package: "{{pakage}}"}, "unknown template variable"},
		{"good template", "npm", manifest.Params{Package: "{{version}}"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := reg.ValidateMethod(tc.via, tc.params)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			mustContain(t, err.Error(), tc.wantErr)
		})
	}

	if err := reg.ValidateManagerAction("g", "install"); err != nil {
		t.Errorf("g install: %v", err)
	}
	if err := reg.ValidateManagerAction("nope", "install"); err == nil {
		t.Error("unknown manager must fail")
	}
	if err := reg.ValidateManagerAction("g", "frobnicate"); err == nil {
		t.Error("unknown action must fail")
	}
	if !reg.IsSystemVia("apt") || reg.IsSystemVia("brew") || reg.IsSystemVia("manager") {
		t.Error("IsSystemVia classification is wrong")
	}
	if len(reg.KnownVias()) < 15 {
		t.Errorf("expected all vias registered, got %v", reg.KnownVias())
	}
	if len(reg.Managers()) != 7 {
		t.Errorf("managers = %d", len(reg.Managers()))
	}
}

func boolPtr(b bool) *bool { return &b }

// TestSystemUninstallSummaryUsesRemove：摘要要反映实际动作——
// 卸载 apt 包必须显示 remove，而不是永远显示 install 的 label。
func TestSystemUninstallSummaryUsesRemove(t *testing.T) {
	loaded := loadManifest(t, `
version: 1
tools:
  - id: jq
    stack: base
    kind: cli
    verify: {command: jq}
    install:
      - via: apt
        params: {packages: [jq], allow_system: true}
`)
	env := newFakeEnv(t).has("apt-get")
	eng, err := NewEngine(EngineOptions{Env: env, Loaded: loaded})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	tool, _ := loaded.ByID("jq")
	method := tool.Install[0]

	for _, tc := range []struct {
		op   Op
		want string
	}{{OpInstall, "apt-get install -y jq"}, {OpUninstall, "apt-get remove -y jq"}} {
		in, ok := eng.Registry().Get("apt")
		if !ok {
			t.Fatal("apt installer missing")
		}
		action, err := in.Plan(context.Background(), env, tool, method, tc.op)
		if err != nil {
			t.Fatalf("%v: %v", tc.op, err)
		}
		if action.Summary != tc.want {
			t.Errorf("%v summary = %q, want %q", tc.op, action.Summary, tc.want)
		}
	}
}

// TestOSEnvTimeoutsAreApplied：`timeouts.lookup` / `timeouts.download` 必须落到
// HTTP 客户端与元数据请求上——"配置写了但没接线"是这类功能最容易翻的车。
func TestOSEnvTimeoutsAreApplied(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer slow.Close()

	client := httpx.New(time.Minute, nil)
	env := NewOSEnv(OSEnvOptions{
		Client:          client,
		HomeDir:         t.TempDir(),
		GOOS:            "linux",
		DownloadTimeout: 5 * time.Minute,
		LookupTimeout:   50 * time.Millisecond,
	})
	if client.DownloadTimeout != 5*time.Minute {
		t.Errorf("客户端下载时限 = %v，期望 5m", client.DownloadTimeout)
	}

	start := time.Now()
	if _, err := env.FetchText(context.Background(), slow.URL+"/checksums.txt"); err == nil {
		t.Fatal("元数据请求超时应当报错")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("%v 才返回：lookup 时限没有生效（应当 ~50ms）", elapsed)
	}
}

// TestOSEnvDownloadsLandInStableCache：归档/脚本必须落到**跨次运行稳定**的
// `<cache>/downloads`。落到每次运行都不同的临时目录，断点续传与缓存命中都无从谈起。
func TestOSEnvDownloadsLandInStableCache(t *testing.T) {
	cache := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "archive-bytes")
	}))
	defer srv.Close()

	mk := func() *OSEnv {
		return NewOSEnv(OSEnvOptions{Client: httpx.New(time.Minute, nil), HomeDir: t.TempDir(),
			GOOS: "linux", Arch: "amd64", CacheDir: cache})
	}
	first := mk()
	if got, want := first.DownloadDir(), filepath.Join(cache, "downloads"); got != want {
		t.Fatalf("DownloadDir = %q，期望 %q", got, want)
	}
	dest, err := first.Download(context.Background(), Download{URL: srv.URL + "/tool.tar.gz"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if filepath.Dir(dest) != first.DownloadDir() {
		t.Errorf("落点 %q 不在下载缓存里", dest)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("文件没落地：%v", err)
	}

	// 第二次运行（新的 OSEnv，同一 cache）：同 URL 无校验和时必须重下（不能拿缓存的
	// 内容冒充），但放的是同一个位置——这正是"能续传/能复用"的前提。
	second := mk()
	again, err := second.Download(context.Background(), Download{URL: srv.URL + "/tool.tar.gz"})
	if err != nil {
		t.Fatalf("第二次 Download: %v", err)
	}
	if again != dest {
		t.Errorf("第二次落点 %q，期望与第一次相同 %q", again, dest)
	}
}

// TestOSEnvPrunesOldDownloadCache：下载缓存不能无限长大——久未使用的条目要在
// 下一次真下载前被清掉，而新鲜文件与半成品之外的东西不受影响。
func TestOSEnvPrunesOldDownloadCache(t *testing.T) {
	cache := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fresh")
	}))
	defer srv.Close()

	env := NewOSEnv(OSEnvOptions{Client: httpx.New(time.Minute, nil), HomeDir: t.TempDir(),
		GOOS: "linux", Arch: "amd64", CacheDir: cache})
	if err := os.MkdirAll(env.DownloadDir(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := filepath.Join(env.DownloadDir(), "old-tool.tar.gz")
	fresh := filepath.Join(env.DownloadDir(), "recent-tool.tar.gz")
	for _, f := range []string{stale, fresh} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	old := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if _, err := env.Download(context.Background(), Download{URL: srv.URL + "/new.tar.gz"}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("过期条目应当被清掉")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("新鲜条目不该被清掉：%v", err)
	}
}

// TestOSEnvDryRunPrunesNothing：干跑连缓存都不整理（"dry-run 零写入"包括删除）。
func TestOSEnvDryRunPrunesNothing(t *testing.T) {
	cache := t.TempDir()
	env := NewOSEnv(OSEnvOptions{HomeDir: t.TempDir(), GOOS: "linux", CacheDir: cache, DryRun: true})
	if err := os.MkdirAll(env.DownloadDir(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := filepath.Join(env.DownloadDir(), "old.tar.gz")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	env.pruneDownloadCache()
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("干跑不该删除任何文件：%v", err)
	}
}
