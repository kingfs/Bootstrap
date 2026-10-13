// Package proxyenv 把代理配置渲染成各种 shell 可 eval 的环境变量文本。
//
// 纯字符串变换，不涉及任何文件或进程操作；被 internal/proxy、internal/app（env 命令）
// 与 internal/shellsnip 三处共用，因此放在 pkg/ 下。
package proxyenv

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Profile 是一组代理环境变量设置。
type Profile struct {
	Name       string `mapstructure:"-" yaml:"-"`
	HTTPProxy  string `mapstructure:"http_proxy"  yaml:"http_proxy"`
	HTTPSProxy string `mapstructure:"https_proxy" yaml:"https_proxy"`
	NoProxy    string `mapstructure:"no_proxy"    yaml:"no_proxy"`
	AllProxy   string `mapstructure:"all_proxy"   yaml:"all_proxy"`
}

// IsZero 报告该 profile 是否没有任何代理值。
func (p Profile) IsZero() bool {
	return p.HTTPProxy == "" && p.HTTPSProxy == "" && p.NoProxy == "" && p.AllProxy == ""
}

// Shell 标识目标 shell 的语法。
type Shell string

// 支持的 shell 方言。
const (
	ShellBash   Shell = "bash"
	ShellZsh    Shell = "zsh"
	ShellFish   Shell = "fish"
	ShellPwsh   Shell = "pwsh"
	ShellCmd    Shell = "cmd"
	ShellDotenv Shell = "dotenv"
	ShellJSON   Shell = "json"
)

// ProxyKeys 是渲染顺序固定的变量表（大写 + 小写成对出现）。
//
// 顺序固定是为了输出可测试、可 diff：bash/zsh/fish 都同时设置大小写两套，
// 与维护者原有 rc 脚本的行为完全一致。
var ProxyKeys = []string{
	"HTTP_PROXY", "http_proxy",
	"HTTPS_PROXY", "https_proxy",
	"ALL_PROXY", "all_proxy",
	"NO_PROXY", "no_proxy",
}

// NormalizeShell 把用户输入的 shell 名归一化为受支持的方言。
func NormalizeShell(name string) (Shell, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bash", "sh", "dash":
		return ShellBash, nil
	case "zsh":
		return ShellZsh, nil
	case "fish":
		return ShellFish, nil
	case "pwsh", "powershell", "powershell.exe", "ps1":
		return ShellPwsh, nil
	case "cmd", "cmd.exe", "batch", "bat":
		return ShellCmd, nil
	case "dotenv", "env":
		return ShellDotenv, nil
	case "json":
		return ShellJSON, nil
	default:
		return "", fmt.Errorf("unsupported shell %q (want bash|zsh|fish|pwsh|cmd|dotenv|json)", name)
	}
}

// Env 返回该 profile 对应的环境变量键值对（大写与小写）。
func (p Profile) Env() map[string]string {
	env := map[string]string{}
	set := func(k, v string) {
		if strings.TrimSpace(v) == "" {
			return
		}
		env[k] = v
	}
	set("HTTP_PROXY", p.HTTPProxy)
	set("http_proxy", p.HTTPProxy)
	set("HTTPS_PROXY", p.HTTPSProxy)
	set("https_proxy", p.HTTPSProxy)
	set("ALL_PROXY", p.AllProxy)
	set("all_proxy", p.AllProxy)
	set("NO_PROXY", p.NoProxy)
	set("no_proxy", p.NoProxy)
	return env
}

// Render 把环境变量集合渲染为指定 shell 的语句，顺序与 ProxyKeys 一致。
func Render(shell Shell, env map[string]string) (string, error) {
	if shell == ShellJSON {
		return renderJSON(env)
	}
	var lines []string
	for _, k := range orderedKeys(env) {
		v := env[k]
		line, err := setStatement(shell, k, v)
		if err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	return join(lines), nil
}

// RenderProfile 渲染一个 profile（等价于 Render(shell, profile.Env())）。
func RenderProfile(shell Shell, p Profile) (string, error) {
	return Render(shell, p.Env())
}

// RenderOff 渲染"关闭代理"的语句：清空全部 ProxyKeys。
//
// 这是 `bs proxy off` 能在当前 shell 真正生效的关键——只 unset 我们设置过的键，
// 不动其他环境变量。
func RenderOff(shell Shell) (string, error) {
	if shell == ShellJSON {
		return "{}", nil
	}
	switch shell {
	case ShellBash, ShellZsh:
		return "unset " + strings.Join(ProxyKeys, " "), nil
	case ShellFish:
		parts := make([]string, 0, len(ProxyKeys))
		for _, k := range ProxyKeys {
			parts = append(parts, "set -e "+k)
		}
		return strings.Join(parts, "; "), nil
	case ShellPwsh:
		parts := make([]string, 0, len(ProxyKeys))
		for _, k := range ProxyKeys {
			parts = append(parts, "Remove-Item Env:"+k+" -ErrorAction SilentlyContinue")
		}
		return strings.Join(parts, "; "), nil
	case ShellCmd:
		parts := make([]string, 0, len(ProxyKeys))
		for _, k := range ProxyKeys {
			parts = append(parts, "set "+k+"=")
		}
		return strings.Join(parts, "\r\n"), nil
	case ShellDotenv:
		parts := make([]string, 0, len(ProxyKeys))
		for _, k := range ProxyKeys {
			parts = append(parts, k+"=")
		}
		return join(parts), nil
	default:
		return "", fmt.Errorf("unsupported shell %q", shell)
	}
}

func setStatement(shell Shell, key, value string) (string, error) {
	switch shell {
	case ShellBash, ShellZsh:
		return "export " + key + "=" + singleQuote(value), nil
	case ShellFish:
		return "set -gx " + key + " " + singleQuote(value), nil
	case ShellPwsh:
		return "$env:" + key + " = " + singleQuote(value), nil
	case ShellCmd:
		return `set "` + key + "=" + value + `"`, nil
	case ShellDotenv:
		return key + "=" + value, nil
	default:
		return "", fmt.Errorf("unsupported shell %q", shell)
	}
}

func orderedKeys(env map[string]string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, k := range ProxyKeys {
		if _, ok := env[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	// 理论上不会出现未知键，但保持确定性：排序后追加。
	var extra []string
	for k := range env {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(keys, extra...)
}

func renderJSON(env map[string]string) (string, error) {
	out := map[string]string{}
	for k, v := range env {
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("marshal proxy env: %w", err)
	}
	return string(b), nil
}

func join(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// singleQuote 用单引号包裹，内部单引号按 POSIX 规则转义。
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// MaskProxyURL 隐去代理 URL 中的凭据，用于日志与终端输出。
//
// 手工拼接而不是走 url.URL.String()：后者会把 "*" 转义成 %2A，日志里很难看。
func MaskProxyURL(raw string) string {
	if raw == "" {
		return ""
	}
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	rest := raw[i+3:]
	slash := strings.IndexByte(rest, '/')
	at := strings.LastIndexByte(rest, '@')
	if at < 0 || (slash >= 0 && at > slash) {
		return raw
	}
	return raw[:i+3] + "***@" + rest[at+1:]
}
