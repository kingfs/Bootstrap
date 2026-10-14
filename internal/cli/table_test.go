package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestTableWriterAlignsByDisplayWidth：中文表头必须按**显示列数**对齐
// （一个汉字占两列）——这是换掉 text/tabwriter 的全部理由。
func TestTableWriterAlignsByDisplayWidth(t *testing.T) {
	var buf bytes.Buffer
	w := newTableWriter(&buf)
	w.Write([]byte("项\t值\n"))
	w.Write([]byte("系统\tlinux/arm64\n"))
	w.Write([]byte("配置文件\t/home/me/.config/bootstrap/config.yaml\n"))
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("行数 = %d, want 3：\n%s", len(lines), buf.String())
	}
	// "配置文件" 是最宽的第一列（4 个汉字 = 8 列），后面补 2 个空格。
	// 第一列按最宽的「配置文件」（8 列）对齐，列间再留 2 个空格：
	// 项(2)+8、系统(4)+6、配置文件(8)+2。
	want := []string{
		"项        值",
		"系统      linux/arm64",
		"配置文件  /home/me/.config/bootstrap/config.yaml",
	}
	for i, wl := range want {
		if lines[i] != wl {
			t.Errorf("第 %d 行 = %q，want %q", i+1, lines[i], wl)
		}
	}
}

// TestTableWriterDetails：最后一列不补空格、空单元格照旧参与对齐、Flush 后可复用。
func TestTableWriterDetails(t *testing.T) {
	var buf bytes.Buffer
	w := newTableWriter(&buf)
	w.Write([]byte("工具\t结果\t方式\t说明\n"))
	w.Write([]byte("git\t完成\n"))                    // 短行：末尾的空列不该留下尾随空格
	w.Write([]byte("go\t失败\tgoinstall\tbuiltin\n")) // 中间空列要补齐
	w.Write([]byte("\t\t\t$ go install x@v1\n"))    // 缩进续行（-v 用）
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	out := buf.String()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("行尾不该有空格：%q", line)
		}
	}
	if !strings.Contains(out, "$ go install x@v1") {
		t.Errorf("续行丢了：\n%s", out)
	}
	// 续行的命令要对齐到最后一列（前面三格按列宽补空格）。
	if !strings.Contains(out, "        $ go install x@v1") {
		t.Errorf("续行没有对齐到「说明」列：\n%s", out)
	}

	buf.Reset()
	w.Write([]byte("a\tb\n"))
	if err := w.Flush(); err != nil {
		t.Fatalf("第二次 flush: %v", err)
	}
	if got := buf.String(); got != "a  b\n" {
		t.Errorf("Flush 之后 writer 应当可以继续用，得到 %q", got)
	}
}

func TestSummarizeList(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e"}
	if got := summarizeList(ids, 12); got != "a, b, c, d, e" {
		t.Errorf("不超过上限时应当全列：%q", got)
	}
	if got := summarizeList(ids, 3); got != "a, b, c 等 5 个" {
		t.Errorf("超过上限时应当带总数：%q", got)
	}
}
