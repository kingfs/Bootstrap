package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/kingfs/Bootstrap/pkg/hostinfo"
)

// huhPrompter 是"真的有终端"时的交互式实现：空格勾选、方向键选择、密码不回显。
//
// 它是**可选**的：所有能力在 Text 里都有等价实现（见 ui.go 的说明），
// 这里只负责好看与好用，因此任何一步失败都可以安全降级或直接报错。
type huhPrompter struct {
	in  io.Reader
	out io.Writer
	// accessible 为真时用 huh 的纯文本问答模式（它自己在 TERM=dumb 下的降级路径）。
	// 生产代码不走这里——我们的降级是 ui.Text；这个开关让单元测试能在没有 PTY 的情况下
	// 覆盖"选项映射 / 回填 / 校验"这些我们自己写的逻辑。
	accessible bool
}

// NewInteractive 在"输入输出都是交互式终端"时返回基于 huh 的实现。
//
// 返回 false 时调用方应当退回 Text：
//   - 拿不到终端（管道 / CI / 测试里的 bytes.Buffer）；
//   - `TERM=dumb`（无能力终端，bubbletea 画不出东西）；
//   - 显式要求纯文本（调用方自己判断，例如 `--plain-ui`）。
func NewInteractive(in io.Reader, out io.Writer) (Prompter, bool) {
	if !hostinfo.IsTerminal(in) || !hostinfo.IsTerminal(out) {
		return nil, false
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb") {
		return nil, false
	}
	return &huhPrompter{in: in, out: out}, true
}

// Interactive 实现 Prompter：走到这里说明终端可用。
func (h *huhPrompter) Interactive() bool { return true }

// MultiSelect 实现 Prompter：空格勾选、/ 过滤、回车确认。
func (h *huhPrompter) MultiSelect(q MultiSelect) ([]string, error) {
	values := make([]string, 0, len(q.Options))
	options := make([]huh.Option[string], 0, len(q.Options))
	for _, o := range q.Options {
		label := o.Display()
		if o.Hint != "" {
			label += "  " + o.Hint
		}
		if o.Disabled {
			// huh 没有"禁用但仍显示"的选项：跳过它，并在标题里说明，
			// 免得用户以为工具消失了（Text 版会保留并拒绝选择）。
			continue
		}
		options = append(options, huh.NewOption(label, o.Value).Selected(o.Selected))
		values = append(values, o.Value)
	}
	// 用户主动取消（Ctrl-C / Esc）时返回 ErrCancelled，与 Text 版语义一致。
	selected := make([]string, 0, len(values))
	if err := h.form(
		huh.NewMultiSelect[string]().
			Title(q.Title).
			Description(q.Description).
			Options(options...).
			Filterable(len(options) > 8).
			Height(14).
			Value(&selected).
			Validate(func(picked []string) error {
				if q.MinSelected > 0 && len(picked) < q.MinSelected {
					return fmt.Errorf("至少需要选择 %d 个", q.MinSelected)
				}
				return nil
			}),
	).Run(); err != nil {
		return nil, mapHuhErr(err)
	}
	return selected, nil
}

// Select 实现 Prompter。
func (h *huhPrompter) Select(q Question, options []Option) (string, error) {
	choices := make([]huh.Option[string], 0, len(options))
	for _, o := range options {
		if o.Disabled {
			continue
		}
		label := o.Display()
		if o.Hint != "" {
			label += "  " + o.Hint
		}
		choices = append(choices, huh.NewOption(label, o.Value))
	}
	if len(choices) == 0 {
		return "", errors.New("ui: no selectable options")
	}
	value := ""
	height := len(choices)
	if height > 12 {
		height = 12
	}
	if err := h.form(
		huh.NewSelect[string]().
			Title(q.Title).
			Description(q.Description).
			Options(choices...).
			Height(height).
			Value(&value),
	).Run(); err != nil {
		return "", mapHuhErr(err)
	}
	return value, nil
}

// Confirm 实现 Prompter：默认高亮"否"（破坏性操作的默认答案是拒绝）。
func (h *huhPrompter) Confirm(q Question) (bool, error) {
	ok := false
	if err := h.form(
		huh.NewConfirm().
			Title(q.Title).
			Description(q.Description).
			Affirmative("继续").
			Negative("取消").
			Value(&ok),
	).Run(); err != nil {
		return false, mapHuhErr(err)
	}
	return ok, nil
}

// Input 实现 Prompter；Secret 时不回显。
func (h *huhPrompter) Input(q Question) (string, error) {
	value := q.Default
	field := huh.NewInput().
		Title(q.Title).
		Description(q.Description).
		Value(&value)
	if q.Secret {
		field = field.EchoMode(huh.EchoModePassword)
	}
	if err := h.form(field).Run(); err != nil {
		return "", mapHuhErr(err)
	}
	return value, nil
}

// form 组装单字段表单；输入输出显式绑定，避免 huh 自己去开 /dev/tty
// （那样在包装器/IDE 终端里会失败，而我们已经有可用的 in/out）。
func (h *huhPrompter) form(fields ...huh.Field) *huh.Form {
	return huh.NewForm(huh.NewGroup(fields...)).
		WithInput(h.in).
		WithOutput(h.out).
		WithAccessible(h.accessible).
		WithShowHelp(true)
}

// mapHuhErr 把 huh 的"用户主动退出"映射成统一的 ErrCancelled，
// 这样命令层只需要认识一种取消语义。
func mapHuhErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, huh.ErrUserAborted) {
		return ErrCancelled
	}
	return err
}
