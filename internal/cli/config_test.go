package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandbox2 在沙箱里先生成一份默认配置（`config set` 会读它、写它）。
func sandbox2(t *testing.T) (*Runtime, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	rt, out, errBuf, root := sandbox(t)
	code, _, stderr := run(t, rt, "config", "init")
	if code != ExitOK {
		t.Fatalf("config init: exit = %d（%s）", code, stderr)
	}
	return rt, out, errBuf, root
}

// TestConfigSetRejectsUnknownKeyAsUsageError：键名写错是"参数非法"，
// 按 README 的退出码表应当是 2（而不是含糊的 1），并且一个字都不能写进文件。
func TestConfigSetRejectsUnknownKeyAsUsageError(t *testing.T) {
	rt, _, _, root := sandbox2(t)
	code, _, stderr := run(t, rt, "config", "set", "tool.nosuchkey", "42")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d（stderr=%s）", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "tool.nosuchkey") {
		t.Errorf("错误里要点出写错的键：%s", stderr)
	}
	path := filepath.Join(root, "config", "config.yaml")
	if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "nosuchkey") {
		t.Errorf("拒绝写入时不该留下这个键：\n%s", data)
	}
}

// TestConfigSetAcceptsValidKeysThroughCLI：正常键与"开放映射"下的子键都要能写，
// 别把修错的补丁变成新的坑。
func TestConfigSetAcceptsValidKeysThroughCLI(t *testing.T) {
	rt, _, _, _ := sandbox2(t)
	for _, args := range [][]string{
		{"config", "set", "log.level", "debug"},
		{"config", "set", "proxy.profiles.office.http_proxy", "http://127.0.0.1:7890"},
	} {
		code, stdout, stderr := run(t, rt, args...)
		if code != ExitOK {
			t.Errorf("%v exit = %d\nstdout=%s\nstderr=%s", args, code, stdout, stderr)
		}
	}
}
