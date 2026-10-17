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
	return normalizeShortName(raw, "node")
}

// normalizeShortName 共用清洗、码点截断与截断后去空白；调用方提供非空的规范缺省名。
func normalizeShortName(raw, fallback string) string {
	name := []rune(sanitize.Text(raw, len(raw)))
	if len(name) > MaxNameRunes {
		name = name[:MaxNameRunes]
	}
	clean := strings.TrimSpace(string(name))
	if clean == "" {
		return fallback
	}
	return clean
}

// UncleanNameError 表示写入的名字不是它自己的 CleanName 结果：带可去除的首尾空白或控制字符。
// 写入口要求调用方先规范化（CleanName 或 AgentNodeName），见到它说明调用方漏了清洗。
type UncleanNameError struct{ Name string }

func (e UncleanNameError) Error() string {
	return fmt.Sprintf("name must not have leading or trailing whitespace or control characters; got %q", e.Name)
}

// checkShortName 要求库里的节点名与 API token 名等于自己的 CleanName 结果，长度为 1–MaxNameRunes。
// 节点的 insertNode、UpdateNodeTasks 与 token 的 CreateAPIToken、UpdateAPIToken 在写事务里检查；
// 配置恢复的 validateSnapshotNodes、validateSnapshotAPITokens 在搬运前检查，旧数据由迁移 48/49 分别规范化。
// 调用方负责规范化，这里只拒绝不改写，避免把不同的写入输入静默落成同一个名字。
func checkShortName(name string) error {
	clean, err := CleanName(name)
	if err != nil {
		return err
	}
	if clean != name {
		return UncleanNameError{Name: name}
	}
	return nil
}
