package install

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kingfs/Bootstrap/pkg/fsx"
)

// copyExecutable 把 source 复制成 target（保留可执行位），并保证原子性。
//
// 用"写临时文件 + rename"而不是直接覆盖：升级过程中被中断时，
// 用户手里仍然是旧版本这个可用文件，而不是半个新文件（§7.6 幂等/可恢复）。
func copyExecutable(source, target string) error {
	if err := fsx.EnsureDir(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s: %w", source, err)
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(target), ".bs-install-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", filepath.Dir(target), err)
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("copy %s: %w", source, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("move %s into place: %w", target, err)
	}
	return nil
}

// replaceSymlink 让 to 指向 from（先删旧的，避免 EEXIST）。
func replaceSymlink(from, to string) error {
	if from == "" || to == "" {
		return nil
	}
	if !fsx.Exists(from) {
		return fmt.Errorf("symlink source %s does not exist", from)
	}
	if err := fsx.EnsureDir(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(to); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			// 不覆盖用户自己的真实文件（安全红线 §8.1）。
			return fmt.Errorf("refusing to replace %s: it is not a symlink", to)
		}
		if err := os.Remove(to); err != nil {
			return fmt.Errorf("remove old symlink %s: %w", to, err)
		}
	}
	if err := os.Symlink(from, to); err != nil {
		return fmt.Errorf("symlink %s → %s: %w", to, from, err)
	}
	return nil
}
