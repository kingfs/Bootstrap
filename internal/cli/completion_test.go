package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestCompletionHelpAndScript 钉住两件事：
//  1. 帮助里必须说清"怎么用"和"怎么自动装"（用户反馈的原始诉求）；
//  2. `bs completion <shell>` 仍然只打印脚本——不能因为新增 install 就改掉这个契约
//     （`> file`、包管理器、现有冒烟断言都依赖它）。
func TestCompletionHelpAndScript(t *testing.T) {
	rt, out, _, _ := sandbox(t)

	code, _, _ := run(t, rt, "completion", "--help")
	if code != ExitOK {
		t.Fatalf("completion --help exit = %d", code)
	}
	help := out.String()
	for _, want := range []string{"bs completion install", "shell.completion", "bash-completion"} {
		if !strings.Contains(help, want) {
			t.Errorf("completion --help 缺少 %q：\n%s", want, help)
		}
	}

	code, _, _ = run(t, rt, "completion", "bash", "--help")
	if code != ExitOK {
		t.Fatalf("completion bash --help exit = %d", code)
	}
	if !strings.Contains(out.String(), "bs completion install <shell>") {
		t.Errorf("子命令帮助里没有指向 install 的提示：\n%s", out.String())
	}

	code, out2, _ := run(t, rt, "completion", "bash")
	if code != ExitOK {
		t.Fatalf("completion bash exit = %d", code)
	}
	if !strings.Contains(out2, "bash completion V2") {
		t.Errorf("completion bash 应当原样打印 cobra 的脚本：\n%s", out2)
	}
}

// TestCompletionInstallThroughCLI 走完整命令行路径：dry-run 不落盘、真装改两处、幂等。
func TestCompletionInstallThroughCLI(t *testing.T) {
	rt, out, _, root := sandbox(t)
	rc := filepath.Join(root, "home", ".bashrc")
	if err := os.MkdirAll(filepath.Dir(rc), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	original := "# 我自己的配置\nexport EDITOR=vim\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfgPath := filepath.Join(root, "config", "config.yaml")

	// dry-run：什么都不该落盘。
	code, _, _ := run(t, rt, "--dry-run", "completion", "install", "bash", "--rc", rc)
	if code != ExitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	if !strings.Contains(out.String(), "[dry-run]") {
		t.Errorf("dry-run 应当明确标注：\n%s", out.String())
	}
	if got := readFileString(t, rc); got != original {
		t.Errorf("dry-run 改了 rc：\n%q", got)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run 创建了配置文件（%v）", err)
	}

	// 真装：配置开关 + rc 受管块。
	code, outStr, errStr := run(t, rt, "completion", "install", "bash", "--rc", rc)
	if code != ExitOK {
		t.Fatalf("install exit = %d, out = %s", code, outStr)
	}
	if !strings.Contains(outStr, "shell.completion") || !strings.Contains(outStr, "受管块") {
		t.Errorf("install 输出没说清改了什么：\n%s", outStr)
	}
	if !strings.Contains(errStr, "exec $SHELL -l") {
		t.Errorf("install 应当提示怎么立即生效：\n%s", errStr)
	}
	if cfg := readFileString(t, cfgPath); !strings.Contains(cfg, "completion: true") {
		t.Errorf("配置里没打开开关：\n%s", cfg)
	}
	installed := readFileString(t, rc)
	if !strings.Contains(installed, ">>> bootstrap") || !strings.Contains(installed, "export EDITOR=vim") {
		t.Errorf("rc 受管块没装好或破坏了原有内容：\n%s", installed)
	}

	// 幂等：第二次两处都不再改。
	code, outStr, _ = run(t, rt, "completion", "install", "bash", "--rc", rc)
	if code != ExitOK {
		t.Fatalf("second install exit = %d", code)
	}
	if !strings.Contains(outStr, "已是 true") || !strings.Contains(outStr, "已是最新") {
		t.Errorf("第二次应当是 no-op：\n%s", outStr)
	}
}

// TestCompletionInstallJSON 断言 --json 下输出可被脚本消费，且包含两处落点。
func TestCompletionInstallJSON(t *testing.T) {
	rt, out, _, root := sandbox(t)
	rc := filepath.Join(root, "home", ".bashrc")

	code, _, _ := run(t, rt, "completion", "install", "--shell", "zsh", "--rc", rc, "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("--json 输出不是合法 JSON: %v\n%s", err, out.String())
	}
	if res["shell"] != "zsh" || res["rc_file"] != rc {
		t.Errorf("json = %v", res)
	}
	if res["config_set"] != true || res["rc_changed"] != true {
		t.Errorf("json 应当汇报两处都改了：%v", res)
	}
	// zsh 的 rc 无需提前存在；受管块仍应写入指定路径。
	if !strings.Contains(readFileString(t, rc), "zsh") {
		t.Errorf("zsh 受管块没写对：\n%s", readFileString(t, rc))
	}
}

// TestCompletionCommandsSurviveBrokenConfig：这几个命令会出现在每个新 shell 的启动路径上
// （shell.completion=true 时由受管块 eval），配置写坏一次不能让登录/补全跟着坏。
func TestCompletionCommandsSurviveBrokenConfig(t *testing.T) {
	rt, _, _, root := sandbox(t)
	writeConfig(t, root, "version: [broken\n")

	code, outStr, _ := run(t, rt, "completion", "bash")
	if code != ExitOK {
		t.Fatalf("completion bash exit = %d（配置坏了也必须能出脚本）", code)
	}
	if !strings.Contains(outStr, "bash completion V2") {
		t.Errorf("输出不像补全脚本：\n%.200s", outStr)
	}

	// cobra 的 Tab 补全每次都调 `__complete`：它拿不到命令注解，只能按名字识别，
	// 必须同样退化为默认配置而不是退出码 4。
	code, outStr, _ = run(t, rt, cobra.ShellCompRequestCmd, "")
	if code != ExitOK {
		t.Fatalf("__complete exit = %d", code)
	}
	if !strings.Contains(outStr, "completion") {
		t.Errorf("补全候选里没有 completion：\n%s", outStr)
	}
}
