package install

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/kingfs/Bootstrap/pkg/ghrelease"
)

// checksumSource 是 `params.checksum_url` 解析出来的"去哪取、在正文里找哪个字段"。
type checksumSource struct {
	// URL 是真正要请求的地址（`#` 之前的部分）。
	URL string
	// Pointer 是 `#` 之后的 RFC 6901 JSON Pointer；为空表示按校验和文件的行式格式解析。
	Pointer string
}

// label 是错误信息里对这个来源的称呼：短，且能看出指的是正文本身还是某个字段。
func (s checksumSource) label() string {
	if s.Pointer == "" {
		return "body"
	}
	return "#" + s.Pointer
}

// parseChecksumSource 拆开 `params.checksum_url`。
//
// 允许 `https://host/file.json#/a/b/0` 这种写法：上游把哈希放在 JSON 的某个字段里时
// （herdr 的 latest.json、zig 的 download/index.json），用标准 JSON Pointer 指出来，
// 比"猜一个字段名"可靠得多；片段本来就不会发给服务端，这里自己拆掉。
func parseChecksumSource(raw string) (checksumSource, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return checksumSource{}, fmt.Errorf("empty url")
	}
	base, pointer, _ := strings.Cut(raw, "#")
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return checksumSource{}, fmt.Errorf("invalid url %q: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return checksumSource{}, fmt.Errorf("url %q must be http(s)", base)
	}
	pointer = strings.TrimSpace(pointer)
	if pointer == "" {
		return checksumSource{URL: u.String()}, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return checksumSource{}, fmt.Errorf("json pointer %q must start with /", pointer)
	}
	return checksumSource{URL: u.String(), Pointer: pointer}, nil
}

// checksumFromBody 从校验和来源的正文里取 sha256。
//
// 两种形态：给了 JSON Pointer 就按 JSON 取值（值可以是裸哈希或 `sha256:<hex>`）；
// 没给就按行式校验和文件解析（`<hex>  <filename>`，或整份只有一个裸哈希）。
// 返回 false 一律表示"这份正文里没有可用的 sha256"——包括正文其实是 HTML 的情况
// （不少站点对任何路径都回 200 + 首页，不能把 200 当成"找到了"）。
func checksumFromBody(body string, src checksumSource, asset string) (string, bool) {
	if src.Pointer == "" {
		if sha, err := ghrelease.ChecksumFor(body, asset); err == nil {
			return sha, true
		}
		// 再接受"整份正文就是一个裸哈希"（Rust 生态的 `<asset>.sha256` 就是这么写的）。
		// 不能退化成"取第一行的哈希"：多资产清单里那一行是**别的文件**的哈希，
		// 拿它去校验只会得到一次莫名其妙的 checksum mismatch。
		if len(strings.Fields(strings.TrimSpace(body))) != 1 {
			return "", false
		}
		return ghrelease.BareChecksum(body)
	}
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return "", false
	}
	raw, ok := jsonPointerValue(doc, src.Pointer)
	if !ok {
		return "", false
	}
	return ghrelease.NormalizeSHA256(raw)
}

// jsonPointerValue 按 RFC 6901 取值，只接受字符串节点。
//
// 数字与嵌套对象都不算哈希：宁可当作"这个来源没有"，也不要拿一个不相干的值去校验。
func jsonPointerValue(doc any, pointer string) (string, bool) {
	cur := doc
	for _, tok := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[tok]
			if !ok {
				return "", false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(node) {
				return "", false
			}
			cur = node[idx]
		default:
			return "", false
		}
	}
	s, ok := cur.(string)
	if !ok {
		return "", false
	}
	return s, true
}

// checksumAttempts 记录"试过哪些来源、各是什么结论"。
//
// 校验和来源有好几个，把每个候选的完整 URL 与响应体都拼进错误信息会让一行错误长到没法读
// （用户实测反馈：一条 1000 字符、里面还塞着 403 的 HTML）。这里只留"来源名 + 一句话结论"，
// 完整细节在各来源自己的 debug 日志里（`msg=http get=… err=…`）。
type checksumAttempts []checksumAttempt

type checksumAttempt struct {
	source  string
	outcome string
}

func (a *checksumAttempts) add(source, outcome string) {
	*a = append(*a, checksumAttempt{source: source, outcome: strings.TrimSpace(outcome)})
}

// len 为 0 时 String 返回空串（调用方据此决定要不要打印括号）。
func (a checksumAttempts) String() string {
	var order []string
	grouped := map[string][]string{}
	for _, at := range a {
		if _, seen := grouped[at.outcome]; !seen {
			order = append(order, at.outcome)
		}
		grouped[at.outcome] = append(grouped[at.outcome], at.source)
	}
	parts := make([]string, 0, len(order))
	for _, outcome := range order {
		joined := strings.Join(grouped[outcome], ", ")
		if outcome == "" {
			parts = append(parts, joined)
			continue
		}
		parts = append(parts, joined+": "+outcome)
	}
	return strings.Join(parts, "; ")
}

// rateLimited 报告是否撞上了 GitHub API 的限流，用于补一句"设 GITHUB_TOKEN"的提示。
func (a checksumAttempts) rateLimited() bool {
	for _, at := range a {
		if at.outcome == rateLimitOutcome {
			return true
		}
	}
	return false
}

// rateLimitOutcome 是 GitHub API 匿名配额（60 次/小时/IP）用尽时的统一说法。
const rateLimitOutcome = "HTTP 403 (rate limit)"

var httpStatusIn = regexp.MustCompile(`returned HTTP (\d{3})`)

// briefFetchError 把 httpx 的长错误压成一句话结论。
//
// `https://… returned HTTP 404: <!DOCTYPE html>…` 里的 URL 与响应体对"接下来怎么办"
// 没有帮助；非 HTTP 错误（DNS、超时、TLS）保留首行的一句话，那通常就是结论本身。
func briefFetchError(err error) string {
	if err == nil {
		return ""
	}
	msg := firstLineOf(err.Error())
	if m := httpStatusIn.FindStringSubmatch(msg); m != nil {
		if (m[1] == "403" || m[1] == "429") && strings.Contains(strings.ToLower(msg), "rate limit") {
			return rateLimitOutcome
		}
		return "HTTP " + m[1]
	}
	// 传输层错误（DNS / 超时 / TLS / 连接被拒）：Go 会把 URL 重复说两遍
	//（`GET <url>: Get "<url>": …`），去掉这层壳只留结论。
	// 但 `(hint: …)` 常常是唯一可行动的信息，截断时单独保住它。
	head, hint := msg, ""
	if i := strings.Index(msg, " (hint: "); i >= 0 {
		head, hint = msg[:i], msg[i:]
	}
	out := truncateRunes(strings.TrimSpace(transportPrefix.ReplaceAllString(head, "")), 80) + hint
	return truncateRunes(out, 200)
}

// transportPrefix 匹配 `GET <url>: Get "<url>": ` 这层重复的 URL 壳。
var transportPrefix = regexp.MustCompile(`^(?:GET \S+: )?Get "?[^" ]+"?: `)

// truncateRunes 按字符（而不是字节）截断，避免把多字节字符切成半个。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
