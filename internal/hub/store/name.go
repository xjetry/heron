package store

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/xjetry/heron/internal/hub/sanitize"
)

const MaxNameRunes = 64

// NameError 表示清洗后的名称长度不满足短名称约束。
type NameError struct{ Runes int }

func (e NameError) Error() string {
	return fmt.Sprintf("name must be 1–%d characters after trimming whitespace and control characters; got %d", MaxNameRunes, e.Runes)
}

// CleanName 按完整清洗结果计算码点数，超长时拒绝，不把截断值当作合法输入。
func CleanName(raw string) (string, error) {
	name := sanitize.Text(raw, len(raw))
	if n := utf8.RuneCountInString(name); n == 0 || n > MaxNameRunes {
		return "", NameError{Runes: n}
	}
	return name, nil
}

// AgentNodeName 把 agent 建议的名称规范化为 CleanName 可接受且不再改写的值。
// 截断可能露出新的尾空白，必须在截断后再裁一次；清洗后为空时使用非空缺省名。
func AgentNodeName(raw string) string {
	name := []rune(sanitize.Text(raw, len(raw)))
	if len(name) > MaxNameRunes {
		name = name[:MaxNameRunes]
	}
	clean := strings.TrimSpace(string(name))
	if clean == "" {
		return "node"
	}
	return clean
}
