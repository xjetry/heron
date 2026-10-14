package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/probelimit"
)

func setWatermark(t *testing.T, s *Store, level string, upto int64) {
	t.Helper()
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE rollup_state SET upto_ts = ? WHERE level = ?", upto, level)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func seedMinute(t *testing.T, s *Store, rows ...metric.ProbeRow) {
	t.Helper()
	if _, err := s.WriteMinuteBatch(t.Context(), metric.Batch{Probes: rows}); err != nil {
		t.Fatal(err)
	}
}

// queryPlans 返回 EXPLAIN QUERY PLAN 的明细行；额度计数与聚合查询的读取路径断言都以它核对。
func queryPlans(t *testing.T, q *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := q.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}

// assertSearch 断言一条源查询经前导等值键 + ts 范围定位（SEARCH），不是整表扫描。
// 这是"读量额度按实际读取行数计数"的前提：计数与聚合走同一条计划，计数读到的行就是聚合
// 要读的行，读量与其他任务、其他节点的行无关。
func assertSearch(t *testing.T, s *Store, table, query string, args ...any) {
	t.Helper()
	plan := queryPlans(t, s.r, query, args...)
	for _, detail := range plan {
		if !strings.Contains(detail, "SEARCH "+table) {
			continue
		}
		if !strings.Contains(detail, "node_id=?") || !strings.Contains(detail, "ts>?") {
			t.Fatalf("%q: source lookup of %s is not a leading-key range search", detail, table)
		}
		if strings.Contains(detail, "task_id") && !strings.Contains(detail, "task_id=?") {
			t.Fatalf("%q: task_id must be an equality constraint", detail)
		}
		return
	}
	t.Fatalf("%s: no SEARCH of %s in plan %v", query, table, plan)
}

// 每族、每级的源查询与额度计数都必须经前导键定位；对比查询另须命中 by_task 索引。
func TestQueryFamilySourceQueriesUseLeadingKeys(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for _, f := range families {
		for i, lv := range levels {
			t.Run(f.name+"/"+lv.Name, func(t *testing.T) {
				single := queryShape{keyWhere: "node_id = ?", seriesLimit: 1}
				comparison := queryShape{keyWhere: "task_id = ? AND node_id IN (?,?)", seriesLimit: MaxComparisonNodes, byTaskIndex: true}
				t.Run("single node", func(t *testing.T) {
					query := f.rangeSQL(i, single) + " AND ts < ?"
					assertSearch(t, s, f.tables[i], query, 1, 0, 86400, 43200)
					count := "SELECT count(*) FROM (SELECT 1 FROM " + f.tables[i] + " WHERE node_id = ? AND ts >= ? AND ts <= ? AND ts < ? LIMIT ?)"
					assertSearch(t, s, f.tables[i], count, 1, 0, 86400, 43200, 1000)
				})
				t.Run("comparison", func(t *testing.T) {
					if f == metricFamily {
						t.Skip("对比查询只读探测表；指标表没有 by_task 索引")
					}
					query := f.rangeSQL(i, comparison) + " AND ts < ?"
					assertSearch(t, s, f.tables[i], query, 5, 1, 2, 0, 86400, 43200)
					plan := strings.Join(queryPlans(t, s.r, query, 5, 1, 2, 0, 86400, 43200), "\n")
					if !strings.Contains(plan, "INDEX "+f.tables[i]+"_by_task") {
						t.Fatalf("comparison lookup must use the by-task index: %s", plan)
					}
					count := "SELECT count(*) FROM (SELECT 1 FROM " + f.tables[i] + byTaskHint(f, i, comparison) + " WHERE task_id = ? AND node_id IN (?,?) AND ts >= ? AND ts <= ? AND ts < ? LIMIT ?)"
					assertSearch(t, s, f.tables[i], count, 5, 1, 2, 0, 86400, 43200, 1000)
					plan = strings.Join(queryPlans(t, s.r, count, 5, 1, 2, 0, 86400, 43200, 1000), "\n")
					if !strings.Contains(plan, "INDEX "+f.tables[i]+"_by_task") {
						t.Fatalf("comparison count must use the by-task index: %s", plan)
					}
				})
			})
		}
	}
}

// comparisonSamples 把对比查询的结果按节点折叠成 (ts → 桶)，供与单节点查询逐桶比对。
func comparisonSamples(rows []metric.ProbeRow) map[int64]map[int64]*metric.ProbeBucket {
	out := map[int64]map[int64]*metric.ProbeBucket{}
	for _, r := range rows {
		if out[r.NodeID] == nil {
			out[r.NodeID] = map[int64]*metric.ProbeBucket{}
		}
		out[r.NodeID][r.TS] = r.Bucket
	}
	return out
}

// 夹具：两个节点、一个任务，窗口内逐分钟样本，两个节点的取值故意不同。
func comparisonFixture(t *testing.T, s *Store) (taskID uint64, base int64, a, b int64) {
	t.Helper()
	a, _, err := s.CreateNode(t.Context(), NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err = s.CreateNode(t.Context(), NewNode{Name: "b"}, hash(2))
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := s.SaveProbeTask(t.Context(), &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "192.0.2.1", IntervalS: 60, TimeoutMs: 1000}, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	taskID = task.Task.Id
	base = 3600 * 24
	var rows []metric.ProbeRow
	for i := range 300 {
		ts := base + int64(i)*60
		rows = append(rows, probeRow(a, ts, taskID, []uint32{uint32(1000 + 10*i)}, 0, 0))
		rows = append(rows, probeRow(b, ts, taskID, []uint32{uint32(2000 + 10*i)}, uint32(i%7), uint32(i%11)))
	}
	seedMinute(t, s, rows...)
	return taskID, base, a, b
}

// 对比查询与单节点查询是同一族控制流程的两种形状：对同一段历史，按任务对比每个节点的样本
// 与逐节点 QueryProbes 过滤该任务后的样本逐桶相同。覆盖三级表与再分桶步长：粗级来源先在
// 源查询里展开成行，再按请求步长二次分桶，与单节点路径的运算一致。
func TestProbeComparisonMatchesSingleNodeAcrossLevels(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	task, base, a, b := comparisonFixture(t, s)
	// 上卷冻结粗级行：粗级来源参与查询。
	if err := s.Rollup(t.Context()); err != nil {
		t.Fatal(err)
	}
	// 维护积压：把 5m 水位回拨到窗口中段，粗级来源截在水位、细级尾巴接上，
	// 对比与单节点读到的来源完全一致。
	rolled := base + 150*60
	s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE rollup_state SET upto_ts = ? WHERE level = 'probe_5m'", rolled)
		return err
	})
	t.Run("5m watermark rolled back", func(t *testing.T) {
		from, to := base-7*86400, base+300*60
		rows, err := s.QueryProbeComparison(t.Context(), task, []int64{a, b}, from, to, levels[1], 3600)
		if err != nil {
			t.Fatal(err)
		}
		got := comparisonSamples(rows)
		for _, node := range []int64{a, b} {
			single, err := s.QueryProbes(t.Context(), node, from, to, levels[1], 3600, 0)
			if err != nil {
				t.Fatal(err)
			}
			want := map[int64]*metric.ProbeBucket{}
			for _, r := range single {
				if r.TaskID == task {
					want[r.TS] = r.Bucket
				}
			}
			if len(got[node]) != len(want) {
				t.Fatalf("node %d: comparison buckets=%d want=%d", node, len(got[node]), len(want))
			}
			for ts, b := range want {
				if *got[node][ts] != *b {
					t.Fatalf("node %d ts %d: comparison=%+v single=%+v", node, ts, got[node][ts], b)
				}
			}
		}
	})
	s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE rollup_state SET upto_ts = ? WHERE level = 'probe_5m'", base+300*60)
		return err
	})
	end := base + 300*60
	for _, c := range []struct {
		name           string
		from, to, step int64
		lv             Level
	}{
		{"1m step", base, end, 60, levels[0]},
		{"1m re-bucketed by 5", base, end, 300, levels[0]},
		{"5m source", base - 7200, end, 300, levels[1]},
		{"5m source hourly step", base - 7200, end, 3600, levels[1]},
		{"1h source", base - 30*86400, end, 3600, levels[2]},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows, err := s.QueryProbeComparison(t.Context(), task, []int64{a, b}, c.from, c.to, c.lv, c.step)
			if err != nil {
				t.Fatal(err)
			}
			got := comparisonSamples(rows)
			for _, node := range []int64{a, b} {
				single, err := s.QueryProbes(t.Context(), node, c.from, c.to, c.lv, c.step, 0)
				if err != nil {
					t.Fatal(err)
				}
				want := map[int64]*metric.ProbeBucket{}
				for _, r := range single {
					if r.TaskID == task {
						want[r.TS] = r.Bucket
					}
				}
				if len(got[node]) == 0 || len(got[node]) != len(want) {
					t.Fatalf("node %d: comparison buckets=%d want=%d", node, len(got[node]), len(want))
				}
				for ts, b := range want {
					if *got[node][ts] != *b {
						t.Fatalf("node %d ts %d: comparison=%+v single=%+v", node, ts, got[node][ts], b)
					}
				}
			}
		})
	}
}

// 请求窗口边缘未对齐步长时按对齐后的窗口读取：首尾桶包含窗口外的分钟行，与单节点路径同一口径。
func TestProbeComparisonAlignsUnalignedWindowEdges(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	task, base, a, _ := comparisonFixture(t, s)
	rows, err := s.QueryProbeComparison(t.Context(), task, []int64{a}, base+130, base+700, levels[0], 300)
	if err != nil {
		t.Fatal(err)
	}
	single, err := s.QueryProbes(t.Context(), a, base+130, base+700, levels[0], 300, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || len(rows) != len(single) {
		t.Fatalf("comparison rows=%d single rows=%d", len(rows), len(single))
	}
	for i := range rows {
		if rows[i].TS != single[i].TS || *rows[i].Bucket != *single[i].Bucket {
			t.Fatalf("row %d: comparison=%+v single=%+v", i, rows[i], single[i])
		}
	}
}

// 水位交界处两侧数据经同一快照拼接：把 5m 水位摆回窗口中间，对比查询的桶与逐节点查询仍一致，
// 且确实落在纯粗级与纯细级都不同的结果上（交界拼接被走到）。
func TestProbeComparisonAcrossWatermarkBoundary(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	task, base, a, b := comparisonFixture(t, s)
	if err := s.Rollup(t.Context()); err != nil {
		t.Fatal(err)
	}
	from, to := base, base+300*60
	mid := base + 150*60
	setWatermark(t, s, "probe_5m", mid)
	rows, err := s.QueryProbeComparison(t.Context(), task, []int64{a, b}, from, to, levels[1], 300)
	if err != nil {
		t.Fatal(err)
	}
	got := comparisonSamples(rows)
	for _, node := range []int64{a, b} {
		single, err := s.QueryProbes(t.Context(), node, from, to, levels[1], 300, 0)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, r := range single {
			if r.TaskID != task {
				continue
			}
			count++
			if b, ok := got[node][r.TS]; !ok || *b != *r.Bucket {
				t.Fatalf("node %d ts %d: %+v vs single %+v", node, r.TS, b, r.Bucket)
			}
		}
		if count == 0 {
			t.Fatal("fixture produced no probe rows")
		}
	}
}

// 全丢包或全错误的桶没有 rtt 样本（min/max 为 NULL），聚合后 RttN 为 0、不伪造成 0µs 读数。
func TestProbeComparisonKeepsNullRttBuckets(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	seedProbeTasks(t, s, 3)
	a, _, err := s.CreateNode(t.Context(), NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.CreateNode(t.Context(), NewNode{Name: "b"}, hash(2))
	if err != nil {
		t.Fatal(err)
	}
	base := int64(3600)
	seedMinute(t, s,
		probeRow(a, base, 3, nil, 4, 0),
		probeRow(b, base, 3, nil, 0, 4),
		probeRow(a, base+60, 3, []uint32{500}, 0, 0),
	)
	rows, err := s.QueryProbeComparison(t.Context(), 3, []int64{a, b}, base, base+3600, levels[0], 60)
	if err != nil {
		t.Fatal(err)
	}
	got := comparisonSamples(rows)
	if b := got[a][base]; b == nil || b.Sent != 4 || b.RttN != 0 || b.RttMinUs != 0 || b.RttMaxUs != 0 {
		t.Fatalf("all-lost bucket = %+v", b)
	}
	if b := got[b][base]; b == nil || b.Sent != 4 || b.RttN != 0 {
		t.Fatalf("all-error bucket = %+v", b)
	}
	if b := got[a][base+60]; b == nil {
		t.Fatal("rtt bucket missing")
	} else if mean, ok := b.RttMean(); !ok || mean != 500 {
		t.Fatalf("rtt bucket = %+v", b)
	}
}

// 删除任务或节点之后，它们的历史在清理作业完成前仍在表里，但对比查询立即看不到：读侧按配置层判定主体存在
// （family.live），与准入检查是否早于删除无关。其余节点、其余任务的结果在删除前后、清理前后逐字相同。
func TestProbeComparisonOmitsDeletedTaskAndNodeHistory(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	task, base, a, b := comparisonFixture(t, s)
	other, _, err := s.SaveProbeTask(ctx, &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "192.0.2.2", IntervalS: 60, TimeoutMs: 1000}, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	var rows []metric.ProbeRow
	for i := range 10 {
		rows = append(rows, probeRow(a, base+int64(i)*60, other.Task.Id, []uint32{700}, 0, 0), probeRow(b, base+int64(i)*60, other.Task.Id, []uint32{800}, 1, 0))
	}
	seedMinute(t, s, rows...)
	query := func(task uint64, nodes ...int64) map[int64]map[int64]*metric.ProbeBucket {
		t.Helper()
		rows, err := s.QueryProbeComparison(ctx, task, nodes, base, base+3600, levels[0], 60)
		if err != nil {
			t.Fatal(err)
		}
		return comparisonSamples(rows)
	}
	if got := query(task, a, b); len(got[a]) != 60 || len(got[b]) != 60 {
		t.Fatalf("before delete: %d/%d buckets, want 60/60", len(got[a]), len(got[b]))
	}
	untouched := query(other.Task.Id, a)
	if _, err := s.DeleteProbeTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if got := query(task, a, b); len(got) != 0 {
		t.Fatalf("deleted task's history answered a comparison before cleanup: %d nodes", len(got))
	}
	if err := s.DeleteNode(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got := query(other.Task.Id, a, b); len(got[b]) != 0 || !reflect.DeepEqual(got[a], untouched[a]) {
		t.Fatalf("after deleting node %d: got %d buckets for it; other node unchanged=%v", b, len(got[b]), reflect.DeepEqual(got[a], untouched[a]))
	}
	if _, err := s.CleanupDeleted(ctx); err != nil {
		t.Fatal(err)
	}
	if got := query(other.Task.Id, a); !reflect.DeepEqual(got, untouched) {
		t.Fatalf("surviving series changed across cleanup: got %v want %v", got, untouched)
	}
	var left int
	if err := s.r.QueryRow("SELECT count(*) FROM probe_1m WHERE task_id = ? OR node_id = ?", task, b).Scan(&left); err != nil || left != 0 {
		t.Fatalf("cleanup left %d rows of the deleted task and node (%v)", left, err)
	}
}

// readCounter 观察读连接上的查询：额度计数（count(*)）与聚合（GROUP BY）各计一项。
type readCounter struct {
	counts atomic.Int64
	groups atomic.Int64
}

type readCountDriver struct{ counter *readCounter }
type readCountConn struct {
	driver.Conn
	counter *readCounter
}

func (d readCountDriver) Open(name string) (driver.Conn, error) {
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &readCountConn{Conn: c, counter: d.counter}, nil
}

func (c *readCountConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "GROUP BY") {
		c.counter.groups.Add(1)
	} else if strings.Contains(q, "count(*)") {
		c.counter.counts.Add(1)
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

// BeginTx 透传只读事务选项：包裹层丢了它，database/sql 会把 ReadOnly 当不支持而拒绝。
func (c *readCountConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

// countReads 换掉读连接池，包一层只观察查询文本的驱动；查询语义不变。
func countReads(t *testing.T, s *Store) *readCounter {
	t.Helper()
	counter := &readCounter{}
	driverName := fmt.Sprintf("read-count-sqlite-%d", traceID.Add(1))
	sql.Register(driverName, readCountDriver{counter})
	reopenReadPools(t, s, driverName)
	return counter
}

// fillProbeRows 生成 count 行 (node, ts, task) 的探测行：ts 从 3600 起按表级步进，task 在
// 0..64 轮转模拟任务更替，每行有一个 rtt 样本。绕过写协程直接批量写入，仅测试夹具使用。
func fillProbeRows(ctx context.Context, db *sql.DB, table string, node, task int64, count int64) error {
	return fillProbeRowsStep(ctx, db, table, node, task, count, 3600, 0)
}

func fillProbeRowsStep(ctx context.Context, db *sql.DB, table string, node, task int64, count int64, step, base int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const batch = 100000
	for start := int64(0); start < count; start += batch {
		end := min(start+batch, count)
		if _, err := tx.ExecContext(ctx,
			"WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i < ?) "+
				"INSERT INTO "+table+" (node_id, ts, task_id, sent, lost, errors, rtt_sum_us, rtt_min_us, rtt_max_us) "+
				"SELECT ?, ?+(i+?)*?, (?+(i % 65)), 1, 0, 0, 100, 100, 100 FROM seq",
			end-start, node, base, start, step, task); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// 读额度边界上的用例都要越过 768000 行的额度，共用 buildProbeHistoryTemplate 的同一份模板，作为这里的并行子测试跑。
// 模板在子测试开始前建好：子测试 t.Parallel 之后要等本函数返回才去抢并行槽，建模板的几十秒里只占本用例这一个槽。
// 若各用例各自是顶层并行用例、在共享的建模板上等，等待者会在整段建模板时间里各占一个并行槽却不干活（-parallel
// 默认等于 GOMAXPROCS，CPU 少的机器上几个等待者就能占满全部槽），别的用例只能排队。
func TestReadQuotaOnProbeHistory(t *testing.T) {
	t.Parallel()
	tpl := buildProbeHistoryTemplate(t)
	for _, c := range []struct {
		name string
		run  func(*testing.T, string)
	}{
		{"RejectsBeforeAggregation", readQuotaRejectsBeforeAggregation},
		{"RejectsTaskTurnoverHistory", readQuotaRejectsTaskTurnoverHistory},
		{"CountStopsAtLimit", readQuotaCountStopsAtLimit},
		{"CountsAlignedWindow", readQuotaCountsAlignedWindow},
		{"UsesFullTaskSlots", readQuotaUsesFullTaskSlots},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t, tpl)
		})
	}
}

// 超过额度：错误给出额度、水位时刻与建议，聚合一次都没有执行；同一段历史上额度权重更大的
// 对比查询（N_max 条序列）仍在额度内照常服务。
func readQuotaRejectsBeforeAggregation(t *testing.T, tpl string) {
	// 一个节点一个任务，行数超过单节点探测额度 64×12000；水位与窗口都盖住全部行。
	const rows = quotaRowsPerSeries*probelimit.MaxTasksPerNode + 1
	s, _ := openProbeHistory(t, tpl, rows)
	counter := countReads(t, s)
	end := (int64(rows) + 1) * 3600
	setWatermark(t, s, "probe_1h", end)
	setWatermark(t, s, "probe_5m", end)
	_, err := s.QueryProbes(t.Context(), 1, 0, end, levels[2], 7*86400, 0)
	if err == nil {
		t.Fatal("query over budget must be rejected")
	}
	var quota ReadQuotaError
	if !errors.As(err, &quota) || quota.Quota != quotaRowsPerSeries*probelimit.MaxTasksPerNode {
		t.Fatalf("err = %v, want ReadQuotaError", err)
	}
	if len(quota.Watermarks) == 0 || quota.Watermarks[len(quota.Watermarks)-1].Level != "probe_1h" {
		t.Fatalf("watermarks = %v, want the 1h probe watermark", quota.Watermarks)
	}
	if !strings.Contains(err.Error(), "narrow the window") || !strings.Contains(err.Error(), "consolidated up to") {
		t.Fatalf("error must offer a remedy and watermark facts: %v", err)
	}
	if n := counter.groups.Load(); n != 0 {
		t.Fatalf("aggregation ran %d times despite quota rejection", n)
	}
	if n := counter.counts.Load(); n == 0 {
		t.Fatal("quota must be counted before rejecting")
	}
	// 对比查询的额度按 N_max 计：窗口收窄到行数远小于 12000×N_max 的区间即放行。
	if _, err := s.QueryProbeComparison(t.Context(), 1, []int64{1}, 0, 300000*3600, levels[2], 7*86400); err != nil {
		t.Fatalf("comparison within its budget: %v", err)
	}
	if n := counter.groups.Load(); n != 1 {
		t.Fatalf("comparison aggregation ran %d times, want 1", n)
	}
}

// 常见窗口全部放行：两级水位健康、窗口覆盖细级与粗级、告警评估的整段 for_minutes 窗口
// （上限 60 分钟 × 每节点上限 64 个任务、外加一个已删除任务的历史行）都在额度内。
func TestReadQuotaAdmitsCommonWindows(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	base := int64(3600 * 24)
	var rows []metric.ProbeRow
	for task := uint64(1); task <= 65; task++ {
		for i := range 70 {
			rows = append(rows, probeRow(1, base+int64(i)*60, task, []uint32{uint32(500 + task + uint64(i))}, 0, 0))
		}
	}
	seedMinute(t, s, rows...)
	if err := s.Rollup(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"metrics 6h", func() error { _, err := s.QueryMetrics(t.Context(), 1, base, base+6*3600, levels[0], 60); return err }},
		{"metrics coverage 7d", func() error {
			_, _, err := s.QueryMetricsCoverage(t.Context(), 1, base, base+7*86400, levels[1], 300)
			return err
		}},
		{"probes alert window", func() error { _, err := s.QueryProbes(t.Context(), 1, base, base+3600, levels[0], 60, 0); return err }},
		{"probes full level", func() error { _, err := s.QueryProbes(t.Context(), 1, base, base+70*60, levels[0], 60, 0); return err }},
		{"probes sparse step", func() error { _, err := s.QueryProbes(t.Context(), 1, base, base+70*60, levels[0], 600, 0); return err }},
		{"comparison", func() error {
			_, err := s.QueryProbeComparison(t.Context(), 1, []int64{1}, base, base+70*60, levels[0], 60)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); err != nil {
				t.Fatalf("within-budget query rejected: %v", err)
			}
		})
	}
}

// 任务更替让每节点的任务数恒为上限，历史行仍随更替增长：1h 表在保留期内堆过 64×B 行时，
// 长窗口按声明被拒；行数是额度关心的事实，与展示步长无关（max_points=1 会把步长放大）。
func readQuotaRejectsTaskTurnoverHistory(t *testing.T, tpl string) {
	const rows = quotaRowsPerSeries*probelimit.MaxTasksPerNode + 24
	s, _ := openProbeHistory(t, tpl, rows)
	end := int64(rows) * 3600
	setWatermark(t, s, "probe_1h", end)
	_, err := s.QueryProbes(t.Context(), 1, 0, end, levels[2], 7*86400, 0)
	var quota ReadQuotaError
	if !errors.As(err, &quota) {
		t.Fatalf("err = %v, want ReadQuotaError", err)
	}
}

// 额度计数的 LIMIT 提前终止：行数翻倍，带剩余额度上限的计数耗时几乎不变，而无上限计数随行数
// 增长（对照组）。断言钉住同一条性质的两面：计数不会在读满之前扫完全表。
//
// 断言是耗时比值，仍与其它用例并行：阈值是无上限计数的四分之一。有上限计数读到额度上限的百余行就停，无上限计数要扫完
// 圈定的全部行（probeHistoryRows 与它的四分之一），两者耗时相差两个数量级以上，所以负载要把一次有上限计数拖慢几十倍
// 以上才会误红；LIMIT 失效时有上限计数与无上限计数同量级，照样越过阈值。
func readQuotaCountStopsAtLimit(t *testing.T, tpl string) {
	s, _ := openProbeHistory(t, tpl, probeHistoryRows)
	bounded := func(limit, hi int64) time.Duration {
		start := time.Now()
		var n int64
		if err := s.r.QueryRow("SELECT count(*) FROM (SELECT 1 FROM probe_1h WHERE node_id = 1 AND ts >= 0 AND ts <= ? LIMIT ?)", hi, limit).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	// 两档行数取自同一份全量副本，用 ts 上界圈出前 size 行：模板第 g 行（从 0 数）的 ts 为 (g+1)×3600，主键以
	// (node_id, ts) 打头，ts ≤ size×3600 的区间恰是前 size 行，两条计数扫过的行与截到 size 行的库上相同，
	// 不必为第二档删掉其余行。
	for _, size := range []int64{probeHistoryRows, probeHistoryRows / 4} {
		hi := size * 3600
		boundedAt := bounded(101, hi)
		var all int64
		start := time.Now()
		if err := s.r.QueryRow("SELECT count(*) FROM probe_1h WHERE node_id = 1 AND ts >= 0 AND ts <= ?", hi).Scan(&all); err != nil {
			t.Fatal(err)
		}
		full := time.Since(start)
		if all != size {
			t.Fatalf("seeded %d rows up to ts %d, counted %d", size, hi, all)
		}
		if boundedAt > full/4 {
			t.Fatalf("bounded count at %d rows took %v, unbounded %v; LIMIT did not terminate early", size, boundedAt, full)
		}
	}
}

// 对比候选：同一事务读任务标注、选择器与可见候选；候选按节点全序，空候选与不存在的任务同形。
func TestListComparisonNodes(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	mk := func(name string, public bool, order int64) int64 {
		id, _, err := s.CreateNode(ctx, NewNode{Name: name}, hash(name[0]))
		if err != nil {
			t.Fatal(err)
		}
		// CreateNode 建的节点默认公开，这里显式写回期望的公开性。
		if _, err := s.UpdateNode(ctx, id, NodeEdit{Name: name, Public: public}); err != nil {
			t.Fatal(err)
		}
		if err := s.write(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec("UPDATE node SET sort_order = ? WHERE id = ?", order, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, bPub, cPub := mk("a", false, 3), mk("b", true, 2), mk("c", true, 1)
	all, _, err := s.SaveProbeTask(ctx, &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "all.example", IntervalS: 60, TimeoutMs: 1000}, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	some, _, err := s.SaveProbeTask(ctx, &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "some.example", IntervalS: 60, TimeoutMs: 1000}, NodeSelector{NodeIDs: []int64{a, bPub}})
	if err != nil {
		t.Fatal(err)
	}
	// 公开候选：all_nodes 任务按节点全序返回全部公开节点；显式任务只剩公开的那个。
	rec, ok, nodes, err := s.ListComparisonNodes(ctx, all.Task.Id, ComparisonPublic)
	if err != nil || !ok {
		t.Fatalf("public all_nodes candidates: %v %v %v", ok, err, nodes)
	}
	if rec.Task.Target != "all.example" || !rec.AllNodes {
		t.Fatalf("all_nodes record = %+v", rec)
	}
	if !slices.Equal(nodes, []int64{cPub, bPub}) {
		t.Fatalf("public candidates must follow the node order: %v", nodes)
	}
	if _, ok, nodes, err = s.ListComparisonNodes(ctx, some.Task.Id, ComparisonPublic); err != nil || !ok || !slices.Equal(nodes, []int64{bPub}) {
		t.Fatalf("explicit task public candidates = %v %v %v", ok, nodes, err)
	}
	// 不存在的任务与候选为空的返回同形，由调用方统一 NotFound。
	if _, ok, nodes, err = s.ListComparisonNodes(ctx, 424242, ComparisonPublic); err != nil || ok || nodes != nil {
		t.Fatalf("missing task = %v %v %v", ok, nodes, err)
	}
	privOnly, _, err := s.SaveProbeTask(ctx, &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "priv.example", IntervalS: 60, TimeoutMs: 1000}, NodeSelector{NodeIDs: []int64{a}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, nodes, err = s.ListComparisonNodes(ctx, privOnly.Task.Id, ComparisonPublic); err != nil || !ok || len(nodes) != 0 {
		t.Fatalf("private-only task = %v %v %v", ok, nodes, err)
	}
	// 管理口径（无 principal）：显式任务的两个节点都可见，含私有节点，按同一全序。
	if _, ok, nodes, err = s.ListComparisonNodes(ctx, some.Task.Id, ComparisonScoped); err != nil || !ok || !slices.Equal(nodes, []int64{bPub, a}) {
		t.Fatalf("scoped(admin) candidates = %v %v %v", ok, nodes, err)
	}
}

// 经迁移 33 升上来的库带对比索引，且对比查询走索引。
func TestMigratedComparisonLookupUsesByTaskIndex(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 32, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO probe_1m (node_id, ts, task_id, sent, lost, errors, rtt_sum_us) VALUES (7, 60, 1, 2, 1, 0, 300)"); err != nil {
			t.Fatal(err)
		}
	})
	for _, table := range probeTables {
		var n int
		if err := migrated.r.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", table+"_by_task").Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s_by_task present=%d err=%v", table, n, err)
		}
	}
	plan := strings.Join(queryPlans(t, migrated.r,
		probeFamily.rangeSQL(0, queryShape{keyWhere: "task_id = ? AND node_id IN (?,?)", seriesLimit: MaxComparisonNodes, byTaskIndex: true}), 1, 7, 8, 0, 86400), "\n")
	if !strings.Contains(plan, "INDEX probe_1m_by_task") {
		t.Fatalf("migrated comparison lookup must use the by-task index: %s", plan)
	}
}

// 只读库与写协程挂起只影响写入口：历史查询（含对比）只占读池，照常返回。api 层拿不到
// store 的这两个私有句柄，但入口调用的正是这些读方法，故障只可能来自这两个私有侧。
func TestComparisonReadsSurviveWriteFaults(t *testing.T) {
	t.Parallel()
	s, path := openAt(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC).Unix()
	b := &metric.ProbeBucket{Sent: 1, RttN: 1, RttSumUs: 100, RttMinUs: 100, RttMaxUs: 100}
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Probes: []metric.ProbeRow{{NodeID: id, TS: now - 60, TaskID: 7, Bucket: b}}}); err != nil {
		t.Fatal(err)
	}

	// 写协程挂起：一个不返回的写占住唯一的写协程；entered 关闭即证明它已停在 fn 里。
	stuck := make(chan struct{})
	entered := make(chan struct{})
	go func() {
		_ = s.write(ctx, func(*sql.Tx) error { close(entered); <-stuck; return nil })
	}()
	<-entered
	minute, _ := LevelByName("1m")
	mustRead := func(label string) {
		t.Helper()
		if _, err := s.QueryProbeComparison(ctx, 7, []int64{id}, now-120, now, minute, 60); err != nil {
			t.Fatalf("%s: comparison err = %v", label, err)
		}
		if _, _, err := s.QueryMetricsCoverage(ctx, id, now-120, now, minute, 60); err != nil {
			t.Fatalf("%s: metrics err = %v", label, err)
		}
	}
	mustRead("writer stuck")
	close(stuck)

	// 只读库：写侧以 mode=ro 重开，任何写报只读；读侧不变。
	s.w.Close()
	ro, err := sql.Open("sqlite", dsn(path, "&mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	old := s.w
	s.w = ro
	if err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO tag (name) VALUES ('x')")
		return err
	}); err == nil {
		t.Fatal("write to read-only database must fail")
	}
	mustRead("database read-only")
	s.w = old
}

// 告警的历史读取（engine.EvaluateProbes：QueryProbes，窗口 = for_minutes 分钟、1m 级）
// 在 for_minutes 的校验上限（alert.CheckRule：1..60）下远不到读量额度：每节点每分钟
// 每任务一行，61 分钟 × 满配 64 个分配槽 + 已删任务的残留行也只有几千行，距
// 64 × quotaRowsPerSeries 很远。这里的用例按上限满打满算地钉住这一点。
func TestAlertWindowStaysWithinQuota(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	now := clk.Now().Unix()
	var probes []metric.ProbeRow
	// 65 个任务编号（64 个满配槽 + 1 个已删除、清理尚未完成的残留）在最近 61 分钟里每分钟各有一行。残留行不出现在
	// 结果里，但查询照样读到它们，额度按读到的行计。残留行绕过写入口直接写：写入口拒收已删任务的行。
	for task := uint64(1); task <= 64; task++ {
		seedProbeTasks(t, s, task)
	}
	for task := uint64(1); task <= 65; task++ {
		for m := int64(0); m <= 60; m++ {
			probes = append(probes, metric.ProbeRow{NodeID: id, TS: now - m*60, TaskID: task,
				Bucket: &metric.ProbeBucket{Sent: 1, RttN: 1, RttSumUs: 100, RttMinUs: 100, RttMaxUs: 100}})
		}
	}
	seedRawProbes(t, s, probes...)
	minute, _ := LevelByName("1m")
	// 与 engine.go 的调用同参：from = minuteTS-(for_minutes-1)*60，to = minuteTS+60，step 60。
	rows, err := s.QueryProbes(ctx, id, now-59*60, now+60, minute, 60, 0)
	if err != nil {
		t.Fatalf("alert-shaped window must stay within quota: %v", err)
	}
	// 窗口覆盖 m=0..59 共 60 分钟（to = minuteTS+60 是开区间端点）。
	if len(rows) != 64*60 {
		t.Fatalf("rows = %d, want %d", len(rows), 64*60)
	}
}

// 额度按对齐后的窗口计数：对齐把窗口向上取整到 step，恰好把一行挤过额度时必须被拒。
// 若把计数换成原始窗口（读取仍是对齐窗口），这行会被漏数——用例在额度边界上钉住。
func readQuotaCountsAlignedWindow(t *testing.T, tpl string) {
	const rows = quotaRowsPerSeries*probelimit.MaxTasksPerNode + 1
	// 行在 1h 层，水位盖过全部数据；原始窗口 [0, rows*3600-1] 恰含 rows-1 行，7d 步长的对齐把最后一行拉进来。
	s, _ := openProbeHistory(t, tpl, rows)
	ctx := t.Context()
	id := createHistoryNode(t, s)
	end := int64(rows) * 3600
	setProbeWatermarks(t, s, end+3600, end+3600)
	lv, _ := LevelByName("1h")
	_, err := s.QueryProbes(ctx, id, 0, end-1, lv, 7*86400, 0)
	var quota ReadQuotaError
	if !errors.As(err, &quota) {
		t.Fatalf("aligned window must push the last row over quota: err = %v", err)
	}
}

// 额度包含水位之后的细级尾巴：粗级行数远在额度内、细级尾巴超过额度时必须被拒。
// 若计数漏掉细级（只数所选级别），这里会被放过——用例钉住"各级都要数"。
func TestReadQuotaCountsFineTail(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	const rows = quotaRowsPerSeries*probelimit.MaxTasksPerNode + 1
	// 7d 窗口落在 5m 级；5m 水位回拨到窗口起点，5m 表为空，全部来源行在 1m 尾巴里。
	base := int64(1767225600) // 2026-01-01T00:00:00Z，分钟对齐
	if err := fillProbeRowsStep(t.Context(), s.w, "probe_1m", id, 1, rows, 60, base); err != nil {
		t.Fatal(err)
	}
	from := base - 60
	to := base + int64(rows)*60
	setProbeWatermarks(t, s, from, from)
	lv, _ := LevelByName("5m")
	_, err = s.QueryProbes(ctx, id, from, to, lv, 3600, 0)
	var quota ReadQuotaError
	if !errors.As(err, &quota) {
		t.Fatalf("fine tail beyond quota must be rejected: err = %v", err)
	}
}

// createHistoryNode 在 openProbeHistory 的库上建节点。模板的探测行属于节点 1，库里此前没有节点，AUTOINCREMENT
// 发出的第一个 id 就是 1；这里核对它，id 对不上时用例查的是一个没有历史的节点。
func createHistoryNode(t *testing.T, s *Store) int64 {
	t.Helper()
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("node id = %d, want 1 (the node the template's probe rows belong to)", id)
	}
	return id
}

func setProbeWatermarks(t *testing.T, s *Store, wm5m, wm1h int64) {
	t.Helper()
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE rollup_state SET upto_ts = ? WHERE level = 'probe_5m'", wm5m)
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE rollup_state SET upto_ts = ? WHERE level = 'probe_1h'", wm1h)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// 额度权重按满配任务槽（64）而不是当前任务数：历史里有 65 个任务编号的行、当前只分配了
// 10 个任务时，760k 行仍在 768k 额度内照常服务。若权重改按当前任务数计，这里会被误拒。
func readQuotaUsesFullTaskSlots(t *testing.T, tpl string) {
	// 760,000 行摊在 65 个任务编号上（10 个在配、55 个已撤/已删），≤ 64×12000。
	s, _ := openProbeHistory(t, tpl, 760000)
	ctx := t.Context()
	id := createHistoryNode(t, s)
	for k := 1; k <= 10; k++ {
		if _, _, err := s.SaveProbeTask(ctx, &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: fmt.Sprintf("192.0.2.%d", k), IntervalS: 60, TimeoutMs: 1000}, NodeSelector{NodeIDs: []int64{id}}); err != nil {
			t.Fatal(err)
		}
	}
	end := int64(768001) * 3600
	setProbeWatermarks(t, s, end, end)
	lv, _ := LevelByName("1h")
	rows, err := s.QueryProbes(ctx, id, 0, end, lv, 7*86400, 0)
	if err != nil {
		t.Fatalf("history of rotated task slots must stay within full-slot quota: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("rows expected")
	}
}
