package shellsnip

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func builtinFS() fstest.MapFS {
	return fstest.MapFS{
		"snippets/posix/10-proxy.sh":   {Data: []byte("# proxy wrapper\nbs() { :; }\n")},
		"snippets/posix/90-aliases.sh": {Data: []byte("# aliases\nalias ll='ls -alh'\n")},
		"snippets/fish/10-proxy.fish":  {Data: []byte("# fish proxy\n")},
	}
}

func TestLoadBuiltinOnly(t *testing.T) {
	set, err := Load(builtinFS(), FamilyPosix, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := set.Names(); len(got) != 2 || got[0] != "10-proxy.sh" || got[1] != "90-aliases.sh" {
		t.Fatalf("Names = %v, want sorted [10-proxy.sh 90-aliases.sh]", got)
	}
	for _, s := range set.Snippets {
		if !s.Builtin || s.Source != "builtin" {
			t.Errorf("snippet %s should be builtin, got %+v", s.Name, s)
		}
	}
	out := set.Render()
	if !strings.Contains(out, "# ─── 10-proxy.sh (builtin) ───") {
		t.Errorf("Render should label the origin:\n%s", out)
	}
	if strings.Index(out, "10-proxy.sh") > strings.Index(out, "90-aliases.sh") {
		t.Error("Render must keep numeric load order")
	}
}

func TestLoadUserOverridesBuiltin(t *testing.T) {
	userDir := t.TempDir()
	posix := filepath.Join(userDir, "posix")
	if err := os.MkdirAll(posix, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(posix, "10-proxy.sh"), []byte("# user override\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(posix, "95-extra.sh"), []byte("# extra\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	set, err := Load(builtinFS(), FamilyPosix, userDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	names := set.Names()
	want := []string{"10-proxy.sh", "90-aliases.sh", "95-extra.sh"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("Names = %v, want %v", names, want)
	}
	first := set.Snippets[0]
	if first.Builtin {
		t.Error("user snippet with the same name must replace the builtin one entirely")
	}
	if !strings.Contains(first.Content, "user override") {
		t.Errorf("content = %q, want the user version", first.Content)
	}
	if strings.Contains(first.Content, "bs() { :; }") {
		t.Error("builtin content leaked into the override")
	}
	if set.Snippets[2].Builtin {
		t.Error("extra user snippet should not be marked builtin")
	}
}

func TestLoadUnknownFamilyIsEmptyNotError(t *testing.T) {
	set, err := Load(builtinFS(), FamilyPwsh, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Snippets) != 0 {
		t.Errorf("expected no pwsh snippets, got %v", set.Names())
	}
}

func TestFamilyForShell(t *testing.T) {
	cases := map[string]Family{
		"bash": FamilyPosix, "zsh": FamilyPosix, "sh": FamilyPosix,
		"fish": FamilyFish, "pwsh": FamilyPwsh, "PowerShell": FamilyPwsh,
	}
	for shell, want := range cases {
		got, err := FamilyForShell(shell)
		if err != nil {
			t.Errorf("FamilyForShell(%q): %v", shell, err)
			continue
		}
		if got != want {
			t.Errorf("FamilyForShell(%q) = %q, want %q", shell, got, want)
		}
	}
	for _, bad := range []string{"cmd", "nonsense", ""} {
		if _, err := FamilyForShell(bad); err == nil {
			t.Errorf("FamilyForShell(%q) should fail", bad)
		}
	}
}

func TestBlockAndSourceLine(t *testing.T) {
	block, err := Block("zsh")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if !strings.HasPrefix(block, BeginMarker) || !strings.HasSuffix(block, EndMarker) {
		t.Errorf("block markers wrong:\n%s", block)
	}
	if !strings.Contains(block, `eval "$(bs shell init zsh)"`) {
		t.Errorf("block should eval shell init:\n%s", block)
	}
	fishBlock, err := Block("fish")
	if err != nil {
		t.Fatalf("Block fish: %v", err)
	}
	if !strings.Contains(fishBlock, "bs shell init fish | source") {
		t.Errorf("fish block wrong:\n%s", fishBlock)
	}
	if _, err := Block("cmd"); err == nil {
		t.Error("cmd.exe has no rc block and must be rejected")
	}
}

func TestUpsertIsIdempotent(t *testing.T) {
	block, err := Block("bash")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	existing := "# my rc\nexport EDITOR=vim\n"

	updated, changed := Upsert(existing, block)
	if !changed {
		t.Fatal("first Upsert must change the file")
	}
	if !strings.HasPrefix(updated, existing) {
		t.Errorf("Upsert must not touch content before the block:\n%s", updated)
	}
	if !strings.Contains(updated, block) {
		t.Error("block missing after Upsert")
	}

	again, changed := Upsert(updated, block)
	if changed {
		t.Error("second Upsert with identical block must be a no-op")
	}
	if again != updated {
		t.Error("idempotent Upsert changed the content")
	}

	// 升级场景：块内容变化时原地替换，不追加第二份。
	upgraded := strings.Replace(block, "bs shell init bash", "bs shell init bash --v2", 1)
	replaced, changed := Upsert(updated, upgraded)
	if !changed {
		t.Fatal("changed block must report a change")
	}
	if strings.Count(replaced, BeginMarker) != 1 {
		t.Errorf("expected exactly one managed block, got:\n%s", replaced)
	}
	if !strings.Contains(replaced, "--v2") {
		t.Error("block was not upgraded in place")
	}
	if !strings.HasPrefix(replaced, existing) {
		t.Error("content outside the block must survive an upgrade")
	}
}

func TestRemoveIsExact(t *testing.T) {
	block, err := Block("bash")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	original := "# my rc\nexport EDITOR=vim\n"
	withBlock, _ := Upsert(original, block)

	restored, removed := Remove(withBlock)
	if !removed {
		t.Fatal("Remove should report removal")
	}
	if restored != original {
		t.Errorf("Remove changed surrounding content:\n%q\nwant\n%q", restored, original)
	}

	if _, removed := Remove(original); removed {
		t.Error("Remove on content without a block must be a no-op")
	}

	// 半个标记（用户手工改坏）不应导致数据丢失。
	broken := original + BeginMarker + "\nfoo\n"
	out, removed := Remove(broken)
	if removed {
		t.Error("Remove must not touch an unterminated block")
	}
	if out != broken {
		t.Error("Remove modified content it did not understand")
	}
}

func TestHasBlock(t *testing.T) {
	if HasBlock("nothing here") {
		t.Error("HasBlock false positive")
	}
	block, _ := Block("bash")
	if !HasBlock("x\n" + block + "\ny") {
		t.Error("HasBlock false negative")
	}
}

func TestDetectRCFile(t *testing.T) {
	home := "/home/kingfs"
	none := func(string) bool { return false }
	blank := func(string) string { return "" }

	cases := []struct {
		name  string
		goos  string
		shell string
		home  string
		geten func(string) string
		exist func(string) bool
		want  string
	}{
		{"linux bash", "linux", "bash", home, blank, none, "/home/kingfs/.bashrc"},
		{"darwin bash prefers profile", "darwin", "bash", home, blank,
			func(p string) bool { return p == "/home/kingfs/.bash_profile" }, "/home/kingfs/.bash_profile"},
		{"zsh", "linux", "zsh", home, blank, none, "/home/kingfs/.zshrc"},
		{"zsh zdotdir", "linux", "zsh", home, func(k string) string {
			if k == "ZDOTDIR" {
				return "/zdot"
			}
			return ""
		}, none, "/zdot/.zshrc"},
		{"fish", "linux", "fish", home, blank, none, "/home/kingfs/.config/fish/conf.d/bootstrap.fish"},
		{"pwsh unix", "linux", "pwsh", home, blank, none, "/home/kingfs/.config/powershell/Microsoft.PowerShell_profile.ps1"},
		// windows 用例刻意用 windows 形状的 home 与反斜杠期望值：
		// 断言必须只取决于目标平台，而不是"跑测试的机器"（CI 的 Windows 任务会跑同一套用例）。
		{"pwsh windows", "windows", "pwsh", `C:\Users\kingfs`, blank, none,
			`C:\Users\kingfs\Documents\PowerShell\Microsoft.PowerShell_profile.ps1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectRCFile(tc.goos, tc.shell, tc.home, tc.geten, tc.exist)
			if err != nil {
				t.Fatalf("DetectRCFile: %v", err)
			}
			if got != tc.want {
				t.Errorf("DetectRCFile = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := DetectRCFile("linux", "cmd", home, blank, none); err == nil {
		t.Error("cmd should not be resolvable to an rc file")
	}
}
