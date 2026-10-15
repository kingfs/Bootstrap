package manifest

import (
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/kingfs/Bootstrap/internal/config"
)

// fakeVias 是 MethodValidator 的测试替身：
// 只有注册表里存在的 via 合法，且 manager 必须带 action。
type fakeVias struct {
	managers map[string][]string
	system   []string
}

func (f fakeVias) KnownVias() []string { return []string{"manager", "apt", "brew", "bun", "release"} }

func (f fakeVias) ValidateMethod(via string, p Params) error {
	for _, v := range f.KnownVias() {
		if v == via {
			if via == "manager" && p.Action == "" {
				return errors.New("via=manager requires params.action")
			}
			return nil
		}
	}
	return errors.New("unknown via " + via)
}

func (f fakeVias) ValidateManagerAction(manager, action string) error {
	actions, ok := f.managers[manager]
	if !ok {
		return errors.New("unknown manager " + manager)
	}
	for _, a := range actions {
		if a == action {
			return nil
		}
	}
	return errors.New("manager " + manager + " does not support action " + action)
}

func (f fakeVias) IsSystemVia(via string) bool {
	for _, v := range f.system {
		if v == via {
			return true
		}
	}
	return false
}

func testVias() fakeVias {
	return fakeVias{
		managers: map[string][]string{"g": {"install", "upgrade", "uninstall"}},
		system:   []string{"apt", "brew"},
	}
}

func builtinFS() fstest.MapFS {
	return fstest.MapFS{
		"common.yaml": &fstest.MapFile{Data: []byte(`
version: 1
tools:
  - id: g
    name: g
    stack: golang
    kind: versionmgr
    verify: {command: g, version_args: [--version], version_regex: 'g version (?P<v>[\d.]+)'}
    install:
      - via: release
        params: {repo: voidint/g, asset: "g{{version}}-{{os}}.tar.gz", dest: "~/.local/bin"}
  - id: golang
    name: Go
    stack: golang
    kind: runtime
    depends_on: [g]
    tags: [lang]
    verify: {command: go, version_args: [version], version_regex: 'go version go(?P<v>\d+\.\d+)', min_version: "1.22"}
    install:
      - via: manager
        params: {manager: g, action: install, version: latest}
profiles:
  base: [g]
`),
		},
		"linux.yaml": &fstest.MapFile{Data: []byte(`
version: 1
tools:
  - id: btop
    name: btop
    stack: base
    kind: cli
    when: {os: [linux], distro: [ubuntu, debian]}
    verify: {command: btop}
    install:
      - via: apt
        params: {packages: [btop], needs_root: true}
`),
		},
	}
}

func loadFor(t *testing.T, userFiles map[string]string, goos string) *Loaded {
	t.Helper()
	dir := t.TempDir()
	for name, content := range userFiles {
		writeFile(t, dir+"/"+name, content)
	}
	l, err := Load(LoadOptions{
		Builtin:   true,
		BuiltinFS: builtinFS(),
		GOOS:      goos,
		UserDirs:  []string{dir},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return l
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFileRaw(path, content); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadBuiltinOnly(t *testing.T) {
	l := loadFor(t, nil, "linux")
	if len(l.Tools) != 3 {
		t.Fatalf("tools = %d, want 3 (%v)", len(l.Tools), ids(l.Tools))
	}
	// 顺序：common.yaml 先，平台文件后；profile 文件不产生工具。
	if got := ids(l.Tools); strings.Join(got, ",") != "g,golang,btop" {
		t.Errorf("order = %v", got)
	}
	g, ok := l.ByID("g")
	if !ok {
		t.Fatal("g missing")
	}
	if g.Verify.Regex() == nil {
		t.Error("version_regex should be compiled at load time")
	}
	if g.Origin != "builtin:common.yaml" {
		t.Errorf("origin = %q", g.Origin)
	}
	go1, _ := l.ByID("golang")
	if go1.Verify.Min() == nil {
		t.Error("min_version should be parsed at load time")
	}
	// 只记录真正读到的文件（测试 FS 里没有 profiles.yaml，所以是 2 个）。
	if len(l.Sources) != 2 {
		t.Errorf("sources = %+v", l.Sources)
	}
}

func TestLoadUserOverrideReplacesArraysAndAppends(t *testing.T) {
	l := loadFor(t, map[string]string{
		"10-local.yaml": `
version: 1
tools:
  - id: golang
    install:
      - via: manager
        params: {manager: g, action: install, version: "1.24.1"}
    +install:
      - via: release
        params: {repo: golang/go, asset: "go{{version}}.{{os}}-{{arch}}.tar.gz", dest: "~/go"}
    +tags: [extra]
`,
	}, "linux")

	go1, _ := l.ByID("golang")
	if len(go1.Install) != 2 {
		t.Fatalf("install chain = %d, want 2", len(go1.Install))
	}
	if got := go1.Install[0].Params.Version; got != "1.24.1" {
		t.Errorf("override version = %q", got)
	}
	if got := go1.Install[1].Via; got != "release" {
		t.Errorf("appended method via = %q", got)
	}
	if strings.Join(go1.Tags, ",") != "lang,extra" {
		t.Errorf("tags = %v", go1.Tags)
	}
	// 未被覆盖的字段必须保留
	if len(go1.DependsOn) != 1 || go1.DependsOn[0] != "g" {
		t.Errorf("depends_on lost: %v", go1.DependsOn)
	}
	if go1.Name != "Go" {
		t.Errorf("name lost: %q", go1.Name)
	}
	if go1.Verify.Min() == nil {
		t.Error("verify.min_version lost")
	}
	if !strings.HasSuffix(go1.Origin, "10-local.yaml") {
		t.Errorf("origin = %q", go1.Origin)
	}
	if len(l.Tools) != 3 {
		t.Errorf("override must not add a duplicate entry: %v", ids(l.Tools))
	}
}

func TestLoadDisabledTool(t *testing.T) {
	l := loadFor(t, map[string]string{
		"10-off.yaml": "version: 1\ntools:\n  - id: btop\n    disabled: true\n",
	}, "linux")
	btop, _ := l.ByID("btop")
	if btop.Enabled() {
		t.Error("btop should be disabled")
	}
}

func TestLoadUserDirsAppliedInOrder(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeFile(t, dir1+"/a.yaml", "version: 1\ntools:\n  - id: x\n    name: first\n    stack: base\n    kind: cli\n    verify: {command: x}\n    install: [{via: apt, params: {packages: [x]}}]\n")
	writeFile(t, dir2+"/b.yaml", "version: 1\ntools:\n  - id: x\n    name: second\n")
	l, err := Load(LoadOptions{Builtin: false, UserDirs: []string{dir1, dir2}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	x, _ := l.ByID("x")
	if x.Name != "second" {
		t.Errorf("later dir must win: name = %q", x.Name)
	}
	if len(x.Install) != 1 {
		t.Errorf("install chain lost: %+v", x.Install)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"unknown field", "version: 1\ntools:\n  - id: a\n    pakage: oops\n", "pakage"},
		{"bad version", "version: 2\ntools: []\n", "unsupported manifest version"},
		{"duplicate id", "version: 1\ntools:\n  - id: a\n  - id: a\n", "duplicate tool id"},
		{"bad regex", "version: 1\ntools:\n  - id: a\n    verify: {version_regex: '['}\n", "version_regex"},
		{"regex without group", "version: 1\ntools:\n  - id: a\n    verify: {version_regex: '(\\d+)'}\n", "named group"},
		{"bad min_version", "version: 1\ntools:\n  - id: a\n    verify: {command: a, min_version: 'not-a-version'}\n", "min_version"},
		{"no install method", "version: 1\ntools:\n  - id: a\n    stack: base\n    kind: cli\n    verify: {command: a}\n", "no install method"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir+"/x.yaml", tc.content)
			_, err := Load(LoadOptions{Builtin: false, UserDirs: []string{dir}})
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadIgnoresMissingUserDir(t *testing.T) {
	l, err := Load(LoadOptions{Builtin: false, UserDirs: []string{"/no/such/dir/at/all"}})
	if err != nil {
		t.Fatalf("missing user dir must be tolerated: %v", err)
	}
	if len(l.Tools) != 0 {
		t.Errorf("tools = %v", ids(l.Tools))
	}
}

func TestValidateCleanManifest(t *testing.T) {
	l := loadFor(t, nil, "linux")
	err := Validate(l, ValidateOptions{
		Vias:                     testVias(),
		AllowSystemFor:           []config.Kind{config.KindCLI, config.KindDevDep, config.KindService},
		Managers:                 map[string]string{"golang": "g"},
		RequireManagerForRuntime: true,
	})
	if err != nil {
		t.Fatalf("builtin manifest should be valid: %v", err)
	}
}

func TestValidateFindsAllProblems(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/x.yaml", `
version: 1
tools:
  - id: g
    stack: golang
    kind: versionmgr
    verify: {command: g}
    install:
      - via: release
        params: {repo: voidint/g, asset: "g{{version}}-{{os}}.tar.gz", dest: "~/.local/bin"}
  - id: Bad_ID
    stack: nonsense
    kind: nonsense
    install:
      - via: nosuchvia
  - id: runtime-tool
    stack: nodejs
    kind: runtime
    verify: {command: node}
    install:
      - via: apt
        params: {packages: [nodejs]}
  - id: crossed
    stack: nodejs
    kind: runtime
    depends_on: [g]
    verify: {command: node}
    install:
      - via: manager
        params: {manager: g, action: install}
  - id: deps
    stack: python
    kind: runtime
    depends_on: [ghost]
    verify: {command: python3}
    install:
      - via: manager
        params: {manager: g, action: explode}
profiles:
  broken:
    tools: [ghost]
    extends: [missing]
`)
	l, err := Load(LoadOptions{Builtin: false, UserDirs: []string{dir}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = Validate(l, ValidateOptions{
		Vias:                     testVias(),
		AllowSystemFor:           []config.Kind{config.KindCLI},
		Managers:                 map[string]string{"nodejs": "fnm"},
		RequireManagerForRuntime: true,
	})
	if err == nil {
		t.Fatal("expected validation problems")
	}
	text := err.Error()
	for _, want := range []string{
		"id must match",
		"unknown stack",
		"unknown kind",
		"unknown via",
		"verify.command, verify.file or verify.version_regex is required",
		"system package manager",
		"must have a `via: manager` path",
		"depends_on \"ghost\" not found",
		"crosses stacks",
		"does not support action",
		"profile \"broken\": tool \"ghost\" not found",
		"extends \"missing\" not found",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("validation missed %q:\n%s", want, text)
		}
	}
}

func TestValidateProfileCycle(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/x.yaml", `
version: 1
profiles:
  a: {extends: [b]}
  b: {extends: [a]}
`)
	l, err := Load(LoadOptions{Builtin: false, UserDirs: []string{dir}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := Validate(l, ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "profile cycle") {
		t.Fatalf("err = %v, want profile cycle", err)
	}
}

func TestProfilesBothFormsAndExtends(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/x.yaml", `
version: 1
tools:
  - id: a
    stack: base
    kind: cli
    verify: {command: a}
    install: [{via: apt, params: {packages: [a]}}]
  - id: b
    stack: base
    kind: cli
    verify: {command: b}
    install: [{via: apt, params: {packages: [b]}}]
  - id: c
    stack: base
    kind: cli
    verify: {command: c}
    install: [{via: apt, params: {packages: [c]}}]
profiles:
  short: [a]
  full: {description: 完整, tools: [b]}
  derived: {extends: [short, full], tools: [c]}
`)
	l, err := Load(LoadOptions{Builtin: false, UserDirs: []string{dir}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(l.Profiles["short"].Tools, ","); got != "a" {
		t.Errorf("short = %q", got)
	}
	if got := l.Profiles["full"].Description; got != "完整" {
		t.Errorf("full description = %q", got)
	}
	ids, err := l.ResolveProfile("derived")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if strings.Join(ids, ",") != "a,b,c" {
		t.Errorf("derived = %v", ids)
	}
	if _, err := l.ResolveProfile("nope"); err == nil {
		t.Error("unknown profile should fail")
	}
}

func TestTopoOrderAndCycle(t *testing.T) {
	tools := []Tool{
		{ID: "gopls", DependsOn: []string{"golang"}},
		{ID: "golang", DependsOn: []string{"g"}},
		{ID: "g"},
		{ID: "rg"},
	}
	order, err := TopoOrder(tools)
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	if got := ids(order); strings.Join(got, ",") != "g,golang,gopls,rg" {
		t.Errorf("order = %v", got)
	}

	cyclic := []Tool{{ID: "a", DependsOn: []string{"b"}}, {ID: "b", DependsOn: []string{"a"}}}
	if _, err := TopoOrder(cyclic); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want cycle", err)
	}
}

func TestExpandDeps(t *testing.T) {
	l := loadFor(t, nil, "linux")
	tools, err := ExpandDeps(l, []string{"golang"})
	if err != nil {
		t.Fatalf("ExpandDeps: %v", err)
	}
	if got := ids(tools); strings.Join(got, ",") != "g,golang" {
		t.Errorf("expand = %v", got)
	}
	if _, err := ExpandDeps(l, []string{"ghost"}); err == nil {
		t.Error("unknown tool should fail")
	}
}

func TestWhenMatches(t *testing.T) {
	env := MatchEnv{
		OS: "linux", Arch: "arm64", Distro: "ubuntu",
		Has: func(bin string) bool { return bin == "bun" },
	}
	tests := []struct {
		name string
		when When
		want bool
	}{
		{"empty matches", When{}, true},
		{"os match", When{OS: []string{"linux", "darwin"}}, true},
		{"os miss", When{OS: []string{"windows"}}, false},
		{"os_not", When{OSNot: []string{"linux"}}, false},
		{"arch", When{Arch: []string{"amd64"}}, false},
		{"distro", When{Distro: []string{"Ubuntu"}}, true},
		{"distro miss", When{Distro: []string{"fedora"}}, false},
		{"distro_not", When{DistroNot: []string{"ubuntu"}}, false},
		{"has ok", When{Has: []string{"bun"}}, true},
		{"has missing", When{Has: []string{"npm"}}, false},
		{"has_not", When{HasNot: []string{"bun"}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := tc.when.Matches(env)
			if got != tc.want {
				t.Errorf("Matches = %v (%s), want %v", got, reason, tc.want)
			}
			if !got && reason == "" {
				t.Error("a non-match must explain why")
			}
		})
	}
	// Has 为 nil 时，has 条件必须判为不匹配而不是崩溃。
	if got, _ := (When{Has: []string{"bun"}}).Matches(MatchEnv{OS: "linux"}); got {
		t.Error("nil Has must not match")
	}
}

// TestWhenMissingHas 锁定依赖推断用的"只差某个命令"判断。
func TestWhenMissingHas(t *testing.T) {
	env := MatchEnv{
		OS: "linux", Arch: "amd64", Distro: "ubuntu",
		Has: func(bin string) bool { return bin == "bun" },
	}
	tests := []struct {
		name string
		when When
		want []string
	}{
		{"已满足", When{Has: []string{"bun"}}, nil},
		{"空条件", When{}, nil},
		{"只缺 has", When{Has: []string{"go"}}, []string{"go"}},
		{"缺多个 has", When{Has: []string{"go", "cargo"}}, []string{"go", "cargo"}},
		{"部分满足", When{Has: []string{"bun", "go"}}, []string{"go"}},
		// 作者明确的排除（os/distro/has_not）不该被"补一个提供者"绕过去。
		{"os 不匹配", When{OS: []string{"darwin"}, Has: []string{"go"}}, nil},
		{"distro 不匹配", When{Distro: []string{"fedora"}, Has: []string{"go"}}, nil},
		{"has_not 冲突", When{HasNot: []string{"bun"}, Has: []string{"go"}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.when.MissingHas(env)
			if len(got) != len(tc.want) {
				t.Fatalf("MissingHas = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("MissingHas = %v, want %v", got, tc.want)
				}
			}
		})
	}
	if got := (When{Has: []string{"go"}}).MissingHas(MatchEnv{OS: "linux"}); len(got) != 1 {
		t.Errorf("nil Has 时应报告缺命令：%v", got)
	}
}

func ids(tools []Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.ID)
	}
	return out
}

// writeFileRaw 让测试辅助函数少一层依赖。
func writeFileRaw(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// TestVerifyFileAndCommandExclusive：verify.file 与 verify.command 二选一，
// 并要求至少声明一个（否则"装没装"无从判断）。
func TestVerifyFileAndCommandExclusive(t *testing.T) {
	l := loadFor(t, map[string]string{"v.yaml": `
version: 1
tools:
  - id: both
    stack: base
    kind: cli
    verify: {command: both, file: ~/.both}
    install:
      - via: brew
        params: {formula: both}
  - id: neither
    stack: base
    kind: cli
    install:
      - via: brew
        params: {formula: neither}
  - id: fileonly
    stack: base
    kind: cli
    verify: {file: ~/.fileonly/nvm.sh}
    install:
      - via: brew
        params: {formula: fileonly}
`}, "linux")
	err := Validate(l, ValidateOptions{})
	if err == nil {
		t.Fatal("expected validation problems")
	}
	msg := err.Error()
	if !strings.Contains(msg, "mutually exclusive") {
		t.Errorf("missing exclusivity problem:\n%s", msg)
	}
	if !strings.Contains(msg, "tool \"neither\"") || !strings.Contains(msg, "is required") {
		t.Errorf("missing 'verify required' problem:\n%s", msg)
	}
	if strings.Contains(msg, "fileonly") {
		t.Errorf("file-only verify must be accepted:\n%s", msg)
	}
}

// TestVerifyRulesPickByWhen：verify 支持写多条规则，加载后由 ResolveVerify 按当前
// 机器挑一条。这条能力是为"同一个工具在不同发行版上命令名不同"加的（Ubuntu 的
// bat/fd 装出来叫 batcat/fdfind），必须有回归保护，否则探测会静默用错规则。
func TestVerifyRulesPickByWhen(t *testing.T) {
	l := loadFor(t, map[string]string{"bat.yaml": `
version: 1
tools:
  - id: bat
    name: bat
    stack: base
    kind: cli
    verify:
      - when: {distro: [ubuntu, debian]}
        command: batcat
        version_args: [--version]
        version_regex: 'bat (?P<v>\d+\.\d+\.\d+)'
      - command: bat
        version_args: [--version]
        version_regex: 'bat (?P<v>\d+\.\d+\.\d+)'
    install:
      - via: apt
        params: {packages: [bat]}
`}, "linux")
	if err := Validate(l, ValidateOptions{}); err != nil {
		t.Fatalf("multi-rule verify must validate: %v", err)
	}

	tool, _ := l.ByID("bat")
	if len(tool.VerifyRules) != 2 {
		t.Fatalf("verify rules = %d, want 2", len(tool.VerifyRules))
	}
	// 加载期先把第一条填进 Verify；真正生效的要等 ResolveVerify 按机器挑。
	if tool.Verify.Command != "batcat" {
		t.Errorf("pre-resolve command = %q, want batcat (first rule)", tool.Verify.Command)
	}

	ubuntu := l.ResolveVerify(MatchEnv{OS: "linux", Arch: "amd64", Distro: "ubuntu"})
	if got := mustTool(t, ubuntu, "bat").Verify.Command; got != "batcat" {
		t.Errorf("ubuntu command = %q, want batcat", got)
	}
	fedora := l.ResolveVerify(MatchEnv{OS: "linux", Arch: "amd64", Distro: "fedora"})
	if got := mustTool(t, fedora, "bat").Verify; got.Command != "bat" || got.Regex() == nil {
		t.Errorf("fedora verify = %+v, want command=bat with compiled regex", got)
	}
	// 兜底规则生效时，正则也必须是编译好的那条（ResolveVerify 换的是整条规则）。
	if got := mustTool(t, fedora, "bat").Verify.Regex(); got == nil || got.String() != `bat (?P<v>\d+\.\d+\.\d+)` {
		t.Errorf("regex = %v", got)
	}
	// 没有多条规则时原样返回同一份清单（不做无谓的复制）。
	if l.ResolveVerify(MatchEnv{OS: "linux"}) == nil {
		t.Error("ResolveVerify must not return nil")
	}
}

// TestVerifyRulesValidation：多条规则的两条硬约束——中间那条不能是兜底（会遮蔽后面的），
// 最后一条必须是兜底（否则有些机器一条都匹配不上）。
func TestVerifyRulesValidation(t *testing.T) {
	l := loadFor(t, map[string]string{"bad.yaml": `
version: 1
tools:
  - id: shadow
    stack: base
    kind: cli
    verify:
      - command: shadow
      - when: {distro: [fedora]}
        command: shadow-fedora
    install:
      - via: brew
        params: {formula: shadow}
  - id: nogeneric
    stack: base
    kind: cli
    verify:
      - when: {distro: [ubuntu]}
        command: ubuntu-only
      - when: {distro: [fedora]}
        command: fedora-only
    install:
      - via: brew
        params: {formula: nogeneric}
`}, "linux")
	err := Validate(l, ValidateOptions{})
	if err == nil {
		t.Fatal("expected validation problems")
	}
	msg := err.Error()
	if !strings.Contains(msg, "only the last verify rule may omit") {
		t.Errorf("missing shadowing problem:\n%s", msg)
	}
	if !strings.Contains(msg, "the last verify rule must omit") {
		t.Errorf("missing fallback problem:\n%s", msg)
	}
}

// TestVerifyRulesStrictFields：Node.Decode 会新建解码器，看不到外层的 KnownFields，
// 所以 verify 的字段名必须由我们手工挡——拼错字段名要被报出来，而不是静默忽略。
func TestVerifyRulesStrictFields(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/bad.yaml", `
version: 1
tools:
  - id: typo
    stack: base
    kind: cli
    verify:
      - command: typo
        version_regexp: 'x'
    install:
      - via: brew
        params: {formula: typo}
`)
	_, err := Load(LoadOptions{UserDirs: []string{dir}})
	if err == nil {
		t.Fatal("expected unknown verify field to fail the load")
	}
	if !strings.Contains(err.Error(), "unknown verify field") {
		t.Errorf("err = %v", err)
	}
}

// TestVerifyRulesOverrideMerge：用户覆盖按序号合并——既能只改字段（不丢 command），
// 也能整条替换（写 when 就换 when）。
func TestVerifyRulesOverrideMerge(t *testing.T) {
	l := loadFor(t, map[string]string{"10-local.yaml": `
version: 1
tools:
  - id: btop
    verify:
      - command: btop-custom
`}, "linux")
	tool, _ := l.ByID("btop")
	if got := tool.VerifyRules[0].Verify.Command; got != "btop-custom" {
		t.Errorf("command = %q, want btop-custom", got)
	}
	if got := tool.Verify.Command; got != "btop-custom" {
		t.Errorf("resolved command = %q, want btop-custom", got)
	}
}

func mustTool(t *testing.T, l *Loaded, id string) Tool {
	t.Helper()
	tool, ok := l.ByID(id)
	if !ok {
		t.Fatalf("tool %q missing", id)
	}
	return tool
}
