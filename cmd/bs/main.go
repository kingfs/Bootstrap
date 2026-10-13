// Command bs 是 Bootstrap 工具链的 CLI 入口。
//
// 这里只做一件事：把控制权交给 internal/cli 并把退出码返回给操作系统。
package main

import (
	"os"

	"github.com/kingfs/Bootstrap/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
