package store

import (
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// v10 的完整 DDL：v9 加上探测任务的全部节点开关。
var schemaV10 = append(slices.Clone(schemaV9), "ALTER TABLE probe_task ADD COLUMN all_nodes INTEGER NOT NULL DEFAULT 0")

// 旧库升级后簿记表存在但没有行：从未成功跑过，不能被读成"刚跑过"。
func TestMigrationFromV10AddsEmptyMaintenanceState(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 10, seedMinuteRow)
	if v := userVersion(t, migrated.r); v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	stats, err := migrated.StorageStats(t.Context())
	if err != nil || stats.LastPrune != nil || stats.LastRollup != nil {
		t.Fatalf("maintenance after migration: prune=%v rollup=%v err=%v", stats.LastPrune, stats.LastRollup, err)
	}
}

func execStmts(t *testing.T, s *Store, stmts ...string) {
	t.Helper()
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range stmts {
			if _, err := tx.Exec(stmt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// finishedAt 直接读簿记表：nil 表示没有行。
func finishedAt(t *testing.T, s *Store, name string) *int64 {
	t.Helper()
	var at int64
	err := s.r.QueryRow("SELECT finished_at FROM maintenance_state WHERE name = ?", name).Scan(&at)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &at
}

func wantFinished(t *testing.T, s *Store, name string, want *int64) {
	t.Helper()
	got := finishedAt(t, s, name)
	if (got == nil) != (want == nil) || (got != nil && *got != *want) {
		t.Fatalf("%s finished_at = %v, want %v", name, deref(got), deref(want))
	}
	// 此处核对刚结束的维护结果，不等待对外统计的复用窗口。
	stats, err := s.computeStorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	read := stats.LastRollup
	if name == MaintenancePrune {
		read = stats.LastPrune
	}
	if (read == nil) != (want == nil) || (read != nil && *read != *want) {
		t.Fatalf("StorageStats %s = %v, want %v", name, deref(read), deref(want))
	}
}

func deref(v *int64) any {
	if v == nil {
		return "absent"
	}
	return *v
}

// 上卷中途失败（指标族已推进、探测族的 1h 水位写不进去）不记簿记：从未成功时仍无行，成功过的保持上次的时刻。
func TestRollupRecordsCompletionOnlyOnSuccess(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	failProbe1h := "CREATE TRIGGER fail_rollup BEFORE UPDATE ON rollup_state WHEN NEW.level = 'probe_1h' BEGIN SELECT RAISE(ABORT, 'rollup rejected'); END"
	execStmts(t, s, failProbe1h)
	if err := s.Rollup(t.Context()); err == nil {
		t.Fatal("rollup with a failing level succeeded")
	}
	wantFinished(t, s, MaintenanceRollup, nil)
	execStmts(t, s, "DROP TRIGGER fail_rollup")
	clk.Advance(time.Hour)
	if err := s.Rollup(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := clk.Now().Unix()
	wantFinished(t, s, MaintenanceRollup, &done)
	execStmts(t, s, failProbe1h)
	clk.Advance(2 * time.Hour)
	if err := s.Rollup(t.Context()); err == nil {
		t.Fatal("rollup with a failing level succeeded")
	}
	wantFinished(t, s, MaintenanceRollup, &done)
	wantFinished(t, s, MaintenancePrune, nil)
}

// prune 中途失败（1h 表的过期行删不掉）同样不记；成功后记下完成时刻。
func TestPruneRecordsCompletionOnlyOnSuccess(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	failDelete := "CREATE TRIGGER fail_prune BEFORE DELETE ON metric_1h BEGIN SELECT RAISE(ABORT, 'prune rejected'); END"
	execStmts(t, s, "INSERT INTO metric_1h (node_id, ts) VALUES (1, 0)", failDelete)
	if _, err := s.Prune(t.Context(), DefaultRetention); err == nil {
		t.Fatal("prune with a failing delete succeeded")
	}
	wantFinished(t, s, MaintenancePrune, nil)
	execStmts(t, s, "DROP TRIGGER fail_prune")
	clk.Advance(time.Minute)
	if n, err := s.Prune(t.Context(), DefaultRetention); err != nil || n != 1 {
		t.Fatalf("prune removed %d rows: %v", n, err)
	}
	done := clk.Now().Unix()
	wantFinished(t, s, MaintenancePrune, &done)
	execStmts(t, s, "INSERT INTO metric_1h (node_id, ts) VALUES (1, 0)", failDelete)
	clk.Advance(time.Minute)
	if _, err := s.Prune(t.Context(), DefaultRetention); err == nil {
		t.Fatal("prune with a failing delete succeeded")
	}
	wantFinished(t, s, MaintenancePrune, &done)
	wantFinished(t, s, MaintenanceRollup, nil)
}

// recordMaintenance 写簿记失败时，Rollup 与 Prune 都把错误包上一层前缀：RunMaintenance 只把返回的 error 原样
// 记成 "rollup failed"/"prune failed" 日志，不包装就只剩 SQLite 原始错误，看日志的人无法判断是上卷/清理本身
// 失败还是簿记没写进去——后者其实已经提交，只是这一轮的完成时刻没记上。
func TestMaintenanceRecordErrorIsWrapped(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	failRecord := "CREATE TRIGGER fail_maintenance_state BEFORE INSERT ON maintenance_state BEGIN SELECT RAISE(ABORT, 'maintenance state rejected'); END"
	execStmts(t, s, failRecord)
	if err := s.Rollup(t.Context()); err == nil || !strings.HasPrefix(err.Error(), "record rollup completion: ") {
		t.Fatalf("Rollup error = %v, want prefix %q", err, "record rollup completion: ")
	}
	if _, err := s.Prune(t.Context(), DefaultRetention); err == nil || !strings.HasPrefix(err.Error(), "record prune completion: ") {
		t.Fatalf("Prune error = %v, want prefix %q", err, "record prune completion: ")
	}
}

func ptr(v int64) *int64 { return &v }

func showSeries(series []SeriesHealth) string {
	var out []string
	for _, h := range series {
		out = append(out, fmt.Sprintf("%s(%s) oldest=%v watermark=%v", h.Table, h.Level.Name, deref(h.Oldest), deref(h.Watermark)))
	}
	return "[" + strings.Join(out, "; ") + "]"
}

// 最老桶按表分别给：六张表各造不同的 ts（另各有一行更新的），读出的是每张表自己的最小值；水位是 rollup_state 的
// 原值；空表与从未跑过的维护是缺失而不是 0。
func TestStorageStatsReportsSeriesHealthPerTable(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	stats, err := s.StorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	m5, h1 := levels[1], levels[2]
	want := []SeriesHealth{
		{Table: "metric_1m", Level: levels[0]}, {Table: "metric_5m", Level: m5, Watermark: ptr(0)}, {Table: "metric_1h", Level: h1, Watermark: ptr(0)},
		{Table: "probe_1m", Level: levels[0]}, {Table: "probe_5m", Level: m5, Watermark: ptr(0)}, {Table: "probe_1h", Level: h1, Watermark: ptr(0)},
	}
	if !reflect.DeepEqual(stats.Series, want) || stats.LastPrune != nil || stats.LastRollup != nil {
		t.Fatalf("empty database health = %s prune=%v rollup=%v, want %s and no maintenance", showSeries(stats.Series), deref(stats.LastPrune), deref(stats.LastRollup), showSeries(want))
	}
	oldest := map[string]int64{"metric_1m": 600, "metric_5m": 1200, "metric_1h": 7200, "probe_1m": 660, "probe_5m": 1500, "probe_1h": 10800}
	var stmts []string
	for table, ts := range oldest {
		cols := "node_id, ts"
		vals := "1, %d"
		if table[:5] == "probe" {
			cols, vals = "node_id, ts, task_id", "1, %d, 1"
		}
		for _, at := range []int64{ts, ts + 86400} {
			stmts = append(stmts, "INSERT INTO "+table+" ("+cols+") VALUES ("+fmt.Sprintf(vals, at)+")")
		}
	}
	stmts = append(stmts,
		"UPDATE rollup_state SET upto_ts = 111 WHERE level = '5m'", "UPDATE rollup_state SET upto_ts = 222 WHERE level = '1h'",
		"UPDATE rollup_state SET upto_ts = 333 WHERE level = 'probe_5m'", "UPDATE rollup_state SET upto_ts = 444 WHERE level = 'probe_1h'",
		"INSERT INTO maintenance_state (name, finished_at) VALUES ('rollup', 555)")
	execStmts(t, s, stmts...)
	// 明确越过复用窗口再检查新数据；窗口的精确边界由缓存测试负责。
	clk.Advance(61 * time.Second)
	stats, err = s.StorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range []*int64{nil, ptr(111), ptr(222), nil, ptr(333), ptr(444)} {
		want[i].Watermark = w
		want[i].Oldest = ptr(oldest[want[i].Table])
	}
	if !reflect.DeepEqual(stats.Series, want) || stats.LastPrune != nil || stats.LastRollup == nil || *stats.LastRollup != 555 {
		t.Fatalf("health = %s prune=%v rollup=%v, want %s, no prune, rollup 555", showSeries(stats.Series), deref(stats.LastPrune), deref(stats.LastRollup), showSeries(want))
	}
}

// 两组判定各自的边界：恰等于阈值不标红，再越过 1 秒标红；读数缺失不标红。
func TestStalenessBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	r := DefaultRetention
	for _, lv := range levels {
		// 60 是 MaintenanceInterval 的秒数，写死而不引用常量：Staleness 用该常量在保留期与桶宽之外再留一个维护间隔，常量改了而这里仍是 60，恰在阈值上的样本会错开，用例要红。
		oldestEdge := now.Unix() - int64(r.ForLevel(lv.Name)/time.Second) - lv.Bucket - 60
		watermarkEdge := now.Unix() - 3*lv.Bucket
		for _, tc := range []struct {
			name string
			h    SeriesHealth
			want Staleness
		}{
			{"oldest at threshold", SeriesHealth{Level: lv, Oldest: ptr(oldestEdge)}, Staleness{}},
			{"oldest past threshold", SeriesHealth{Level: lv, Oldest: ptr(oldestEdge - 1)}, Staleness{Oldest: true}},
			{"watermark at threshold", SeriesHealth{Level: lv, Watermark: ptr(watermarkEdge)}, Staleness{}},
			{"watermark past threshold", SeriesHealth{Level: lv, Watermark: ptr(watermarkEdge - 1)}, Staleness{Watermark: true}},
			{"absent readings", SeriesHealth{Level: lv}, Staleness{}},
		} {
			if got := tc.h.Staleness(now, r); got != tc.want {
				t.Errorf("%s %s: %+v, want %+v", lv.Name, tc.name, got, tc.want)
			}
		}
	}
}
