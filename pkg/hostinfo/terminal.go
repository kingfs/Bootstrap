package hostinfo

import "os"

// IsTerminal 报告给定的读/写目标是不是交互式终端。
//
// 只依赖标准库：拿不到 *os.File、或它不是字符设备（管道、文件、bytes.Buffer），
// 就认为不是终端。**保守判断是刻意的**——宁可少一次花哨界面，
// 也不要让命令在 CI/管道里阻塞等待输入。
func IsTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
