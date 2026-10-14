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
	// wentBack 记录当前这次表单运行里用户按了"返回上一步"。
	// 表单是阻塞运行的、prompter 也不跨 goroutine 复用，所以一个布尔字段就够；
	// 每次 runForm 开始时清零。
	wentBack bool
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
	// 开了"返回上一步"就把键位提示写进说明：huh 的帮助行里没有它（单字段表单里
	// huh 会把 Prev 置成 disabled，帮助行自然不会画）。
	desc := withBackHint(q.Description, q.Back, huhBackHint)
	// 工具清单动辄几十条：列表按当前终端高度撑满，窗口变大时还能再长（见 listField）。
	field := huh.NewMultiSelect[string]().
		Title(q.Title).
		Description(desc).
		Options(options...).
		Filterable(len(options) > 8).
		Value(&selected).
		Validate(func(picked []string) error {
			if q.MinSelected > 0 && len(picked) < q.MinSelected {
				return fmt.Errorf("至少需要选择 %d 个", q.MinSelected)
			}
			return nil
		})
	list := &listField{
		title:       q.Title,
		description: desc,
		fallback:    14,
		total:       len(options),
		set:         func(height int) { field.Height(height) },
	}
	width, height := h.termSize()
	list.resize(width, height)
	if err := h.runForm(h.form(field, formOptions{list: list, back: q.Back})); err != nil {
		return nil, err
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
	desc := withBackHint(q.Description, q.Back, huhBackHint)
	field := huh.NewSelect[string]().
		Title(q.Title).
		Description(desc).
		Options(choices...).
		Value(&value)
	// 选项不多时交给 huh 自己撑开（Height 是含标题的整段高度，写死个数会少一行）。
	list := &listField{
		title:       q.Title,
		description: desc,
		fallback:    12,
		total:       len(choices),
		set:         func(height int) { field.Height(height) },
	}
	width, height := h.termSize()
	list.resize(width, height)
	if err := h.runForm(h.form(field, formOptions{list: list, back: q.Back})); err != nil {
		return "", err
	}
	return value, nil
}

// Confirm 实现 Prompter：默认高亮"否"（破坏性操作的默认答案是拒绝）。
func (h *huhPrompter) Confirm(q Question) (bool, error) {
	ok := false
	if err := h.runForm(h.form(
		huh.NewConfirm().
			Title(q.Title).
			Description(q.Description).
			Affirmative("继续").
			Negative("取消").
			Value(&ok),
		formOptions{},
	)); err != nil {
		return false, err
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
	if err := h.runForm(h.form(field, formOptions{})); err != nil {
		return "", err
	}
	return value, nil
}

// formOptions 是单字段表单的两个可选行为（都靠同一个 tea.WithFilter 实现）。
type formOptions struct {
	// list 非 nil 时，它的高度跟着终端窗口走（见 listField）。
	list *listField
	// back 为真时 Shift+Tab 表示"返回上一步"，让 runForm 返回 ErrBack。
	back bool
}

// huhBackKey 是 huh 界面里的"返回上一步"按键。
//
// 用 Shift+Tab 而不是 Esc / ←：huh 自己把 Prev 绑成 shift+tab（默认帮助文案就叫 back），
// 而单字段表单里 huh 会把它置成 disabled（没有上一个字段可去），所以这个键本来什么都不做，
// 我们接管它不会挤掉任何现有功能。Esc 在可过滤列表里是"退出/清空过滤"、← 在 Select 里是
// "上一条"，两者都不能动（见 AGENTS.md §10）。
const huhBackKey = "shift+tab"

// huhBackHint 是写在字段说明里的键位提示：huh 不会把 disabled 的 Prev 画进帮助行，
// 只能自己说清楚（不然用户根本不知道有这么个入口）。
const huhBackHint = "Shift+Tab 返回上一步"

// form 组装单字段表单；输入输出显式绑定，避免 huh 自己去开 /dev/tty
// （那样在包装器/IDE 终端里会失败，而我们已经有可用的 in/out）。
//
// 交互式界面统一走**备用屏幕**（AltScreen）：表单铺满整屏，退出时终端恢复原样，
// 而不是在已打印的概览表格下面挤出一小块（这正是用户抱怨的"局限在下面一点点"）。
// 注意 WithProgramOptions 是**替换**而不是追加，所以必须排在最前面，
// 否则后面 WithInput/WithOutput 追加上去的选项会被它清空。
//
// filter 在消息进 huh 的 Form.Update 之前跑，我们只用它做两件 huh 自己做不到的事：
//   - WindowSizeMsg：重算列表高度（huh 会重排宽度与分组高度，但 Height 是我们算的常量）；
//   - Shift+Tab：换来一个"返回上一步"（huh 在单字段表单里把这个键禁掉了）。
//
// 两件事都不改写 huh 的语义：消息原样转发，只有"返回"是我们自己发起的退出。
func (h *huhPrompter) form(field huh.Field, opts formOptions) *huh.Form {
	form := huh.NewForm(huh.NewGroup(field))
	form.WithProgramOptions(
		tea.WithAltScreen(),
		tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			switch m := msg.(type) {
			case tea.WindowSizeMsg:
				opts.list.resize(m.Width, m.Height)
			case tea.KeyMsg:
				if opts.back && m.String() == huhBackKey {
					h.wentBack = true
					// 让 bubbletea 立刻收工：huh 不会因此置 aborted，
					// Run 正常返回，runForm 再把"返回"翻译成 ErrBack。
					return tea.QuitMsg{}
				}
			}
			return msg
		}),
	)
	return form.
		WithInput(h.in).
		WithOutput(h.out).
		WithAccessible(h.accessible).
		WithShowHelp(true)
}

// runForm 跑一个单字段表单，并把结果翻译成统一的错误（取消 → ErrCancelled，
// 返回 → ErrBack）。
func (h *huhPrompter) runForm(form *huh.Form) error {
	h.wentBack = false
	if err := form.Run(); err != nil {
		return mapHuhErr(err)
	}
	if h.wentBack {
		return ErrBack
	}
	return nil
}

// withBackHint 在开了"返回上一步"时把键位提示接到说明后面（说明可以是空的）。
func withBackHint(description string, back bool, hint string) string {
	if !back {
		return description
	}
	return appendHint(description, hint)
}

// listField 是"高度跟着终端走"的列表字段：窗口一变就按新尺寸重算 Height。
//
// 为什么需要它：huh v1.0.0 自己会跟着终端重排（`Form.Update` 里那两个
// `if f.width == 0` / `if f.height == 0` 分支其实每次都进，因为 huh 从不给
// f.width/f.height 赋值），但 `Select.Height()` 是我们在表单启动**之前**按当时的
// 终端算死的常量——窗口一变大，它还是原来那么高，用户把终端拉满也看不到更多行。
type listField struct {
	title       string
	description string
	// fallback 是拿不到终端尺寸时的列表行数（保守值）。
	fallback int
	// total 是选项总数：放得下就不设 Height，让 huh 按内容精确撑开。
	total int
	// set 把算好的 Height 写回字段（0 表示不设 Height）。
	set func(height int)
}

// resize 按终端尺寸重算列表高度；l 为 nil 时什么都不做（普通字段表单）。
func (l *listField) resize(termWidth, termHeight int) {
	if l == nil {
		return
	}
	l.set(listHeight(l.title, l.description, termWidth, termHeight, l.fallback, l.total))
}

// listHeight 返回"这个列表字段该设多高"（0 = 不设 Height，交给 huh 按内容撑开）。
//
// 两个坑叠在一起，必须一起解决：
//   - huh 的 Height 是**整个字段**的高度，viewport = Height − 标题行数 − 说明行数；
//   - 列表要按**当前**终端高度算，而不是表单启动那一刻的。
//
// 所以：留出标题/说明（按显示宽度折行算）+ 4 行（huh 在字段与帮助之间还会画一行空行、
// 帮助本身一行、再留一行余量免得刚好撑满导致整屏滚动），剩下的都给列表。
func listHeight(title, description string, termWidth, termHeight, fallback, total int) int {
	chrome := chromeRows(title, description, termWidth)
	rows := fallback
	if termHeight > 0 {
		rows = max(3, termHeight-chrome-4)
	}
	if total <= rows {
		// 放得下就别设 Height：huh 会按内容精确撑开，一行不多一行不少。
		return 0
	}
	return rows + chrome
}

// chromeRows 估算标题与说明会折成几行（huh 把它们算进 Height 里）。
//
// 按"可写宽度再窄一点"来算：算宽了最多让列表多显示一行，算窄了会吃掉一行选项。
func chromeRows(title, description string, termWidth int) int {
	if termWidth <= 0 {
		termWidth = 80 // 拿不到终端宽度时的兜底，与 huh 自己的默认一致
	}
	// 扣掉字段的左边框+内边距、组缩进与右侧留白。
	width := termWidth - 6
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

// termSize 取输出终端的尺寸；拿不到时返回 (0, 0)，调用方按保守值处理。
func (h *huhPrompter) termSize() (width, height int) {
	f, ok := h.out.(*os.File)
	if !ok {
		return 0, 0
	}
	w, hgt, err := xterm.GetSize(f.Fd())
	if err != nil {
		return 0, 0
	}
	return w, hgt
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
