// Package tools 内嵌内置工具清单与 shell 片段。
//
// 清单与片段都随二进制发布（ADR-0002 / ADR-0012）：换机器无需同步任何额外文件，
// 升级 bs 即自动带上最新的清单与片段。
package tools

import "embed"

// Snippets 是内置 shell 片段，按 shell 家族分目录：
//
//	snippets/posix/*.sh    → bash / zsh
//	snippets/fish/*.fish   → fish
//	snippets/pwsh/*.ps1    → PowerShell
//
//go:embed snippets
var Snippets embed.FS

// Manifests 是内置工具清单（M2 起填充具体工具）。
//
//go:embed common.yaml linux.yaml darwin.yaml windows.yaml profiles.yaml
var Manifests embed.FS
