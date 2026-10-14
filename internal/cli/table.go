package cli

import (
	"bytes"
	"io"
	"strings"

	"github.com/kingfs/Bootstrap/pkg/textwidth"
)

// tableWriter 是按**终端显示列数**对齐的极简表格 writer：调用方用 `\t` 分列，
// 列宽取该列所有单元格的最大显示宽度，列间补两个空格。
//
// 为什么不用 text/tabwriter：它按字节（或 Rune 数）算宽度，而中文一个字占两列，
// 于是"中文表头 + 英文值"的表格会整体错位——这不是 tabwriter 的 bug，是它拿不到
// "终端宽度"这个信息（终端属性），只能由我们按 East Asian Width 自己算（pkg/textwidth）。
//
// 用法与 tabwriter 一致：写若干 `\t` 分隔的行，最后 Flush；行尾的最后一列不补空格
// （免得输出里带一堆看不见的尾随空白）。列宽按**整行**里最宽的那个单元格算，
// 因此必须先把行攒起来——表格都不大，这点内存无所谓。
type tableWriter struct {
	out   io.Writer
	buf   bytes.Buffer
	rows  [][]string
	width []int
}

func newTableWriter(out io.Writer) *tableWriter { return &tableWriter{out: out} }

// Write 攒行；只有遇到换行才成行，没写完的部分留到下次（tabwriter 的接口习惯）。
func (t *tableWriter) Write(p []byte) (int, error) {
	t.buf.Write(p)
	for {
		line, err := t.buf.ReadString('\n')
		if err != nil {
			// 还没有整行：把残余放回去等下一次 Write / Flush。
			t.buf.Reset()
			t.buf.WriteString(line)
			break
		}
		t.addRow(strings.TrimSuffix(line, "\n"))
	}
	return len(p), nil
}

func (t *tableWriter) addRow(line string) {
	line = strings.TrimSuffix(line, "\r")
	cells := strings.Split(line, "\t")
	for i, c := range cells {
		if i >= len(t.width) {
			t.width = append(t.width, 0)
		}
		if w := textwidth.Width(c); w > t.width[i] {
			t.width[i] = w
		}
	}
	t.rows = append(t.rows, cells)
}

// Flush 打印所有攒下的行，并清空状态（可以继续用同一个 writer 写下一张表）。
func (t *tableWriter) Flush() error {
	if rest := t.buf.String(); rest != "" {
		t.addRow(strings.TrimSuffix(rest, "\n"))
	}
	t.buf.Reset()
	var sb strings.Builder
	for _, row := range t.rows {
		for i, c := range row {
			if i == len(row)-1 {
				sb.WriteString(c)
				break
			}
			sb.WriteString(c)
			for pad := t.width[i] - textwidth.Width(c) + 2; pad > 0; pad-- {
				sb.WriteByte(' ')
			}
		}
		sb.WriteByte('\n')
	}
	t.rows = nil
	t.width = nil
	if _, err := io.WriteString(t.out, sb.String()); err != nil {
		return err
	}
	return nil
}
