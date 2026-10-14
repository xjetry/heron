package ingest

import (
	"context"
	"time"

	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/probelimit"
)

// FlushPeriod 与分钟桶的闭合周期一致；nextFlushAt 把调度目标放在闭合后半秒，
// 不把调度目标当作写协程完成落盘的时限。
const FlushPeriod = time.Minute

// MaxProbeAge 是探测结果的迟到预算；它与刷出周期共同约束上卷滞后，
// 为数据在其分钟桶被冻结之前完成落盘预留时间。
const MaxProbeAge = probelimit.MaxResultAge

// 上卷滞后必须覆盖"迟到上限 + 一个刷出周期 + 写协程排队余量"，否则合法数据会
// 落在水位之前被写协程丢弃。差值为负时无法转换为 uint64，编译即失败；
// 仍满足该不等式的常量变更不会被拒绝。
const _ = uint64(store.RollupLag - (MaxProbeAge + FlushPeriod + 60*time.Second))

// maxPendingBatches 限制待重试列表：数据库长时间不可用时内存不无限增长，
// 满了丢最旧的一批并记日志。
const maxPendingBatches = 64

// Flush 把已闭合的分钟桶（all 为 true 时包括仍开着的）交给写协程。
// 写失败的批次留在待重试列表，下次先重试它们；被写协程按水位拒绝的行
// 不算失败——它们已经不可能再被正确并入。
func (s *Service) Flush(ctx context.Context, all bool) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	var batch metric.Batch
	if all {
		batch = s.live.Drain()
	} else {
		batch = s.live.Flush()
	}
	if !batch.Empty() {
		s.pending = append(s.pending, batch)
	}
	for len(s.pending) > maxPendingBatches {
		s.log.Error("dropping oldest unflushed minute batch", "rows", len(s.pending[0].Rows), "probes", len(s.pending[0].Probes))
		s.pending = s.pending[1:]
	}
	for len(s.pending) > 0 {
		batch := s.pending[0]
		rejected, err := s.writer.WriteMinuteBatch(ctx, batch)
		if err != nil {
			s.log.Error("minute flush failed, keeping batch for retry", "err", err, "batches", len(s.pending))
			return
		}
		if rejected > 0 {
			s.log.Warn("minute rows rejected by storage", "rejected", rejected)
		}
		s.pending = s.pending[1:]
	}
}

// RunFlusher 在每个分钟边界后半秒刷出一次，ctx 结束时把全部桶刷出后返回。
func (s *Service) RunFlusher(ctx context.Context) {
	for {
		wall := s.clk.Now()
		next := nextFlushAt(wall)
		timer := time.NewTimer(next.Sub(wall))
		select {
		case <-ctx.Done():
			timer.Stop()
			s.Flush(context.Background(), true)
			return
		case <-timer.C:
			s.Flush(ctx, false)
		}
	}
}

// nextFlushAt 的半秒偏移保证按墙钟分钟闭合的桶在刷出时确实已闭合。
func nextFlushAt(wall time.Time) time.Time {
	return wall.Truncate(FlushPeriod).Add(FlushPeriod + 500*time.Millisecond)
}
