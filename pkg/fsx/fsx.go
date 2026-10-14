// Package fsx 提供与业务无关的文件系统小工具：路径展开、幂等写入、原子替换、存在性判断。
//
// 这些函数被 config / state / shellsnip / httpx 等多处共用，因此按设计文档 §4.1 的
// "pkg 准入三条"放在 pkg/ 下：纯逻辑、可独立复用、不含任何业务词汇。
package fsx

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// HomeDir 返回当前用户家目录；失败时返回空串（调用方通常有兜底逻辑）。
func HomeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if h := os.Getenv("USERPROFILE"); h != "" {
		return h
	}
	return ""
}

// JoinFor 按"目标平台"拼接路径元素。
//
// 需要它是因为本仓库会**在 A 平台上计算 B 平台的路径**：config.ResolvePathsWith(goos, …)
// 与 shellsnip.DetectRCFile(goos, …) 都接受一个可能与宿主不同的 goos（跨平台单测、
// `bs config path` 的交叉说明）。用宿主的 filepath.Join 会得到宿主的分隔符，于是
// "linux 的配置目录"在 Windows 上变成 `\home\kingfs\.config`——既不是 linux 的写法，
// 也让"在任何机器上跑同一套断言"变成不可能。
//
// windows 目标用反斜杠，其余目标用斜杠；清理（`.`/`..`）按斜杠语义做完再换分隔符，
// 因此结果与宿主无关。
func JoinFor(goos string, elem ...string) string {
	parts := make([]string, 0, len(elem))
	for _, e := range elem {
		if e != "" {
			parts = append(parts, e)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	clean := path.Clean(strings.Join(parts, "/"))
	if goos == "windows" {
		return strings.ReplaceAll(clean, "/", `\`)
	}
	return clean
}

// ExpandPath 展开路径中的 ~、~user 形式与环境变量引用。
//
// home 显式传入以便测试；为空时回退到 HomeDir()。
func ExpandPath(path, home string) string {
	if path == "" {
		return ""
	}
	if home == "" {
		home = HomeDir()
	}
	p := os.ExpandEnv(path)
	switch {
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case strings.HasPrefix(p, `~\`):
		p = filepath.Join(home, p[2:])
	}
	return p
}

// Exists 报告路径是否存在（不区分文件/目录）。
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsDir 报告路径是否存在且为目录。
func IsDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// IsFile 报告路径是否存在且为普通文件。
func IsFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// IsExecutable 报告路径是否是"可执行文件"。
//
// Windows 没有 POSIX 权限位，普通文件即视为可执行；其余平台要求可执行位。
// 用于判断"工具是否通过 release/source 方式装到了某个 provides 路径上"。
func IsExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

// EnsureDir 创建目录（含父目录），已存在时是 no-op。
func EnsureDir(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return fmt.Errorf("create directory %s: %w", path, err)
	}
	return nil
}

// AtomicWriteFile 以"临时文件 + rename"的方式写入文件，保证不会出现半截内容。
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := EnsureDir(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// 成功路径下 rename 已让 tmpName 消失，这里的清理是失败路径的兜底。
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// WriteFileIfChanged 仅在内容变化时写文件，返回是否发生了写入。
//
// 用于 state/受管块这类"重复执行应当无副作用"的场景：内容相同则不触碰 mtime。
func WriteFileIfChanged(path string, data []byte, perm os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return false, nil
	}
	if err := AtomicWriteFile(path, data, perm); err != nil {
		return false, err
	}
	return true, nil
}

// BackupFile 把文件复制一份带时间戳的备份，返回备份路径。
func BackupFile(path, stamp string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	backup := fmt.Sprintf("%s.bs-backup.%s", path, stamp)
	if err := AtomicWriteFile(backup, data, 0o644); err != nil {
		return "", err
	}
	return backup, nil
}
