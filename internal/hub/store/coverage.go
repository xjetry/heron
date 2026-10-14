package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/xjetry/heron/internal/hub/metric"
)

// 已证实为真不会被未知覆盖；SQLite 标量 max(NULL, 1) 不具备这个性质。
func coverageOr(a, b string) string {
	return "CASE WHEN " + a + " = 1 OR " + b + " = 1 THEN 1 WHEN " + a + " IS NULL OR " + b + " IS NULL THEN NULL ELSE 0 END"
}

func coverageAnd(a, b string) string {
	return "CASE WHEN " + a + " = 0 OR " + b + " = 0 THEN 0 WHEN " + a + " IS NULL OR " + b + " IS NULL THEN NULL ELSE 1 END"
}

func coverageSource(i int) []string {
	if i == 0 {
		return []string{"reported AS minutes", "observed", coverageAnd("reported", "observed") + " AS both"}
	}
	return []string{"minutes", "observed", "both"}
}

// 每列独立传播未知；SUM 单独使用会把未知分钟当成不存在。
func coverageAggregates() []string {
	var out []string
	for _, c := range []string{"minutes", "observed", "both"} {
		out = append(out, "CASE WHEN count("+c+") = count(*) THEN sum("+c+") ELSE NULL END")
	}
	return out
}

func coverageCount(n sql.NullInt64) *uint32 {
	if !n.Valid {
		return nil
	}
	v := uint32(n.Int64)
	return &v
}

func (s *Store) CoverageStarts(ctx context.Context) (map[int64]int64, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT node_id, start_ts FROM node_coverage")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, start int64
		if err := rows.Scan(&id, &start); err != nil {
			return nil, err
		}
		out[id] = start
	}
	return out, rows.Err()
}

// QueryMetricsCoverage 的起点、水位、指标和覆盖汇总共用 queryFamily 的同一个快照。
// 汇总在输出重分桶之前读取源桶宽度，maxPoints 只改变展示，不改变可证明的分钟数。
func (s *Store) QueryMetricsCoverage(ctx context.Context, nodeID, from, to int64, lv Level, step int64) ([]metric.Row, metric.CoverageSummary, error) {
	var summary metric.CoverageSummary
	now := alignDown(s.clk.Now().Unix(), 60)
	rows, err := queryFamily(ctx, s, requestRead, metricFamily, queryShape{keyWhere: "node_id = ?", keyArgs: []any{nodeID}, seriesLimit: 1}, from, to, lv, step,
		func(rows *sql.Rows) ([]metric.Row, error) { return scanBucketRows(rows, nodeID) },
		func(tx *sql.Tx, sources string, args []any) error {
			var start int64
			err := tx.QueryRowContext(ctx, "SELECT start_ts FROM node_coverage WHERE node_id = ?", nodeID).Scan(&start)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			summary.CoverageStart = &start
			// 使用差值避免 ceil(from/60)*60 在极大 Unix 秒处溢出。
			end := min(alignDown(to, 60), now)
			begin := max(from, start)
			if end <= begin {
				return nil
			}
			summary.EligibleMinutes = uint64((end - begin) / 60)
			begin = end - int64(summary.EligibleMinutes)*60
			params := append(append([]any{}, args...), begin, end)
			// 两列各自求和仍守恒 observed_reported ≤ observed，前提是 reported = 0 的 1m 行必有 observed = 1（writeCoverageStart
			// 对纯观测行强制，upsert 的三值或合并保持它）：于是 1m 行 both = 1 ⇒ observed = 1、both 已知 ⇒ observed 已知，
			// 逐列 NULL 上卷把这两条带到 5m/1h 源行。每个源行对 both 之和的贡献因此不超过它对 observed 之和的贡献，
			// SUM 跳过 NULL 时也成立。
			return tx.QueryRowContext(ctx, "SELECT coalesce(sum(observed),0), coalesce(sum(both),0) FROM ("+sources+") WHERE ts >= ? AND source_width <= ? - ts", params...).Scan(&summary.ObservedMinutes, &summary.ObservedReportedMinutes)
		})
	return rows, summary, err
}

// coverageRowError 是一行的覆盖事实与指标层不变式矛盾时的拒绝原因。判定只取决于行本身和已提交的 node_coverage，
// 同一行重试必得同一结果，所以 WriteMinuteBatch 按行拒绝它；若当作整批错误返回，ingest.Flush 会把这一批留在
// 待重试队首、每轮先重试它，所有节点的分钟写入都被这一行挡住。
type coverageRowError struct{ reason string }

func (e coverageRowError) Error() string { return e.reason }

// writeCoverageStart 先完成全部判定再写起点：被拒绝的行不留下任何覆盖状态。起点用 DO NOTHING 写入，
// 已提交的起点不被后到的批改写。起点必须是正的分钟对齐时刻——0 会让 eligible 从 1970 年算起，
// 且 DO NOTHING 使它永远无法被后来的正确起点纠正。
func writeCoverageStart(tx *sql.Tx, r metric.Row) error {
	if r.CoverageStart <= 0 || r.CoverageStart%60 != 0 {
		return coverageRowError{fmt.Sprintf("coverage start %d is not a positive minute-aligned time", r.CoverageStart)}
	}
	if r.ObservationOnly {
		// 汇总的守恒 observed_reported ≤ observed 以"reported = 0 的行必有 observed = 1"为前提（见 QueryMetricsCoverage），
		// 纯观测行是唯一写入 reported = 0 的来源，所以它必须带 observed。
		if !r.Observed || !r.LastSeen.IsZero() || r.Source != "" {
			return coverageRowError{"observation-only row carries report state"}
		}
		for _, n := range r.Bucket.N {
			if n != 0 {
				return coverageRowError{"observation-only row carries metric samples"}
			}
		}
	}
	start := r.CoverageStart
	var stored int64
	switch err := tx.QueryRow("SELECT start_ts FROM node_coverage WHERE node_id=?", r.NodeID).Scan(&stored); {
	case err == nil:
		start = stored
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if r.Observed && (start > r.TS || r.CoverageStart > r.TS) {
		return coverageRowError{"observation precedes coverage start"}
	}
	_, err := tx.Exec("INSERT INTO node_coverage(node_id,start_ts) VALUES (?,?) ON CONFLICT DO NOTHING", r.NodeID, r.CoverageStart)
	return err
}

func metricSourceSQL(table string, i int) string {
	return "SELECT node_id, ts, " + strings.Join(append(metricColumnNames(), coverageSource(i)...), ", ") + " FROM " + table
}
