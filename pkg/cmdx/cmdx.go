// Package cmdx 是唯一允许执行外部命令的出口：统一处理 dry-run、超时、环境注入与日志脱敏。
//
// 所有需要跑外部程序的模块（gitrepo、install、doctor）都依赖本包的 Runner，
// 测试时用 Fake 替换即可在无网络、无特权、无副作用的前提下覆盖全部逻辑。
package cmdx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Command 描述一条待执行的外部命令。
type Command struct {
	Name string
	Args []string
	Dir  string
	// Env 是叠加在进程环境之上的额外变量（例如代理、PATH）。
	Env map[string]string
	// Stdin 可选；为空时继承 /dev/null（避免子进程误读终端）。
	Stdin io.Reader
	// Timeout 可选，覆盖 Runner 的默认超时。
	Timeout time.Duration
}

// String 返回可直接复制到终端执行的命令行（已做 shell 引用）。
func (c Command) String() string {
	parts := append([]string{c.Name}, c.Args...)
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		quoted = append(quoted, shellQuote(p))
	}
	return strings.Join(quoted, " ")
}

// Result 是一次执行的完整结果。
type Result struct {
	Command  Command
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	// DryRun 为真表示命令没有被真正执行。
	DryRun bool
}

// Failed 报告命令是否以非零状态结束。
func (r Result) Failed() bool { return r.ExitCode != 0 }

// ExitError 表示命令以非零状态退出。
type ExitError struct {
	Result Result

	cause error
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("command failed (exit %d): %s", e.Result.ExitCode, e.Result.Command.String())
	if s := strings.TrimSpace(lastLines(e.Result.Stderr, 3)); s != "" {
		msg += "\n" + s
	}
	return msg
}

// Unwrap 返回底层错误（如 exec.ExitError），便于 errors.As 继续下钻。
func (e *ExitError) Unwrap() error { return e.cause }

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Runner 执行外部命令。
type Runner struct {
	// DryRun 为真时只记录不执行。
	DryRun bool
	// Env 是每条命令都会叠加的基础环境变量（例如当前生效的代理）。
	Env map[string]string
	// Dir 是默认工作目录。
	Dir string
	// Stdout/Stderr 非空时，命令输出会同时写入这里（用于 -v 实时透传）。
	Stdout io.Writer
	Stderr io.Writer
	// Log 记录每条命令；nil 时静默。
	Log *slog.Logger
	// DefaultTimeout 为 0 时使用 DefaultCommandTimeout。
	DefaultTimeout time.Duration
	// Secrets 是在日志与输出中需要脱敏的字符串（例如代理 URL 里的密码）。
	Secrets []string
}

// DefaultCommandTimeout 是外部命令的默认超时（设计文档 §13）。
const DefaultCommandTimeout = 10 * time.Minute

// New 构造一个 Runner。
func New(log *slog.Logger) *Runner {
	return &Runner{Log: log, DefaultTimeout: DefaultCommandTimeout}
}

// IsDryRun 报告当前是否处于干跑模式。
//
// dry-run 下 Run 的返回值里本来就带 DryRun，但有的准备动作（写 known_hosts、
// 解析远端地址）要在发命令**之前**决定做不做，光看结果来不及。
func (r *Runner) IsDryRun() bool { return r.DryRun }

// Run 执行命令。
//
// 返回的 error 在"命令以非零状态退出"时是 *ExitError；dry-run 时永远返回 nil。
func (r *Runner) Run(ctx context.Context, c Command) (Result, error) {
	if c.Name == "" {
		return Result{}, errors.New("empty command name")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = r.DefaultTimeout
	}
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	// 上层（`--timeout`）可能挂了更短的总时限：报错文案要说**真正生效**的那个数，
	// 否则用户按 "(timeout 10m0s)" 去查，永远对不上自己设的 30s。
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < timeout {
			timeout = left
		}
	}

	res := Result{Command: c}
	if r.DryRun {
		res.DryRun = true
		r.logf(ctx, slog.LevelInfo, "dry-run", "cmd", r.Redact(c.String()))
		return res, nil
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, c.Name, c.Args...)
	dir := c.Dir
	if dir == "" {
		dir = r.Dir
	}
	cmd.Dir = dir
	cmd.Env = mergeEnv(os.Environ(), r.Env, c.Env)
	cmd.Stdin = c.Stdin

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if r.Stdout != nil {
		cmd.Stdout = io.MultiWriter(&stdout, r.Stdout)
	}
	if r.Stderr != nil {
		cmd.Stderr = io.MultiWriter(&stderr, r.Stderr)
	}

	// 取消时杀整棵进程树；WaitDelay 是兜底：万一某个子孙进程仍抱着管道不放，
	// 等待最多 3 秒就放弃读取（否则 `cmd.Wait` 会被孤儿进程拖住，超时形同虚设）。
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 3 * time.Second

	start := time.Now()
	r.logf(ctx, slog.LevelDebug, "exec", "cmd", r.Redact(c.String()), "dir", dir)
	err := cmd.Run()
	res.Duration = time.Since(start)
	res.Stdout = r.Redact(stdout.String())
	res.Stderr = r.Redact(stderr.String())
	res.ExitCode = cmd.ProcessState.ExitCode()
	if err == nil {
		res.ExitCode = 0
		return res, nil
	}

	exitErr := &ExitError{Result: res}
	exitErr.cause = err
	if cctx.Err() == context.DeadlineExceeded {
		// 把 context 的错误一起包进来：上层（例如总时限提示）用 errors.Is 判得出来，
		// 只看文案的话"命令被杀"和"命令自己失败"是分不清的。
		return res, fmt.Errorf("%w (timeout %s): %w", exitErr, timeout, context.DeadlineExceeded)
	}
	return res, exitErr
}

// RunCapture 执行命令并返回 stdout（成功时），常用于探测版本号。
func (r *Runner) RunCapture(ctx context.Context, name string, args ...string) (string, error) {
	res, err := r.Run(ctx, Command{Name: name, Args: args})
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// LookPath 在 PATH（含 Runner 注入的 PATH）中查找可执行文件。
func (r *Runner) LookPath(name string) (string, bool) {
	path := os.Getenv("PATH")
	if v, ok := r.Env["PATH"]; ok && v != "" {
		path = v
	}
	if strings.ContainsAny(name, `/\`) {
		if fileExists(name) {
			return name, true
		}
		return "", false
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		if fileExists(cand) {
			return cand, true
		}
		// Windows 下可执行文件带扩展名。
		for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
			if fileExists(cand + ext) {
				return cand + ext, true
			}
		}
	}
	return "", false
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Redact 把敏感串替换为掩码。
func (r *Runner) Redact(s string) string {
	for _, secret := range r.Secrets {
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}

func (r *Runner) logf(ctx context.Context, level slog.Level, msg string, args ...any) {
	if r.Log == nil {
		return
	}
	r.Log.Log(ctx, level, msg, args...)
}

// mergeEnv 按 base < overlays 的顺序合并环境变量，后者覆盖前者。
func mergeEnv(base []string, overlays ...map[string]string) []string {
	merged := map[string]string{}
	order := []string{}
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, seen := merged[k]; !seen {
			order = append(order, k)
		}
		merged[k] = v
	}
	for _, m := range overlays {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, seen := merged[k]; !seen {
				order = append(order, k)
			}
			merged[k] = m[k]
		}
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+merged[k])
	}
	return out
}

// shellQuote 把一个参数放进"可直接复制到终端执行"的命令行。
func shellQuote(s string) string { return shellQuoteFor(runtime.GOOS, s) }

// shellQuoteFor 是 shellQuote 的平台无关实现：goos 显式传入，
// 这样 Windows 的引用规则也能在跑不了 Windows 二进制的机器上用单测覆盖。
//
// 引用规则随目标 shell 走：POSIX shell 用单引号；Windows 的 cmd.exe 不认单引号
// （会原样把引号当参数内容传给程序），PowerShell 里单引号虽合法但双引号同样可用，
// 所以 Windows 统一用双引号，并且反斜杠是路径分隔符而不是转义符。
func shellQuoteFor(goos, s string) string {
	if s == "" {
		if goos == "windows" {
			return `""`
		}
		return "''"
	}
	// 需要引用的信号：空白 + 各 shell 的元字符。反斜杠在 Windows 路径里必然出现，
	// 这里同样视为需要引用（用双引号包住，语义不变），避免为两套字符集分别维护逻辑。
	if !strings.ContainsAny(s, " \t\n\"'\\$`|&;<>()*?[]{}!#~") {
		return s
	}
	if goos == "windows" {
		// cmd.exe 里参数内部的双引号用 "" 表示；反斜杠保持原样。
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
