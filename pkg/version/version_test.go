package version

import (
	"regexp"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    []int
		pre     string
		wantErr bool
	}{
		{in: "1.24.0", want: []int{1, 24, 0}},
		{in: "v1.24.0", want: []int{1, 24, 0}},
		{in: "go version go1.26.9 linux/arm64", want: []int{1, 26, 9}},
		{in: "1.22", want: []int{1, 22}},
		{in: "1.24.0-rc1", want: []int{1, 24, 0}, pre: "rc1"},
		{in: "2.0.0+build.5", want: []int{2, 0, 0}},
		{in: "", wantErr: true},
		{in: "no digits here", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %+v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tc.in, err)
			}
			if len(got.Numbers) != len(tc.want) {
				t.Fatalf("Numbers = %v, want %v", got.Numbers, tc.want)
			}
			for i := range tc.want {
				if got.Numbers[i] != tc.want[i] {
					t.Fatalf("Numbers = %v, want %v", got.Numbers, tc.want)
				}
			}
			if got.Pre != tc.pre {
				t.Errorf("Pre = %q, want %q", got.Pre, tc.pre)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.24.0", "1.24.0", 0},
		{"v1.24.0", "1.24.0", 0},
		{"1.23.9", "1.24.0", -1},
		{"1.24.1", "1.24.0", 1},
		{"1.24", "1.24.0", 0},
		{"1.24.0-rc1", "1.24.0", -1},
		{"1.24.0", "1.24.0-rc1", 1},
		{"2.0", "10.0", -1},
	}
	for _, tc := range tests {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMeetsMin(t *testing.T) {
	tests := []struct {
		actual, min string
		want        bool
	}{
		{"1.24.0", "", true},
		{"1.24.0", "1.22", true},
		{"1.20.0", "1.22", false},
		{"", "1.22", false},
		{"1.22", "1.22", true},
	}
	for _, tc := range tests {
		if got := MeetsMin(tc.actual, tc.min); got != tc.want {
			t.Errorf("MeetsMin(%q, %q) = %v, want %v", tc.actual, tc.min, got, tc.want)
		}
	}
}

func TestExtract(t *testing.T) {
	re := regexp.MustCompile(`g version (?P<v>[\d.]+)`)
	got, ok := Extract(re, "g version 1.6.2\nextra")
	if !ok || got != "1.6.2" {
		t.Errorf("Extract = (%q, %v), want (1.6.2, true)", got, ok)
	}

	// 没有命名组时退回第一个捕获组。
	re2 := regexp.MustCompile(`v(\d+\.\d+\.\d+)`)
	if got, ok := Extract(re2, "node v20.11.1"); !ok || got != "20.11.1" {
		t.Errorf("Extract = (%q, %v), want (20.11.1, true)", got, ok)
	}

	if _, ok := Extract(re, "no version"); ok {
		t.Error("Extract should report no match")
	}
}

func TestIsLatest(t *testing.T) {
	for _, s := range []string{"", "latest", "LATEST", "stable", "lts"} {
		if !IsLatest(s) {
			t.Errorf("IsLatest(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"1.2.3", "v1.2.3"} {
		if IsLatest(s) {
			t.Errorf("IsLatest(%q) = true, want false", s)
		}
	}
}
