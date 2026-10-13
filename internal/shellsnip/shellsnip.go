// Package shellsnip 负责 shell 片段的装载/合并，以及 rc 文件中"受管块"的幂等维护。
//
// 片段来源（ADR-0012）：
//  1. 内置片段（tools/snippets/<family>/，随二进制发布）—— 主要载体
//  2. 用户片段（<config>/shell/<family>/）—— 机器特定/临时覆盖，同名整体替换内置
//
// 受管块（ADR-0007）：rc 里只有一行 `eval "$(bs shell init <shell>)"`，
// 由固定标记包围，可幂等替换、可精确移除，块外内容字节级不动。
package shellsnip

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Family 是 shell 家族，决定使用哪一套片段与语法。
type Family string

// 支持的 shell 家族。
const (
	FamilyPosix Family = "posix" // bash / zsh / sh
	FamilyFish  Family = "fish"
	FamilyPwsh  Family = "pwsh"
)

// FamilyForShell 把 shell 名映射到家族。
func FamilyForShell(shell string) (Family, error) {
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "bash", "zsh", "sh", "dash", "ksh":
		return FamilyPosix, nil
	case "fish":
		return FamilyFish, nil
	case "pwsh", "powershell", "powershell.exe":
		return FamilyPwsh, nil
	case "cmd", "cmd.exe", "batch", "bat":
		return "", fmt.Errorf("cmd.exe 没有 rc 文件，无法使用受管块；请改用 PowerShell：bs shell init pwsh")
	default:
		return "", fmt.Errorf("unsupported shell %q (want bash|zsh|fish|pwsh)", shell)
	}
}

// Snippet 是一段 shell 片段。
type Snippet struct {
	Name    string // 文件名，前缀数字决定加载顺序
	Family  Family
	Builtin bool
	Source  string // 内置为 "builtin"，用户片段为实际文件路径
	Content string
}

// Set 是某个 shell 家族合并后的片段集合（已排序）。
type Set struct {
	Family   Family
	Snippets []Snippet
}

// Load 合并内置片段与用户片段。
//
// builtinFS 是 tools.Snippets（或测试用的内存 FS）；userDir 可为空。
func Load(builtinFS fs.FS, family Family, userDir string) (*Set, error) {
	set := &Set{Family: family}
	byName := map[string]Snippet{}
	var names []string

	add := func(s Snippet) {
		if _, exists := byName[s.Name]; !exists {
			names = append(names, s.Name)
		}
		byName[s.Name] = s
	}

	if builtinFS != nil {
		dir := path.Join("snippets", string(family))
		entries, err := fs.ReadDir(builtinFS, dir)
		if err != nil && !isNotExist(err) {
			return nil, fmt.Errorf("read builtin snippets %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, err := fs.ReadFile(builtinFS, path.Join(dir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("read builtin snippet %s: %w", e.Name(), err)
			}
			add(Snippet{Name: e.Name(), Family: family, Builtin: true, Source: "builtin", Content: string(data)})
		}
	}

	if userDir != "" {
		dir := filepath.Join(userDir, string(family))
		if entries, err := readDirIfExists(dir); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				full := filepath.Join(dir, e.Name())
				data, err := readFile(full)
				if err != nil {
					return nil, fmt.Errorf("read user snippet %s: %w", full, err)
				}
				add(Snippet{Name: e.Name(), Family: family, Builtin: false, Source: full, Content: string(data)})
			}
		} else {
			return nil, err
		}
	}

	sort.Strings(names)
	for _, n := range names {
		set.Snippets = append(set.Snippets, byName[n])
	}
	return set, nil
}

// Render 把片段拼接为可直接 eval 的文本；每段前加注释标明来源，便于排障。
func (s *Set) Render() string {
	var b strings.Builder
	for i, sn := range s.Snippets {
		if i > 0 {
			b.WriteString("\n")
		}
		origin := "builtin"
		if !sn.Builtin {
			origin = sn.Source
		}
		fmt.Fprintf(&b, "# ─── %s (%s) ───\n", sn.Name, origin)
		content := strings.TrimRight(sn.Content, "\n")
		if content != "" {
			b.WriteString(content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Names 返回片段文件名列表（按加载顺序）。
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.Snippets))
	for _, sn := range s.Snippets {
		out = append(out, sn.Name)
	}
	return out
}
