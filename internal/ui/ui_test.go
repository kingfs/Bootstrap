package ui

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// script 造一个"用户按顺序输入这些行"的输入源。
func script(lines ...string) io.Reader {
	if len(lines) == 0 {
		return strings.NewReader("")
	}
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

// eof 造一个"输入已经结束"的源：用来区分"用户敲了回车"和"没有更多输入"。
func eof() io.Reader { return strings.NewReader("") }

func testOptions() []Option {
	return []Option{
		{Value: "golang", Label: "Go 工具链", Hint: "via g", Selected: true},
		{Value: "nodejs", Label: "Node.js", Hint: "via fnm"},
		{Value: "k9s", Label: "k9s", Hint: "不适用于当前平台", Disabled: true},
	}
}

func TestTextMultiSelect(t *testing.T) {
	cases := []struct {
		name    string
		input   io.Reader
		min     int
		want    []string
		wantErr error
	}{
		{name: "回车用默认勾选", input: script(""), want: []string{"golang"}},
		{name: "按编号", input: script("2"), want: []string{"nodejs"}},
		{name: "逗号分隔", input: script("1,2"), want: []string{"golang", "nodejs"}},
		{name: "中文逗号与空格", input: script("1， 2"), want: []string{"golang", "nodejs"}},
		{name: "区间", input: script("1-2"), want: []string{"golang", "nodejs"}},
		{name: "全选跳过不可选", input: script("a"), want: []string{"golang", "nodejs"}},
		{name: "全不选清掉默认勾选", input: script("n"), want: nil},
		{name: "全不选写 none 也行", input: script("none"), want: nil},
		{name: "全不选不够最少数量就重问", input: script("n", "1,2"), min: 2, want: []string{"golang", "nodejs"}},
		{name: "直接写值", input: script("GOLANG"), want: []string{"golang"}},
		{name: "去重", input: script("1,1,1"), want: []string{"golang"}},
		{name: "取消", input: script("q"), wantErr: ErrCancelled},
		{name: "输入结束视为取消", input: eof(), wantErr: ErrCancelled},
		{name: "编号越界后重问", input: script("9", "1"), want: []string{"golang"}},
		{name: "看不懂后重问", input: script("zzz", "2"), want: []string{"nodejs"}},
		{name: "不可选后重问", input: script("3", "2"), want: []string{"nodejs"}},
		{name: "不够最少数量就重问", input: script("", "1,2"), min: 2, want: []string{"golang", "nodejs"}},
		{name: "反复乱输就放弃", input: script("x", "x", "x", "x", "x", "x"), wantErr: ErrTooManyAttempts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := NewText(tc.input, &out).MultiSelect(MultiSelect{
				Title:       "选择要安装的工具",
				Options:     testOptions(),
				MinSelected: tc.min,
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v（输出：%s）", err, tc.wantErr, out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v（输出：%s）", err, out.String())
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("选中 = %v, want %v", got, tc.want)
			}
			if !strings.Contains(out.String(), "选择要安装的工具") {
				t.Errorf("提示里应该有标题，实际：%s", out.String())
			}
			if !strings.Contains(out.String(), "［不可选］") {
				t.Errorf("不可选项应该被标注，实际：%s", out.String())
			}
		})
	}
}

func TestTextMultiSelectEmptyOptions(t *testing.T) {
	var out bytes.Buffer
	// 没有选项时不该提问（否则纯文本路径会卡在 EOF 上）。
	got, err := NewText(script(""), &out).MultiSelect(MultiSelect{Title: "空"})
	if err != nil || got != nil {
		t.Fatalf("got %v, %v; want nil, nil", got, err)
	}
	if out.Len() != 0 {
		t.Errorf("没有选项时不该有任何输出，实际：%s", out.String())
	}
}

func TestTextSelect(t *testing.T) {
	opts := []Option{
		{Value: "base", Hint: "≈12 个工具"},
		{Value: "full", Hint: "全部"},
		{Value: "nope", Disabled: true},
	}
	cases := []struct {
		name    string
		input   io.Reader
		def     string
		want    string
		wantErr error
	}{
		{name: "回车用默认", input: script(""), def: "full", want: "full"},
		{name: "回车无默认时取第一个可选项", input: script(""), want: "base"},
		{name: "按编号", input: script("2"), want: "full"},
		{name: "按名字", input: script("full"), want: "full"},
		{name: "大小写不敏感", input: script("BASE"), want: "base"},
		{name: "取消", input: script("q"), wantErr: ErrCancelled},
		{name: "输入结束视为取消", input: eof(), wantErr: ErrCancelled},
		{name: "不可选后重问", input: script("3", "1"), want: "base"},
		{name: "乱输后重问", input: script("??", "2"), want: "full"},
		{name: "反复乱输就放弃", input: script("?", "?", "?", "?", "?", "?"), wantErr: ErrTooManyAttempts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := NewText(tc.input, &out).Select(Question{Title: "选个套装", Default: tc.def}, opts)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v（输出：%s）", err, tc.wantErr, out.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTextSelectNoOptions(t *testing.T) {
	if _, err := NewText(script(""), &bytes.Buffer{}).Select(Question{}, nil); err == nil {
		t.Fatal("没有选项时应当报错")
	}
}

// TestTextBack 锁住纯文本界面的"返回上一步"：开了 Back 才认 b，而且选项优先。
//
// 用户的实测反馈是"进了勾选就回不去上一级菜单"，所以这条不能只靠 huh 那边的按键。
func TestTextBack(t *testing.T) {
	t.Run("多选按 b 返回上一步", func(t *testing.T) {
		var out bytes.Buffer
		_, err := NewText(script("b"), &out).MultiSelect(MultiSelect{Title: "勾工具", Options: testOptions(), Back: true})
		if !errors.Is(err, ErrBack) {
			t.Fatalf("err = %v, want ErrBack（输出：%s）", err, out.String())
		}
		if !strings.Contains(out.String(), "b=返回上一步") {
			t.Errorf("开了返回就该把键位写进提示行，实际：%s", out.String())
		}
	})

	t.Run("单选按 b 返回上一步", func(t *testing.T) {
		var out bytes.Buffer
		_, err := NewText(script("b"), &out).Select(
			Question{Title: "想怎么装", Back: true},
			[]Option{{Value: "profile"}, {Value: "manual"}},
		)
		if !errors.Is(err, ErrBack) {
			t.Fatalf("err = %v, want ErrBack（输出：%s）", err, out.String())
		}
		if !strings.Contains(out.String(), "b=返回上一步") {
			t.Errorf("开了返回就该把键位写进提示行，实际：%s", out.String())
		}
	})

	t.Run("没开 Back 时 b 只是看不懂的输入", func(t *testing.T) {
		var out bytes.Buffer
		got, err := NewText(script("b", "1"), &out).MultiSelect(MultiSelect{Title: "勾工具", Options: testOptions()})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if strings.Join(got, ",") != "golang" {
			t.Errorf("选中 = %v, want [golang]", got)
		}
		if strings.Contains(out.String(), "返回上一步") {
			t.Errorf("没开 Back 就不该出现返回提示，实际：%s", out.String())
		}
	})

	t.Run("选项值真的叫 b 时选项优先", func(t *testing.T) {
		var out bytes.Buffer
		got, err := NewText(script("b"), &out).Select(
			Question{Title: "选个套装", Back: true},
			[]Option{{Value: "base"}, {Value: "b"}},
		)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != "b" {
			t.Errorf("got %q, want %q（返回键不该顶掉同名选项）", got, "b")
		}
	})
}

func TestTextConfirmDefaultsToNo(t *testing.T) {
	cases := []struct {
		name  string
		input io.Reader
		want  bool
	}{
		{name: "y", input: script("y"), want: true},
		{name: "YES", input: script("YES"), want: true},
		{name: "回车＝拒绝", input: script(""), want: false},
		{name: "n", input: script("n"), want: false},
		{name: "看不懂也＝拒绝", input: script("maybe"), want: false},
		{name: "EOF＝拒绝且不报错", input: eof(), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := NewText(tc.input, &out).Confirm(Question{Title: "确认继续？"})
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			if !strings.Contains(out.String(), "[y/N]") {
				t.Errorf("提示里应显示默认是 No，实际：%s", out.String())
			}
		})
	}
}

func TestTextInput(t *testing.T) {
	// 两侧空白会被去掉：用户从别处粘贴路径时经常带上空格。
	var out bytes.Buffer
	got, err := NewText(script("  /tmp/定制  "), &out).Input(Question{Title: "目标目录", Default: "/tmp/default"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "/tmp/定制" {
		t.Errorf("got %q, want trimmed value", got)
	}

	// 回车采用默认值。
	out.Reset()
	got, err = NewText(script(""), &out).Input(Question{Title: "目标目录", Default: "/tmp/default"})
	if err != nil || got != "/tmp/default" {
		t.Fatalf("got %q, %v; want default", got, err)
	}
	if !strings.Contains(out.String(), "[/tmp/default]") {
		t.Errorf("提示里应显示默认值，实际：%s", out.String())
	}

	// 输入结束（EOF）＝取消，而不是拿到空值继续跑。
	if _, err := NewText(eof(), &bytes.Buffer{}).Input(Question{Title: "x"}); !errors.Is(err, ErrCancelled) {
		t.Errorf("EOF 应当返回 ErrCancelled，实际 %v", err)
	}

	// Secret 只说"会回显"，不假装安全。
	out.Reset()
	_, _ = NewText(script("tok"), &out).Input(Question{Title: "token", Secret: true})
	if !strings.Contains(out.String(), "回显") {
		t.Errorf("Secret 应当提示会回显，实际：%s", out.String())
	}
}

func TestNonInteractiveAlwaysRefuses(t *testing.T) {
	var p NonInteractive
	if p.Interactive() {
		t.Error("NonInteractive.Interactive() 应为 false")
	}
	if _, err := p.MultiSelect(MultiSelect{}); !errors.Is(err, ErrNotInteractive) {
		t.Errorf("MultiSelect err = %v", err)
	}
	if _, err := p.Select(Question{}, testOptions()); !errors.Is(err, ErrNotInteractive) {
		t.Errorf("Select err = %v", err)
	}
	if _, err := p.Confirm(Question{}); !errors.Is(err, ErrNotInteractive) {
		t.Errorf("Confirm err = %v", err)
	}
	if _, err := p.Input(Question{}); !errors.Is(err, ErrNotInteractive) {
		t.Errorf("Input err = %v", err)
	}
}

func TestParseSelection(t *testing.T) {
	opts := testOptions()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "1", want: "golang"},
		{in: "1,2", want: "golang,nodejs"},
		{in: "2 1", want: "nodejs,golang"}, // 保持输入顺序
		{in: "1-2", want: "golang,nodejs"},
		{in: "golang", want: "golang"},
		{in: "0", wantErr: true},
		{in: "4", wantErr: true},
		{in: "3", wantErr: true}, // 不可选
		{in: "unknown", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseSelection(tc.in, opts)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSelection(%q) 应当报错，实际 %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSelection(%q): %v", tc.in, err)
			continue
		}
		if strings.Join(got, ",") != tc.want {
			t.Errorf("parseSelection(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSortedValuesDoesNotMutateInput(t *testing.T) {
	in := []string{"b", "a"}
	got := SortedValues(in)
	if strings.Join(got, "") != "ab" {
		t.Errorf("SortedValues = %v", got)
	}
	if in[0] != "b" || in[1] != "a" {
		t.Errorf("入参被改动了：%v", in)
	}
}
