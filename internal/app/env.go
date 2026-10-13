package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kingfs/Bootstrap/pkg/proxyenv"
)

// EnvReport 是 `bs env show` 的结构化结果。
type EnvReport struct {
	PathEntries []PathEntry  `json:"path_entries"`
	Proxy       ProxySummary `json:"proxy"`
}

// PathEntry 是一条将被加入 PATH 的目录及其来源。
type PathEntry struct {
	Path   string `json:"path"`
	Source string `json:"source"`
	Exists bool   `json:"exists"`
}

// PathEntries 汇总所有需要加入 PATH 的目录。
//
// M1 来源：配置 tool.extra_path。M2 起会追加"已安装工具声明的 provides 目录"。
func (a *App) PathEntries() []PathEntry {
	var out []PathEntry
	seen := map[string]bool{}
	add := func(p, source string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, PathEntry{Path: p, Source: source, Exists: dirExists(p)})
	}
	for _, p := range a.Cfg.Tool.ExtraPath {
		add(p, "tool.extra_path")
	}
	return out
}

// RenderPath 渲染把 PathEntries 前置到 PATH 的 shell 语句。
//
// 只包含当前 PATH 里还没有的目录，因此在 shell 里反复 eval 是幂等的。
func (a *App) RenderPath(shell proxyenv.Shell) (string, error) {
	current := map[string]bool{}
	for _, p := range filepath.SplitList(getenv("PATH")) {
		current[strings.TrimRight(p, "/")] = true
	}
	var add []string
	for _, e := range a.PathEntries() {
		if current[strings.TrimRight(e.Path, "/")] {
			continue
		}
		add = append(add, e.Path)
	}
	if len(add) == 0 {
		return "", nil
	}

	switch shell {
	case proxyenv.ShellBash, proxyenv.ShellZsh:
		return "export PATH=" + quotePOSIX(strings.Join(add, ":")) + `:"$PATH"`, nil
	case proxyenv.ShellFish:
		quoted := make([]string, 0, len(add))
		for _, p := range add {
			quoted = append(quoted, quotePOSIX(p))
		}
		return "set -gx PATH " + strings.Join(quoted, " ") + " $PATH", nil
	case proxyenv.ShellPwsh:
		return `$env:PATH = ` + quotePOSIX(strings.Join(add, ";")) + ` + ";" + $env:PATH`, nil
	case proxyenv.ShellCmd:
		return "set PATH=" + strings.Join(add, ";") + ";%PATH%", nil
	case proxyenv.ShellDotenv:
		return "PATH=" + strings.Join(add, ":") + ":" + getenv("PATH"), nil
	default:
		return "", fmt.Errorf("unsupported shell %q for PATH rendering", shell)
	}
}

// ShowEnv 返回 env 报告。
func (a *App) ShowEnv() (EnvReport, error) {
	proxy, err := a.ProxyStatus()
	if err != nil {
		return EnvReport{}, err
	}
	return EnvReport{PathEntries: a.PathEntries(), Proxy: proxy}, nil
}

func quotePOSIX(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
