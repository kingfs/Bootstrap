package textwidth

import "testing"

func TestWidth(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"空串", "", 0},
		{"纯 ASCII", "git 2.55.0", 10},
		{"中文表头", "系统", 4},
		{"中英混排", "系统 linux", 10},
		{"全角标点", "（ubuntu 24.04）", 16},
		{"中文括号算全角", "（a）", 5},
		{"组合记号不占位", "e\u0301", 1},
		{"零宽空格不占位", "a\u200bb", 2},
		{"emoji 算两列", "🚀", 2},
		{"tab 不算列宽", "a\tb", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Width(tc.in); got != tc.want {
				t.Errorf("Width(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestRuneWidthRanges：抽查各区间的代表字符，防止区间表被改窄。
func TestRuneWidthRanges(t *testing.T) {
	for _, r := range []rune{'中', 'あ', '한', 'Ａ', '￥', '（', '　', '🚀'} {
		if got := RuneWidth(r); got != 2 {
			t.Errorf("RuneWidth(%q) = %d, want 2", r, got)
		}
	}
	for _, r := range []rune{'a', 'Z', '0', '-', 'é'} {
		if got := RuneWidth(r); got != 1 {
			t.Errorf("RuneWidth(%q) = %d, want 1", r, got)
		}
	}
	for _, r := range []rune{0, '\t', '\u200b', '\ufe0f', '\u0301'} {
		if got := RuneWidth(r); got != 0 {
			t.Errorf("RuneWidth(%q) = %d, want 0", r, got)
		}
	}
}
