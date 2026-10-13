// Package archive 提供 tar.gz / zip 的安全解压。
//
// 与业务无关，因此放在 pkg/：它只关心"把归档解开"，
// 并且**必须**拒绝路径穿越（zip slip）条目——这是 release 安装路径上最容易出问题的地方。
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// maxFileSize 限制单个解出的文件大小（防止把磁盘写满）。
const maxFileSize = 2 << 30

// Extract 按扩展名自动选择解压方式。
//
// strip 表示去掉路径最前面的 N 层目录（release 归档常见的
// `go1.24.1.linux-amd64/bin/go` → strip=1 得到 `bin/go`）。
func Extract(archivePath, dest string, strip int) ([]string, error) {
	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return ExtractTarGzFile(archivePath, dest, strip)
	case strings.HasSuffix(lower, ".tar.bz2"), strings.HasSuffix(lower, ".tbz2"):
		return nil, fmt.Errorf("unsupported archive type: %s (bzip2 not implemented)", filepath.Base(archivePath))
	case strings.HasSuffix(lower, ".zip"):
		return ExtractZipFile(archivePath, dest, strip)
	case strings.HasSuffix(lower, ".tar"), strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"):
		return nil, fmt.Errorf("unsupported archive type: %s", filepath.Base(archivePath))
	}
	return nil, fmt.Errorf("unknown archive type: %s", filepath.Base(archivePath))
}

// ExtractTarGzFile 解压 .tar.gz 文件。
func ExtractTarGzFile(path, dest string, strip int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip %s: %w", path, err)
	}
	defer gz.Close()
	return ExtractTar(gz, dest, strip)
}

// ExtractTar 从流里解压 tar。
func ExtractTar(r io.Reader, dest string, strip int) ([]string, error) {
	if err := fsx.EnsureDir(dest, 0o755); err != nil {
		return nil, err
	}
	tr := tar.NewReader(r)
	var written []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return written, fmt.Errorf("read tar: %w", err)
		}
		name := stripComponents(hdr.Name, strip)
		if name == "" {
			continue
		}
		target, err := safeJoin(dest, name)
		if err != nil {
			return written, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := fsx.EnsureDir(target, 0o755); err != nil {
				return written, err
			}
		case tar.TypeReg:
			if hdr.Size > maxFileSize {
				return written, fmt.Errorf("refusing to extract %s: %d bytes exceeds limit", name, hdr.Size)
			}
			if err := fsx.EnsureDir(filepath.Dir(target), 0o755); err != nil {
				return written, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return written, fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(out, io.LimitReader(tr, maxFileSize)); err != nil {
				out.Close()
				return written, fmt.Errorf("write %s: %w", target, err)
			}
			if err := out.Close(); err != nil {
				return written, err
			}
			written = append(written, target)
		case tar.TypeSymlink:
			// 符号链接只允许指向归档内部，避免解压出指向 /etc 的链接。
			if filepath.IsAbs(hdr.Linkname) || strings.HasPrefix(filepath.Clean(hdr.Linkname), "..") {
				return written, fmt.Errorf("refusing to extract symlink %s → %s", name, hdr.Linkname)
			}
			if err := fsx.EnsureDir(filepath.Dir(target), 0o755); err != nil {
				return written, err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return written, fmt.Errorf("symlink %s: %w", target, err)
			}
			written = append(written, target)
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		default:
			continue
		}
	}
	return written, nil
}

// ExtractZipFile 解压 .zip 文件。
func ExtractZipFile(path, dest string, strip int) ([]string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open zip %s: %w", path, err)
	}
	defer zr.Close()

	if err := fsx.EnsureDir(dest, 0o755); err != nil {
		return nil, err
	}
	var written []string
	for _, f := range zr.File {
		name := stripComponents(f.Name, strip)
		if name == "" {
			continue
		}
		target, err := safeJoin(dest, name)
		if err != nil {
			return written, err
		}
		if f.FileInfo().IsDir() {
			if err := fsx.EnsureDir(target, 0o755); err != nil {
				return written, err
			}
			continue
		}
		if f.UncompressedSize64 > maxFileSize {
			return written, fmt.Errorf("refusing to extract %s: %d bytes exceeds limit", name, f.UncompressedSize64)
		}
		if err := fsx.EnsureDir(filepath.Dir(target), 0o755); err != nil {
			return written, err
		}
		rc, err := f.Open()
		if err != nil {
			return written, fmt.Errorf("open %s in zip: %w", f.Name, err)
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			rc.Close()
			return written, fmt.Errorf("create %s: %w", target, err)
		}
		if _, err := io.Copy(out, io.LimitReader(rc, maxFileSize)); err != nil {
			rc.Close()
			out.Close()
			return written, fmt.Errorf("write %s: %w", target, err)
		}
		rc.Close()
		if err := out.Close(); err != nil {
			return written, err
		}
		written = append(written, target)
	}
	return written, nil
}

// stripComponents 去掉最前面的 n 层路径；结果为空表示该条目应被跳过。
func stripComponents(name string, n int) string {
	name = filepath.ToSlash(name)
	name = strings.TrimPrefix(name, "./")
	if n <= 0 {
		return name
	}
	parts := strings.Split(name, "/")
	if len(parts) <= n {
		return ""
	}
	return strings.Join(parts[n:], "/")
}

// safeJoin 把归档内的相对路径拼到 dest 下，并拒绝任何越界。
func safeJoin(dest, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("refusing absolute path in archive: %s", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing path traversal in archive: %s", name)
	}
	target := filepath.Join(dest, clean)
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing path outside destination: %s", name)
	}
	return target, nil
}

// FindBinary 在解压结果里找目标可执行文件。
//
// 先按"文件名精确匹配"找，再退化为"任意 bin/ 目录下的同名文件"，
// 因为不同项目的归档布局差异很大（顶层 ./g、./bin/g、./g_1.2.3_linux_amd64/g）。
func FindBinary(files []string, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	base := filepath.Base(name)
	for _, f := range files {
		if filepath.Base(f) == base && fsx.IsFile(f) {
			return f, true
		}
	}
	for _, f := range files {
		if filepath.Base(f) != base || !fsx.IsFile(f) {
			continue
		}
		return f, true
	}
	return "", false
}
