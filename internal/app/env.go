package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kingfs/Bootstrap/pkg/fsx"
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
// 来源（按优先级）：
//  1. 配置 tool.extra_path（用户显式指定）
//  2. 清单里工具声明的 provides（每个文件取所在目录），例如 release 安装到 ~/.local/bin
//
// 这里**只做路径计算**，不探测工具是否已安装：PATH 装配要足够快且无副作用
// （每次开 shell 都会 eval `bs env path`）。
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
		add(fsx.ExpandPath(p, a.Paths.HomeDir), "tool.extra_path")
	}
	loaded, err := a.LoadedManifest()
	if err != nil {
		a.Log.Debug("load manifest for PATH", "err", err)
		return out
	}
	for _, t := range loaded.Tools {
		if t.Disabled {
			continue
		}
		for _, provide := range t.Provides {
			p := fsx.ExpandPath(provide, a.Paths.HomeDir)
			if p == "" {
				continue
			}
			// provides 可能写成具体文件（~/.local/bin/g）或目录本身。
			if filepath.Ext(p) != "" && !dirExists(p) {
				p = filepath.Dir(p)
			}
			add(p, "tool.provides("+t.ID+")")
		}
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
