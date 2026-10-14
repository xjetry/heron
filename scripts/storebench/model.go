package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/hub/metric"
)

// 取值模型。库的大小由每个值在 SQLite 记录里占几个字节决定，而不是由值本身：整数按量级取 0（恰为 0 或 1）、1、2、3、4、6、
// 8 字节（界是 127、32767、8388607、2^31−1、2^47−1），REAL 列里绝对值小于 2^47 的整值按整数存（0.0 占 0 字节），其余 8 字节；
// 记录头每列另占 1 字节的类型码。这组规则在 modernc.org/sqlite v1.59.0（SQLite 3.53.4）上实测过，size 的逐列核算按它计算，
// 核算出的记录长与 dbstat 的载荷一致（size_test 守住）。所以模型要给对的是每列落在哪个字节档、多大比例为 0 或缺失，下面逐列写出档位与依据。
//
// 生产只给得出总量（prod-facts：指标三表约 42 万行、库 82.8 MB，每行上界 197 字节），给不出逐列分布；逐列的量级按 agent
// 采样的含义与小型 VPS 的常见规模定，作为参数化的假设，结论里与生产上界对照。
//
// 每个 1m 桶的样本数：hub 默认 TTL 30 s（cmd/hub/config.go 的 defaultTTL），上报间隔 = TTL / agentwire.ReportsPerTTL =
// 10 s，一分钟 6 次上报、每次一份 Metrics，所以 *_n 典型为 6；上报相位抖动让少数分钟落 5 或 7 次。5m 桶约 30、1h 桶约 360，
// 都是 1 字节档之上：30 占 1 字节，360 占 2 字节。
const defaultReportInterval = 30 * time.Second / agentwire.ReportsPerTTL

// samplesPerMinute 由上面的间隔推出（6），模型不另写一个 6。
var samplesPerMinute = int(time.Minute / defaultReportInterval)

// nodeProfile 是一个节点在整个时间轴上的典型读数。逐行的值在典型值附近抖动：和 = 样本数 × 典型值 × [0.9, 1.1]，
// 峰值 = 典型值 × [1, 1.5]（百分比封顶 100）。抖动不改变字节档，只让 REAL 和不落在整数上。
type nodeProfile struct {
	// container 为真表示 LXC 一类的环境范围节点：agent 的 CPU 走 cgroup，不报 steal / iowait；负载范围不是宿主，load1 与
	// load1_per_core 不报；容器里没有整盘设备计数，磁盘速率不报（internal/agent/collect/collect.go）。这些列 n = 0，
	// 和与峰值落库为 0，各占 0 字节。
	container bool
	// stealProne 为假的宿主节点 steal 恒为 0.0（多数 KVM 小机型 /proc/stat 的 steal 增量是 0），和为 0.0、占 0 字节。
	stealProne bool
	cpu        float64 // 百分比，对数均匀 [0.5, 40]：REAL，和与峰值各 8 字节
	memUsed    float64 // 字节，总内存 {0.5,1,2,4,8,16} GiB × [0.2, 0.8]：峰值 4 字节（< 2 GiB）或 6 字节，和（×6）6 字节
	memPct     float64
	swapUsed   float64 // 一半节点无 swap，读数恒为 0（n 照常计数，和占 0 字节）；其余 [0, 300] MiB：和 4～6 字节
	diskUsed   float64 // 字节，盘 {10,20,40,80,160,320} GB × [0.1, 0.8]：和 6 字节
	diskPct    float64
	cores      float64
	load1      float64 // 核数 × [0.02, 0.8]：REAL 8 字节
	tcp        float64 // 对数均匀 [5, 2000]：和 1～4 字节
	udp        float64 // 对数均匀 [1, 100]：和 1～2 字节
	procs      float64 // 均匀 [50, 400]：和 2 字节
	netRx      float64 // 字节/秒，对数均匀 [500, 5e6]：速率均值与峰值 2～3 字节，分钟字节数 3～4 字节
	netTx      float64
	diskRead   float64 // 字节/秒，对数均匀 [100, 1e6]
	diskWrite  float64 // 字节/秒，对数均匀 [1e3, 2e6]
	steal      float64 // 百分比 [0.05, 3]，仅 stealProne
	iowait     float64 // 百分比 [0.01, 2]
	// TCP 速率：TCP 计数属于网络命名空间，容器节点同样上报，不设 absent。速率是 REAL：和（建连两列
	// 还有峰值）带抖动不落在整值上，各 8 字节。
	tcpActiveOpens  float64 // 主动建连 次/秒，对数均匀 [0.05, 20]
	tcpPassiveOpens float64 // 被动接入 次/秒，对数均匀 [0.05, 50]
	tcpOutSegs      float64 // 发出段 段/秒，对数均匀 [20, 5000]
	tcpRetransSegs  float64 // 重传段 段/秒 = 发出段 × [1e-4, 2e-2]
}

func logUniform(r *rand.Rand, lo, hi float64) float64 {
	return math.Exp(math.Log(lo) + r.Float64()*(math.Log(hi)-math.Log(lo)))
}

func pick(r *rand.Rand, values []float64, weights []int) float64 {
	total := 0
	for _, w := range weights {
		total += w
	}
	x := r.IntN(total)
	for i, w := range weights {
		if x < w {
			return values[i]
		}
		x -= w
	}
	return values[len(values)-1]
}

const gib = 1 << 30

func newProfile(r *rand.Rand, containerFrac float64) nodeProfile {
	var p nodeProfile
	p.container = r.Float64() < containerFrac
	p.stealProne = !p.container && r.Float64() < 0.3
	p.cpu = logUniform(r, 0.5, 40)
	memTotal := pick(r, []float64{0.5 * gib, gib, 2 * gib, 4 * gib, 8 * gib, 16 * gib}, []int{10, 25, 30, 20, 10, 5})
	frac := 0.2 + 0.6*r.Float64()
	p.memUsed, p.memPct = memTotal*frac, frac*100
	if r.Float64() < 0.5 {
		p.swapUsed = 300 * (1 << 20) * r.Float64()
	}
	diskTotal := pick(r, []float64{10e9, 20e9, 40e9, 80e9, 160e9, 320e9}, []int{10, 30, 30, 15, 10, 5})
	frac = 0.1 + 0.7*r.Float64()
	p.diskUsed, p.diskPct = diskTotal*frac, frac*100
	p.cores = pick(r, []float64{1, 2, 4, 8}, []int{30, 40, 20, 10})
	p.load1 = p.cores * (0.02 + 0.78*r.Float64())
	p.tcp = logUniform(r, 5, 2000)
	p.udp = logUniform(r, 1, 100)
	p.procs = 50 + 350*r.Float64()
	p.netRx = logUniform(r, 500, 5e6)
	p.netTx = logUniform(r, 500, 5e6)
	p.diskRead = logUniform(r, 100, 1e6)
	p.diskWrite = logUniform(r, 1e3, 2e6)
	if p.stealProne {
		p.steal = 0.05 + 2.95*r.Float64()
	}
	p.iowait = 0.01 + 1.99*r.Float64()
	// TCP 速率的抽样放在末尾：已有字段先取完各自的随机数，不受新增抽样影响；同一条随机流上更后面的 profile
	// 与探测剖面整体后移，分布不变。重传段 = 发出段 × 重传比 [1e-4, 2e-2]：重传的取值只有相对发出段才有意义。
	p.tcpActiveOpens = logUniform(r, 0.05, 20)
	p.tcpPassiveOpens = logUniform(r, 0.05, 50)
	p.tcpOutSegs = logUniform(r, 20, 5000)
	p.tcpRetransSegs = p.tcpOutSegs * logUniform(r, 1e-4, 2e-2)
	return p
}

// colReading 是一列在一个桶里的读数来源：level 是每样本典型值，absent 为真即该列不入账（n = 0）。
type colReading struct {
	level   float64
	absent  bool
	percent bool
}

// readings 按 metric.Columns 的名字给出每列的读数来源。描述表里出现模型不认识的列时直接报错：新增指标列而模型没有跟上，
// 生成出的库会让这一列恒为默认 0，大小被低估且没有任何提示。
func (p *nodeProfile) readings() ([]colReading, error) {
	out := make([]colReading, len(metric.Columns))
	for i, c := range metric.Columns {
		var cr colReading
		switch c.Name {
		case "cpu":
			cr = colReading{level: p.cpu, percent: true}
		case "mem_used":
			cr = colReading{level: p.memUsed}
		case "swap_used":
			cr = colReading{level: p.swapUsed}
		case "disk_used":
			cr = colReading{level: p.diskUsed}
		case "load1":
			cr = colReading{level: p.load1, absent: p.container}
		case "tcp":
			cr = colReading{level: p.tcp}
		case "udp":
			cr = colReading{level: p.udp}
		case "procs":
			cr = colReading{level: p.procs}
		case "rx_bytes":
			// Sum 列：每次上报入账一次增量，增量 = 速率 × 上报间隔。
			cr = colReading{level: p.netRx * defaultReportInterval.Seconds()}
		case "tx_bytes":
			cr = colReading{level: p.netTx * defaultReportInterval.Seconds()}
		case "memory_used_pct":
			cr = colReading{level: p.memPct, percent: true}
		case "disk_used_pct":
			cr = colReading{level: p.diskPct, percent: true}
		case "net_rx_bps":
			cr = colReading{level: p.netRx}
		case "net_tx_bps":
			cr = colReading{level: p.netTx}
		case "disk_read_bps":
			cr = colReading{level: p.diskRead, absent: p.container}
		case "disk_write_bps":
			cr = colReading{level: p.diskWrite, absent: p.container}
		case "cpu_steal_pct":
			cr = colReading{level: p.steal, absent: p.container, percent: true}
		case "cpu_iowait_pct":
			cr = colReading{level: p.iowait, absent: p.container, percent: true}
		case "load1_per_core":
			cr = colReading{level: p.load1 / p.cores, absent: p.container}
		case "tcp_active_opens_ps":
			cr = colReading{level: p.tcpActiveOpens}
		case "tcp_passive_opens_ps":
			cr = colReading{level: p.tcpPassiveOpens}
		case "tcp_out_segs_ps":
			cr = colReading{level: p.tcpOutSegs}
		case "tcp_retrans_segs_ps":
			cr = colReading{level: p.tcpRetransSegs}
		default:
			return nil, fmt.Errorf("value model has no distribution for metric column %q", c.Name)
		}
		out[i] = cr
	}
	return out, nil
}

// bucketSamples 是一个桶的上报次数：1m 桶 6 次，相位抖动让约 2% 的分钟少一次、约 2% 多一次；粗级桶是其覆盖分钟的和。
func bucketSamples(r *rand.Rand, minutes int) int {
	n := 0
	for range minutes {
		switch x := r.IntN(100); {
		case x < 2:
			n += samplesPerMinute - 1
		case x < 4:
			n += samplesPerMinute + 1
		default:
			n += samplesPerMinute
		}
	}
	return n
}

// newBucket 生成一个桶：samples 是桶的上报次数。gen 写库与 time 的并发写入共用它，两处的行是同一个模型。
func newBucket(r *rand.Rand, cols []colReading, samples int) *metric.Bucket {
	b := metric.NewBucket()
	for i := range metric.Columns {
		cr := cols[i]
		if cr.absent {
			continue
		}
		b.N[i] = uint32(samples)
		if cr.level == 0 {
			continue
		}
		b.Sum[i] = float64(samples) * cr.level * (0.9 + 0.2*r.Float64())
		b.Max[i] = cr.level * (1 + 0.5*r.Float64())
		if cr.percent {
			b.Sum[i] = math.Min(b.Sum[i], 100*float64(samples))
			b.Max[i] = math.Min(b.Max[i], 100)
		}
	}
	return b
}

// bucketValues 把桶展开成值列，与 store 的 bucketArgs 同序同型（metric.Columns 顺序：_sum、_n，MeanMax 再加 _max；
// Int 列转 int64，Float 列保持 float64）。
func bucketValues(b *metric.Bucket, args []any) []any {
	for i, c := range metric.Columns {
		if c.Type == metric.Int {
			args = append(args, int64(b.Sum[i]), int64(b.N[i]))
			if c.Kind == metric.MeanMax {
				args = append(args, int64(b.Max[i]))
			}
		} else {
			args = append(args, b.Sum[i], int64(b.N[i]))
			if c.Kind == metric.MeanMax {
				args = append(args, b.Max[i])
			}
		}
	}
	return args
}

// probeProfile 是一个 (节点, 任务) 对的典型探测结果。默认任务与面板新建任务的默认值一致（web/src/pages/ProbeTasks.tsx 的
// emptyDraft：ICMP、间隔 60 s、超时 1000 ms），一分钟一次探测，1m 行 sent = 1。
//   - rtt 对数均匀 [2, 250] ms，以微秒存：单个样本 2～3 字节；5m、1h 的和 3～4 字节，最小 / 最大 2～3 字节。
//   - 丢包：70% 的对不丢，25% 丢 0.5%，5% 丢 3%；全丢的桶没有 rtt 样本，rtt_min_us / rtt_max_us 为 NULL（metric.ProbeBucket）。
//   - errors：每次探测 0.1% 概率，计入 sent、不计 rtt。
type probeProfile struct {
	rttUs    float64
	lossRate float64
}

func newProbeProfile(r *rand.Rand) probeProfile {
	p := probeProfile{rttUs: logUniform(r, 2000, 250000)}
	switch x := r.IntN(100); {
	case x < 70:
	case x < 95:
		p.lossRate = 0.005
	default:
		p.lossRate = 0.03
	}
	return p
}

// newProbeBucket 生成一个探测桶：sent 次探测逐次按丢包率、错误率与 rtt 抖动折叠（metric.ProbeBucket 的 Add 语义）。
func newProbeBucket(r *rand.Rand, p probeProfile, sent int) *metric.ProbeBucket {
	b := &metric.ProbeBucket{}
	for range sent {
		b.Sent++
		x := r.Float64()
		switch {
		case x < p.lossRate:
			b.Lost++
			continue
		case x < p.lossRate+0.001:
			b.Errors++
			continue
		}
		rtt := uint32(p.rttUs * (0.9 + 0.3*r.Float64()))
		if b.RttN == 0 || rtt < b.RttMinUs {
			b.RttMinUs = rtt
		}
		if b.RttN == 0 || rtt > b.RttMaxUs {
			b.RttMaxUs = rtt
		}
		b.RttSumUs += uint64(rtt)
		b.RttN++
	}
	return b
}

// probeBucketValues 与 store 的 probeArgs 同序（sent、lost、errors、rtt_sum_us、rtt_min_us、rtt_max_us）：没有 rtt 样本时
// 最小 / 最大为 NULL。
func probeBucketValues(b *metric.ProbeBucket, args []any) []any {
	args = append(args, int64(b.Sent), int64(b.Lost), int64(b.Errors), int64(b.RttSumUs))
	if b.RttN == 0 {
		return append(args, nil, nil)
	}
	return append(args, int64(b.RttMinUs), int64(b.RttMaxUs))
}
