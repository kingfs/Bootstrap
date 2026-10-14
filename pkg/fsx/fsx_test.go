package fsx

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestExpandPath(t *testing.T) {
	// ExpandPath 是**宿主**语义：它展开的是运行时的真实家目录，拼出来的分隔符
	// 也由宿主决定（Windows 上是 `\`）。所以期望值必须用 filepath.Join 构造，
	// 而不是写死 `/home/kingfs/...`——后者在 Windows 上必然对不上。
	home := "/home/kingfs"
	tests := map[string]string{
		"~":             home,
		"~/src":         filepath.Join(home, "src"),
		"~/.config/bs":  filepath.Join(home, ".config", "bs"),
		"/data/src":     "/data/src",
		"":              "",
		"~/a/$BS_UNSET": filepath.Join(home, "a"), // filepath.Join 会清理尾部斜杠
	}
	for in, want := range tests {
		if got := ExpandPath(in, home); got != want {
			t.Errorf("ExpandPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpandPathUsesEnv(t *testing.T) {
	t.Setenv("BS_TEST_ROOT", "/opt/bs")
	if got := ExpandPath("$BS_TEST_ROOT/bin", "/home/x"); got != "/opt/bs/bin" {
		t.Errorf("ExpandPath with env = %q, want /opt/bs/bin", got)
	}
}

func TestAtomicWriteFileAndExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "file.txt")

	if Exists(path) {
		t.Fatal("file should not exist yet")
	}
	if err := AtomicWriteFile(path, []byte("hello"), 0o640); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}
	if !Exists(path) || !IsFile(path) || IsDir(path) {
		t.Fatalf("file state wrong: exists=%v file=%v dir=%v", Exists(path), IsFile(path), IsDir(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("content = %q, want hello", data)
	}
	// Windows 没有 POSIX 权限位：os.FileMode 的 Perm() 读出来恒是 0666/0777 之类，
	// 与请求的 0640 无关，因此这条断言在 Windows 上不成立（其它断言照旧）。
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if st.Mode().Perm() != 0o640 {
			t.Errorf("perm = %v, want 0640", st.Mode().Perm())
		}
	}
	// 不应留下临时文件。
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want 1 (no temp leftovers)", len(entries))
	}
}

// TestWriteFileIfChangedIsIdempotent 是无副作用要求的最小验证：
// 内容未变时不得触碰文件（mtime 不变、不产生新 inode 写入）。
func TestWriteFileIfChangedIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.yaml")
	content := []byte("version: 1\n")

	changed, err := WriteFileIfChanged(path, content, 0o644)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if !changed {
		t.Fatal("first write should report change")
	}
	beBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	changed, err = WriteFileIfChanged(path, content, 0o644)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if changed {
		t.Error("second write with identical content should be a no-op")
	}
	beAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !beBefore.ModTime().Equal(beAfter.ModTime()) {
		t.Errorf("mtime changed: %v -> %v", beBefore.ModTime(), beAfter.ModTime())
	}

	changed, err = WriteFileIfChanged(path, []byte("version: 2\n"), 0o644)
	if err != nil {
		t.Fatalf("third write: %v", err)
	}
	if !changed {
		t.Error("changed content should be written")
	}
}

func TestBackupFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".bashrc")
	if err := os.WriteFile(path, []byte("export A=1\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	backup, err := BackupFile(path, "20261013T180000")
	if err != nil {
		t.Fatalf("BackupFile: %v", err)
	}
	if want := path + ".bs-backup.20261013T180000"; backup != want {
		t.Errorf("backup path = %q, want %q", backup, want)
	}
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(data) != "export A=1\n" {
		t.Errorf("backup content = %q", data)
	}
}

func TestBackupFileMissing(t *testing.T) {
	if _, err := BackupFile(filepath.Join(t.TempDir(), "nope"), "x"); err == nil {
		t.Error("BackupFile on missing file should fail")
	}
}

func TestEnsureDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	if err := EnsureDir(dir, 0o755); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if !IsDir(dir) {
		t.Error("directory not created")
	}
	if err := EnsureDir(dir, 0o755); err != nil {
		t.Errorf("EnsureDir should be idempotent: %v", err)
	}
}

// TestJoinFor：拼接结果只取决于目标平台，与宿主无关。
// 这是"在 linux 上算 windows 路径"这类跨平台逻辑的前提。
func TestJoinFor(t *testing.T) {
	cases := []struct {
		goos string
		elem []string
		want string
	}{
		{"linux", []string{"/home/kingfs", ".config", "bootstrap"}, "/home/kingfs/.config/bootstrap"},
		{"darwin", []string{"/home/kingfs", ".zshrc"}, "/home/kingfs/.zshrc"},
		{"windows", []string{`C:\Users\k`, "AppData", "Roaming", "bootstrap"}, `C:\Users\k\AppData\Roaming\bootstrap`},
		{"windows", []string{`C:\Users\k\AppData\Roaming`, "bootstrap"}, `C:\Users\k\AppData\Roaming\bootstrap`},
		{"linux", []string{"/x//cfg/", "bootstrap"}, "/x/cfg/bootstrap"},
		{"linux", []string{"/home/kingfs", "", ".bashrc"}, "/home/kingfs/.bashrc"},
		{"linux", []string{"/a/b", "..", "c"}, "/a/c"},
		{"windows", []string{"/home/kingfs", "Documents"}, `\home\kingfs\Documents`},
		{"linux", []string{"/x/cfg"}, "/x/cfg"},
		{"linux", nil, ""},
	}
	for _, tc := range cases {
		if got := JoinFor(tc.goos, tc.elem...); got != tc.want {
			t.Errorf("JoinFor(%q, %v) = %q, want %q", tc.goos, tc.elem, got, tc.want)
		}
	}
}
