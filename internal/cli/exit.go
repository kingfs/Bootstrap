package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kingfs/Bootstrap/internal/ui"
	"github.com/kingfs/Bootstrap/pkg/cmdx"
)

// 退出码约定（docs/design.md §10）。脚本可依赖这些值。
const (
	ExitOK               = 0
	ExitFail             = 1
	ExitUsage            = 2
	ExitPartial          = 3
	ExitPrecondition     = 4
	ExitNeedsInteraction = 5
)

// codedError 携带退出码。
type codedError struct {
	code int
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// coded 包装一个带退出码的错误。
func coded(code int, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

// usageErr 表示命令行用法错误（退出码 2）。
func usageErr(err error) error { return coded(ExitUsage, err) }

// usageErrf 构造用法错误。
func usageErrf(format string, args ...any) error {
	return coded(ExitUsage, fmt.Errorf(format, args...))
}

// failf 构造一般失败（退出码 1）。
func failf(format string, args ...any) error {
	return coded(ExitFail, fmt.Errorf(format, args...))
}

// preconditionErr 表示前置条件不满足（退出码 4）。
func preconditionErr(err error) error { return coded(ExitPrecondition, err) }

// preconditionErrf 构造前置条件错误。
func preconditionErrf(format string, args ...any) error {
	return coded(ExitPrecondition, fmt.Errorf(format, args...))
}

// ExitCode 把 error 映射为进程退出码。
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	var exitErr *cmdx.ExitError
	if errors.As(err, &exitErr) {
		return ExitFail
	}
	// 向导类错误：问不了用户 = 需要交互（退出码 5）；用户主动取消不是失败（退出码 0）。
	// 取消通常在上层就被拦下并打印"已取消"，这里兜底以免冒出一个吓人的退出码。
	if errors.Is(err, ui.ErrNotInteractive) {
		return ExitNeedsInteraction
	}
	if errors.Is(err, ui.ErrCancelled) {
		return ExitOK
	}
	// cobra 的用法类错误：cobra 不区分类型，只能按文案识别（cobra 的固定前缀）。
	msg := err.Error()
	for _, prefix := range []string{
		"unknown command", "unknown flag", "unknown shorthand flag",
		"invalid argument", "requires at least", "accepts at most", "accepts ", "required flag",
	} {
		if strings.HasPrefix(msg, prefix) || strings.Contains(msg, prefix) {
			return ExitUsage
		}
	}
	return ExitFail
}
