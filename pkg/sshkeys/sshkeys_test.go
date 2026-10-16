package sshkeys

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"testing"
)

const (
	ed25519Key = "AAAAC3NzaC1lZDI1NTE5AAAAIB0h0h0h0h0h0h0h0h0h0h0h0h0h0h0h0h0h"
	rsaKey     = "AAAAB3NzaC1yc2EAAAADAQABAAABgQDZ0h0h0h0h0h0h0h0h0h0h0h0h0h0h0h"
)

func TestHostPattern(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
	}{
		{"git.example.com", 22, "git.example.com"},
		{"git.example.com", 0, "git.example.com"},
		{"Git.Example.COM", 22, "git.example.com"},
		{"git.example.com", 2222, "[git.example.com]:2222"},
	}
	for _, c := range cases {
		if got := HostPattern(c.host, c.port); got != c.want {
			t.Errorf("HostPattern(%q, %d) = %q, want %q", c.host, c.port, got, c.want)
		}
	}
}

func TestContains(t *testing.T) {
	hashed := hashedLine("git.example.com", []byte("0123456789abcdef"))
	cases := []struct {
		name string
		data string
		host string
		port int
		want bool
	}{
		{
			name: "普通条目",
			data: "git.example.com ssh-ed25519 " + ed25519Key + "\n",
			host: "git.example.com", port: 22, want: true,
		},
		{
			name: "大小写不敏感",
			data: "git.example.com ssh-ed25519 " + ed25519Key + "\n",
			host: "Git.Example.com", port: 22, want: true,
		},
		{
			name: "端口只在方括号形式里匹配",
			data: "[git.example.com]:2222 ssh-ed25519 " + ed25519Key + "\n",
			host: "git.example.com", port: 2222, want: true,
		},
		{
			name: "非默认端口不会被裸主机名条目命中",
			data: "git.example.com ssh-ed25519 " + ed25519Key + "\n",
			host: "git.example.com", port: 2222, want: false,
		},
		{
			name: "默认端口不会被带端口的条目命中",
			data: "[git.example.com]:2222 ssh-ed25519 " + ed25519Key + "\n",
			host: "git.example.com", port: 22, want: false,
		},
		{
			name: "逗号分隔的多个主机",
			data: "other.example.com,git.example.com ssh-rsa " + rsaKey + "\n",
			host: "git.example.com", port: 22, want: true,
		},
		{
			name: "通配模式",
			data: "*.example.com ssh-ed25519 " + ed25519Key + "\n",
			host: "git.internal.example.com", port: 22, want: true,
		},
		{
			name: "否定模式优先",
			data: "*.example.com,!bad.example.com ssh-ed25519 " + ed25519Key + "\n",
			host: "bad.example.com", port: 22, want: false,
		},
		{
			name: "哈希条目",
			data: hashed + " ssh-ed25519 " + ed25519Key + "\n",
			host: "git.example.com", port: 22, want: true,
		},
		{
			name: "哈希条目端口不匹配",
			data: hashed + " ssh-ed25519 " + ed25519Key + "\n",
			host: "git.example.com", port: 2222, want: false,
		},
		{
			name: "注释与空行被忽略",
			data: "# git.example.com:22 SSH-2.0-OpenSSH_9.6\n\n",
			host: "git.example.com", port: 22, want: false,
		},
		{
			name: "条目字段不足时忽略",
			data: "git.example.com ssh-ed25519\n",
			host: "git.example.com", port: 22, want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Contains([]byte(c.data), c.host, c.port); got != c.want {
				t.Errorf("Contains(%q, %d) = %v, want %v", c.host, c.port, got, c.want)
			}
		})
	}
}

func TestParseScan(t *testing.T) {
	data := "# git.example.com:22 SSH-2.0-OpenSSH_9.6\n" +
		"# git.example.com:22 SSH-2.0-OpenSSH_9.6\n" +
		"git.example.com ssh-rsa " + rsaKey + "\n" +
		"git.example.com ssh-ed25519 " + ed25519Key + "\n" +
		"git.example.com ecdsa-sha2-nistp256 AAAAECDSA\n" +
		"\n" +
		"git.example.com not-a-key-type " + ed25519Key + "\n" +
		"git.example.com ssh-ed25519\n"
	got := ParseScan([]byte(data))
	if len(got) != 3 {
		t.Fatalf("ParseScan = %v, want 3 条（注释/空行/非法算法/字段不足都该丢掉）", got)
	}
	if got[0] != (Entry{Host: "git.example.com", Type: "ssh-rsa", Key: rsaKey}) {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[2].Type != "ecdsa-sha2-nistp256" {
		t.Errorf("got[2] = %+v", got[2])
	}
	if want := "git.example.com ssh-rsa " + rsaKey; got[0].String() != want {
		t.Errorf("String() = %q, want %q", got[0].String(), want)
	}
}

func TestMissingDropsDuplicates(t *testing.T) {
	entries := []Entry{
		{Host: "git.example.com", Type: "ssh-ed25519", Key: ed25519Key},
		{Host: "git.example.com", Type: "ssh-rsa", Key: rsaKey},
		{Host: "git.example.com", Type: "ssh-ed25519", Key: ed25519Key}, // 同一次输出里的重复
	}
	existing := "git.example.com ssh-ed25519 " + ed25519Key + "\n"
	got := Missing([]byte(existing), entries)
	if len(got) != 1 || got[0].Type != "ssh-rsa" {
		t.Fatalf("Missing = %v, want 只剩 ssh-rsa 一条", got)
	}
	if again := Missing([]byte(existing), got); len(again) != 1 {
		t.Fatalf("第二次调用不该把已写入的条目重复留下：%v", again)
	}
}

func TestMissingTreatsEverythingAsMissingOnEmptyFile(t *testing.T) {
	entries := ParseScan([]byte("git.example.com ssh-ed25519 " + ed25519Key + "\n"))
	if got := Missing(nil, entries); len(got) != 1 {
		t.Fatalf("Missing(nil) = %v, want 1", got)
	}
}

// hashedLine 按 OpenSSH 的 HashKnownHosts 格式算一行，用来验证哈希条目的匹配。
func hashedLine(host string, salt []byte) string {
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
