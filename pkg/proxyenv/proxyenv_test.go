package proxyenv

import (
	"strings"
	"testing"
)

func testProfile() Profile {
	return Profile{
		Name:       "local",
		HTTPProxy:  "http://127.0.0.1:7890",
		HTTPSProxy: "http://127.0.0.1:7890",
		NoProxy:    "127.0.0.1,localhost,10.2.0.0/16",
	}
}

func TestProfileEnvSetsUpperAndLower(t *testing.T) {
	env := testProfile().Env()
	want := map[string]string{
		"HTTP_PROXY":  "http://127.0.0.1:7890",
		"http_proxy":  "http://127.0.0.1:7890",
		"HTTPS_PROXY": "http://127.0.0.1:7890",
		"https_proxy": "http://127.0.0.1:7890",
		"NO_PROXY":    "127.0.0.1,localhost,10.2.0.0/16",
		"no_proxy":    "127.0.0.1,localhost,10.2.0.0/16",
	}
	if len(env) != len(want) {
		t.Fatalf("Env() has %d keys, want %d: %v", len(env), len(want), env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("Env()[%q] = %q, want %q", k, env[k], v)
		}
	}
	if _, ok := env["ALL_PROXY"]; ok {
		t.Error("ALL_PROXY should be omitted when unset")
	}
}

func TestRenderBash(t *testing.T) {
	out, err := RenderProfile(ShellBash, testProfile())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantLines := []string{
		"export HTTP_PROXY='http://127.0.0.1:7890'",
		"export http_proxy='http://127.0.0.1:7890'",
		"export HTTPS_PROXY='http://127.0.0.1:7890'",
		"export https_proxy='http://127.0.0.1:7890'",
		"export NO_PROXY='127.0.0.1,localhost,10.2.0.0/16'",
		"export no_proxy='127.0.0.1,localhost,10.2.0.0/16'",
	}
	if got := strings.Split(out, "\n"); len(got) != len(wantLines) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(wantLines), out)
	}
	for i, want := range wantLines {
		if got := strings.Split(out, "\n")[i]; got != want {
			t.Errorf("line %d = %q, want %q", i, got, want)
		}
	}
}

func TestRenderDeterministic(t *testing.T) {
	p := testProfile()
	p.AllProxy = "socks5://127.0.0.1:1080"
	first, err := RenderProfile(ShellBash, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := RenderProfile(ShellBash, p)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if again != first {
			t.Fatalf("render is not deterministic:\n%s\n---\n%s", first, again)
		}
	}
	if !strings.Contains(first, "export ALL_PROXY='socks5://127.0.0.1:1080'") {
		t.Errorf("ALL_PROXY missing in:\n%s", first)
	}
}

func TestRenderShells(t *testing.T) {
	p := Profile{HTTPProxy: "http://p:1"}
	tests := []struct {
		shell Shell
		want  string
	}{
		{ShellFish, "set -gx HTTP_PROXY 'http://p:1'"},
		{ShellPwsh, "$env:HTTP_PROXY = 'http://p:1'"},
		{ShellCmd, `set "HTTP_PROXY=http://p:1"`},
		{ShellDotenv, "HTTP_PROXY=http://p:1"},
	}
	for _, tc := range tests {
		t.Run(string(tc.shell), func(t *testing.T) {
			out, err := RenderProfile(tc.shell, p)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// 同一个值会同时设置大写与小写两个变量，因此只断言包含关系。
			if !strings.Contains(out, tc.want) {
				t.Errorf("Render(%s) = %q, want it to contain %q", tc.shell, out, tc.want)
			}
			if !strings.Contains(out, strings.ToLower(tc.want)) || strings.ToLower(tc.want) == tc.want {
				t.Errorf("Render(%s) = %q, want a lowercase counterpart", tc.shell, out)
			}
		})
	}
}

func TestRenderOff(t *testing.T) {
	tests := []struct {
		shell     Shell
		mustHave  string
		mustNotBe string
	}{
		{ShellBash, "unset HTTP_PROXY http_proxy", "export"},
		{ShellFish, "set -e HTTP_PROXY", "set -gx"},
		{ShellPwsh, "Remove-Item Env:HTTP_PROXY", "$env:"},
		{ShellCmd, "set HTTP_PROXY=", "export"},
		{ShellDotenv, "HTTP_PROXY=", "unset"},
	}
	for _, tc := range tests {
		t.Run(string(tc.shell), func(t *testing.T) {
			out, err := RenderOff(tc.shell)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(out, tc.mustHave) {
				t.Errorf("RenderOff(%s) = %q, want it to contain %q", tc.shell, out, tc.mustHave)
			}
			if tc.mustNotBe != "" && strings.Contains(out, tc.mustNotBe) {
				t.Errorf("RenderOff(%s) = %q, should not contain %q", tc.shell, out, tc.mustNotBe)
			}
		})
	}
}

func TestNormalizeShell(t *testing.T) {
	ok := map[string]Shell{
		"bash": ShellBash, "zsh": ShellZsh, "sh": ShellBash,
		"fish": ShellFish, "pwsh": ShellPwsh, "PowerShell": ShellPwsh,
		"cmd": ShellCmd, "dotenv": ShellDotenv, "json": ShellJSON,
	}
	for in, want := range ok {
		got, err := NormalizeShell(in)
		if err != nil {
			t.Fatalf("NormalizeShell(%q) error: %v", in, err)
		}
		if got != want {
			t.Errorf("NormalizeShell(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := NormalizeShell("csh"); err == nil {
		t.Error("NormalizeShell(csh) should fail")
	}
}

func TestQuotingHandlesSingleQuote(t *testing.T) {
	out, err := RenderProfile(ShellBash, Profile{NoProxy: "a'b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `'a'\''b'`) {
		t.Errorf("POSIX single-quote escaping missing in %q", out)
	}
}

func TestMaskProxyURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"http://127.0.0.1:7890", "http://127.0.0.1:7890"},
		{"http://user:secret@proxy.local:8123", "http://***@proxy.local:8123"},
		{"socks5://user@proxy.local:1080", "socks5://***@proxy.local:1080"},
	}
	for _, tc := range tests {
		if got := MaskProxyURL(tc.in); got != tc.want {
			t.Errorf("MaskProxyURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsZero(t *testing.T) {
	if !(Profile{}).IsZero() {
		t.Error("empty profile should be zero")
	}
	if (Profile{NoProxy: "x"}).IsZero() {
		t.Error("profile with values should not be zero")
	}
}
