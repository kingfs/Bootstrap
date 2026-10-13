package install

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// splitArgs 把命令行拆成 argv，支持单引号与双引号（不做变量展开）。
//
// 清单里的模板是"命令行的样子"，但执行时必须是 argv——
// 否则等于让清单决定 shell 语义，注入风险与可移植性都会变差。
func splitArgs(line string) ([]string, error) {
	var (
		args               []string
		cur                strings.Builder
		inSingle, inDouble bool
		escaped            bool
		started            bool
	)
	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
			started = true
		case r == '\\' && !inSingle:
			escaped = true
			started = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
			started = true
		case r == '"' && !inSingle:
			inDouble = !inDouble
			started = true
		case (r == ' ' || r == '\t' || r == '\n') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unbalanced quote in command template %q", line)
	}
	if escaped {
		return nil, fmt.Errorf("trailing escape in command template %q", line)
	}
	flush()
	return args, nil
}

// shellQuote 把字符串安全地放进 shell 命令行（用于 source 脚本路径等）。
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '/' || r == '.' || r == '_' || r == '-' || r == '+' || r == ':' || r == '@' || r == '=') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runCommands 顺序执行 Action 里的全部命令，遇到第一个失败就返回。
func runCommands(ctx context.Context, a Action, e ExecEnv) error {
	for _, c := range a.Commands {
		if _, err := e.Run(ctx, c); err != nil {
			return fmt.Errorf("%s: %w", c.String(), err)
		}
	}
	return nil
}

// rootCommand 在需要 root 且当前不是 root 时加 sudo 前缀。
func rootCommand(c Command, needsRoot bool, e ExecEnv) Command {
	if !needsRoot || e.IsRoot() {
		return c
	}
	args := append([]string{c.Name}, c.Args...)
	return Command{Name: "sudo", Args: args, Dir: c.Dir, Env: c.Env}
}

// needsRootParam 解析 needs_root 参数（缺省按 def）。
func needsRootParam(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// logEnv 把一次工具安装里所有命令的输出写进日志文件。
//
// 用装饰器而不是改 ExecEnv 接口或各个 Installer：所有 via 共用同一条执行路径，
// 拦在这里就天然覆盖全部安装方式，而且每个工具一个实例，不存在并发写同一文件。
type logEnv struct {
	ExecEnv
	w io.Writer
}

// Run 执行命令并把"命令行 + 输出 + 退出码"追加到日志。
//
// 命令行**先写**再执行：慢下载/被中断时日志里至少留下"在跑什么"，
// 否则一个中途被 kill 的长时间命令只会留下一个空文件。
// （输出只能等命令返回后补写——ExecEnv 没有流式接口，真实 Runner 的实时透传
// 仍然走 `-v` 的 stdout/stderr。）
func (l *logEnv) Run(ctx context.Context, c Command) (Result, error) {
	fmt.Fprintf(l.w, "=== %s $ %s\n", time.Now().Format(time.RFC3339), c.String())
	res, err := l.ExecEnv.Run(ctx, c)
	if res.Stdout != "" {
		fmt.Fprintf(l.w, "%s", ensureTrailingNewline(res.Stdout))
	}
	if res.Stderr != "" {
		fmt.Fprintf(l.w, "--- stderr ---\n%s", ensureTrailingNewline(res.Stderr))
	}
	if err != nil {
		fmt.Fprintf(l.w, "!!! exit %d: %v\n", res.ExitCode, err)
	}
	return res, err
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
