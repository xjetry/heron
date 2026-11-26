package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/hub/metric"
)

// TrafficCounter 是差分基线的一项：Name 为空是合计形态的唯一一项，非空是一张网卡。计数是入账时截到 MaxInt64 的值，
// 与 SQLite INTEGER 同宽，非负。
type TrafficCounter struct {
	Name   string
	Rx, Tx int64
}

// TrafficRecord 是 traffic 表的一行加上它在 traffic_baseline 表里的基线行。基线（BootID、NetCounterEpoch、Baseline）
// 与累计值必须同一事务写入：只要 agent 未换启动周期、计数未倒退（合计形态另要求集合摘要不变），崩溃后下一次上报
// 相对已落盘基线做差分能补回丢失的内存增量；若两者分开落盘，基线新、累计旧的一次崩溃就会永久少记一段。
//
// Baseline 恰是三种形态之一（CheckTrafficBaseline）：nil 表示没有基线；一项且名称为空是合计形态，NetCounterEpoch
// 是它的集合摘要；其余是逐网卡形态，按名称字节序、名称唯一，NetCounterEpoch 为空。
type TrafficRecord struct {
	NodeID                               int64
	BootID                               string
	NetCounterEpoch                      string
	Baseline                             []TrafficCounter
	TotalRx, TotalTx, PeriodRx, PeriodTx int64
	PeriodStart                          time.Time
	// ObservedAt 与基线同事务保存，Unix 秒 0 表示未知，读回为零值 time.Time。
	ObservedAt time.Time
}

// CheckTrafficBaseline 是三种基线形态的唯一判定，写库、读库与恢复预检共用：
//   - 没有基线：零项，摘要为空。旧库里校正建立、last_rx = last_tx = -1 的行迁移成这种形态。
//   - 合计形态：恰一项、名称为空，摘要为空或合法的 SHA-256 小写十六进制。
//   - 逐网卡形态：一到 agentwire.MaxNetInterfaces 项，名称经 agentwire.ValidateInterfaceName 准入、按字节序严格递增，
//     摘要为空——集合由名称本身给出，合计视图按 agentwire.SummarizeNetInterfaces 推导，不另存一份可能不一致的摘要。
//
// 计数都必须非负。形态混用（空名与网卡名并存、逐网卡带摘要）会让账本对同一份基线有两种读法，一律拒收。
func CheckTrafficBaseline(epoch string, baseline []TrafficCounter) error {
	if err := agentwire.ValidateCounterEpoch(epoch); err != nil {
		return err
	}
	for _, c := range baseline {
		if c.Rx < 0 || c.Tx < 0 {
			return fmt.Errorf("traffic baseline: counters must be non-negative")
		}
	}
	switch {
	case len(baseline) == 0:
		if epoch != "" {
			return fmt.Errorf("traffic baseline: net_counter_epoch must be empty without a baseline")
		}
	case len(baseline) == 1 && baseline[0].Name == "":
	default:
		if len(baseline) > agentwire.MaxNetInterfaces {
			return fmt.Errorf("traffic baseline: at most %d interfaces; got %d", agentwire.MaxNetInterfaces, len(baseline))
		}
		if epoch != "" {
			return fmt.Errorf("traffic baseline: net_counter_epoch must be empty for a per-interface baseline")
		}
		for i, c := range baseline {
			if err := agentwire.ValidateInterfaceName(c.Name); err != nil {
				return fmt.Errorf("traffic baseline: interface %w", err)
			}
			if i > 0 && baseline[i-1].Name >= c.Name {
				return fmt.Errorf("traffic baseline: interface names must be unique and sorted by bytes")
			}
		}
	}
	return nil
}

const upsertTraffic = `INSERT INTO traffic (node_id, boot_id, total_rx, total_tx, period_rx, period_tx, period_start, updated_at, net_counter_epoch, observed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (node_id) DO UPDATE SET boot_id = excluded.boot_id,
  total_rx = excluded.total_rx, total_tx = excluded.total_tx, period_rx = excluded.period_rx, period_tx = excluded.period_tx,
  period_start = excluded.period_start, updated_at = excluded.updated_at, net_counter_epoch = excluded.net_counter_epoch,
  observed_at = excluded.observed_at`

// WriteTraffic 在一个事务里写入全部记录，并返回实际写入的节点 ID。每条记录整体替换该节点的基线行。节点已删除的
// 记录跳过：删除后迟到的刷出不得重建从属行，与 WriteMinuteBatch 同一处理；存在性在写事务内判定，与 DeleteNode 串行。
// 任一记录的基线形态不合法则整批不写。
func (s *Store) WriteTraffic(ctx context.Context, recs []TrafficRecord) ([]int64, error) {
	var written []int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		written = nil
		now := s.clk.Now().Unix()
		for _, r := range recs {
			if err := CheckTrafficBaseline(r.NetCounterEpoch, r.Baseline); err != nil {
				return fmt.Errorf("node %d: %w", r.NodeID, err)
			}
			exists, err := nodeExistsTx(tx, r.NodeID)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
			var observed int64
			if !r.ObservedAt.IsZero() {
				observed = r.ObservedAt.Unix()
			}
			if _, err := tx.Exec(upsertTraffic, r.NodeID, r.BootID, r.TotalRx, r.TotalTx,
				r.PeriodRx, r.PeriodTx, r.PeriodStart.Unix(), now, r.NetCounterEpoch, observed); err != nil {
				return err
			}
			if _, err := tx.Exec("DELETE FROM traffic_baseline WHERE node_id = ?", r.NodeID); err != nil {
				return err
			}
			for _, c := range r.Baseline {
				if _, err := tx.Exec("INSERT INTO traffic_baseline (node_id, iface, rx, tx) VALUES (?, ?, ?, ?)", r.NodeID, c.Name, c.Rx, c.Tx); err != nil {
					return err
				}
			}
			written = append(written, r.NodeID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return written, nil
}

// LoadTraffic 读出全部记录。两次查询在同一个读事务里，基线行与 traffic 行来自同一次提交。基线形态不合法、或基线行
// 没有对应的 traffic 行时报错而不是丢弃：账本会从不合法的基线算出错误的增量，宁可让 hub 起不来也不静默放过。
func (s *Store) LoadTraffic(ctx context.Context) ([]TrafficRecord, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	baselines, err := readTrafficBaselines(ctx, tx, "main")
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT node_id, boot_id, total_rx, total_tx, period_rx, period_tx, period_start, net_counter_epoch, observed_at FROM traffic`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrafficRecord
	for rows.Next() {
		var r TrafficRecord
		var start, observed int64
		if err := rows.Scan(&r.NodeID, &r.BootID, &r.TotalRx, &r.TotalTx, &r.PeriodRx, &r.PeriodTx, &start, &r.NetCounterEpoch, &observed); err != nil {
			return nil, err
		}
		r.Baseline = baselines[r.NodeID]
		delete(baselines, r.NodeID)
		if err := CheckTrafficBaseline(r.NetCounterEpoch, r.Baseline); err != nil {
			return nil, fmt.Errorf("node %d: %w", r.NodeID, err)
		}
		r.PeriodStart = time.Unix(start, 0).UTC()
		if observed != 0 {
			r.ObservedAt = time.Unix(observed, 0).UTC()
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := orphanBaselines(baselines); err != nil {
		return nil, err
	}
	return out, nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// readTrafficBaselines 按节点读出 schema 里 traffic_baseline 的全部行，每个节点的行按名称字节序（空名排最前）。
func readTrafficBaselines(ctx context.Context, q queryer, schema string) (map[int64][]TrafficCounter, error) {
	rows, err := q.QueryContext(ctx, "SELECT node_id, iface, rx, tx FROM "+schema+".traffic_baseline ORDER BY node_id, iface")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]TrafficCounter{}
	for rows.Next() {
		var id int64
		var c TrafficCounter
		if err := rows.Scan(&id, &c.Name, &c.Rx, &c.Tx); err != nil {
			return nil, err
		}
		out[id] = append(out[id], c)
	}
	return out, rows.Err()
}

// orphanBaselines 报告取走全部 traffic 行之后剩下的基线行：WriteTraffic 与迁移总是连同 traffic 行一起写，
// 删除节点与恢复的孤儿清理按 nodeConfigTables 一起删，剩下的只能来自手工改库或损坏的快照。
func orphanBaselines(left map[int64][]TrafficCounter) error {
	for id := range left {
		return fmt.Errorf("node %d: traffic baseline rows without a traffic row", id)
	}
	return nil
}

// TrafficSchedules 返回每个节点的周期配置（重置日、重置时刻、计费时区）。它们住在 node 表：是节点的配置，
// 在节点建好、尚无一次上报时就该可改，而 traffic 行只在有流量状态后才存在。
func (s *Store) TrafficSchedules(ctx context.Context) (map[int64]TrafficSchedule, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT id, traffic_reset_day, traffic_reset_minute, billing_timezone FROM node")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]TrafficSchedule{}
	for rows.Next() {
		var id int64
		var c TrafficSchedule
		if err := rows.Scan(&id, &c.Day, &c.Minute, &c.Timezone); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// NetBytesBetween 返回节点在 [from, to) 内分钟桶里的收发字节和，口径是"起点落在区间里的分钟"：一分钟的增量全部
// 归到它起始的那一刻，所以端点不在整分上时（to 是现在）只有起点不早于 to 的分钟被排除。桶里只有 ingest 判定能归到
// 某一分钟的增量：hub 重启后的首次上报与跨过离线 TTL 的上报不进桶（ingest.Service.Report），校正也不进，所以这个和不大于
// 账本在同一区间的入账。to 不晚于 from 时为 0。
//
// 读法：区间按整点切开，整小时的中段读小时级（1h 行加上水位之后尚未上卷的 5m、1m 行，与 QueryMetrics 的小时级相同）；
// 端点所在、只取了一部分的那一小时（周期边界在 +05:30 这类非整点偏移的时区，或重置时刻是 HH:30 时就是这样）不整块
// 归到一侧，而是在仍保留的细级里逐级细分到端点（edgeHour）。全部读取在同一个只读事务里，水位、各级的行与清理的进度
// 属于同一个快照。
func (s *Store) NetBytesBetween(ctx context.Context, nodeID int64, from, to time.Time) (rx, tx int64, err error) {
	if !to.After(from) {
		return 0, 0, nil
	}
	// 端点换成整秒：桶的起点都是整秒，起点不早于 from 等价于不早于 ⌈from⌉，早于 to 等价于早于 ⌈to⌉。
	lo, hi := ceilSecond(from), ceilSecond(to)
	hour, _ := LevelByName("1h")
	// 预计扫描量：中段每小时至多一行，两端各至多一小时的 1m、5m、1h 行。估计只定首次选池，偏小由封顶计数兜住。
	estimate := ceilDiv(hi-lo, hour.Bucket) + 2*(hour.Bucket/60+hour.Bucket/300+1)
	db, scanCap := s.readPoolFor(estimate)
	rx, tx, err = s.netBytesBetween(ctx, db, scanCap, nodeID, lo, hi)
	if errors.Is(err, errScanCapped) {
		// 轻池上的事务已在 netBytesBetween 返回前回滚、归还连接，再到大扫描池上重跑（同 queryFamily）。
		return s.netBytesBetween(ctx, s.hr, 0, nodeID, lo, hi)
	}
	return rx, tx, err
}

// netBytesBetween 在 db 的一个只读事务里读 [lo, hi)（Unix 秒）的收发字节和，读法见 NetBytesBetween。
func (s *Store) netBytesBetween(ctx context.Context, db *sql.DB, scanCap int64, nodeID, lo, hi int64) (rx, tx int64, err error) {
	t, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, 0, err
	}
	defer t.Rollback()
	budget := newScanBudget(scanCap)
	const hour = 3600
	firstFull, lastFull := ceilDiv(lo, hour)*hour, alignDown(hi, hour)
	var sum netSum
	if firstFull < lastFull {
		rows, err := readNetLevel(ctx, t, budget, nodeID, 2, firstFull, lastFull)
		if err != nil {
			return 0, 0, err
		}
		sum.add(rows, firstFull, lastFull)
	}
	// 两端只取了一部分的小时：起点所在的那一小时（lo 不在整点上时），与终点所在的那一小时（hi 不在整点上、且不是同一小时
	// 或同一小时已被起点那一端读过时不重读）。
	for _, h := range edgeHours(lo, hi) {
		part, err := edgeHour(ctx, t, budget, nodeID, h, max(lo, h), min(hi, h+hour))
		if err != nil {
			return 0, 0, err
		}
		sum.rx, sum.tx = sum.rx+part.rx, sum.tx+part.tx
	}
	return bytesInt(sum.rx), bytesInt(sum.tx), nil
}

// ceilSecond 是不早于 t 的最近整秒，Unix 秒。
func ceilSecond(t time.Time) int64 {
	if t.Nanosecond() == 0 {
		return t.Unix()
	}
	return t.Unix() + 1
}

// edgeHours 是 [lo, hi) 里只取了一部分的整点小时的起点，至多两个，升序不重复。
func edgeHours(lo, hi int64) []int64 {
	const hour = 3600
	var out []int64
	if lo%hour != 0 {
		out = append(out, alignDown(lo, hour))
	}
	if h := alignDown(hi, hour); hi%hour != 0 && (len(out) == 0 || out[0] != h) {
		out = append(out, h)
	}
	return out
}

// edgeHour 读整点小时 [h, h+3600) 里 [a, b) 的收发字节和：从小时桶起逐级细分，只在跨在端点上的桶里往下一级看。
//
// 一个桶在下一级是否仍完整由数据本身判定：同一快照里，下一级落在这个桶里的行的和等于这个桶的值。上卷只把细级的和
// 原样写进粗级、清理只删已被上卷消费的细级行（Prune 的截止点不越过粗一级的水位），所以下一级的和不会大于这个桶；
// 相等就说明这个桶里有字节的细级行都还在。于是：
//   - 整个落在 [a, b) 里的桶整块计入，整个落在外面的不计入，不论下一级在不在；
//   - 跨在端点上的桶，下一级完整就换成下一级的行再按同样的规则分（1h → 5m → 1m），1m 的行按起点归属；
//   - 下一级不完整（那一级在这个桶里已过保留期被清理）时，这个桶整块归到它起始的一侧：起点不早于 a 且早于 b 才计入。
//
// 所以端点附近的 1m 行还在时结果就是逐分钟求和；1m 已清理而 5m 还在时，端点落在 5 分钟整点上（+05:30、+05:45 这类
// 偏移与 HH:00、HH:30 这类重置时刻都是）仍与逐分钟求和一致，否则跨在端点上的那个 5 分钟桶整块归到它起始的一侧；
// 细级都已清理时，端点所在的那一小时整块归到它起始的一侧——用于周期重算时，就是这一小时全部算在边界之前的周期里。
// 这是按保留期能给出的精度，不按分钟数比例摊：摊出来的数不对应任何真实读数。
func edgeHour(ctx context.Context, t *sql.Tx, budget *scanBudget, nodeID, h, a, b int64) (netSum, error) {
	const hour = 3600
	// 三级各读一次这一小时的行：小时级是逐级拼接的小时总量（一行），5m 级拼接其水位之后的 1m 行，1m 级只有 1m 表。
	var byLevel [3][]metric.Row
	for i := range byLevel {
		rows, err := readNetLevel(ctx, t, budget, nodeID, i, h, h+hour)
		if err != nil {
			return netSum{}, err
		}
		byLevel[i] = rows
	}
	return splitBuckets(byLevel[:], 2, a, b), nil
}

// splitBuckets 按 edgeHour 的规则把 levels[i] 级的桶（rows[i]）分到 [a, b) 两侧，跨在端点上的桶往 rows[i-1] 细分。
func splitBuckets(rows [][]metric.Row, i int, a, b int64) netSum {
	width := levels[i].Bucket
	var sum netSum
	for _, r := range rows[i] {
		switch {
		case r.TS >= a && r.TS+width <= b:
			sum.add([]metric.Row{r}, a, b)
		case r.TS+width <= a || r.TS >= b:
		default:
			var finer, whole netSum
			if i > 0 {
				finer.add(rows[i-1], r.TS, r.TS+width)
				whole.add([]metric.Row{r}, r.TS, r.TS+width)
			}
			if i > 0 && finer == whole {
				child := append([][]metric.Row(nil), rows[:i]...)
				child[i-1] = inBucket(rows[i-1], r.TS, r.TS+width)
				part := splitBuckets(child, i-1, a, b)
				sum.rx, sum.tx = sum.rx+part.rx, sum.tx+part.tx
				continue
			}
			sum.add([]metric.Row{r}, a, b)
		}
	}
	return sum
}

// inBucket 是起点在 [from, to) 里的行。
func inBucket(rows []metric.Row, from, to int64) []metric.Row {
	var out []metric.Row
	for _, r := range rows {
		if r.TS >= from && r.TS < to {
			out = append(out, r)
		}
	}
	return out
}

// readNetLevel 读 levels[i] 级在 [from, to)（两端都按该级桶长对齐）里逐桶的行：粗级拼接其水位之后尚未上卷的细级行
// （readFamilyTx），按该级桶长分桶。
func readNetLevel(ctx context.Context, t *sql.Tx, budget *scanBudget, nodeID int64, i int, from, to int64) ([]metric.Row, error) {
	lv := levels[i]
	return readFamilyTx(ctx, t, budget, metricFamily, queryShape{keyWhere: "node_id = ?", keyArgs: []any{nodeID}, seriesLimit: 1}, i, from, to-1, lv.Bucket,
		func(rows *sql.Rows) ([]metric.Row, error) { return scanBucketRows(rows, nodeID) }, nil)
}

// netSum 是一组桶的收发字节和，按桶里的 float64 累加（和在 2^53 以内精确，见 metric.Bucket）。比较两级的和是否相等
// 因此是精确比较：同一组整数按不同顺序相加，结果相同。
type netSum struct{ rx, tx float64 }

// add 累加起点在 [from, to) 里的桶。
func (s *netSum) add(rows []metric.Row, from, to int64) {
	for _, r := range rows {
		if r.TS >= from && r.TS < to {
			s.rx += r.Bucket.Sum[metric.RxBytes]
			s.tx += r.Bucket.Sum[metric.TxBytes]
		}
	}
}

// bytesInt 把桶里以 float64 累加的字节和转回 int64：和在 2^53 以内精确（见 metric.Bucket），到界时停在 MaxInt64，
// 手工改库留下的负和收敛到 0。
func bytesInt(v float64) int64 {
	switch {
	case v >= math.MaxInt64:
		return math.MaxInt64
	case v <= 0:
		return 0
	}
	return int64(v)
}
