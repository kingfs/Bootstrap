package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// makeTarGz 构造一个内存里的 tar.gz，便于测试各种危险条目。
func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typ, Linkname: e.link}
		if e.typ == tar.TypeReg || e.typ == 0 {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	name string
	body string
	mode int64
	typ  byte
	link string
}

func TestExtractTarWithStrip(t *testing.T) {
	data := makeTarGz(t, []tarEntry{
		{name: "go/bin/go", body: "#!/bin/sh\n", mode: 0o755},
		{name: "go/LICENSE", body: "BSD", mode: 0o644},
		{name: "go/", typ: tar.TypeDir, mode: 0o755},
	})
	dest := t.TempDir()
	files, err := ExtractTar(bytes.NewReader(decompress(t, data)), dest, 1)
	if err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	bin := filepath.Join(dest, "bin", "go")
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read extracted binary: %v", err)
	}
	if string(body) != "#!/bin/sh\n" {
		t.Errorf("content = %q", body)
	}
	// Windows 没有 POSIX 权限位：解压出来的文件 Mode().Perm() 读不出 tar 里的 0755，
	// "可执行"在产品里由 fsx.IsExecutable 表达为"普通文件即可"，这条断言不适用。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(bin)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Error("executable bit lost")
		}
	}
	if _, ok := FindBinary(files, "go"); !ok {
		t.Error("FindBinary should find the extracted binary")
	}
	if _, ok := FindBinary(files, "nope"); ok {
		t.Error("FindBinary must not invent a file")
	}
}

// TestExtractRejectsTraversal 是安全断言：归档里的 ../ 不能写到目标目录之外。
func TestExtractRejectsTraversal(t *testing.T) {
	cases := []tarEntry{
		{name: "../escape.sh", body: "x", mode: 0o644},
		{name: "/abs/escape.sh", body: "x", mode: 0o644},
		{name: "a/../../escape.sh", body: "x", mode: 0o644},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.name, "/", "_"), func(t *testing.T) {
			data := makeTarGz(t, []tarEntry{c})
			parent := t.TempDir()
			dest := filepath.Join(parent, "out")
			if _, err := ExtractTar(bytes.NewReader(decompress(t, data)), dest, 0); err == nil {
				t.Fatalf("traversal entry %q must be rejected", c.name)
			}
			if _, err := os.Stat(filepath.Join(parent, "escape.sh")); !os.IsNotExist(err) {
				t.Error("file escaped the destination directory")
			}
		})
	}
}

func TestExtractRejectsEvilSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 创建符号链接需要管理员权限或开发者模式，解压时 os.Symlink 会直接失败，
		// 于是"拒绝指向外部的符号链接"这条安全断言根本没走到，fixture 无法构造。
		t.Skip("Windows 创建符号链接需要特权，fixture 无法构造")
	}
	data := makeTarGz(t, []tarEntry{{name: "link", typ: tar.TypeSymlink, link: "/etc/passwd", mode: 0o777}})
	dest := t.TempDir()
	if _, err := ExtractTar(bytes.NewReader(decompress(t, data)), dest, 0); err == nil {
		t.Fatal("symlink pointing outside must be rejected")
	}
}

func TestExtractZipAndDetect(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "a.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("pkg/bin/tool")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write([]byte("binary")); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	f.Close()

	out := filepath.Join(dir, "out")
	files, err := Extract(zipPath, out, 1)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	if _, err := os.Stat(filepath.Join(out, "bin", "tool")); err != nil {
		t.Errorf("extracted file missing: %v", err)
	}
	if _, err := Extract(filepath.Join(dir, "x.rar"), out, 0); err == nil {
		t.Error("unknown archive type must fail")
	}
	if _, err := Extract(filepath.Join(dir, "x.tar.bz2"), out, 0); err == nil {
		t.Error("unsupported compression must fail rather than silently doing nothing")
	}
}

func TestStripComponents(t *testing.T) {
	tests := []struct {
		in    string
		strip int
		want  string
	}{
		{"a/b/c", 0, "a/b/c"},
		{"a/b/c", 1, "b/c"},
		{"a/b/c", 3, ""},
		{"./a/b", 1, "b"},
		{"a", 1, ""},
	}
	for _, tc := range tests {
		if got := stripComponents(tc.in, tc.strip); got != tc.want {
			t.Errorf("stripComponents(%q, %d) = %q, want %q", tc.in, tc.strip, got, tc.want)
		}
	}
}

func decompress(t *testing.T, data []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gz.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(gz); err != nil {
		t.Fatalf("read: %v", err)
	}
	return out.Bytes()
}
