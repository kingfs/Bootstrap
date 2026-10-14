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

// IsPathSnippet 报告片段是否为 PATH 装配片段。
//
// 约定：`20-path.*` 是唯一的 PATH 装配片段（AGENTS.md §6），
// `shell.auto_path=false` 时由调用方跳过它。
func IsPathSnippet(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "20-path")
}

// AutoProxyContent 返回"启动时自动应用活跃代理 profile"的片段内容。
//
// 默认不开（`shell.auto_proxy=false`）：它会让 bs 的状态决定每个新 shell 的代理变量，
// 包括在 `bs proxy off` 之后把 shell 里已有的代理变量 unset 掉。开了就是明确选择这条语义。
func AutoProxyContent(family Family) (string, error) {
	switch family {
	case FamilyPosix:
		return "# 由 shell.auto_proxy=true 生成：启动时应用 bs 当前活跃的代理 profile\n" +
			"if command -v bs >/dev/null 2>&1; then\n" +
			"  eval \"$(command bs proxy env --apply --shell \"${BS_SHELL:-bash}\")\"\n" +
			"fi\n", nil
	case FamilyFish:
		return "# 由 shell.auto_proxy=true 生成：启动时应用 bs 当前活跃的代理 profile\n" +
			"if command -q bs\n" +
			"    command bs proxy env --apply --shell fish | source\n" +
			"end\n", nil
	case FamilyPwsh:
		return "# 由 shell.auto_proxy=true 生成：启动时应用 bs 当前活跃的代理 profile\n" +
			"$autoProxyExe = (Get-Command bs -CommandType Application -ErrorAction SilentlyContinue).Source\n" +
			"if ($autoProxyExe) {\n" +
			"    (& $autoProxyExe proxy env --apply --shell pwsh | Out-String) | Invoke-Expression\n" +
			"}\n", nil
	}
	return "", fmt.Errorf("unsupported shell family %q", family)
}

// CompletionContent 返回"加载 bs 补全脚本"的片段内容。
//
// 与 AutoProxyContent 一样，它是由配置开关（`shell.completion`）生成的片段，不放进
// tools/snippets：它必须区分**具体 shell**（bash 与 zsh 同属 posix 家族，补全脚本却不同），
// 而且默认关闭（见 config.ShellConfig.Completion 的注释）。
//
// 三种写法的共同前提是"这段代码每次开 shell 都会跑"：因此一律先探测 bs 在不在 PATH，
// 不在就静默跳过，绝不能让用户的 shell 报错。
func CompletionContent(family Family, shell string) (string, error) {
	name := normalizeShellName(shell)
	switch family {
	case FamilyPosix:
		if name == "zsh" {
			// cobra 的 zsh 补全脚本以 compdef 注册，而 compdef 由 compinit 提供。受管块
			// 通常追加在 rc 末尾，用户自己的 compinit 早已跑过；裸 zsh 在这里补一次默认
			// 初始化，否则每次开 shell 都会报 `compdef: command not found`。
			// -C 只跳过"不安全目录"的完整校验，避免把这笔开销加到每次启动上。
			return "# 由 shell.completion=true 生成：加载 bs 补全（zsh）\n" +
				"if command -v bs >/dev/null 2>&1; then\n" +
				"  if ! typeset -f compdef >/dev/null 2>&1; then\n" +
				"    autoload -Uz compinit && compinit -C\n" +
				"  fi\n" +
				"  eval \"$(command bs completion zsh)\"\n" +
				"fi\n", nil
		}
		// bash 分支要挡住 dash/ksh：FamilyForShell 把它们都归到 posix，但 cobra 的
		// bash 补全脚本依赖 complete/compgen，在 dash 下执行只会报错。
		// 注意脚本运行时还会用 bash-completion 包的 _get_comp_words_by_ref，
		// 所以"光有脚本"不代表能用——`bs completion install` 会就此给出提示。
		return "# 由 shell.completion=true 生成：加载 bs 补全（bash）\n" +
			"if [ -n \"${BASH_VERSION:-}\" ] && command -v bs >/dev/null 2>&1; then\n" +
			"  eval \"$(command bs completion bash)\"\n" +
			"fi\n", nil
	case FamilyFish:
		return "# 由 shell.completion=true 生成：加载 bs 补全（fish）\n" +
			"if command -q bs\n" +
			"    command bs completion fish | source\n" +
			"end\n", nil
	case FamilyPwsh:
		return "# 由 shell.completion=true 生成：加载 bs 补全（pwsh）\n" +
			"$completionExe = (Get-Command bs -CommandType Application -ErrorAction SilentlyContinue).Source\n" +
			"if ($completionExe) {\n" +
			"    (& $completionExe completion powershell | Out-String) | Invoke-Expression\n" +
			"}\n", nil
	}
	return "", fmt.Errorf("unsupported shell family %q", family)
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
