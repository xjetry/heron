package update

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// commandOutput 运行 cmd 并返回去掉首尾空白的标准输出。失败的错误带上 name 与截断后的 stderr：查询已装版本的错误
// 经 Engine.reason 成为 UpdateStatus.reason，面板据此显示节点为何不支持在线更新，只有 "exit status 1" 的原因无从排查。
// stderr 只在 cmd.Stderr 未设置时由 Output 收进 ExitError，调用方不能自己接管 Stderr。
func commandOutput(cmd *exec.Cmd, name string) (string, error) {
	b, err := cmd.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				return "", fmt.Errorf("%s: %w: %s", name, err, boundedError(stderr))
			}
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}
