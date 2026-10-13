package hostinfo

// Supported 报告当前平台是否在官方支持矩阵内。
//
// 支持矩阵（docs/design.md §4.1）：linux/darwin/windows × amd64/arm64。
// 其它组合（如 linux/386、freebsd）仍可编译运行，但工具清单与产物不保证。
func (p Platform) Supported() bool {
	switch p.OS {
	case "linux", "darwin", "windows":
	default:
		return false
	}
	switch normalizeArch(p.Arch) {
	case "amd64", "arm64":
		return true
	}
	return false
}

// Label 返回 "os/arch" 形式的短标签。
func (p Platform) Label() string { return p.OS + "/" + p.Arch }
