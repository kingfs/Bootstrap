// Package sshkeys 处理 known_hosts 里的主机公钥：判断某个 host:port 是否已经可信、
// 解析 ssh-keyscan 的输出、按 OpenSSH 的约定渲染条目。
//
// 这里全是纯字符串/哈希逻辑（不发网络、不碰文件），因此可以表驱动测试覆盖；
// 真正执行 ssh-keyscan 与追加 known_hosts 的事情在 internal/gitrepo（见 hostkey.go）。
package sshkeys

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"strings"
)

// HostPattern 返回 host:port 在 known_hosts 里的主机写法。
//
// OpenSSH 的约定：默认 22 端口写裸主机名，其余端口写 `[host]:port`；
// 主机名一律按小写比较（ssh 自己也会先把主机名小写化再做匹配与哈希）。
func HostPattern(host string, port int) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if port <= 0 || port == 22 {
		return h
	}
	return "[" + h + "]:" + strconv.Itoa(port)
}

// Contains 报告 known_hosts 的内容里是否已经有该主机的主机公钥。
//
// 只判断"有没有"，不校验公钥本身：调用方（首次连接前）据此决定要不要跑
// ssh-keyscan；已有条目说明这台机器以前信任过它，此时绝不改动文件。
func Contains(data []byte, host string, port int) bool {
	name := HostPattern(host, port)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if matchPatternList(fields[0], name) {
			return true
		}
	}
	return false
}

// Entry 是一条 known_hosts 形式的公钥记录。
type Entry struct {
	Host string // 主机模式（默认端口是裸主机名，其余是 [host]:port）
	Type string // ssh-ed25519 / ecdsa-sha2-nistp256 / ssh-rsa / ...
	Key  string // base64 公钥
}

// String 渲染成一行 known_hosts 内容（不含换行）。
func (e Entry) String() string {
	return e.Host + " " + e.Type + " " + e.Key
}

// ParseScan 解析 ssh-keyscan 的输出。
//
// 只保留形如 `<host> <算法> <base64>` 的行：注释（ssh-keyscan 会写
// `# host:port SSH-2.0-OpenSSH_...`）与其它噪声都丢掉——写进用户 known_hosts 的
// 每一行都必须是 ssh 认得的格式，宁可少写也不能写坏。
func ParseScan(data []byte) []Entry {
	var out []Entry
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if !IsKeyType(fields[1]) || fields[2] == "" {
			continue
		}
		out = append(out, Entry{Host: fields[0], Type: fields[1], Key: fields[2]})
	}
	return out
}

// IsKeyType 报告算法名是否是 OpenSSH 认得的公钥类型。
func IsKeyType(typ string) bool {
	switch typ {
	case "ssh-rsa", "ssh-dss", "ssh-ed25519":
		return true
	}
	// ecdsa-sha2-nistp256/384/521 与 FIDO 的 sk-* 类型：前缀匹配足够，
	// 因为这里只是"挡住明显不是公钥的噪声"，不是在做算法白名单。
	return strings.HasPrefix(typ, "ecdsa-sha2-") ||
		strings.HasPrefix(typ, "sk-ssh-ed25519@") ||
		strings.HasPrefix(typ, "sk-ecdsa-sha2-")
}

// Missing 返回 entries 里还没有出现在 data 中的条目（按 主机+算法+公钥 判重）。
//
// 用途：ssh-keyscan 会把 rsa/ecdsa/ed25519 一起扫回来，其中一部分可能已经在
// known_hosts 里了；只追加缺的那些，重复执行才不会改变文件（AGENTS §4.3 的幂等要求）。
func Missing(data []byte, entries []Entry) []Entry {
	have := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		have[fingerprint(fields[0], fields[1], fields[2])] = true
	}
	var out []Entry
	for _, e := range entries {
		fp := fingerprint(e.Host, e.Type, e.Key)
		if have[fp] {
			continue
		}
		have[fp] = true // 同一次 keyscan 输出里的重复条目也只追加一次
		out = append(out, e)
	}
	return out
}

func fingerprint(host, typ, key string) string {
	return strings.ToLower(strings.TrimSpace(host)) + " " + typ + " " + key
}

// matchPatternList 按 OpenSSH 语义匹配逗号分隔的主机模式列表。
//
// 规则：任一带 `!` 的否定模式命中即视为不匹配（优先于肯定模式）；否则任一肯定模式
// 命中即匹配。否定优先是 ssh 自己的行为，改掉会让 `*.example.com,!bad.example.com`
// 这类写法失去意义。
func matchPatternList(list, name string) bool {
	matched := false
	for _, p := range strings.Split(list, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		negated := strings.HasPrefix(p, "!")
		if negated {
			p = p[1:]
		}
		if !matchPattern(p, name) {
			continue
		}
		if negated {
			return false
		}
		matched = true
	}
	return matched
}

func matchPattern(pattern, name string) bool {
	if strings.HasPrefix(pattern, "|") {
		return matchHashed(pattern, name)
	}
	return globMatch(strings.ToLower(pattern), name)
}

// matchHashed 匹配 HashKnownHosts 写出的 `|1|<base64 salt>|<base64 HMAC-SHA1>`。
//
// 哈希的对象是"查表用的主机名"（含端口的方括号形式），不是模式，因此必须先算
// 再比；这也是 OpenSSH 自己 hostfile.c 的做法。
func matchHashed(pattern, name string) bool {
	parts := strings.Split(pattern, "|")
	if len(parts) != 4 || parts[1] != "1" {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(name))
	return hmac.Equal(mac.Sum(nil), want)
}

// globMatch 是 known_hosts 主机模式的通配匹配：`*` 匹配任意长度（含空）、`?` 匹配单个字符。
//
// 反斜杠转义（OpenSSH 支持 `\*` 这类写法）刻意不支持：常见的 known_hosts 里几乎不出现，
// 而"把带反斜杠的模式当成普通字符去比较"只会让它匹配不上——宁可保守地认为"不知道"，
// 也不能把不该信任的主机判成已信任。
func globMatch(pattern, name string) bool {
	var (
		p, n       = 0, 0
		star, mark = -1, 0
	)
	for n < len(name) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == name[n]):
			p++
			n++
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, n
			p++
		case star >= 0:
			p = star + 1
			mark++
			n = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
