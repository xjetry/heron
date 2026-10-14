package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/xjetry/heron/internal/hub/metric"
)

var (
	upsertMinute = metricUpsert("metric_1m")
	selectMinute = metricSelect("metric_1m")
)

// WriteMinuteBatch 是两族 1m 行的唯一写入口：同一事务里写指标行与探测行，节点存在性与探测行的任务存在性
// 在事务内检查（与 DeleteNode、DeleteProbeTask 串行）。删除事务提交之后到达的行——ingest 的待重试批次、删除前已累积
// 的分钟桶——因此一律被拒，已删主体的时序行在删除提交后不再增加，清理作业的复扫（cleanup.go）不会被之后落库的行
// 推翻。各族按自己的 5m 水位做冻结检查——两族各自上卷，
// 一族的水位不能替另一族决定是否接受写入。被拒绝的行计数返回并记日志，其余行照常写入。
// 比较的是已持久化的水位而不是时钟，墙钟回拨或重试旧桶都不能改写已冻结的历史。
// 按行拒绝的判定都是确定性的（重试同一行必得同一结果）；只有 SQL 错误作为整批错误返回，留给调用方重试。
func (s *Store) WriteMinuteBatch(ctx context.Context, batch metric.Batch) (int, error) {
	rejected := 0
	err := s.write(ctx, func(tx *sql.Tx) error {
		var metricUpto, probeUpto int64
		if err := tx.QueryRow("SELECT upto_ts FROM rollup_state WHERE level = ?", metricFamily.states[1]).Scan(&metricUpto); err != nil {
			return err
		}
		if err := tx.QueryRow("SELECT upto_ts FROM rollup_state WHERE level = ?", probeFamily.states[1]).Scan(&probeUpto); err != nil {
			return err
		}
		existing := map[int64]bool{}
		exists := func(nodeID int64) (bool, error) {
			if ok, checked := existing[nodeID]; checked {
				return ok, nil
			}
			ok, err := nodeExistsTx(tx, nodeID)
			if err == nil {
				existing[nodeID] = ok
			}
			return ok, err
		}
		existingTasks := map[uint64]bool{}
		taskExists := func(taskID uint64) (bool, error) {
			if ok, checked := existingTasks[taskID]; checked {
				return ok, nil
			}
			var ok bool
			err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM probe_task WHERE id = ?)", int64(taskID)).Scan(&ok)
			if err == nil {
				existingTasks[taskID] = ok
			}
			return ok, err
		}
		for _, r := range batch.Rows {
			ok, err := exists(r.NodeID)
			if err != nil {
				return err
			}
			if !ok {
				rejected++
				s.log.Warn("minute row for deleted node dropped", "node", r.NodeID)
				continue
			}
			if r.TS < metricUpto {
				rejected++
				s.log.Warn("minute row before rollup watermark dropped", "node", r.NodeID, "ts", r.TS, "watermark", metricUpto)
				continue
			}
			args := append([]any{r.NodeID, r.TS}, bucketArgs(r.Bucket)...)
			if err := writeCoverageStart(tx, r); err != nil {
				var bad coverageRowError
				if !errors.As(err, &bad) {
					return err
				}
				// 与"节点已删除""早于水位"不同，这不是正常运行会产生的状态：live 构造的行总满足这些判定，
				// 出现即说明上游或库内状态有缺陷，所以记 Error 而不是 Warn。
				rejected++
				s.log.Error("minute row with inconsistent coverage fact dropped", "node", r.NodeID, "ts", r.TS, "reason", bad.reason)
				continue
			}
			args = append(args, !r.ObservationOnly, r.Observed)
			if _, err := tx.Exec(upsertMinute, args...); err != nil {
				return err
			}
			if !r.LastSeen.IsZero() {
				// last_source 与 last_seen_at 取自同一次上报，同一条语句写入；空串（那次取不到对端）保留已有的值。
				// 写入后的来源与 country_ip 不同时一并清空查得的国家：它是对旧地址的答案（见 node.country）。读者看不到
				// "新地址配旧国家"的中间态，因为两处改动在同一个写事务里提交（整批在 s.write 的一个事务里），读连接池
				// 只读已提交的快照。SET 右侧读的都是更新前的列值，所以清空条件要重算一遍写入后的来源，不能引用刚赋的
				// last_source。
				if _, err := tx.Exec(`UPDATE node SET last_seen_at = ?1, last_source = COALESCE(NULLIF(?2, ''), last_source),
					country = CASE WHEN COALESCE(NULLIF(?2, ''), last_source) = country_ip THEN country ELSE '' END,
					country_ip = CASE WHEN COALESCE(NULLIF(?2, ''), last_source) = country_ip THEN country_ip ELSE '' END
					WHERE id = ?3`,
					r.LastSeen.Unix(), r.Source, r.NodeID); err != nil {
					return err
				}
			}
		}
		for _, r := range batch.Probes {
			ok, err := exists(r.NodeID)
			if err != nil {
				return err
			}
			if !ok {
				rejected++
				s.log.Warn("probe row for deleted node dropped", "node", r.NodeID, "task", r.TaskID)
				continue
			}
			if ok, err = taskExists(r.TaskID); err != nil {
				return err
			} else if !ok {
				rejected++
				s.log.Warn("probe row for deleted task dropped", "node", r.NodeID, "task", r.TaskID)
				continue
			}
			if r.TS < probeUpto {
				rejected++
				s.log.Warn("probe row before rollup watermark dropped", "node", r.NodeID, "task", r.TaskID, "ts", r.TS, "watermark", probeUpto)
				continue
			}
			if _, err := tx.Exec(upsertProbeMinute, probeArgs(r)...); err != nil {
				return err
			}
		}
		return nil
	})
	return rejected, err
}

func (s *Store) ReadMinuteRows(ctx context.Context, nodeID int64, from, to int64) ([]metric.Row, error) {
	rows, err := s.r.QueryContext(ctx, selectMinute, nodeID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBucketRows(rows, nodeID)
}

// scanBucketRows 按 metricColumnNames 的顺序扫描 (ts, 列…) 行；整数列先落到
// int64 再转回 float64。分钟读与聚合读共用它，列序只在描述表里定义一次。
func scanBucketRows(rows *sql.Rows, nodeID int64) ([]metric.Row, error) {
	var out []metric.Row
	for rows.Next() {
		b := metric.NewBucket()
		var ts int64
		var minutes, observed, both sql.NullInt64
		dest := []any{&ts}
		// 扫描目标与 metricColumnNames 同序；整数列先落到 int64 再转回 float64。
		ints := make([]int64, 0, 3*len(metric.Columns))
		for _, c := range metric.Columns {
			if c.Type == metric.Int {
				ints = append(ints, 0, 0)
				dest = append(dest, &ints[len(ints)-2], &ints[len(ints)-1])
				if c.Kind == metric.MeanMax {
					ints = append(ints, 0)
					dest = append(dest, &ints[len(ints)-1])
				}
			} else {
				ints = append(ints, 0)
				dest = append(dest, new(float64), &ints[len(ints)-1])
				if c.Kind == metric.MeanMax {
					dest = append(dest, new(float64))
				}
			}
		}
		dest = append(dest, &minutes, &observed, &both)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		di := 1
		for i, c := range metric.Columns {
			if c.Type == metric.Int {
				b.Sum[i] = float64(*dest[di].(*int64))
				b.N[i] = uint32(*dest[di+1].(*int64))
				di += 2
				if c.Kind == metric.MeanMax {
					b.Max[i] = float64(*dest[di].(*int64))
					di++
				}
			} else {
				b.Sum[i] = *dest[di].(*float64)
				b.N[i] = uint32(*dest[di+1].(*int64))
				di += 2
				if c.Kind == metric.MeanMax {
					b.Max[i] = *dest[di].(*float64)
					di++
				}
			}
		}
		out = append(out, metric.Row{NodeID: nodeID, TS: ts, Bucket: b, Coverage: metric.Coverage{
			Minutes: coverageCount(minutes), Observed: coverageCount(observed), ObservedReported: coverageCount(both),
		}})
	}
	return out, rows.Err()
}
