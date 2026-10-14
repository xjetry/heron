package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/xjetry/heron/internal/releasesig"
)

// 一次从 GitHub 取回的期限分两种，判定分开：总上限经 ctx 交给来源（见 source），只管"慢到不可用"；
// 停滞期限在读正文时判定（watchedBody），只管"不再前进"。两者都不用连接上的总时限（http.Client.Timeout）承载：
// 它按请求计时、覆盖读完正文，比总上限短就取代总上限成为归档实际能用的时长，比总上限长又分不出慢与停。
// 更新器从 GitHub 取（OfficialSource.Fetch）与 hub 中转从 GitHub 取（updates.Relay 的取回函数即 OfficialSource.Fetch）
// 读的是同一个 OfficialSource.get，用的是这里同一组常量。
const (
	// minDownloadRate 是一次取回在总上限内必须能完成的最低平均速率（字节/秒），DownloadLimit 由它推出。
	minDownloadRate = 64 << 10
	// DownloadLimit 是一次从 GitHub 取回（清单、签名、归档三个顺序请求）的总上限。最大的取回是三份文件各自的上限之和
	// （maxArchive 128 MiB + releasesig.MaxSums 1 MiB + releasesig.MaxFile 1 KiB），在 minDownloadRate 下约 2064 秒
	// （34 分 24 秒），向上取整为 35 分钟（TestDownloadLimitDerivation 从常量重算）。实际的 agent 归档约 10 MiB，
	// 在该速率下约 160 秒；连接不再前进时由 stallTimeout 更早结束，不必等到这里。
	DownloadLimit = 35 * time.Minute
	// stallTimeout 是读正文时连续收不到任何字节即判失败的时长。minDownloadRate 的链路每秒都有字节到达，几十秒一个字节
	// 都没有说明连接已不再前进（对端停发、中间设备丢了连接状态），继续等至多把失败推迟到 DownloadLimit，且分不出慢与停。
	// 取 60 秒而不更短，是给短暂断流（链路切换、重传退避）留余量：它只决定"停了多久才认定"，不影响慢而不停的取回。
	stallTimeout = 60 * time.Second
)

// maxFetchBytes 是一次从 GitHub 取回的字节上界，DownloadLimit 按它推导。
const maxFetchBytes = maxArchive + releasesig.MaxSums + releasesig.MaxFile

var errStalled = errors.New("download stalled")

// timeLimit 返回调用方经 ctx 给这次取回的总时长；没有期限的 ctx 返回 errUnbounded（见 source），来源据此不发请求。
func timeLimit(ctx context.Context) (time.Duration, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, errUnbounded
	}
	return roundDuration(time.Until(deadline)), nil
}

// watchedBody 包装一次请求的正文：每次 Read 收到字节就把停滞计时器推回 stall，计时器到点时以 errStalled 取消这次
// 请求的 ctx。停滞时 Read 正阻塞着、不会自己返回，判定必须能结束它：请求 ctx 结束时 net/http 关闭连接、让 Read
// 返回错误，所以判定落在取消 ctx 上，不能只在 Read 返回后比较时间。
type watchedBody struct {
	r     io.Reader
	timer *time.Timer
	stall time.Duration
	n     int64
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.n += int64(n)
		b.timer.Reset(b.stall)
	}
	return n, err
}

// transferError 的文案带进度，cause 供 errors.Is 区分停滞、超总上限与其他读错误。
type transferError struct {
	msg   string
	cause error
}

func (e *transferError) Error() string { return e.msg }
func (e *transferError) Unwrap() error { return e.cause }

// readError 按 cause 把读正文的失败写成带进度的文案：停滞写停了多久与已用时长，超总上限写上限；其余保留原错误。
// cause 取自请求 ctx：watchedBody 的计时器以 errStalled 取消它，调用方 ctx 到期时它继承 context.DeadlineExceeded。
func readError(err, cause error, stall, limit time.Duration, got, total int64, elapsed time.Duration) error {
	progress := "after " + formatBytes(got)
	if total >= 0 {
		progress += " of " + formatBytes(total)
	}
	switch {
	case errors.Is(cause, errStalled):
		return &transferError{fmt.Sprintf("read official release: no data for %gs %s in %s", stall.Seconds(), progress, roundDuration(elapsed)), errStalled}
	case errors.Is(cause, context.DeadlineExceeded):
		return &transferError{fmt.Sprintf("read official release: download exceeded %s %s", limit, progress), context.DeadlineExceeded}
	}
	return &transferError{fmt.Sprintf("read official release: %v %s in %s", err, progress, roundDuration(elapsed)), err}
}

func formatBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// roundDuration 让文案里的时长可读：一秒以上取整到秒，不足一秒取整到毫秒（只在缩短了期限的测试里出现）。
func roundDuration(d time.Duration) time.Duration {
	if d >= time.Second {
		return d.Round(time.Second)
	}
	return d.Round(time.Millisecond)
}
