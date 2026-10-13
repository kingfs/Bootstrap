// Package buildinfo 保存构建期通过 -ldflags 注入的版本信息。
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// 这些变量由 goreleaser / Makefile 通过
// `-ldflags "-X .../internal/buildinfo.Version=v1.0.0"` 注入。
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// Info 是版本信息的结构化表示（供 --json 输出）。
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get 返回当前二进制信息，缺失字段会尝试从 debug.ReadBuildInfo 补齐。
func Get() Info {
	commit, date := Commit, Date
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if commit == "none" && s.Value != "" {
					commit = s.Value
					if len(commit) > 12 {
						commit = commit[:12]
					}
				}
			case "vcs.time":
				if date == "unknown" && s.Value != "" {
					date = s.Value
				}
			}
		}
	}
	return Info{
		Version:   Version,
		Commit:    commit,
		Date:      date,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

// String 返回人类可读的一行版本信息。
func String() string {
	i := Get()
	return fmt.Sprintf("bs %s (commit %s, built %s, %s/%s, %s)",
		i.Version, i.Commit, i.Date, i.OS, i.Arch, i.GoVersion)
}

// UserAgent 用于 HTTP 请求（下载 release 资产等）。
func UserAgent() string {
	i := Get()
	return fmt.Sprintf("bootstrap/%s (%s/%s)", i.Version, i.OS, i.Arch)
}
