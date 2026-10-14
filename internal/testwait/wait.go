// Package testwait 只给测试用。正向等待的上界不参与被测性质。
package testwait

import (
	"testing"
	"time"
)

// Bound 是正向等待的上界：只为挂死时能以具体失败信息结束，不参与被测性质。
// 空载开 -race 时单次 argon2id 校验（internal/hub/auth 的 argonTime、argonMemory、argonThreads）约 270ms，不开约 30ms。
// 负载约 40 时 Login 曾超过 2 秒。
// 30 秒没有在更高负载下实测过，只要求正确实现碰不到它。
const Bound = 30 * time.Second

// When 推迟到格式化时再求值。Until 在超时当时才格式化参数，失败信息里要读那时状态的值用它包一层。
type When func() string

func (w When) String() string { return w() }

// Until 在 Bound 内轮询 cond。间隔由调用方给：观察的是协程还是外部进程，频率不该写死在上界旁边。
// 到上界仍未满足时用 format 结束，调用点不必再判断返回值。参数在报告时才格式化。
func Until(t testing.TB, interval time.Duration, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(Bound)
	for {
		if cond() {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf(format, args...)
		}
		time.Sleep(interval)
	}
}
