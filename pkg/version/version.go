// Package version 提供版本号抽取与比较，不含任何业务语义。
//
// 被 internal/detect（判断是否已安装/是否需升级）、清单校验（min_version）共用。
package version

import (
	"regexp"
	"strconv"
	"strings"
)

// Version 是一个宽松解析的版本号：主数字段 + 预发布后缀。
//
// 之所以不用完整 semver 库：工具链的 `--version` 输出五花八门，我们需要的是
// "能比较大小"而不是"严格校验合法性"。
type Version struct {
	Numbers []int  // 例如 1.24.0 -> [1 24 0]
	Pre     string // 预发布后缀，例如 "rc1"；空表示正式版
	Raw     string
}

// Parse 解析版本字符串。容忍前缀 v/V、任意非数字前后缀、缺省的分量。
func Parse(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	v := Version{Raw: raw}
	if raw == "" {
		return v, &ParseError{Input: raw, Reason: "empty version string"}
	}

	// 去掉前导的非数字噪声（例如 "g version "、"go version go"、"v"）。
	i := 0
	for i < len(raw) && (raw[i] < '0' || raw[i] > '9') {
		i++
	}
	body := raw[i:]
	if body == "" {
		return v, &ParseError{Input: raw, Reason: "no digits found"}
	}

	// 预发布后缀由第一个 '-' 开始；构建元数据 '+' 之后的内容直接丢弃
	// （semver 中 build metadata 不参与比较）。
	if idx := strings.IndexByte(body, '-'); idx >= 0 {
		pre := body[idx+1:]
		if j := strings.IndexByte(pre, '+'); j >= 0 {
			pre = pre[:j]
		}
		v.Pre = strings.TrimSpace(pre)
		body = body[:idx]
	} else if idx := strings.IndexByte(body, '+'); idx >= 0 {
		body = body[:idx]
	}

	for _, part := range strings.Split(body, ".") {
		n, err := leadingInt(part)
		if err != nil {
			break
		}
		v.Numbers = append(v.Numbers, n)
	}
	if len(v.Numbers) == 0 {
		return v, &ParseError{Input: raw, Reason: "no numeric components"}
	}
	return v, nil
}

// ParseError 表示版本字符串无法解析。
type ParseError struct {
	Input  string
	Reason string
}

func (e *ParseError) Error() string {
	return "parse version " + strconv.Quote(e.Input) + ": " + e.Reason
}

func leadingInt(s string) (int, error) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, strconv.ErrSyntax
	}
	return strconv.Atoi(s[:i])
}

func (v Version) at(i int) int {
	if i < len(v.Numbers) {
		return v.Numbers[i]
	}
	return 0
}

// Compare 比较两个版本：a<b 返回 -1，a==b 返回 0，a>b 返回 1。
//
// 预发布版本小于同号正式版（1.24.0-rc1 < 1.24.0）。
func Compare(a, b string) int {
	va, errA := Parse(a)
	vb, errB := Parse(b)
	if errA != nil || errB != nil {
		// 解析不了就退化为字符串比较，保证确定性而非 panic。
		return strings.Compare(strings.TrimSpace(a), strings.TrimSpace(b))
	}
	n := len(va.Numbers)
	if len(vb.Numbers) > n {
		n = len(vb.Numbers)
	}
	for i := 0; i < n; i++ {
		if va.at(i) != vb.at(i) {
			if va.at(i) < vb.at(i) {
				return -1
			}
			return 1
		}
	}
	switch {
	case va.Pre == vb.Pre:
		return 0
	case va.Pre == "":
		return 1 // 正式版 > 预发布
	case vb.Pre == "":
		return -1
	default:
		return strings.Compare(va.Pre, vb.Pre)
	}
}

// Less 报告 a 是否小于 b。
func Less(a, b string) bool { return Compare(a, b) < 0 }

// Equal 报告两个版本是否等价（忽略前缀噪声）。
func Equal(a, b string) bool { return Compare(a, b) == 0 }

// MeetsMin 报告 actual 是否 >= min；min 为空表示不约束。
func MeetsMin(actual, min string) bool {
	if strings.TrimSpace(min) == "" {
		return true
	}
	if strings.TrimSpace(actual) == "" {
		return false
	}
	return Compare(actual, min) >= 0
}

// Extract 用带命名组 "v" 的正则（或首个捕获组）从输出中抽取版本字符串。
//
// 正则应在清单加载期编译一次，避免每条命令都重复编译。
func Extract(re *regexp.Regexp, s string) (string, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	if re.SubexpIndex("v") > 0 {
		return m[re.SubexpIndex("v")], true
	}
	if len(m) > 1 {
		return m[1], true
	}
	return m[0], true
}

// IsLatest 报告版本标记是否表示"最新"。
func IsLatest(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "latest", "stable", "lts":
		return true
	default:
		return false
	}
}
