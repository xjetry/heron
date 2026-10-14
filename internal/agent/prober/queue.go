package prober

import (
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/probelimit"
	"google.golang.org/protobuf/proto"
)

// QueueCap 按硬限制任务的产出上界预留：Take 只丢严格超龄结果，MaxResultAge=120s 的闭区间
// 内每任务至多 MaxResultAge/MinIntervalS+2=120/5+2=26 次，MaxTasksPerNode=64 共 1664 条。
// 两个额外位置分别覆盖闭区间触发边界与区间前触发、区间内完成的一次测量；
// Scheduler.run 保证单任务串行及触发间隔，最小间隔由 probelimit.CheckTask 保证。
// 回队批次属于同一窗口，不重复预留；Push/Requeue 在锁内维持 At 有序，trim 丢最旧，
// 因而容量覆盖窗口上界时超容只会丢已超龄结果，剩余容量为余量。
const QueueCap = 4096

const _ = uint(QueueCap - probelimit.MaxTasksPerNode*(int(probelimit.MaxResultAge/time.Second)/probelimit.MinIntervalS+2))

type Result struct {
	TaskID  uint64
	Outcome Outcome
	At      time.Duration
	// ConfigID 是产生本结果时任务的配置身份（ProbeTask.config_id 原样，空 = 身份未知）。
	// 队列里的结果保留自己的身份：任务之后改了内容也不改写已入队结果的身份。
	ConfigID []byte
}

// Queue 满时丢最旧并计数，不等待上报腾出空间；mu 只保护有界内存操作。
type Queue struct {
	mu      sync.Mutex
	items   []Result
	cap     int
	dropped uint64
}

func NewQueue(capacity int) *Queue {
	if capacity < 0 {
		panic("queue capacity must be nonnegative")
	}
	return &Queue{cap: capacity}
}

func (q *Queue) Push(r Result) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.insert(r, false)
	q.trim()
}

// At 在取得队列锁前记录，协程抢占可让完成较早的测量更晚入队；插入时不能假定时间有序。
// 同刻的新结果追加，回队则让旧批优先，保留同刻结果的重试顺序。调用者持 mu。
func (q *Queue) insert(r Result, beforeEqual bool) {
	i := len(q.items)
	for i > 0 && (q.items[i-1].At > r.At || (beforeEqual && q.items[i-1].At == r.At)) {
		i--
	}
	q.items = append(q.items, Result{})
	copy(q.items[i+1:], q.items[i:])
	q.items[i] = r
}

func (q *Queue) trim() {
	if n := len(q.items) - q.cap; n > 0 {
		q.dropped += uint64(n)
		copy(q.items, q.items[n:])
		clear(q.items[len(q.items)-n:])
		q.items = q.items[:len(q.items)-n]
	}
}

// Take 丢弃超龄结果：hub 会拒收它们，继续保留只会占用上报体积。
func (q *Queue) Take(now, maxAge time.Duration, limit int) []Result {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Result
	remaining := q.items[:0]
	for _, r := range q.items {
		if now-r.At > maxAge {
			q.dropped++
		} else if len(out) < limit {
			out = append(out, r)
		} else {
			remaining = append(remaining, r)
		}
	}
	clear(q.items[len(remaining):])
	q.items = remaining
	return out
}

// Clear 作废全部已入队结果并计入 dropped：休眠信号（规则见 client.Runner）触发时，跨越休眠的结果
// 在队列里没有字段可区分（age_ms 由单调钟折算，单调钟不含休眠时间），只能整体丢弃；
// 丢弃口径与 trim / Take 的超龄丢弃共用同一个计数。
func (q *Queue) Clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropped += uint64(len(q.items))
	clear(q.items)
	q.items = q.items[:0]
}

// Requeue 合回失败批次；期间可能有较早完成却延迟入队的结果，不能直接把整批前置。
func (q *Queue) Requeue(rs []Result) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := len(rs) - 1; i >= 0; i-- {
		q.insert(rs[i], true)
	}
	q.trim()
}

func (q *Queue) Dropped() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

func ToProto(rs []Result, now time.Duration) []*heronv1.ProbeResult {
	out := make([]*heronv1.ProbeResult, 0, len(rs))
	for _, r := range rs {
		// 取 now 与取队列不是原子操作；较晚入队的结果不能转成溢出的无符号 age。
		p := &heronv1.ProbeResult{TaskId: r.TaskID, AgeMs: uint32(min(max((now-r.At)/time.Millisecond, 0), math.MaxUint32)), TaskConfigId: r.ConfigID}
		switch {
		case r.Outcome.Err != "":
			// 协议字符串必须是合法 UTF-8；只在出队编码处统一限制字节数，不切断多字节字符。
			message := strings.ToValidUTF8(r.Outcome.Err, "\uFFFD")
			if len(message) > probelimit.MaxErrorMessageLen {
				end := probelimit.MaxErrorMessageLen
				for !utf8.RuneStart(message[end]) {
					end--
				}
				message = message[:end]
			}
			p.Outcome = &heronv1.ProbeResult_Error{Error: &heronv1.ProbeError{Message: message}}
		case r.Outcome.Timeout:
			p.Outcome = &heronv1.ProbeResult_Timeout{Timeout: &heronv1.Timeout{}}
			// 候选只随 timeout 结果；整条消息的有无即候选的有无。
			if pr := r.Outcome.Presented; pr != nil {
				p.Presented = &heronv1.PresentedCertificate{SpkiSha256: pr.SPKI[:], NotAfterS: pr.NotAfterS, Reason: pr.Reason}
			}
		default:
			p.Outcome = &heronv1.ProbeResult_RttUs{RttUs: r.Outcome.RttUs}
			// 证书到期时刻只在 rtt_us 成功结果上透传；0 表示未携带，不下发。
			if r.Outcome.CertNotAfter != 0 {
				p.CertNotAfterS = proto.Int64(r.Outcome.CertNotAfter)
			}
		}
		out = append(out, p)
	}
	return out
}
