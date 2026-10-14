package store

import (
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/metric"
)

// seedRawProbes 绕过 WriteMinuteBatch 直接写 1m 探测行，用来构造已删主体的残留行：写入口拒收已删节点与已删任务的行。
func seedRawProbes(t *testing.T, s *Store, rows ...metric.ProbeRow) {
	t.Helper()
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.Exec(upsertProbeMinute, probeArgs(r)...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// schemaV38 是 v37 加清理作业表，逐字冻结。
var schemaV38 = append(slices.Clone(schemaV37),
	`CREATE TABLE cleanup_job (
  kind TEXT NOT NULL,
  node_id INTEGER NOT NULL DEFAULT 0,
  task_id INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (kind, node_id, task_id)
) WITHOUT ROWID`)

// 迁移前删任务从不删历史：已删任务的行要在迁移里补登记作业，现存任务的行不登记。三张探测表里出现的任务各自
// 被找到（只在 1h 里还有行的旧任务也算），任务 id 跳读不漏、不重。
func TestMigrationFromV37RegistersOrphanTaskJobs(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 37, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		if _, err := db.Exec(`INSERT INTO probe_task (id, kind, target, interval_s, timeout_ms, created_at, config_id) VALUES (5, 1, 'kept', 60, 1000, 0, x'00')`); err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct {
			table string
			task  int64
		}{{"probe_1m", 5}, {"probe_1m", 3}, {"probe_5m", 3}, {"probe_5m", 9}, {"probe_1h", 1}, {"probe_1h", 9}} {
			if _, err := db.Exec("INSERT INTO "+row.table+" (node_id, ts, task_id) VALUES (7, 600, ?)", row.task); err != nil {
				t.Fatal(err)
			}
		}
	})
	var got []int64
	rows, err := s.r.Query("SELECT task_id FROM cleanup_job WHERE kind = 'task' AND node_id = 0 AND created_at > 0 ORDER BY task_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []int64{1, 3, 9}; !slices.Equal(got, want) {
		t.Fatalf("migrated orphan task jobs = %v, want %v", got, want)
	}
	var all int
	if err := s.r.QueryRow("SELECT count(*) FROM cleanup_job").Scan(&all); err != nil || all != 3 {
		t.Fatalf("cleanup_job rows = %d (%v), want only the three task jobs", all, err)
	}
}

// seedProbeTasks 按给定 id 直接写入任务行。探测历史只为存在的任务落库（WriteMinuteBatch）、只对存在的任务可读
// （family.live），直接按 id 写探测行的用例要先让这些任务存在。任务不分配给任何节点，只占住 id。
func seedProbeTasks(t *testing.T, s *Store, ids ...uint64) {
	t.Helper()
	for _, id := range ids {
		if _, err := s.w.ExecContext(t.Context(), `INSERT OR IGNORE INTO probe_task (id, kind, target, interval_s, timeout_ms, created_at, config_id)
			VALUES (?, 1, 'example.com', 60, 1000, 0, randomblob(16))`, int64(id)); err != nil {
			t.Fatal(err)
		}
	}
}

// 夹具的每条序列在三级各跨若干个 pruneSlice：1m 每 6 小时一行共 3 天（3 片）、5m 每天一行共 14 天（2 片）、
// 1h 每 5 天一行共 60 天（2 片），一条序列 7 片。
const fixtureSlicesPerSeries = 7

var fixtureBase = time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC).Unix()

func fixtureTimes(level int) []int64 {
	var out []int64
	switch level {
	case 0:
		for k := range int64(12) {
			out = append(out, fixtureBase+k*6*3600)
		}
	case 1:
		for d := range int64(14) {
			out = append(out, fixtureBase+d*86400)
		}
	default:
		for k := range int64(12) {
			out = append(out, fixtureBase+k*5*86400)
		}
	}
	return out
}

// seedHistory 直接写入 node 在三级指标表（withMetrics 时）与三级探测表（每个任务）里的夹具行，绕过写入口与上卷。
func seedHistory(t *testing.T, s *Store, node int64, withMetrics bool, tasks ...uint64) {
	t.Helper()
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		for i := range levels {
			for _, ts := range fixtureTimes(i) {
				if withMetrics {
					if _, err := tx.Exec(insertMetricFixture(metricFamily.tables[i]), append([]any{node, ts}, bucketArgs(bucket(1))...)...); err != nil {
						return err
					}
				}
				for _, task := range tasks {
					if _, err := tx.Exec("INSERT INTO "+probeFamily.tables[i]+" (node_id, ts, task_id, sent) VALUES (?, ?, ?, 1)", node, ts, int64(task)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// subjectRows 数某主体在时序表里还剩多少行：node 非 0 按节点数六张表，否则按任务数三张探测表。
func subjectRows(t *testing.T, s *Store, node int64, task uint64) int64 {
	t.Helper()
	var total int64
	for _, f := range families {
		for _, table := range f.tables {
			var where string
			var arg int64
			switch {
			case node != 0:
				where, arg = "node_id = ?", node
			case f == probeFamily:
				where, arg = "task_id = ?", int64(task)
			default:
				continue
			}
			var n int64
			if err := s.r.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table+" WHERE "+where, arg).Scan(&n); err != nil {
				t.Fatal(err)
			}
			total += n
		}
	}
	return total
}

func jobCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.r.QueryRowContext(t.Context(), "SELECT count(*) FROM cleanup_job").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 删除节点与删除任务的写事务不触及任何时序表：六张表挂上 RAISE(ABORT) 触发器，删除仍然成功；作业已登记、时序行原样留着。
func TestDeleteTransactionsDoNotTouchHistoryTables(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	node, _, _ := s.CreateNode(ctx, NewNode{Name: "gone"}, hash(1))
	other, _, _ := s.CreateNode(ctx, NewNode{Name: "kept"}, hash(2))
	task, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	seedHistory(t, s, node, true, task.Task.Id)
	seedHistory(t, s, other, true, task.Task.Id)
	before := rowCounts(t, s)
	for _, table := range nodeHistoryTables {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			if _, err := s.w.ExecContext(ctx, "CREATE TRIGGER guard_"+table+"_"+op+" BEFORE "+op+" ON "+table+
				" BEGIN SELECT RAISE(ABORT, 'delete transaction touched "+table+"'); END"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.DeleteNode(ctx, node); err != nil {
		t.Fatalf("delete node: %v", err)
	}
	if _, err := s.DeleteProbeTask(ctx, task.Task.Id); err != nil {
		t.Fatalf("delete probe task: %v", err)
	}
	after := rowCounts(t, s)
	for _, table := range nodeHistoryTables {
		if after[table] != before[table] {
			t.Fatalf("%s rows %d -> %d across the delete transactions", table, before[table], after[table])
		}
	}
	if n := jobCount(t, s); n != 2 {
		t.Fatalf("cleanup jobs after deleting a node and a task = %d, want 2", n)
	}
}

// 一轮至多 maxSlices 片，作业轮转：两个作业交替各删一片，谁都不会被另一个饿死。作业要到复扫为零才删，删光之前
// 每轮都留着；排干所需的片数就是夹具的序列数 × 每条序列的片数，加上每轮重新展开时看到的空序列不算片。
// 另一个节点、另一个任务的行一行不少。
func TestCleanupDrainsWithinBudgetAndRotatesJobs(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	gone, _, _ := s.CreateNode(ctx, NewNode{Name: "gone"}, hash(1))
	kept, _, _ := s.CreateNode(ctx, NewNode{Name: "kept"}, hash(2))
	seedProbeTasks(t, s, 1, 2, 3)
	seedHistory(t, s, gone, true, 1, 2)
	seedHistory(t, s, kept, true, 1, 3)
	keptBefore := subjectRows(t, s, kept, 0)
	if err := s.DeleteNode(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteProbeTask(ctx, 3); err != nil {
		t.Fatal(err)
	}
	// node 作业：指标 1 条 + 探测 2 条；task 作业：只剩 kept 上的 1 条（gone 上没有任务 3 的行）。
	wantSlices := (3 + 1) * fixtureSlicesPerSeries
	// 预算不是作业数的整数倍：轮转到一半预算用尽也必须停下。
	const budget = 3
	total, rounds := 0, 0
	for jobCount(t, s) > 0 {
		if rounds++; rounds > 100 {
			t.Fatal("cleanup did not drain in 100 rounds")
		}
		round, err := s.cleanupRound(ctx, budget, time.Hour)
		if err != nil || round.Failed != 0 {
			t.Fatalf("round %d: %+v %v", rounds, round, err)
		}
		if round.Slices > budget {
			t.Fatalf("round %d used %d slices, budget %d", rounds, round.Slices, budget)
		}
		if rounds == 1 {
			// 两个作业轮转：第一轮 node 作业两片、task 作业一片，各自的最老一片都已删掉。
			if subjectRows(t, s, gone, 0) == 3*int64(len(fixtureTimes(0))+len(fixtureTimes(1))+len(fixtureTimes(2))) {
				t.Fatal("node job got no slice in the first round")
			}
			if subjectRows(t, s, 0, 3) == int64(len(fixtureTimes(0))+len(fixtureTimes(1))+len(fixtureTimes(2))) {
				t.Fatal("task job got no slice in the first round")
			}
		}
		total += round.Slices
	}
	if total != wantSlices {
		t.Fatalf("drained in %d slices, want %d", total, wantSlices)
	}
	if n := subjectRows(t, s, gone, 0); n != 0 {
		t.Fatalf("deleted node has %d rows left", n)
	}
	if n := subjectRows(t, s, 0, 3); n != 0 {
		t.Fatalf("deleted task has %d rows left", n)
	}
	perTask := int64(len(fixtureTimes(0)) + len(fixtureTimes(1)) + len(fixtureTimes(2)))
	if n := subjectRows(t, s, kept, 0); n != keptBefore-perTask {
		t.Fatalf("kept node has %d rows, want %d (only the deleted task's series removed)", n, keptBefore-perTask)
	}
	if round, err := s.cleanupRound(ctx, budget, time.Hour); err != nil || round != (CleanupRound{}) {
		t.Fatalf("round with no jobs = %+v %v, want nothing done", round, err)
	}
}

// 时间预算用尽就不再取片：预算为零的一轮什么都不删，作业原样留着。
func TestCleanupRoundStopsAtTimeBudget(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	gone, _, _ := s.CreateNode(ctx, NewNode{Name: "gone"}, hash(1))
	seedHistory(t, s, gone, true)
	if err := s.DeleteNode(ctx, gone); err != nil {
		t.Fatal(err)
	}
	before := subjectRows(t, s, gone, 0)
	if round, err := s.cleanupRound(ctx, cleanupSlicesPerRound, 0); err != nil || round.Slices != 0 {
		t.Fatalf("zero time budget: %+v %v", round, err)
	}
	if n := subjectRows(t, s, gone, 0); n != before || jobCount(t, s) != 1 {
		t.Fatalf("zero time budget changed rows %d -> %d or jobs %d", before, n, jobCount(t, s))
	}
}

// 一片失败：记 attempts 与 last_error，作业留着，本轮其余作业照常推进；故障消除后下一轮从头重试并完成。
func TestCleanupFailureIsRecordedAndRetried(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	gone, _, _ := s.CreateNode(ctx, NewNode{Name: "gone"}, hash(1))
	kept, _, _ := s.CreateNode(ctx, NewNode{Name: "kept"}, hash(2))
	seedProbeTasks(t, s, 4)
	seedHistory(t, s, gone, true)
	seedHistory(t, s, kept, false, 4)
	if err := s.DeleteNode(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteProbeTask(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.ExecContext(ctx, "CREATE TRIGGER fail_metric BEFORE DELETE ON metric_1m BEGIN SELECT RAISE(ABORT, 'disk on fire'); END"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		round, err := s.CleanupDeleted(ctx)
		if err != nil || round.Failed != 1 {
			t.Fatalf("round with a failing slice = %+v %v, want one failed job", round, err)
		}
	}
	var attempts int
	var lastError string
	if err := s.r.QueryRowContext(ctx, "SELECT attempts, last_error FROM cleanup_job WHERE kind = 'node' AND node_id = ?", gone).Scan(&attempts, &lastError); err != nil {
		t.Fatalf("failed job must stay: %v", err)
	}
	if attempts != 2 || !strings.Contains(lastError, "disk on fire") {
		t.Fatalf("attempts=%d last_error=%q, want 2 and the cause", attempts, lastError)
	}
	if n := subjectRows(t, s, 0, 4); n != 0 {
		t.Fatalf("the healthy task job did not progress past the failing one: %d rows left", n)
	}
	if _, err := s.w.ExecContext(ctx, "DROP TRIGGER fail_metric"); err != nil {
		t.Fatal(err)
	}
	if round, err := s.CleanupDeleted(ctx); err != nil || round.Completed != 1 {
		t.Fatalf("retry after the fault cleared = %+v %v", round, err)
	}
	if n := subjectRows(t, s, gone, 0); n != 0 || jobCount(t, s) != 0 {
		t.Fatalf("after retry: %d rows, %d jobs left", n, jobCount(t, s))
	}
}

// 级别从细到粗：片间穿插上卷（上卷只从细一级生成粗一级）时，按这个顺序走完一遍的作业复扫为零，当轮完成。
// 夹具让上卷持续有东西可重生：已删任务在 5m 水位之后留着两小时的 1m 行，每片之间时钟走 5 分钟、上卷随之多聚合一个
// 5m 桶，凑满一小时再聚合出一个 1h 桶。先删粗级的话，1h 那一级早已走过，之后上卷生成的 1h 行没人再删，复扫不为零。
func TestCleanupFineBeforeCoarseLeavesNothingForRollupToRegenerate(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	node, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	seedProbeTasks(t, s, 6)
	base := clk.Now().Truncate(time.Hour).Unix()
	setWatermark(t, s, "probe_5m", base+3600)
	setWatermark(t, s, "probe_1h", base+3600)
	if err := s.write(ctx, func(tx *sql.Tx) error {
		for m := int64(0); m < 120; m++ {
			if _, err := tx.Exec("INSERT INTO probe_1m (node_id, ts, task_id, sent) VALUES (?, ?, 6, 1)", node, base+3600+m*60); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("INSERT INTO probe_5m (node_id, ts, task_id, sent) VALUES (?, ?, 6, 5)", node, base); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO probe_1h (node_id, ts, task_id, sent) VALUES (?, ?, 6, 60)", node, base-86400)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteProbeTask(ctx, 6); err != nil {
		t.Fatal(err)
	}
	// 上卷的上界是 now − RollupLag：从 5m 水位处起步，此刻上卷还没有可聚合的整桶。
	now := base + 3600 + int64(RollupLag/time.Second)
	c := s.newCleanupCursor(cleanupJob{kind: cleanupKindTask, taskID: 6})
	for steps := 0; ; steps++ {
		if steps > 200 {
			t.Fatal("cursor did not finish in 200 steps")
		}
		deleted, err := c.step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !deleted {
			break
		}
		now += 300
		clk.SetWall(time.Unix(now, 0))
		if err := s.Rollup(ctx); err != nil {
			t.Fatal(err)
		}
	}
	done, err := c.finish(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		left := map[string]int64{}
		for _, table := range probeTables {
			var n int64
			if err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE task_id = 6").Scan(&n); err != nil {
				t.Fatal(err)
			}
			left[table] = n
		}
		t.Fatalf("one pass with rollups between slices left rows for the rescan: %v", left)
	}
}

// 完成以复扫为准，不以"片都跑过了"为准：游标走完之后、复扫之前出现的行（上卷在片间重生的那类）让作业留下，
// 下一轮重新展开时删掉，之后才完成。
func TestCleanupCompletionRequiresEmptyRescan(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	node, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	seedProbeTasks(t, s, 6)
	seedHistory(t, s, node, false, 6)
	if _, err := s.DeleteProbeTask(ctx, 6); err != nil {
		t.Fatal(err)
	}
	c := s.newCleanupCursor(cleanupJob{kind: cleanupKindTask, taskID: 6})
	for {
		deleted, err := c.step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !deleted {
			break
		}
	}
	seedRawProbes(t, s, probeRow(node, fixtureBase, 6, []uint32{1}, 0, 0))
	if done, err := c.finish(ctx); err != nil || done {
		t.Fatalf("finish with a row left = %v %v, want the job kept", done, err)
	}
	if jobCount(t, s) != 1 {
		t.Fatal("job removed while rows remained")
	}
	if round, err := s.CleanupDeleted(ctx); err != nil || round.Completed != 1 || subjectRows(t, s, 0, 6) != 0 {
		t.Fatalf("next round = %+v %v, rows left %d", round, err, subjectRows(t, s, 0, 6))
	}
}

// 删除之后、清理之前：被删节点与被删任务的行对任何查询都不存在；其余节点、其余任务的查询结果在删除前、清理前、
// 清理后逐字相同。
func TestDeletedHistoryIsInvisibleBeforeCleanupAndOthersUnchanged(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	gone, _, _ := s.CreateNode(ctx, NewNode{Name: "gone"}, hash(1))
	kept, _, _ := s.CreateNode(ctx, NewNode{Name: "kept"}, hash(2))
	seedProbeTasks(t, s, 1, 2)
	seedHistory(t, s, gone, true, 1, 2)
	seedHistory(t, s, kept, true, 1, 2)
	from, to := fixtureBase, fixtureBase+60*86400
	type view struct {
		metrics []metric.Row
		probes  []metric.ProbeRow
		compare []metric.ProbeRow
	}
	read := func(node int64) view {
		t.Helper()
		var v view
		var err error
		for _, lv := range levels {
			rows, e := s.QueryMetrics(ctx, node, from, to, lv, lv.Bucket*1440)
			v.metrics = append(v.metrics, rows...)
			probes, e2 := s.QueryProbes(ctx, node, from, to, lv, lv.Bucket*1440, 2)
			v.probes = append(v.probes, probes...)
			compare, e3 := s.QueryProbeComparison(ctx, 1, []int64{node}, from, to, lv, lv.Bucket*1440)
			v.compare = append(v.compare, compare...)
			if err = errors.Join(err, e, e2, e3); err != nil {
				t.Fatal(err)
			}
		}
		return v
	}
	keptBefore := read(kept)
	if len(keptBefore.metrics) == 0 || len(keptBefore.probes) == 0 || len(keptBefore.compare) == 0 {
		t.Fatalf("fixture must be visible before delete: %d/%d/%d", len(keptBefore.metrics), len(keptBefore.probes), len(keptBefore.compare))
	}
	if err := s.DeleteNode(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteProbeTask(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if v := read(gone); len(v.metrics)+len(v.probes)+len(v.compare) != 0 {
		t.Fatalf("deleted node still answers before cleanup: %d/%d/%d rows", len(v.metrics), len(v.probes), len(v.compare))
	}
	keptMid := read(kept)
	for _, r := range keptMid.probes {
		if r.TaskID == 2 {
			t.Fatalf("deleted task's row answered for the other node before cleanup: %+v", r)
		}
	}
	var withoutTask2 []metric.ProbeRow
	for _, r := range keptBefore.probes {
		if r.TaskID != 2 {
			withoutTask2 = append(withoutTask2, r)
		}
	}
	keptBefore.probes = withoutTask2
	if !reflect.DeepEqual(keptMid, keptBefore) {
		t.Fatal("the other node's results changed when a node and a task were deleted")
	}
	for jobCount(t, s) > 0 {
		if _, err := s.CleanupDeleted(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(read(kept), keptBefore) {
		t.Fatal("the other node's results changed across cleanup")
	}
}

// 恢复：配置快照带着作业回来，指标快照里被删节点的行由孤儿清理同步删掉，作业复扫为零后完成；指标快照里有、
// 配置快照里没有的任务的行登记成新作业，由清理删掉。
func TestRestoreCarriesCleanupJobsAndEnqueuesOrphanTasks(t *testing.T) {
	t.Parallel()
	source, clk := open(t)
	ctx := t.Context()
	gone, _, _ := source.CreateNode(ctx, NewNode{Name: "gone"}, hash(1))
	kept, _, _ := source.CreateNode(ctx, NewNode{Name: "kept"}, hash(2))
	seedHistory(t, source, gone, true)
	if err := source.DeleteNode(ctx, gone); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := source.SnapshotConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	// 配置快照之后才建的任务：指标快照里有它的行，配置快照里没有它。
	seedProbeTasks(t, source, 8)
	seedHistory(t, source, kept, false, 8)
	metrics := filepath.Join(t.TempDir(), "metrics.db")
	if err := source.SnapshotMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored.db")
	if _, err := Restore(ctx, target, config, metrics, "", clk.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(target, clk, slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	jobs, err := restored.cleanupJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []cleanupJob{{kind: cleanupKindNode, nodeID: gone}, {kind: cleanupKindTask, taskID: 8}}
	if !slices.Equal(jobs, want) {
		t.Fatalf("jobs after restore = %+v, want %+v", jobs, want)
	}
	if n := subjectRows(t, restored, gone, 0); n != 0 {
		t.Fatalf("restore left %d rows of the node deleted before the config snapshot", n)
	}
	if n := subjectRows(t, restored, 0, 8); n == 0 {
		t.Fatal("fixture must carry the orphan task's rows into the restored database")
	}
	if round, err := restored.CleanupDeleted(ctx); err != nil || round.Completed != 2 {
		t.Fatalf("cleanup after restore = %+v %v, want both jobs completed", round, err)
	}
	if n := subjectRows(t, restored, 0, 8); n != 0 || jobCount(t, restored) != 0 {
		t.Fatalf("after cleanup: %d orphan task rows, %d jobs", n, jobCount(t, restored))
	}
}

// 清理的读与删都经已有的主键或 (task_id, node_id, ts) 索引的前缀定位，不扫全表、不需要新索引。核对的是游标实际执行的语句。
func TestCleanupStatementsUseExistingKeys(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for i := range levels {
		probe, metricTable := probeFamily.tables[i], metricFamily.tables[i]
		probeSeries := cleanupSeries{table: probe, level: i, where: "task_id = ? AND node_id = ?"}
		metricSeries := cleanupSeries{table: metricTable, level: i, where: "node_id = ?"}
		for _, tc := range []struct {
			query string
			args  []any
			want  string
		}{
			{nodeTasksSQL(probe), []any{1}, "SEARCH " + probe + " USING PRIMARY KEY (node_id=?)"},
			{taskNodesSQL(probe), []any{1}, "SEARCH " + probe + " USING COVERING INDEX " + probe + "_by_task (task_id=?)"},
			{probeSeries.oldestSQL(), []any{1, 2}, "SEARCH " + probe + " USING COVERING INDEX " + probe + "_by_task (task_id=? AND node_id=?)"},
			{probeSeries.deleteSQL(), []any{1, 2, 0, 60}, "SEARCH " + probe + " USING INDEX " + probe + "_by_task (task_id=? AND node_id=? AND ts>? AND ts<?)"},
			{metricSeries.oldestSQL(), []any{1}, "SEARCH " + metricTable + " USING PRIMARY KEY (node_id=?)"},
			{metricSeries.deleteSQL(), []any{1, 0, 60}, "SEARCH " + metricTable + " USING PRIMARY KEY (node_id=? AND ts>? AND ts<?)"},
			{orphanTaskJobSQL(probe), []any{1}, "SEARCH " + probe + " USING COVERING INDEX " + probe + "_by_task (task_id>?)"},
			{migrationV38OrphanTaskJobs(probe), nil, "SEARCH " + probe + " USING COVERING INDEX " + probe + "_by_task (task_id>?)"},
		} {
			plan := strings.Join(queryPlans(t, s.r, tc.query, tc.args...), "\n")
			if !strings.Contains(plan, tc.want) {
				t.Errorf("%s\nplan: %s\nwant: %s", tc.query, plan, tc.want)
			}
		}
	}
}

// 删除任务提交之后到达的探测行（ingest 的待重试批次、删除前已累积的分钟桶）被写入口拒收，与已删节点同一判定；
// 同一批里其余任务的行照常写入。
func TestMinuteBatchRejectsRowsOfDeletedTask(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	node, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	seedProbeTasks(t, s, 1, 2)
	if _, err := s.DeleteProbeTask(ctx, 2); err != nil {
		t.Fatal(err)
	}
	n, err := s.WriteMinuteBatch(ctx, metric.Batch{Probes: []metric.ProbeRow{probeRow(node, 600, 1, []uint32{5}, 0, 0), probeRow(node, 600, 2, []uint32{5}, 0, 0)}})
	if err != nil || n != 1 {
		t.Fatalf("batch with a deleted task's row: rejected=%d err=%v, want 1", n, err)
	}
	if left := subjectRows(t, s, 0, 2); left != 0 {
		t.Fatalf("deleted task's row landed after the delete: %d rows", left)
	}
	if kept := subjectRows(t, s, 0, 1); kept != 1 {
		t.Fatalf("live task's row in the same batch: %d rows, want 1", kept)
	}
}
