package install

import (
	"errors"
	"strings"
	"testing"
)

// sum64 造一个 64 位十六进制哈希（测试里只关心形态，不关心真值）。
func sum64(c string) string { return strings.Repeat(c, 32) }

func TestParseChecksumSource(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantURL     string
		wantPointer string
		wantErr     string
	}{
		{name: "普通 URL 按行式文件解析", raw: "https://example.com/SHA256SUMS",
			wantURL: "https://example.com/SHA256SUMS"},
		{name: "带 JSON Pointer", raw: "https://herdr.dev/latest.json#/releases/0.9.5/sha256/linux-x86_64",
			wantURL: "https://herdr.dev/latest.json", wantPointer: "/releases/0.9.5/sha256/linux-x86_64"},
		{name: "去掉首尾空白", raw: "  https://example.com/x.sha256  ",
			wantURL: "https://example.com/x.sha256"},
		{name: "空 URL", raw: "   ", wantErr: "empty url"},
		{name: "非 http(s)", raw: "ftp://example.com/x", wantErr: "must be http(s)"},
		{name: "Pointer 必须以 / 开头", raw: "https://example.com/a.json#sha256",
			wantErr: "must start with /"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseChecksumSource(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseChecksumSource: %v", err)
			}
			if got.URL != tc.wantURL || got.Pointer != tc.wantPointer {
				t.Fatalf("got {%q, %q}, want {%q, %q}", got.URL, got.Pointer, tc.wantURL, tc.wantPointer)
			}
		})
	}
}

func TestChecksumFromBody(t *testing.T) {
	sum := sum64("ab")
	tests := []struct {
		name    string
		body    string
		pointer string
		asset   string
		want    string
		wantOK  bool
	}{
		{name: "JSON Pointer 取字段", body: `{"sha256":{"linux-x86_64":"` + sum + `"}}`,
			pointer: "/sha256/linux-x86_64", want: sum, wantOK: true},
		{name: "sha256: 前缀归一成小写裸哈希",
			body:    `{"a":{"b":"sha256:` + strings.ToUpper(sum) + `"}}`,
			pointer: "/a/b", want: sum, wantOK: true},
		{name: "数组下标", body: `{"assets":[{"sha256":"` + sum + `"}]}`,
			pointer: "/assets/0/sha256", want: sum, wantOK: true},
		{name: "~1 与 ~0 转义", body: `{"a/b":{"c~d":"` + sum + `"}}`,
			pointer: "/a~1b/c~0d", want: sum, wantOK: true},
		{name: "字段不存在", body: `{"sha256":{}}`, pointer: "/sha256/nope"},
		{name: "值不是字符串", body: `{"sha256":{"x":123}}`, pointer: "/sha256/x"},
		{name: "值不是哈希", body: `{"sha256":{"x":"deadbeef"}}`, pointer: "/sha256/x"},
		{name: "算法不是 sha256", body: `{"x":"sha512:` + sum + `"}`, pointer: "/x"},
		{name: "正文压根不是 JSON（站点对任何路径都回首页）",
			body: "<!doctype html><html>…", pointer: "/sha256/x"},
		{name: "行式校验和文件命中资产", body: sum + "  herdr-linux-x86_64\n",
			asset: "herdr-linux-x86_64", want: sum, wantOK: true},
		{name: "行式文件里没有这个资产", body: sum + "  other-file\n", asset: "herdr-linux-x86_64"},
		{name: "整份就是一个裸哈希", body: sum + "\n", asset: "herdr-linux-x86_64", want: sum, wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := checksumFromBody(tc.body, checksumSource{URL: "https://example.com/x", Pointer: tc.pointer}, tc.asset)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("checksumFromBody = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestChecksumAttemptsString 锁定错误信息里的来源摘要：相同结论合并，顺序稳定。
func TestChecksumAttemptsString(t *testing.T) {
	var a checksumAttempts
	a.add("checksums.txt", "HTTP 404")
	a.add("sha256sum.txt", "HTTP 404")
	a.add("api digest", rateLimitOutcome)
	want := "checksums.txt, sha256sum.txt: HTTP 404; api digest: " + rateLimitOutcome
	if got := a.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if !a.rateLimited() {
		t.Error("rateLimited() = false, want true")
	}
	var empty checksumAttempts
	if got := empty.String(); got != "" {
		t.Errorf("空 attempts 应当渲染成空串，得到 %q", got)
	}
	if empty.rateLimited() {
		t.Error("空 attempts 不该报限流")
	}
}

func TestBriefFetchError(t *testing.T) {
	long := "https://github.com/x/y/releases/download/v1/checksums.txt returned HTTP 404: <!DOCTYPE html>\n<html lang=\"en\">" +
		strings.Repeat("x", 400)
	tests := []struct {
		name string
		err  error
		want string
		// max 是该条期望的最大长度（默认 90）；带 TLS 提示的那条允许长一些，
		// 因为 `(hint: …)` 是唯一可行动的信息，宁可长也不能切掉。
		max int
	}{
		{name: "nil", err: nil, want: ""},
		{name: "404 只留状态码", err: errors.New(long), want: "HTTP 404"},
		{name: "限流单独说清楚",
			err:  errors.New("https://api.github.com/repos/a/b/releases/tags/v1 returned HTTP 403: {\"message\":\"API rate limit exceeded\"}"),
			want: rateLimitOutcome},
		{name: "429 限流同样识别",
			err:  errors.New("https://api.github.com/x returned HTTP 429: rate limit"),
			want: rateLimitOutcome},
		{name: "传输层错误去掉重复的 URL 壳",
			err: errors.New(`GET https://api.github.com/x: Get "https://api.github.com/x": ` +
				`dial tcp 127.0.0.1:9: connect: connection refused`),
			want: "dial tcp 127.0.0.1:9: connect: connection refused"},
		{name: "保留 TLS 提示（截断不能切掉 hint）",
			err: errors.New(`GET https://x/y: Get "https://x/y": x509: certificate signed by unknown authority ` +
				`(hint: no usable system CA certificates; install your distro's CA package, e.g. ca-certificates)`),
			want: "x509: certificate signed by unknown authority (hint: no usable system CA certificates; " +
				"install your distro's CA package, e.g. ca-certificates)",
			max: 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := briefFetchError(tc.err)
			if got != tc.want {
				t.Fatalf("briefFetchError = %q, want %q", got, tc.want)
			}
			limit := tc.max
			if limit == 0 {
				limit = 90
			}
			if len(got) > limit {
				t.Errorf("结论还是太长（%d 字符）：%q", len(got), got)
			}
		})
	}
}

// TestTruncateRunes 确认按字符截断（CJK 不会被切成半个字节）。
func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("一二三四五", 3); got != "一二三…" {
		t.Errorf("truncateRunes = %q", got)
	}
	if got := truncateRunes("abc", 5); got != "abc" {
		t.Errorf("truncateRunes = %q", got)
	}
}
