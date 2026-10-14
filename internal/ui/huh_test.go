package ui

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
)

// TestNewInteractiveRefusesNonTerminals 锁定"拿不准就别上花哨界面"：
// 管道、bytes.Buffer、CI 里必须退回纯文本，不能让 huh 去开 /dev/tty 然后卡住。
func TestNewInteractiveRefusesNonTerminals(t *testing.T) {
	var buf bytes.Buffer
	if p, ok := NewInteractive(&buf, &buf); ok || p != nil {
		t.Error("非 *os.File 的读写端不该启用交互式界面")
	}
	if p, ok := NewInteractive(bytes.NewReader(nil), os.Stderr); ok || p != nil {
		t.Error("输入不是终端时不该启用交互式界面")
	}
	// 真终端场景由 PTY 冒烟验证（见 docs/design.md §12），单测里不伪造 TTY。
}

// TestNewInteractiveRefusesDumbTerminal：TERM=dumb 时 bubbletea 画不出东西，
// 必须走纯文本（huh 自己也会在这种终端下切到 accessible 模式）。
func TestNewInteractiveRefusesDumbTerminal(t *testing.T) {
	// 这里的终端判断要先通过才谈得上 TERM；用一个真实存在的字符设备当替身不可靠，
	// 因此直接验证"TERM=dumb 时不启用"这一条（即便 in/out 不是终端也应拒绝）。
	t.Setenv("TERM", "dumb")
	if p, ok := NewInteractive(os.Stdin, os.Stderr); ok || p != nil {
		t.Error("TERM=dumb 时不该启用交互式界面")
	}
}

// TestMapHuhErrCancelsLikeText：huh 的"用户退出"必须映射成统一的 ErrCancelled，
// 这样命令层只认识一种取消语义（退出码 0 + "未做任何修改"）。
func TestMapHuhErrCancelsLikeText(t *testing.T) {
	if err := mapHuhErr(nil); err != nil {
		t.Errorf("nil 应当原样返回 nil，得到 %v", err)
	}
	if err := mapHuhErr(huh.ErrUserAborted); !errors.Is(err, ErrCancelled) {
		t.Errorf("ErrUserAborted 应映射成 ErrCancelled，得到 %v", err)
	}
	sentinel := errors.New("boom")
	if err := mapHuhErr(sentinel); !errors.Is(err, sentinel) {
		t.Errorf("其它错误应原样透传，得到 %v", err)
	}
}

// TestHuhPrompterIsInteractive：只要构造出来就代表"问得到用户"。
func TestHuhPrompterIsInteractive(t *testing.T) {
	var h Prompter = &huhPrompter{in: bytes.NewReader(nil), out: &bytes.Buffer{}}
	if !h.Interactive() {
		t.Error("huhPrompter 应当报告 Interactive=true")
	}
	var n Prompter = NonInteractive{}
	if n.Interactive() {
		t.Error("NonInteractive 应当报告 Interactive=false")
	}
}

// TestHuhPrompterAccessibleMapping 用 huh 自己的纯文本模式覆盖**我们写的映射逻辑**：
// 选项的展示名/说明/默认勾选怎么传进去、用户选了什么怎么带回来、校验怎么报错。
// 真正的花哨界面（空格勾选）没有 PTY 就无法在单测里驱动，由 PTY 冒烟验证覆盖。
func TestHuhPrompterAccessibleMapping(t *testing.T) {
	var out bytes.Buffer
	p := &huhPrompter{in: strings.NewReader("1,3\n"), out: &out, accessible: true}

	got, err := p.MultiSelect(MultiSelect{
		Title: "选择工具",
		Options: []Option{
			{Value: "go", Label: "Go 工具链", Hint: "via g", Selected: true},
			{Value: "node", Label: "Node.js", Hint: "via fnm"},
			{Value: "rust", Label: "Rust", Hint: "via rustup", Selected: true},
		},
		MinSelected: 1,
	})
	if err != nil {
		t.Fatalf("MultiSelect: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"go", "rust"}) {
		t.Errorf("选择结果 = %v，期望 [go rust]", got)
	}
	if !strings.Contains(out.String(), "Go 工具链") || !strings.Contains(out.String(), "via fnm") {
		t.Errorf("展示名/说明没有传给 huh：\n%s", out.String())
	}
}

// TestHuhPrompterAccessibleSelectConfirmInput 覆盖剩下三种提问的映射。
func TestHuhPrompterAccessibleSelectConfirmInput(t *testing.T) {
	var out bytes.Buffer

	sel := &huhPrompter{in: strings.NewReader("2\n"), out: &out, accessible: true}
	got, err := sel.Select(Question{Title: "怎么装", Default: "profile"}, []Option{
		{Value: "profile", Label: "按套装"},
		{Value: "manual", Label: "手工挑选", Hint: "逐个勾选"},
		{Value: "skip", Label: "不装", Disabled: true},
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got != "manual" {
		t.Errorf("Select = %q，期望 manual", got)
	}
	// 不可选项必须从 huh 的选项里剔除，否则用户能选中一个我们不会处理的值。
	if strings.Contains(out.String(), "不装") {
		t.Errorf("Disabled 选项不该出现在交互界面里：\n%s", out.String())
	}

	conf := &huhPrompter{in: strings.NewReader("y\n"), out: &out, accessible: true}
	ok, err := conf.Confirm(Question{Title: "要执行吗"})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !ok {
		t.Error("输入 y 应当得到 true")
	}

	inp := &huhPrompter{in: strings.NewReader("我的答案\n"), out: &out, accessible: true}
	v, err := inp.Input(Question{Title: "代理地址", Default: "http://127.0.0.1:7890"})
	if err != nil {
		t.Fatalf("Input: %v", err)
	}
	if v != "我的答案" {
		t.Errorf("Input = %q，期望 我的答案", v)
	}
}

// TestHuhPrompterSelectWithoutOptions：没有任何可选项时给出明确错误，
// 绝不能返回空值让上层以为"用户选了空字符串"。
func TestHuhPrompterSelectWithoutOptions(t *testing.T) {
	p := &huhPrompter{in: strings.NewReader("\n"), out: &bytes.Buffer{}, accessible: true}
	if _, err := p.Select(Question{Title: "x"}, []Option{{Value: "a", Disabled: true}}); err == nil {
		t.Error("没有可选项时应当报错")
	}
}

// TestHuhPrompterListHeightFallsBack：拿不到终端尺寸（输出不是 *os.File）时用保守值，
// 绝不能算出 0 让勾选框什么都不显示。
func TestHuhPrompterListHeightFallsBack(t *testing.T) {
	p := &huhPrompter{in: strings.NewReader(""), out: &bytes.Buffer{}}
	if got := p.listHeight(14); got != 14 {
		t.Errorf("listHeight 应当回落到调用方给的值，得到 %d", got)
	}
	if got := p.listHeight(0); got != 0 {
		t.Errorf("fallback 应原样返回，得到 %d", got)
	}
}
