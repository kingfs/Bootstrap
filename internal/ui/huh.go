package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	xterm "github.com/charmbracelet/x/term"

	"github.com/kingfs/Bootstrap/pkg/hostinfo"
	"github.com/kingfs/Bootstrap/pkg/textwidth"
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
	// 工具清单动辄几十条，按终端高度给足列表空间（全屏表单，见 form()）。
	field := huh.NewMultiSelect[string]().
		Title(q.Title).
		Description(q.Description).
		Options(options...).
		Filterable(len(options) > 8).
		Value(&selected).
		Validate(func(picked []string) error {
			if q.MinSelected > 0 && len(picked) < q.MinSelected {
				return fmt.Errorf("至少需要选择 %d 个", q.MinSelected)
			}
			return nil
		})
	if height := h.fieldHeight(q.Title, q.Description, h.listRows(14), len(options)); height > 0 {
		field.Height(height)
	}
	if err := h.form(field).Run(); err != nil {
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
	field := huh.NewSelect[string]().
		Title(q.Title).
		Description(q.Description).
		Options(choices...).
		Value(&value)
	// 选项不多时交给 huh 自己撑开（Height 是含标题的整段高度，写死个数会少一行）。
	if height := h.fieldHeight(q.Title, q.Description, h.listRows(12), len(choices)); height > 0 {
		field.Height(height)
	}
	if err := h.form(field).Run(); err != nil {
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
//
// 交互式界面统一走**备用屏幕**（AltScreen）：表单铺满整屏，退出时终端恢复原样，
// 而不是在已打印的概览表格下面挤出一小块（这正是用户抱怨的"局限在下面一点点"）。
// 注意 WithProgramOptions 是**替换**而不是追加，所以必须排在最前面，
// 否则后面 WithInput/WithOutput 追加上去的选项会被它清空。
func (h *huhPrompter) form(fields ...huh.Field) *huh.Form {
	return huh.NewForm(huh.NewGroup(fields...)).
		WithProgramOptions(tea.WithAltScreen()).
		WithInput(h.in).
		WithOutput(h.out).
		WithAccessible(h.accessible).
		WithShowHelp(true)
}

// listRows 计算列表最多能占几行：终端高度里扣掉标题/说明/帮助与上下留白。
//
// 拿不到终端尺寸时（例如换了输出 writer）退回调用方给的保守值——
// 宁可列表短一点，也不能算出 0 让界面什么都不显示。
func (h *huhPrompter) listRows(fallback int) int {
	f, ok := h.out.(*os.File)
	if !ok {
		return fallback
	}
	_, height, err := xterm.GetSize(f.Fd())
	if err != nil || height <= 0 {
		return fallback
	}
	if n := height - 10; n > 3 {
		return n
	}
	// 终端特别矮时也别把整屏撑爆：给列表留 3 行，其余靠滚动。
	return 3
}

// fieldHeight 把"列表想占几行"换算成 huh 的 Height 参数。
//
// huh 的坑：Height 是**整个字段**的高度，viewport 高度 = Height − 标题行数 − 说明行数
// （见 huh 的 Select.updateViewportHeight）。以前直接传选项个数，于是
// "4 个选项 + Height(4)"只显示 3 行、第 4 个要滚动才看得到——用户实测反馈的正是这个。
// 所以分两种：
//   - 选项本来就能全放下 → 返回 0（不设 Height）：huh 会按内容精确撑开，一行不多一行不少；
//   - 放不下 → 补回标题/说明占的行，让列表稳定占满 rows 行，多出来的靠滚动。
func (h *huhPrompter) fieldHeight(title, description string, rows, total int) int {
	if total <= rows {
		return 0
	}
	return rows + h.chromeRows(title, description)
}

// chromeRows 估算标题与说明会折成几行（huh 把它们算进 Height 里）。
//
// 按"可写宽度再窄一点"来算：算宽了最多让列表多显示一行，算窄了会吃掉一行选项。
func (h *huhPrompter) chromeRows(title, description string) int {
	// 扣掉字段的左边框+内边距、组缩进与右侧留白。
	width := h.termWidth() - 6
	if width < 16 {
		width = 16
	}
	rows := 0
	for _, s := range []string{title, description} {
		if strings.TrimSpace(s) == "" {
			continue
		}
		rows += max(1, (textwidth.Width(s)+width-1)/width)
	}
	return rows
}

// termWidth 取终端宽度；拿不到时按 80 算（huh 自己也这么兜底）。
func (h *huhPrompter) termWidth() int {
	f, ok := h.out.(*os.File)
	if !ok {
		return 80
	}
	width, _, err := xterm.GetSize(f.Fd())
	if err != nil || width <= 0 {
		return 80
	}
	return width
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
