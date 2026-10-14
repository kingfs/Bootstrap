package config

import (
	"strings"
	"testing"
	"time"
)

// TestParseTimeout：时限字符串的解析规则——空=用默认值，必须带单位，必须为正。
func TestParseTimeout(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "空表示用默认值", raw: "", want: 0},
		{name: "空白也算空", raw: "   ", want: 0},
		{name: "秒", raw: "30s", want: 30 * time.Second},
		{name: "分", raw: "10m", want: 10 * time.Minute},
		{name: "小时", raw: "1h", want: time.Hour},
		{name: "裸数字不接受", raw: "300", wantErr: true},
		{name: "零不接受", raw: "0s", wantErr: true},
		{name: "负数不接受", raw: "-5s", wantErr: true},
		{name: "乱写不接受", raw: "nonsense", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTimeout("timeouts.global", tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTimeout(%q) 应当报错，实际得 %v", tc.raw, got)
				}
				if !strings.Contains(err.Error(), "timeouts.global") {
					t.Errorf("错误信息应当带上配置键名：%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTimeout(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("ParseTimeout(%q) = %v，期望 %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestResolvedTimeoutsAndValidate：五个字段各归各位；坏值必须在 config validate 阶段被拦住。
func TestResolvedTimeoutsAndValidate(t *testing.T) {
	cfg := Defaults()
	cfg.Time = TimeoutConfig{
		Global: "30m", Command: "20m", Probe: "8s", Lookup: "90s", Download: "25m",
	}
	got, err := cfg.ResolvedTimeouts()
	if err != nil {
		t.Fatalf("ResolvedTimeouts: %v", err)
	}
	want := Timeouts{Global: 30 * time.Minute, Command: 20 * time.Minute,
		Probe: 8 * time.Second, Lookup: 90 * time.Second, Download: 25 * time.Minute}
	if got != want {
		t.Errorf("ResolvedTimeouts = %+v，期望 %+v", got, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("合法时限不该报错：%v", err)
	}

	// 默认值必须全是 0（＝各环节用自己的默认时限），否则"没配"会变成"配了 0"。
	def := Defaults()
	if t0, err := def.ResolvedTimeouts(); err != nil || t0 != (Timeouts{}) {
		t.Errorf("默认时限 = %+v, err=%v，期望全 0", t0, err)
	}

	bad := Defaults()
	bad.Time.Probe = "5" // 缺单位
	err = bad.Validate()
	if err == nil {
		t.Fatal("非法时限应当让 Validate 报错")
	}
	if !strings.Contains(err.Error(), "timeouts.probe") {
		t.Errorf("校验错误应当指明是哪个字段：%v", err)
	}
}
