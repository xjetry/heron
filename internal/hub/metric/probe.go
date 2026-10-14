package metric

import heronv1 "github.com/xjetry/heron/gen/heron/v1"

// ProbeBucket 是一分钟内某任务探测结果的可加折叠。
// ingest 负责在结构校验时拒绝缺 outcome 的结果；这里也只统计三种明确的结果，
// 缺 outcome 不增加任何计数，避免破坏读侧推导 RTT 样本数所依赖的恒等式。
//
// RttN 是带 rtt 的结果数，恒等于 Sent − Lost − Errors；RttN 为 0 时 RttMinUs / RttMaxUs
// 没有含义，落库为 NULL——若落成 0，上卷的 min() 会把"没有样本"当成 0 µs。
type ProbeBucket struct {
	Sent, Lost, Errors uint32
	RttSumUs           uint64
	RttN               uint32
	RttMinUs, RttMaxUs uint32
}

func (b *ProbeBucket) Add(r *heronv1.ProbeResult) {
	switch o := r.GetOutcome().(type) {
	case *heronv1.ProbeResult_RttUs:
		b.Sent++
		b.addRtt(o.RttUs)
	case *heronv1.ProbeResult_Timeout:
		b.Sent++
		b.Lost++
	case *heronv1.ProbeResult_Error:
		b.Sent++
		b.Errors++
	}
}

func (b *ProbeBucket) addRtt(us uint32) {
	if b.RttN == 0 || us < b.RttMinUs {
		b.RttMinUs = us
	}
	if b.RttN == 0 || us > b.RttMaxUs {
		b.RttMaxUs = us
	}
	b.RttSumUs += uint64(us)
	b.RttN++
}

// Merge 把 o 加进 b；与写库时的 ON CONFLICT 合并是同一种运算。
func (b *ProbeBucket) Merge(o *ProbeBucket) {
	b.Sent += o.Sent
	b.Lost += o.Lost
	b.Errors += o.Errors
	if o.RttN > 0 {
		if b.RttN == 0 || o.RttMinUs < b.RttMinUs {
			b.RttMinUs = o.RttMinUs
		}
		if b.RttN == 0 || o.RttMaxUs > b.RttMaxUs {
			b.RttMaxUs = o.RttMaxUs
		}
		b.RttSumUs += o.RttSumUs
		b.RttN += o.RttN
	}
}

// RttMean 的第二个返回值为 false 表示桶内没有任何 rtt 样本。
func (b *ProbeBucket) RttMean() (uint32, bool) {
	if b.RttN == 0 {
		return 0, false
	}
	return uint32(b.RttSumUs / uint64(b.RttN)), true
}

// ProbeRow 是一个 (节点, 分钟, 任务) 桶：live 刷出，store 也用它返回上卷与查询聚合后的桶。
type ProbeRow struct {
	NodeID int64
	TS     int64
	TaskID uint64
	Bucket *ProbeBucket
}

// Batch 是一次刷出的全部行：指标桶与探测桶来自同一批闭合的分钟，由写协程在同一事务落盘。
type Batch struct {
	Rows   []Row
	Probes []ProbeRow
}

func (b Batch) Empty() bool { return len(b.Rows) == 0 && len(b.Probes) == 0 }

// WithoutNode 在批次层统一过滤各族，删除节点时不能把任一族的待写行留给重试。
// 返回独立的行切片，不改变原批次；桶内容只读复用。
func (b Batch) WithoutNode(id int64) Batch {
	var kept Batch
	for _, row := range b.Rows {
		if row.NodeID != id {
			kept.Rows = append(kept.Rows, row)
		}
	}
	for _, row := range b.Probes {
		if row.NodeID != id {
			kept.Probes = append(kept.Probes, row)
		}
	}
	return kept
}
