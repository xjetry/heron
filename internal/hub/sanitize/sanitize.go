// Package sanitize 清洗会进入日志、终端与页面的外来字符串。
package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// String 剔除控制字符（C0、C1、DEL）与双向控制符，并在 maxBytes 处按完整字符
// 截断。其他格式字符（ZWNJ、ZWJ 等）保留：它们是合法文字的一部分，剔除会毁掉
// 波斯语、印地语等文字里的正常词。
func String(s string, maxBytes int) string {
	var b strings.Builder
	for _, r := range s {
		if IsControl(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > maxBytes {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// IsControl 是 String 剔除的字符集：控制字符（C0、C1、DEL）与双向控制符。拒绝而不是剔除这些字符的入口（标签名）
// 经它判定，两处对"控制字符"是同一个口径。
func IsControl(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)
}

// Text 是显示成一行文字的外来字符串（节点名、页面标题、agent 上报的主机名）的共同口径：String 之后再裁首尾空白。
// 顺序不能反：先裁空白，剔除控制字符后会露出新的首尾空白。截断发生在裁空白之前，所以结果可能短于 maxBytes。
func Text(s string, maxBytes int) string {
	return strings.TrimSpace(String(s, maxBytes))
}
