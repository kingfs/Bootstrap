package shellsnip

import (
	"fmt"
	"path/filepath"
	"strings"
)

// 受管块标记。只识别这两个标记之间的内容，块外字节级不动（docs/design.md §16）。
const (
	BeginMarker = "# >>> bootstrap (managed by 'bs shell install') >>>"
	EndMarker   = "# <<< bootstrap <<<"
)

// SourceLine 返回受管块中真正生效的那一行。
func SourceLine(shell string) (string, error) {
	family, err := FamilyForShell(shell)
	if err != nil {
		return "", err
	}
	switch family {
	case FamilyPosix:
		return fmt.Sprintf("eval \"$(bs shell init %s)\"", normalizeShellName(shell)), nil
	case FamilyFish:
		return "bs shell init fish | source", nil
	case FamilyPwsh:
		return "bs shell init pwsh | Out-String | Invoke-Expression", nil
	default:
		return "", fmt.Errorf("unsupported shell family %q", family)
	}
}

// Block 返回完整的受管块文本。
func Block(shell string) (string, error) {
	line, err := SourceLine(shell)
	if err != nil {
		return "", err
	}
	return BeginMarker + "\n" + line + "\n" + EndMarker, nil
}

// HasBlock 报告内容里是否已存在受管块。
func HasBlock(content string) bool {
	return strings.Contains(content, BeginMarker) && strings.Contains(content, EndMarker)
}

// Upsert 幂等地写入受管块：已存在则原地替换（升级即生效），否则追加到文件末尾。
//
// 返回新内容与是否发生变化；内容完全相同时 changed=false，调用方据此避免无谓写盘。
func Upsert(existing, block string) (string, bool) {
	begin := strings.Index(existing, BeginMarker)
	if begin >= 0 {
		if rel := strings.Index(existing[begin:], EndMarker); rel >= 0 {
			endAbs := begin + rel + len(EndMarker)
			if existing[begin:endAbs] == block {
				return existing, false
			}
			return existing[:begin] + block + existing[endAbs:], true
		}
	}

	out := existing
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if out != "" {
		out += "\n"
	}
	out += block + "\n"
	return out, true
}

// Remove 精确移除受管块及其周围的空行。
func Remove(existing string) (string, bool) {
	begin := strings.Index(existing, BeginMarker)
	if begin < 0 {
		return existing, false
	}
	rel := strings.Index(existing[begin:], EndMarker)
	if rel < 0 {
		return existing, false
	}
	endAbs := begin + rel + len(EndMarker)
	for endAbs < len(existing) && existing[endAbs] == '\n' {
		endAbs++
	}
	start := begin
	if start >= 2 && existing[start-1] == '\n' && existing[start-2] == '\n' {
		start--
	}
	return existing[:start] + existing[endAbs:], true
}

// DetectRCFile 探测某个 shell 的 rc 文件路径。
//
// 传入 exists 便于测试；为 nil 时使用真实文件系统。
func DetectRCFile(goos, shell, home string, getenv func(string) string, exists func(string) bool) (string, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if exists == nil {
		exists = func(string) bool { return false }
	}
	family, err := FamilyForShell(shell)
	if err != nil {
		return "", err
	}

	pick := func(candidates ...string) string {
		for _, c := range candidates {
			if exists(c) {
				return c
			}
		}
		if len(candidates) > 0 {
			return candidates[0]
		}
		return ""
	}

	switch family {
	case FamilyPosix:
		if normalizeShellName(shell) == "zsh" {
			zdotdir := getenv("ZDOTDIR")
			if zdotdir == "" {
				zdotdir = home
			}
			return filepath.Join(zdotdir, ".zshrc"), nil
		}
		if goos == "darwin" {
			return pick(
				filepath.Join(home, ".bash_profile"),
				filepath.Join(home, ".bashrc"),
				filepath.Join(home, ".profile"),
			), nil
		}
		return pick(
			filepath.Join(home, ".bashrc"),
			filepath.Join(home, ".bash_profile"),
			filepath.Join(home, ".profile"),
		), nil
	case FamilyFish:
		base := getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
		return filepath.Join(base, "fish", "conf.d", "bootstrap.fish"), nil
	case FamilyPwsh:
		if goos == "windows" {
			docs := filepath.Join(home, "Documents")
			return filepath.Join(docs, "PowerShell", "Microsoft.PowerShell_profile.ps1"), nil
		}
		base := getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
		return filepath.Join(base, "powershell", "Microsoft.PowerShell_profile.ps1"), nil
	default:
		return "", fmt.Errorf("unsupported shell %q", shell)
	}
}

func normalizeShellName(shell string) string {
	s := strings.ToLower(strings.TrimSpace(shell))
	switch s {
	case "sh", "dash", "ksh":
		return "bash" // POSIX 兼容，统一用 bash 的加载语法
	case "powershell", "powershell.exe":
		return "pwsh"
	case "cmd.exe", "batch", "bat":
		return "cmd"
	default:
		return s
	}
}
