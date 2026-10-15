// Package ui 是终端交互的唯一落点。
//
// 为什么要有这层抽象（docs/design.md ADR-0003）：
//
//  1. 交互库（charmbracelet/huh）只允许出现在这里，别处一律通过 Prompter 接口提问。
//     交互库的依赖树不小、行为依赖终端，把它圈在一个包里，换掉或去掉都不影响业务代码。
//  2. 非交互环境（管道、CI、--json、--non-interactive）**绝不能**阻塞等待输入。
//     这里用 NonInteractive 显式表达"问不了"，让上层把"问不了"映射成退出码 5，
//     而不是到处判断 os.Stdin 是不是终端。
//  3. 纯文本路径必须一直可用：它是测试里唯一能跑的实现，也是 huh 在非终端下的降级。
package ui

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ErrCancelled 表示用户主动取消（输入 q、Ctrl-C、或明确拒绝）。
//
// 取消**不是失败**：调用方应把它转成退出码 0 并提示"未做任何修改"。
var ErrCancelled = errors.New("cancelled by user")

// ErrBack 表示用户要求"返回上一步"（不是取消整个流程）。
//
// 与 ErrCancelled 的区别很关键：取消意味着中止并提示"未做任何修改"，
// 而返回意味着调用方应当退回上一个问题重问（`bs init` 的工具菜单就是这么用的）。
// 只有把提问标成 Back: true 的地方才可能返回它——第一步没有"上一步"，
// 就不该给出这个入口（否则用户按下去只能得到一句"回不去了"）。
var ErrBack = errors.New("back to previous step")

// ErrNotInteractive 表示当前环境无法向用户提问。
//
// 上层把它映射为退出码 5（ExitNeedsInteraction）并提示加 --yes 或改用参数。
var ErrNotInteractive = errors.New("not interactive")

// ErrTooManyAttempts 表示用户连续给了看不懂的输入，为免死循环而放弃。
var ErrTooManyAttempts = errors.New("too many invalid answers")

// maxAttempts 限制"输入看不懂就重问"的次数：脚本喂了垃圾输入时不能永远问下去。
const maxAttempts = 5

// Option 是一个可选项。
type Option struct {
	Value    string // 机器可读的值（工具 id、profile 名……）
	Label    string // 展示名，留空则用 Value
	Hint     string // 附加说明（括号里的灰色小字）
	Selected bool   // 默认是否勾选（多选）
	Disabled bool   // 不可选，例如"不适用于当前平台"
}

// Display 返回展示用的名字。
func (o Option) Display() string {
	if o.Label != "" {
		return o.Label
	}
	return o.Value
}

// line 返回"名字（说明）"形式的一行，供纯文本渲染使用。
func (o Option) line() string {
	if o.Hint == "" {
		return o.Display()
	}
	return fmt.Sprintf("%s（%s）", o.Display(), o.Hint)
}

// MultiSelect 是一次多选提问。
type MultiSelect struct {
	Title       string
	Description string
	Options     []Option
	// MinSelected > 0 时要求至少选这么多个，否则重新提问。
	MinSelected int
	// Back 为真时提供"返回上一步"的入口（huh 界面 Shift+Tab、纯文本 b），
	// 用户按下就返回 ErrBack。菜单套菜单的地方才该打开它。
	Back bool
}

// Question 是单选 / 确认 / 文本输入的公共部分。
type Question struct {
	Title       string
	Description string
	// Default：单选是默认选中的 Value，输入是回车时采用的值。
	Default string
	// Secret 为真时输入不回显（纯文本实现下会打印一句提示，真正的隐藏由 huh 负责）。
	Secret bool
	// Back 为真时提供"返回上一步"的入口；只有 Select 认它（见 ErrBack）。
	// Confirm / Input 不认：危险确认的"上一步"就是拒绝，文本输入里也没有不歧义的返回键。
	Back bool
}

// Prompter 抽象"向用户提问"。
type Prompter interface {
	MultiSelect(q MultiSelect) ([]string, error)
	Select(q Question, options []Option) (string, error)
	Confirm(q Question) (bool, error)
	Input(q Question) (string, error)
	// Interactive 报告这个实现是否真的能问到用户。
	Interactive() bool
}

// NonInteractive 是"问不了"的 Prompter：所有提问都返回 ErrNotInteractive。
//
// 它的存在让"非交互时不能阻塞"成为类型层面的约束，而不是散落各处的 if。
type NonInteractive struct{}

// MultiSelect 实现 Prompter。
func (NonInteractive) MultiSelect(MultiSelect) ([]string, error) { return nil, ErrNotInteractive }

// Select 实现 Prompter。
func (NonInteractive) Select(Question, []Option) (string, error) { return "", ErrNotInteractive }

// Confirm 实现 Prompter。
func (NonInteractive) Confirm(Question) (bool, error) { return false, ErrNotInteractive }

// Input 实现 Prompter。
func (NonInteractive) Input(Question) (string, error) { return "", ErrNotInteractive }

// Interactive 实现 Prompter。
func (NonInteractive) Interactive() bool { return false }

// Text 是零依赖的纯文本 Prompter：读行、打印提示。
//
// 它同时承担三个角色：huh 不可用时的降级、测试里的驱动方式、以及
// 想要朴素交互的场景（`BS_PLAIN_UI=1`）。
type Text struct {
	// In 是输入来源；Out 是提示信息的去向（一般传 stderr，避免污染 stdout 的机器可读输出）。
	In  io.Reader
	Out io.Writer
}

// NewText 构造纯文本 Prompter。
func NewText(in io.Reader, out io.Writer) *Text {
	return &Text{In: in, Out: out}
}

// Interactive 实现 Prompter：纯文本实现总能问（该不该问由调用方决定）。
func (t *Text) Interactive() bool { return true }

// MultiSelect 实现 Prompter。
func (t *Text) MultiSelect(q MultiSelect) ([]string, error) {
	if len(q.Options) == 0 {
		return nil, nil
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		t.printTitle(q.Title, q.Description)
		for i, o := range q.Options {
			name := o.line()
			if o.Disabled {
				name += "［不可选］"
			}
			fmt.Fprintf(t.Out, "  [%s] %d) %s\n", checkMark(o.Selected), i+1, name)
		}
		fmt.Fprint(t.Out, "选择（编号，逗号/空格分隔，可写 1-3；a=全选，n=全不选，回车=用默认，q=取消"+backHint(q.Back)+"）: ")

		line, err := t.readLine()
		if err != nil {
			return nil, err
		}
		var values []string
		switch strings.ToLower(line) {
		case "q", "quit":
			return nil, ErrCancelled
		case "":
			values = defaultSelection(q.Options)
		case "a", "all", "*":
			values = enabledValues(q.Options)
		case "n", "none":
			// "全不选"与 huh 界面的 Ctrl+X 对应（见 huh.go 的 huhSelectNoneKey）：
			// 先把默认勾选清掉，用户再按编号挑几个。MinSelected 仍会拦"一个都没选"。
			values = nil
		default:
			parsed, perr := parseSelection(line, q.Options)
			if perr != nil {
				// 先试选项再认"返回"：万一哪个工具 id 真叫 b，选项优先。
				if isBackAnswer(q.Back, line) {
					return nil, ErrBack
				}
				fmt.Fprintf(t.Out, "%v，请重新选择。\n", perr)
				continue
			}
			values = parsed
		}
		if q.MinSelected > 0 && len(values) < q.MinSelected {
			fmt.Fprintf(t.Out, "至少需要选择 %d 个，请重新选择。\n", q.MinSelected)
			continue
		}
		return values, nil
	}
	return nil, ErrTooManyAttempts
}

// Select 实现 Prompter。
func (t *Text) Select(q Question, options []Option) (string, error) {
	if len(options) == 0 {
		return "", errors.New("no options to select from")
	}
	fallback := q.Default
	if fallback == "" {
		for _, o := range options {
			if !o.Disabled {
				fallback = o.Value
				break
			}
		}
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		t.printTitle(q.Title, q.Description)
		for i, o := range options {
			name := o.line()
			if o.Disabled {
				name += "［不可选］"
			}
			fmt.Fprintf(t.Out, "  [%s] %d) %s\n", checkMark(o.Value == fallback), i+1, name)
		}
		fmt.Fprintf(t.Out, "选择（编号或名字，回车=%s，q=取消%s）: ", fallback, backHint(q.Back))

		line, err := t.readLine()
		if err != nil {
			return "", err
		}
		switch strings.ToLower(line) {
		case "q", "quit":
			return "", ErrCancelled
		case "":
			return fallback, nil
		}
		for i, o := range options {
			if line == strconv.Itoa(i+1) || strings.EqualFold(line, o.Value) {
				if o.Disabled {
					fmt.Fprintf(t.Out, "%s 不可选，请重新选择。\n", o.Display())
					break
				}
				return o.Value, nil
			}
		}
		// 同样先试选项再认"返回"：值真的叫 b 的选项不会变得选不了。
		if isBackAnswer(q.Back, line) {
			return "", ErrBack
		}
		fmt.Fprintln(t.Out, "看不懂这个选择，请重新选择。")
	}
	return "", ErrTooManyAttempts
}

// Confirm 实现 Prompter：回车/EOF 都算"否"（危险操作默认拒绝）。
func (t *Text) Confirm(q Question) (bool, error) {
	t.printTitle(q.Title, q.Description)
	fmt.Fprint(t.Out, "[y/N] ")
	line, err := t.readLine()
	if err != nil {
		// 输入已经结束（管道/EOF）：按"否"处理，与直接回车一致。
		if errors.Is(err, ErrCancelled) {
			return false, nil
		}
		return false, err
	}
	switch strings.ToLower(line) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// Input 实现 Prompter：回车采用默认值，EOF 视为取消。
func (t *Text) Input(q Question) (string, error) {
	t.printTitle(q.Title, q.Description)
	if q.Secret {
		// 纯文本实现无法真正隐藏输入；至少说明这一点，别让人误以为它是安全的。
		fmt.Fprint(t.Out, "（纯文本模式，输入会回显）")
	}
	if q.Default != "" {
		fmt.Fprintf(t.Out, "[%s] ", q.Default)
	} else {
		fmt.Fprint(t.Out, "> ")
	}
	line, err := t.readLine()
	if err != nil {
		return "", err
	}
	if line == "" {
		return q.Default, nil
	}
	return line, nil
}

func (t *Text) printTitle(title, description string) {
	if title != "" {
		fmt.Fprintln(t.Out, title)
	}
	if description != "" {
		fmt.Fprintln(t.Out, description)
	}
}

// readLine 读一行并去掉首尾空白；EOF（没有更多输入）统一表达为 ErrCancelled，
// 让"脚本喂完了输入"和"用户按了 q"走上层的同一条路。
func (t *Text) readLine() (string, error) {
	line, err := readLine(t.In)
	if errors.Is(err, io.EOF) {
		return "", ErrCancelled
	}
	if err != nil {
		return "", fmt.Errorf("read prompt input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// readLine 从 r 逐字节读一行（不含换行）。逐字节是为了不引入 bufio 的预读，
// 否则同一个 reader 上后续的读取会丢数据（测试里会换输入源）。
func readLine(r io.Reader) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return sb.String(), nil
			}
			if buf[0] != '\r' {
				sb.WriteByte(buf[0])
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && sb.Len() > 0 {
				return sb.String(), nil
			}
			return sb.String(), err
		}
	}
}

func checkMark(selected bool) string {
	if selected {
		return "x"
	}
	return " "
}

// textBackAnswer 是纯文本界面里"返回上一步"的答案。
//
// 纯文本没有按键这一说，只能挑一个短词；`b` 与 huh 界面的 Shift+Tab 一样是"back"的意思。
const textBackAnswer = "b"

// backHint 返回提示串里那一段"怎么返回"，没开 Back 时是空串（不占地方）。
func backHint(back bool) string {
	if !back {
		return ""
	}
	return "，" + textBackAnswer + "=返回上一步"
}

// appendHint 把一句提示接到说明后面（说明可能是空的），两种界面共用。
func appendHint(description, hint string) string {
	if strings.TrimSpace(description) == "" {
		return hint
	}
	return description + "；" + hint
}

// isBackAnswer 判断用户是不是在要"返回上一步"：只在提问开了 Back 时才认。
// 调用方一律先尝试按选项解析，失败后才问它——这样"值恰好叫 b"的选项不会被顶掉。
func isBackAnswer(back bool, line string) bool {
	if !back {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case textBackAnswer, "back":
		return true
	}
	return false
}

func defaultSelection(options []Option) []string {
	var out []string
	for _, o := range options {
		if o.Selected && !o.Disabled {
			out = append(out, o.Value)
		}
	}
	return out
}

func enabledValues(options []Option) []string {
	out := make([]string, 0, len(options))
	for _, o := range options {
		if !o.Disabled {
			out = append(out, o.Value)
		}
	}
	return out
}

// parseSelection 解析 "1,3 5-7" 这类输入，返回去重后的 Value 列表（保持输入顺序）。
func parseSelection(line string, options []Option) ([]string, error) {
	fields := strings.FieldsFunc(line, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == ';' || r == '，' || r == '、'
	})
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	pick := func(idx int) error {
		if idx < 1 || idx > len(options) {
			return fmt.Errorf("编号 %d 超出范围（1-%d）", idx, len(options))
		}
		if options[idx-1].Disabled {
			return fmt.Errorf("%s 不可选", options[idx-1].Display())
		}
		add(options[idx-1].Value)
		return nil
	}
	for _, f := range fields {
		if lo, hi, ok := cutRange(f); ok {
			for i := lo; i <= hi; i++ {
				if err := pick(i); err != nil {
					return nil, err
				}
			}
			continue
		}
		if idx, err := strconv.Atoi(f); err == nil {
			if err := pick(idx); err != nil {
				return nil, err
			}
			continue
		}
		// 也接受直接写值（工具 id），方便脚本化输入。
		matched := false
		for _, o := range options {
			if strings.EqualFold(o.Value, f) {
				if o.Disabled {
					return nil, fmt.Errorf("%s 不可选", o.Display())
				}
				add(o.Value)
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("看不懂 %q", f)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("没有解析出任何选择")
	}
	return out, nil
}

// cutRange 解析 "1-3" 形式的区间。
func cutRange(s string) (int, int, bool) {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, false
	}
	a, err1 := strconv.Atoi(strings.TrimSpace(lo))
	b, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil || a > b {
		return 0, 0, false
	}
	return a, b, true
}

// SortedValues 返回按 Value 排序的副本：让测试断言与输出稳定，不受选项顺序影响。
func SortedValues(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
