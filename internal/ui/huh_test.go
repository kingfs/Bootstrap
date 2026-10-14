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

// TestHuhPrompterTermSizeFallsBack：拿不到终端尺寸（输出不是 *os.File）时返回 0，
// 调用方据此用保守值——绝不能算出 0 行让勾选框什么都不显示。
func TestHuhPrompterTermSizeFallsBack(t *testing.T) {
	p := &huhPrompter{in: strings.NewReader(""), out: &bytes.Buffer{}}
	if w, h := p.termSize(); w != 0 || h != 0 {
		t.Errorf("拿不到终端尺寸时应当返回 0,0，得到 %d,%d", w, h)
	}
	if got := listHeight("标题", "说明", 0, 0, 14, 77); got != 16 {
		t.Errorf("没有终端尺寸时用保守值 14 + 标题说明 2 行 = 16，得到 %d", got)
	}
}

// TestListHeight：huh 的 Height 是**整段字段**的高度（含标题与说明），
// 直接传选项个数会少显示一行——用户实测"4 个选项只看得见 3 个"。
// 另外列表要按**当前**终端高度算：窗口越大给的行越多（用户在窗口里拉大终端后
// 仍只看到原来那么多行，就是这条没做到）。
func TestListHeight(t *testing.T) {
	const title = "本机 linux/arm64，适用工具 77 个（已装 0，缺失 77）。想怎么装？"

	if got := listHeight("标题", "", 100, 40, 14, 4); got != 0 {
		t.Errorf("选项全能放下时不设 Height（交给 huh 精确撑开），得到 %d", got)
	}
	if got := listHeight("标题", "", 100, 40, 14, 0); got != 0 {
		t.Errorf("没有选项时也不设 Height，得到 %d", got)
	}

	// 放不下：高度 = 终端高 − 标题/说明折行 − 4，再加回标题/说明本身。
	// 40 行终端、标题 1 行 → 列表 35 行 + 标题 1 行 = 36。
	if got := listHeight(title, "", 100, 40, 14, 77); got != 36 {
		t.Errorf("40 行终端里长列表应当是 36（35 行列表 + 1 行标题），得到 %d", got)
	}
	// 终端越大给得越多：这正是"拉大窗口能多看点"。
	small := listHeight(title, "", 100, 24, 14, 77)
	large := listHeight(title, "", 100, 60, 14, 77)
	if small <= 14 || large <= small {
		t.Errorf("终端变高时列表应当变长：24 行 → %d，60 行 → %d", small, large)
	}
	// 再矮的终端也得给列表留 3 行，不能撑爆屏幕。
	if got := listHeight(title, "", 100, 6, 14, 77); got < 3 {
		t.Errorf("极矮终端至少要留 3 行列表，得到 %d", got)
	}

	if n := chromeRows("一行标题", "一行说明", 100); n != 2 {
		t.Errorf("一行标题 + 一行说明 = 2 行，得到 %d", n)
	}
	if n := chromeRows("", "", 100); n != 0 {
		t.Errorf("没有标题/说明时应当是 0 行，得到 %d", n)
	}
	// 超长标题要按宽度折行计数，不能永远算 1 行；窄终端折得更多。
	long := strings.Repeat("很长的标题", 40)
	if n := chromeRows(long, "", 100); n < 2 {
		t.Errorf("超长标题应当折成多行，得到 %d", n)
	}
	if wide, narrow := chromeRows(long, "", 100), chromeRows(long, "", 40); narrow <= wide {
		t.Errorf("同样长的标题在窄终端里应当折更多行：100 列 %d，40 列 %d", wide, narrow)
	}
}

// TestListFieldResize：终端尺寸变化时列表高度跟着变（nil 安全）。
func TestListFieldResize(t *testing.T) {
	var nilList *listField
	nilList.resize(100, 40) // 普通字段表单会传 nil，不能 panic

	got := 0
	l := &listField{
		title: "标题", fallback: 14, total: 77,
		set: func(height int) { got = height },
	}
	l.resize(100, 24)
	short := got
	l.resize(100, 60)
	if got <= short {
		t.Errorf("窗口变高后列表高度应当变大：24 行 → %d，60 行 → %d", short, got)
	}
	l.resize(0, 0) // 拿不到尺寸 → 保守值 14 行 + 1 行标题
	if got != 15 {
		t.Errorf("拿不到尺寸时用保守值 14 + 1 行标题 = 15，得到 %d", got)
	}
}
