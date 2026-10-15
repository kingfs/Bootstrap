// Package giturl 把用户给出的各种仓库地址写法归一化为一个仓库引用。
//
// 支持的输入形态（详见 docs/design.md §9.1）：
//
//	kingfs/Bootstrap
//	https://github.com/kingfs/Bootstrap
//	https://github.com/kingfs/Bootstrap.git
//	git@github.com:kingfs/Bootstrap.git
//	ssh://git@gitlab.com/group/sub/proj.git
//	https://github.com/kingfs/Bootstrap/tree/main/internal/foo
//	https://github.com/kingfs/Bootstrap/releases/tag/v0.3.0
//	https://github.com/kingfs/Bootstrap#dev
//
// 纯字符串处理，不发任何网络请求，因此可被大量表驱动测试覆盖。
package giturl

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultHost 是简写形式（owner/repo）默认归属的主机。
const DefaultHost = "github.com"

// Ref 是一个归一化后的仓库引用。
type Ref struct {
	Host    string // github.com / gitlab.com / <自建域名>
	Owner   string // kingfs；GitLab 多级 namespace 形如 group/sub
	Repo    string // Bootstrap
	Ref     string // 分支/标签/提交，可空
	SubPath string // 仓库内路径，可空
	Raw     string // 原始输入
	SCP     bool   // 输入是否是 git@host:path 形式
	Scheme  string // https / http / ssh / git，可空
}

// Slug 返回 host/owner/repo 三段式标识。
func (r Ref) Slug() string {
	return r.Host + "/" + r.Owner + "/" + r.Repo
}

// String 返回人类可读的完整标识（含 ref/subpath）。
func (r Ref) String() string {
	s := r.Slug()
	if r.Ref != "" {
		s += "@" + r.Ref
	}
	if r.SubPath != "" {
		s += " (" + r.SubPath + ")"
	}
	return s
}

// CloneURL 按协议生成 git clone 用的地址。
//
// protocol 取值 "https"（默认）或 "ssh"；来自 scp-like 或 ssh scheme 的输入天然按 ssh 处理。
func (r Ref) CloneURL(protocol string) string {
	if strings.EqualFold(protocol, "ssh") || r.SCP || r.Scheme == "ssh" || r.Scheme == "git" {
		return "git@" + r.Host + ":" + r.Owner + "/" + r.Repo + ".git"
	}
	return "https://" + r.Host + "/" + r.Owner + "/" + r.Repo + ".git"
}

// webPathKeywords 是出现在 owner/repo 之后、用于定位 ref 与子路径的网页路径关键字。
//
// 只在段下标 >= 2 处匹配，因此 owner 与 repo 自身叫这些名字不受影响
// （仓库名在 index 1，多级 namespace 的中间段在 index >= 2，存在理论歧义，见 docs）。
var webPathKeywords = map[string]bool{
	"tree": true, "blob": true, "commit": true, "commits": true,
	"releases": true, "pull": true, "pulls": true, "merge_requests": true,
	"issues": true, "compare": true, "raw": true, "archive": true,
	"wiki": true, "actions": true, "blame": true, "tags": true,
	"branches": true, "network": true, "discussions": true,
	"security": true, "projects": true, "graphs": true, "settings": true,
}

var segmentRe = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// Parse 归一化输入。defaultHost 为空时使用 DefaultHost。
func Parse(raw, defaultHost string) (Ref, error) {
	if defaultHost == "" {
		defaultHost = DefaultHost
	}
	in := strings.TrimSpace(raw)
	if in == "" {
		return Ref{}, fmt.Errorf("empty repository reference")
	}
	ref := Ref{Raw: raw, Host: defaultHost}

	// 1) 去掉 query 与 fragment；fragment 可能是 ref 分隔符（repo.git#branch）。
	if i := strings.IndexAny(in, "?#"); i >= 0 {
		if in[i] == '#' {
			frag := strings.Trim(in[i+1:], "/")
			if frag != "" && !strings.Contains(frag, "/") {
				ref.Ref = frag
			}
		}
		in = in[:i]
	}
	in = strings.TrimSuffix(strings.TrimSpace(in), "/")
	if in == "" {
		return Ref{}, fmt.Errorf("empty repository reference %q", raw)
	}

	var path string
	switch {
	case strings.Contains(in, "://"):
		scheme, rest, _ := strings.Cut(in, "://")
		ref.Scheme = strings.ToLower(scheme)
		if at := strings.LastIndex(rest, "@"); at >= 0 && !strings.Contains(rest[:at], "/") {
			rest = rest[at+1:]
		}
		host, p, ok := strings.Cut(rest, "/")
		if !ok {
			return Ref{}, fmt.Errorf("cannot parse repository reference %q: missing path", raw)
		}
		ref.Host = strings.ToLower(host)
		path = p
	case scpLike(in):
		ref.SCP = true
		ref.Scheme = "ssh"
		before, after, _ := strings.Cut(in, ":")
		if at := strings.LastIndex(before, "@"); at >= 0 {
			before = before[at+1:]
		}
		ref.Host = strings.ToLower(before)
		path = after
	default:
		path = in
	}

	segs := splitSegments(path)
	if len(segs) == 0 {
		return Ref{}, fmt.Errorf("cannot parse repository reference %q: no path segments", raw)
	}

	// 2) 判定首段是 host 还是 owner（简写形式）。
	if ref.Scheme == "" && !ref.SCP && looksLikeHost(segs[0]) {
		ref.Host = strings.ToLower(segs[0])
		segs = segs[1:]
	}
	if len(segs) < 2 {
		return Ref{}, fmt.Errorf("cannot parse repository reference %q: expected owner/repo", raw)
	}

	// 3) 切出 owner/repo 与网页路径部分。
	repoSegs, wp := segs, webPart{}
	if !ref.SCP && ref.Scheme != "ssh" && ref.Scheme != "git" && !strings.HasSuffix(segs[len(segs)-1], ".git") {
		if ref.Host == "github.com" || ref.Host == "gitee.com" || ref.Host == "codeberg.org" || ref.Host == "bitbucket.org" {
			repoSegs, wp = splitAtWebKeyword(segs)
		} else {
			// GitLab 用 /-/ 分隔 namespace 和网页路径，不能把子组名 tree/issues 当关键字。
			for i := 2; i+1 < len(segs); i++ {
				if segs[i] == "-" && webPathKeywords[strings.ToLower(segs[i+1])] {
					repoSegs = segs[:i]
					wp = webPart{keyword: strings.ToLower(segs[i+1]), rest: segs[i+2:]}
					break
				}
			}
		}
	}
	if len(repoSegs) < 2 {
		return Ref{}, fmt.Errorf("cannot parse repository reference %q: expected owner/repo, got %q", raw, strings.Join(segs, "/"))
	}
	ref.Repo = strings.TrimSuffix(repoSegs[len(repoSegs)-1], ".git")
	ref.Owner = strings.Join(repoSegs[:len(repoSegs)-1], "/")

	// 4) 从网页路径提取 ref 与子路径（显式 fragment 优先）。
	webRef, subPath := interpretWebPath(wp)
	if ref.Ref == "" {
		ref.Ref = webRef
	}
	ref.SubPath = subPath

	if err := ref.validate(); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

func scpLike(in string) bool {
	i := strings.Index(in, ":")
	if i < 0 {
		return false
	}
	if strings.Contains(in[:i], "/") {
		return false
	}
	return strings.Contains(in, "@") || strings.Contains(in[:i], ".")
}

func splitSegments(p string) []string {
	raw := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

func looksLikeHost(seg string) bool {
	if strings.EqualFold(seg, "localhost") {
		return true
	}
	return strings.Contains(seg, ".") && !strings.HasPrefix(seg, ".")
}

// webPart 描述 owner/repo 之后的网页路径。
type webPart struct {
	keyword string   // tree / blob / releases / ...，空表示没有
	rest    []string // 关键字之后的段
}

func (w webPart) empty() bool { return w.keyword == "" }

// splitAtWebKeyword 把段列表切成 owner/repo 与网页路径两部分。
//
// 同时兼容 GitLab 的 `/-/tree/...` 分隔符（末尾的 "-" 段会被丢弃）。
func splitAtWebKeyword(segs []string) ([]string, webPart) {
	for i := 2; i < len(segs); i++ {
		if !webPathKeywords[strings.ToLower(segs[i])] {
			continue
		}
		repo := segs[:i]
		if len(repo) > 0 && repo[len(repo)-1] == "-" {
			repo = repo[:len(repo)-1]
		}
		return repo, webPart{keyword: strings.ToLower(segs[i]), rest: segs[i+1:]}
	}
	return segs, webPart{}
}

// interpretWebPath 把网页路径翻译成 (ref, subPath)。
func interpretWebPath(wp webPart) (string, string) {
	if wp.empty() {
		return "", ""
	}
	rest := wp.rest
	switch wp.keyword {
	case "tree", "blob":
		// 注意：分支名本身含 '/' 时（feat/x）无法离线消除歧义，按"第一段是 ref"处理，
		// 需要时用 `bs repo get <url> --ref feat/x` 显式覆盖。
		if len(rest) == 0 {
			return "", ""
		}
		return rest[0], strings.Join(rest[1:], "/")
	case "commit", "commits", "branches", "tags":
		if len(rest) == 0 {
			return "", ""
		}
		return rest[0], ""
	case "releases":
		if len(rest) >= 2 && strings.EqualFold(rest[0], "tag") {
			return rest[1], ""
		}
		return "", ""
	case "compare":
		if len(rest) == 0 {
			return "", ""
		}
		return strings.Join(rest, "/"), ""
	default:
		// issues / pull / actions / wiki 等：只识别到"这是仓库的某个页面"，
		// ref 与子路径留空，不做无根据的猜测。
		return "", ""
	}
}

func (r Ref) validate() error {
	if r.Host == "" {
		return fmt.Errorf("cannot parse repository reference %q: empty host", r.Raw)
	}
	if r.Owner == "" || r.Repo == "" {
		return fmt.Errorf("cannot parse repository reference %q: empty owner or repo", r.Raw)
	}
	for _, part := range strings.Split(r.Owner, "/") {
		if !segmentRe.MatchString(part) {
			return fmt.Errorf("cannot parse repository reference %q: invalid owner segment %q", r.Raw, part)
		}
	}
	if !segmentRe.MatchString(r.Repo) {
		return fmt.Errorf("cannot parse repository reference %q: invalid repo name %q", r.Raw, r.Repo)
	}
	return nil
}
