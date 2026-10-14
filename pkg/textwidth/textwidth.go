// Package textwidth 计算文本在等宽终端里占多少**列**。
//
// 标准库给不出这个数：len(s) 是字节数，utf8.RuneCountInString(s) 是字符数，
// 而中日韩文字与全角标点在终端里占 **2** 列。表格要靠"列数"才能对齐——
// 否则中文表头的表格会随内容错位（`bs init` 的环境概览正是这种表）。
package textwidth

// Width 返回 s 在终端里占的列数。
func Width(s string) int {
	n := 0
	for _, r := range s {
		n += RuneWidth(r)
	}
	return n
}

// RuneWidth 返回单个字符占的列数：东亚宽/全角字符 2，组合与零宽字符 0，其余 1。
//
// 实现依据 Unicode 的 East Asian Width 属性（W/F 视为 2 列），用区间表近似——
// 目标是"表格对齐"，不追求 wcwidth 那样的完备（那需要整张 Unicode 表）。
func RuneWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20, r >= 0x7f && r < 0xa0:
		// 控制字符（含 \t 已在调用方拆列）：算 0 而不是 1，
		// 免得把不可见的字节算进列宽、又反过来让表格错位。
		return 0
	case r >= 0x300 && r <= 0x36f, // 组合用附加记号
		r >= 0x1ab0 && r <= 0x1aff,
		r >= 0x20d0 && r <= 0x20ff,
		r >= 0xfe00 && r <= 0xfe0f, // 变体选择符
		r >= 0xfe20 && r <= 0xfe2f,
		r == 0x200b, r >= 0x200b && r <= 0x200f, // 零宽空格/方向控制
		r == 0xfeff: // BOM
		return 0
	}
	if isWide(r) {
		return 2
	}
	return 1
}

// wideRanges 是 East Asian Width 为 W/F 的区间（按 lo 升序排列，便于二分……但区间很少，
// 顺序比较就够了）。
var wideRanges = [...]struct{ lo, hi rune }{
	{0x1100, 0x115f},   // 谚文字母
	{0x2e80, 0x303e},   // CJK 部首、康熙部首、CJK 符号（含表意空格 U+3000）
	{0x3041, 0x33ff},   // 假名、注音符号、CJK 兼容
	{0x3400, 0x4dbf},   // CJK 扩展 A
	{0x4e00, 0x9fff},   // CJK 统一表意文字
	{0xa000, 0xa4cf},   // 彝文
	{0xa960, 0xa97f},   // 谚文字母扩展 A
	{0xac00, 0xd7a3},   // 谚文音节
	{0xf900, 0xfaff},   // CJK 兼容表意文字
	{0xfe10, 0xfe19},   // 竖排标点
	{0xfe30, 0xfe6f},   // CJK 兼容形式与小写变体
	{0xff00, 0xff60},   // 全角 ASCII
	{0xffe0, 0xffe6},   // 全角符号（￥ 等）
	{0x1b000, 0x1b2ff}, // 假名补充、假名扩展
	{0x1f300, 0x1f64f}, // 表情符号
	{0x1f680, 0x1f6ff}, // 交通与地图符号
	{0x1f900, 0x1f9ff}, // 补充符号
	{0x1fa70, 0x1faff}, // 扩展 A 符号与图形
	{0x20000, 0x2fffd}, // CJK 扩展 B 及以后
	{0x30000, 0x3fffd}, // CJK 扩展 G 及以后
}

func isWide(r rune) bool {
	for _, rg := range wideRanges {
		if r >= rg.lo && r <= rg.hi {
			return true
		}
	}
	return false
}
