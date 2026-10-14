package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TaskFingerprint 是探测任务在基线意义上的身份：种类与目标。两者任一变化，旧样本测的就不是同一条线路，
// 基线作废并从变化时刻重新累积。间隔、超时等不进指纹：它们改变采样密度，不改变被测的时延。
func TaskFingerprint(kind int64, target string) string { return fmt.Sprintf("%d:%s", kind, target) }

type baselineKey struct{ rule, node int64 }

// AlertBaseline 是 alert_baseline 的一行，列的含义见 ddlAlertBaseline。
type AlertBaseline struct {
	RuleID, NodeID  int64
	BaselineUs      int64
	Buckets         int
	ComputedAt      time.Time
	TaskFingerprint string
	// AccumulateFrom 零值表示没有下界（库里是 0）。
	AccumulateFrom time.Time
}

// Baselines 是一次读出的全部基线行与各探测任务当前的指纹，二者来自同一个读事务：行的指纹与任务的指纹比较时，
// 不会把并发改任务前后的两份状态拼在一起。
type Baselines struct {
	rows         map[baselineKey]AlertBaseline
	fingerprints map[uint64]string
}

// Row 是 (规则, 节点) 的基线行；ok 为假表示没有行。
func (b Baselines) Row(ruleID, nodeID int64) (AlertBaseline, bool) {
	row, ok := b.rows[baselineKey{ruleID, nodeID}]
	return row, ok
}

// Fingerprint 是任务当前的指纹；任务不存在时 ok 为假。
func (b Baselines) Fingerprint(taskID uint64) (string, bool) {
	fp, ok := b.fingerprints[taskID]
	return fp, ok
}

// Usable 给出 (规则, 节点) 此刻可用于判定的基线（微秒）：行存在、行的指纹等于规则所指任务当前的指纹、桶数不少于规则的
// 最小样本数。否则基线无效，规则既不触发也不恢复。指纹在这里与行比较而不只靠重算轮次去重置：任务改目标之后、
// 下一轮重算之前的评估同样不能拿旧目标的基线去判新目标的时延。
func (b Baselines) Usable(r AlertRule, nodeID int64) (AlertBaseline, bool) {
	row, ok := b.Row(r.ID, nodeID)
	if !ok || row.Buckets < r.BaselineMinSamples || row.Buckets == 0 {
		return AlertBaseline{}, false
	}
	if fp, ok := b.Fingerprint(r.TaskID); !ok || fp != row.TaskFingerprint {
		return AlertBaseline{}, false
	}
	return row, true
}

// ReadAlertBaselines 读出全部基线行与全部任务的指纹。两张都是配置层的小表（行数 ≤ 规则数 × 节点数、任务数），
// 走轻池：它不是历史读，不经 EvaluationReader。
func (s *Store) ReadAlertBaselines(ctx context.Context) (Baselines, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Baselines{}, err
	}
	defer tx.Rollback()
	out := Baselines{rows: map[baselineKey]AlertBaseline{}, fingerprints: map[uint64]string{}}
	rows, err := tx.QueryContext(ctx, "SELECT rule_id, node_id, baseline_us, buckets, computed_at, task_fingerprint, accumulate_from FROM alert_baseline")
	if err != nil {
		return Baselines{}, err
	}
	for rows.Next() {
		var b AlertBaseline
		var computed, from int64
		if err := rows.Scan(&b.RuleID, &b.NodeID, &b.BaselineUs, &b.Buckets, &computed, &b.TaskFingerprint, &from); err != nil {
			rows.Close()
			return Baselines{}, err
		}
		b.ComputedAt = time.Unix(computed, 0).UTC()
		if from != 0 {
			b.AccumulateFrom = time.Unix(from, 0).UTC()
		}
		out.rows[baselineKey{b.RuleID, b.NodeID}] = b
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Baselines{}, err
	}
	tasks, err := tx.QueryContext(ctx, "SELECT id, kind, target FROM probe_task")
	if err != nil {
		return Baselines{}, err
	}
	defer tasks.Close()
	for tasks.Next() {
		var id, kind int64
		var target string
		if err := tasks.Scan(&id, &kind, &target); err != nil {
			return Baselines{}, err
		}
		out.fingerprints[uint64(id)] = TaskFingerprint(kind, target)
	}
	if err := tasks.Err(); err != nil {
		return Baselines{}, err
	}
	return out, tx.Commit()
}

// SaveAlertBaseline 写一行基线，返回是否写入。重算轮次不持引擎的写锁，读历史与这次写之间规则可能被删、
// 改成别的任务或不再用自适应基线，节点可能被删，任务可能改了目标；所以写事务里重新核对：规则仍是指向 b 所据任务的
// 自适应基线规则、节点存在、任务当前的指纹等于 b.TaskFingerprint。任一不成立就不写（返回 false），删除之后排队的写
// 不会重建孤儿行，也不会把按旧目标算出的基线挂到新目标上。
func (s *Store) SaveAlertBaseline(ctx context.Context, taskID uint64, b AlertBaseline) (bool, error) {
	var written bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		written = false
		var kind, rttMode, baselineMode, metric sql.NullString
		var task sql.NullInt64
		err := tx.QueryRow("SELECT kind, metric, task_id, rtt_mode, baseline_mode FROM alert_rule WHERE id = ?", b.RuleID).Scan(&kind, &metric, &task, &rttMode, &baselineMode)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rule := AlertRule{Kind: AlertKind(kind.String), Metric: ProbeMetric(metric.String), RttMode: RttMode(rttMode.String), BaselineMode: BaselineMode(baselineMode.String)}
		if !rule.AdaptiveBaseline() || uint64(task.Int64) != taskID {
			return nil
		}
		exists, err := nodeExistsTx(tx, b.NodeID)
		if err != nil || !exists {
			return err
		}
		var taskKind int64
		var target string
		err = tx.QueryRow("SELECT kind, target FROM probe_task WHERE id = ?", int64(taskID)).Scan(&taskKind, &target)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if TaskFingerprint(taskKind, target) != b.TaskFingerprint {
			return nil
		}
		var from int64
		if !b.AccumulateFrom.IsZero() {
			from = b.AccumulateFrom.Unix()
		}
		if _, err := tx.Exec("INSERT OR REPLACE INTO alert_baseline (rule_id, node_id, baseline_us, buckets, computed_at, task_fingerprint, accumulate_from) VALUES (?, ?, ?, ?, ?, ?, ?)",
			b.RuleID, b.NodeID, b.BaselineUs, b.Buckets, b.ComputedAt.Unix(), b.TaskFingerprint, from); err != nil {
			return err
		}
		written = true
		return nil
	})
	return written, err
}

// PruneAlertBaselines 删除规则在 keep 之外的节点上的基线行：作用域或任务分配收缩之后，那些 (规则, 节点) 不再被评估。
func (s *Store) PruneAlertBaselines(ctx context.Context, ruleID int64, keep []int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		query := "DELETE FROM alert_baseline WHERE rule_id = ?"
		args := []any{ruleID}
		if len(keep) > 0 {
			query += " AND node_id NOT IN (" + placeholders(len(keep)) + ")"
			for _, id := range keep {
				args = append(args, id)
			}
		}
		_, err := tx.Exec(query, args...)
		return err
	})
}

// probeBucketMeansSQL 钉 INDEXED BY probe_5m_by_task：store 从不跑 ANALYZE，无统计时规划器可能按主键
// (node_id, ts, task_id) 定位，读出该节点窗口内全部任务的行再逐行过滤，读量随节点的任务数放大。走
// (task_id, node_id, ts) 前缀时读到的只有这一个任务在这个节点上的桶。它不经 queryFamily，所以自己带上
// probeFamily.live：删除节点或任务之后、清理作业完成之前的孤儿桶对这条读同样不可见（见 family.live）。
var probeBucketMeansSQL = `SELECT CAST(rtt_sum_us AS REAL) / (sent - lost - errors) FROM probe_5m INDEXED BY probe_5m_by_task
WHERE task_id = ? AND node_id = ? AND ` + probeFamily.live + ` AND ts >= ? AND ts <= ? AND sent - lost - errors > 0 ORDER BY ts`

// ProbeBucketMeans 返回一个任务在一个节点上、整段落在 [from, to) 内的各个 5 分钟桶的 rtt 桶均值（微秒），按时间升序；
// 没有 rtt 样本的桶（RttN = sent − lost − errors 为 0）不在其中。桶均值与每分钟评估的分钟均值同一口径
// （RttSumUs / RttN，见 metric.ProbeBucket）。只读 probe_5m：基线是小时级缓存，尚未上卷的最近几分钟本就落在判定窗口
// 附近，不属于基线。窗口不得超过 MaxBaselineWindowS，一次至多 8640 个桶；读走评估池（见 evaluationPoolSize）。
func (e *EvaluationReader) ProbeBucketMeans(ctx context.Context, nodeID int64, taskID uint64, from, to int64) ([]float64, error) {
	lv, _ := LevelByName("5m")
	if to <= from || to-from > MaxBaselineWindowS {
		return nil, fmt.Errorf("probe bucket means: window [%d, %d) must be non-empty and at most %d seconds", from, to, MaxBaselineWindowS)
	}
	rows, err := e.s.ev.QueryContext(ctx, probeBucketMeansSQL, int64(taskID), nodeID, from, to-lv.Bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var mean float64
		if err := rows.Scan(&mean); err != nil {
			return nil, err
		}
		out = append(out, mean)
	}
	return out, rows.Err()
}
