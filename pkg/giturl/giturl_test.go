package giturl

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantHost   string
		wantOwner  string
		wantRepo   string
		wantRef    string
		wantSub    string
		wantSCP    bool
		wantScheme string
	}{
		{name: "gitlab keyword subgroup", in: "https://git.in.chaitin.net/core/tree/issues/project", wantHost: "git.in.chaitin.net", wantOwner: "core/tree/issues", wantRepo: "project", wantScheme: "https"},
		{name: "gitlab keyword clone", in: "git@gitlab.com:core/sub/tree/repo.git", wantHost: "gitlab.com", wantOwner: "core/sub/tree", wantRepo: "repo", wantSCP: true, wantScheme: "ssh"},
		{name: "gitlab keyword web", in: "https://gitlab.com/core/sub/tree/repo/-/tree/develop/src", wantHost: "gitlab.com", wantOwner: "core/sub/tree", wantRepo: "repo", wantRef: "develop", wantSub: "src", wantScheme: "https"},
		{name: "shorthand", in: "kingfs/Bootstrap", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap"},
		{name: "https", in: "https://github.com/kingfs/Bootstrap", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantScheme: "https"},
		{name: "https dot git", in: "https://github.com/kingfs/Bootstrap.git", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantScheme: "https"},
		{name: "trailing slash", in: "https://github.com/kingfs/Bootstrap/", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantScheme: "https"},
		{name: "scp like", in: "git@github.com:kingfs/Bootstrap.git", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantSCP: true, wantScheme: "ssh"},
		{name: "ssh scheme gitlab groups", in: "ssh://git@gitlab.com/group/sub/proj.git", wantHost: "gitlab.com", wantOwner: "group/sub", wantRepo: "proj", wantScheme: "ssh"},
		{name: "no scheme with host", in: "github.com/kingfs/Bootstrap", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap"},
		{name: "tree path", in: "https://github.com/kingfs/Bootstrap/tree/main/internal/foo", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantRef: "main", wantSub: "internal/foo", wantScheme: "https"},
		{name: "blob path", in: "https://github.com/kingfs/Bootstrap/blob/main/README.md", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantRef: "main", wantSub: "README.md", wantScheme: "https"},
		{name: "tree one segment", in: "https://github.com/kingfs/Bootstrap/tree/dev", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantRef: "dev", wantScheme: "https"},
		{name: "release tag", in: "https://github.com/kingfs/Bootstrap/releases/tag/v0.3.0", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantRef: "v0.3.0", wantScheme: "https"},
		{name: "release download ignored", in: "https://github.com/kingfs/Bootstrap/releases/download/v0.3.0/bs_linux_amd64.tar.gz", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantScheme: "https"},
		{name: "pull page", in: "https://github.com/kingfs/Bootstrap/pull/12", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantScheme: "https"},
		{name: "issues page", in: "https://github.com/kingfs/Bootstrap/issues", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantScheme: "https"},
		{name: "query ignored", in: "https://github.com/kingfs/Bootstrap?tab=readme", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantScheme: "https"},
		{name: "fragment as ref", in: "https://github.com/kingfs/Bootstrap#dev", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantRef: "dev", wantScheme: "https"},
		{name: "gitlab dash separator", in: "https://gitlab.com/group/sub/proj/-/tree/main/x", wantHost: "gitlab.com", wantOwner: "group/sub", wantRepo: "proj", wantRef: "main", wantSub: "x", wantScheme: "https"},
		{name: "commit page", in: "https://github.com/kingfs/Bootstrap/commit/abc123", wantHost: "github.com", wantOwner: "kingfs", wantRepo: "Bootstrap", wantRef: "abc123", wantScheme: "https"},
		{name: "self hosted", in: "https://git.example.com/team/repo.git", wantHost: "git.example.com", wantOwner: "team", wantRepo: "repo", wantScheme: "https"},
		{name: "localhost host", in: "http://localhost:3000/team/repo", wantHost: "localhost:3000", wantOwner: "team", wantRepo: "repo", wantScheme: "http"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in, "")
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", tc.in, err)
			}
			if got.Host != tc.wantHost {
				t.Errorf("Host = %q, want %q", got.Host, tc.wantHost)
			}
			if got.Owner != tc.wantOwner {
				t.Errorf("Owner = %q, want %q", got.Owner, tc.wantOwner)
			}
			if got.Repo != tc.wantRepo {
				t.Errorf("Repo = %q, want %q", got.Repo, tc.wantRepo)
			}
			if got.Ref != tc.wantRef {
				t.Errorf("Ref = %q, want %q", got.Ref, tc.wantRef)
			}
			if got.SubPath != tc.wantSub {
				t.Errorf("SubPath = %q, want %q", got.SubPath, tc.wantSub)
			}
			if got.SCP != tc.wantSCP {
				t.Errorf("SCP = %v, want %v", got.SCP, tc.wantSCP)
			}
			if tc.wantScheme != "" && got.Scheme != tc.wantScheme {
				t.Errorf("Scheme = %q, want %q", got.Scheme, tc.wantScheme)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"https://github.com/",
		"https://github.com/kingfs",
		"justoneword",
		"https://github.com/king fs/Bootstrap",
	} {
		t.Run(in, func(t *testing.T) {
			if got, err := Parse(in, ""); err == nil {
				t.Fatalf("Parse(%q) = %+v, want error", in, got)
			}
		})
	}
}

func TestParseDefaultHost(t *testing.T) {
	got, err := Parse("kingfs/Bootstrap", "gitlab.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Host != "gitlab.com" {
		t.Errorf("Host = %q, want gitlab.com", got.Host)
	}
	// 带 host 的输入不受 default_host 影响。
	got, err = Parse("https://github.com/kingfs/Bootstrap", "gitlab.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Host != "github.com" {
		t.Errorf("Host = %q, want github.com", got.Host)
	}
}

func TestCloneURL(t *testing.T) {
	ref, err := Parse("kingfs/Bootstrap", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := ref.CloneURL("https"), "https://github.com/kingfs/Bootstrap.git"; got != want {
		t.Errorf("CloneURL(https) = %q, want %q", got, want)
	}
	if got, want := ref.CloneURL("ssh"), "git@github.com:kingfs/Bootstrap.git"; got != want {
		t.Errorf("CloneURL(ssh) = %q, want %q", got, want)
	}
	// 默认（空协议）走 https。
	if got, want := ref.CloneURL(""), "https://github.com/kingfs/Bootstrap.git"; got != want {
		t.Errorf("CloneURL(\"\") = %q, want %q", got, want)
	}

	scp, err := Parse("git@github.com:kingfs/Bootstrap.git", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// scp-like 输入即使用 https 协议也保持 ssh（用户显式写了 git@）。
	if got, want := scp.CloneURL("https"), "git@github.com:kingfs/Bootstrap.git"; got != want {
		t.Errorf("scp CloneURL(https) = %q, want %q", got, want)
	}

	multi, err := Parse("ssh://git@gitlab.com/group/sub/proj.git", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 用户显式粘贴了 ssh 地址 → 尊重其意图，即使请求 https 也保持 ssh。
	if got, want := multi.CloneURL("https"), "git@gitlab.com:group/sub/proj.git"; got != want {
		t.Errorf("CloneURL(https) = %q, want %q", got, want)
	}
	// 普通 https 输入在请求 ssh 时切换为 ssh 地址。
	if got, want := ref.CloneURL("ssh"), "git@github.com:kingfs/Bootstrap.git"; got != want {
		t.Errorf("ref CloneURL(ssh) = %q, want %q", got, want)
	}
}

func TestSlugAndString(t *testing.T) {
	ref, err := Parse("https://github.com/kingfs/Bootstrap/tree/main/internal", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := ref.Slug(), "github.com/kingfs/Bootstrap"; got != want {
		t.Errorf("Slug() = %q, want %q", got, want)
	}
	if got, want := ref.String(), "github.com/kingfs/Bootstrap@main (internal)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestParseSSHTarget：只有真正走 SSH 的地址才需要 known_hosts 层面的准备，
// 所以判断必须精确——把 https 判成 ssh 会白白往用户 known_hosts 里写东西。
func TestParseSSHTarget(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   SSHTarget
		wantOK bool
	}{
		{name: "scp-like", in: "git@git.example.com:group/repo.git", want: SSHTarget{Host: "git.example.com", Port: 22}, wantOK: true},
		{name: "scp-like 无用户名", in: "git.example.com:group/repo.git", want: SSHTarget{Host: "git.example.com", Port: 22}, wantOK: true},
		{name: "scp-like 大写主机", in: "git@Git.Example.COM:group/repo.git", want: SSHTarget{Host: "git.example.com", Port: 22}, wantOK: true},
		{name: "ssh scheme", in: "ssh://git@git.example.com/group/repo.git", want: SSHTarget{Host: "git.example.com", Port: 22}, wantOK: true},
		{name: "ssh scheme 带端口", in: "ssh://git@git.example.com:2222/group/repo.git", want: SSHTarget{Host: "git.example.com", Port: 2222}, wantOK: true},
		{name: "ssh scheme IPv6", in: "ssh://git@[2001:db8::1]:2222/group/repo.git", want: SSHTarget{Host: "2001:db8::1", Port: 2222}, wantOK: true},
		{name: "https", in: "https://git.example.com/group/repo.git", wantOK: false},
		{name: "git 协议", in: "git://git.example.com/group/repo.git", wantOK: false},
		{name: "本地路径", in: "/data/src/git.example.com/group/repo", wantOK: false},
		{name: "空", in: "  ", wantOK: false},
		{name: "只有主机名", in: "git.example.com", wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseSSHTarget(c.in)
			if ok != c.wantOK {
				t.Fatalf("ParseSSHTarget(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			}
			if ok && got != c.want {
				t.Errorf("ParseSSHTarget(%q) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}
