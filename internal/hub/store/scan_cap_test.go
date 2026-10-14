package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"modernc.org/sqlite"
)

// poolEvent 是读池驱动钩子记下的一件事：哪个池上开了事务、执行了什么查询、事务怎样结束。
type poolEvent struct {
	pool string
	// kind 取 begin、query、commit、rollback。
	kind  string
	query string
	// limit 与 count 只对额度计数语句有意义：limit 是它的 LIMIT 参数，count 是它读回的行数。
	limit, count int64
}

type poolTrace struct {
	mu     sync.Mutex
	events []poolEvent
}

func (p *poolTrace) add(e poolEvent) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	return len(p.events) - 1
}

func (p *poolTrace) setCount(i int, n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events[i].count = n
}

func (p *poolTrace) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = nil
}

func (p *poolTrace) snapshot() []poolEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.events)
}

type poolTraceDriver struct {
	pool  string
	trace *poolTrace
}

type poolTraceConn struct {
	driver.Conn
	pool  string
	trace *poolTrace
}

type poolTraceTx struct {
	driver.Tx
	pool  string
	trace *poolTrace
}

// poolTraceRows 把额度计数语句读回的那一个值记进对应的事件。
type poolTraceRows struct {
	driver.Rows
	trace *poolTrace
	event int
}

func (d poolTraceDriver) Open(name string) (driver.Conn, error) {
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &poolTraceConn{Conn: c, pool: d.pool, trace: d.trace}, nil
}

func isQuotaCount(q string) bool { return strings.HasPrefix(q, "SELECT count(*) FROM (SELECT 1 FROM ") }

func (c *poolTraceConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	e := poolEvent{pool: c.pool, kind: "query", query: q}
	if isQuotaCount(q) {
		e.limit = args[len(args)-1].Value.(int64)
	}
	i := c.trace.add(e)
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil || !isQuotaCount(q) {
		return rows, err
	}
	return &poolTraceRows{Rows: rows, trace: c.trace, event: i}, nil
}

func (r *poolTraceRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil {
		r.trace.setCount(r.event, dest[0].(int64))
	}
	return err
}

// BeginTx 透传只读事务选项：包裹层丢了它，database/sql 会把 ReadOnly 当不支持而拒绝。
func (c *poolTraceConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.trace.add(poolEvent{pool: c.pool, kind: "begin"})
	return &poolTraceTx{Tx: tx, pool: c.pool, trace: c.trace}, nil
}

func (t *poolTraceTx) Commit() error {
	t.trace.add(poolEvent{pool: t.pool, kind: "commit"})
	return t.Tx.Commit()
}

func (t *poolTraceTx) Rollback() error {
	t.trace.add(poolEvent{pool: t.pool, kind: "rollback"})
	return t.Tx.Rollback()
}

// tracePools 把全部读池换成按池记事件的驱动；查询语义不变。
func tracePools(t *testing.T, s *Store) *poolTrace {
	t.Helper()
	trace := &poolTrace{}
	id := traceID.Add(1)
	reopenReadPoolsBy(t, s, func(pool string) string {
		name := fmt.Sprintf("pool-trace-%s-%d", pool, id)
		sql.Register(name, poolTraceDriver{pool: pool, trace: trace})
		return name
	})
	return trace
}

// checkCappedRetry 断言 events 是一次"轻池封顶、改在历史池重跑"的完整过程：
//   - 轻池上只有水位读与额度计数，没有聚合；
//   - 轻池上每条计数的 LIMIT 加上此前各层已读到的行数不超过 lightScanRows+1（封顶跨层累计），且计数确实读到了
//     lightScanRows+1 行（是封顶触发了重跑，而不是别的错误）；
//   - 轻池的事务先回滚，历史池的事务才开始（r 尝试在自己的函数帧里归还连接）；
//   - 聚合只在历史池上执行一次。
func checkCappedRetry(t *testing.T, events []poolEvent) {
	t.Helper()
	var lightCounts []poolEvent
	lightRollback, historyBegin, historyGroups := -1, -1, 0
	for i, e := range events {
		switch {
		case e.pool == "light" && e.kind == "query":
			if isQuotaCount(e.query) {
				lightCounts = append(lightCounts, e)
			} else if !strings.HasPrefix(e.query, "SELECT upto_ts FROM rollup_state") {
				t.Errorf("light pool ran %q; want only watermark reads and quota counts there", e.query)
			}
		case e.pool == "light" && e.kind == "rollback" && lightRollback < 0:
			lightRollback = i
		case e.pool == "history" && e.kind == "begin" && historyBegin < 0:
			historyBegin = i
		case e.pool == "history" && e.kind == "query" && strings.Contains(e.query, "GROUP BY"):
			historyGroups++
		case e.pool == "evaluation":
			t.Errorf("request read touched the evaluation pool: %+v", e)
		}
	}
	if len(lightCounts) == 0 {
		t.Fatalf("no quota count ran on the light pool; events = %+v", events)
	}
	var read int64
	for k, c := range lightCounts {
		if read+c.limit > lightScanRows+1 {
			t.Errorf("light count %d: LIMIT %d after %d rows already counted exceeds the cap %d+1 (limits must shrink with what earlier levels read)", k, c.limit, read, lightScanRows)
		}
		read += c.count
	}
	if read != lightScanRows+1 {
		t.Errorf("light pool counted %d rows over %d levels, want exactly %d (the cap tripping)", read, len(lightCounts), lightScanRows+1)
	}
	if lightRollback < 0 || historyBegin < 0 || lightRollback > historyBegin {
		t.Errorf("light rollback at event %d, history begin at event %d; want the light transaction returned before the history one starts", lightRollback, historyBegin)
	}
	if historyGroups != 1 {
		t.Errorf("aggregation ran %d times on the history pool, want 1", historyGroups)
	}
}

// fillProbeLevel 给 node 在 table 里写 tsCount 个时刻（从 base 起按 step）、每个时刻 perTS 行的探测行。task_id 在
// [1, tasks] 里轮转，每个时刻的 perTS 行各不相同，模拟任务更替：窗口里出现的 task_id 远多于任一时刻的行数。绕过写协程
// 直接写，仅测试夹具使用。
func fillProbeLevel(t *testing.T, s *Store, table string, node, base, step, tsCount, perTS, tasks int64) int64 {
	t.Helper()
	if _, err := s.w.ExecContext(t.Context(),
		"WITH RECURSIVE ts(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM ts WHERE i+1 < ?), "+
			"k(j) AS (SELECT 0 UNION ALL SELECT j+1 FROM k WHERE j+1 < ?) "+
			"INSERT INTO "+table+" (node_id, ts, task_id, sent, lost, errors, rtt_sum_us, rtt_min_us, rtt_max_us) "+
			"SELECT ?, ? + i*?, 1 + ((i*? + j) % ?), 1, 0, 0, 100, 100, 100 FROM ts, k",
		tsCount, perTS, node, base, step, perTS, tasks); err != nil {
		t.Fatal(err)
	}
	return tsCount * perTS
}

// 维护停滞让细级尾巴变长：预计扫描量（请求级每桶 64 条序列、不计细级尾巴）在分界之下，查询首次进轻池；三层（1h 主体 +
// 5m 尾 + 1m 尾）合计的实际行数超过 lightScanRows，粗级两层未触顶、第三层触顶。轻池上只计数、按跨层累计的余量封顶，
// 回滚后改在历史池聚合；结果与同一数据经不封顶的评估池读出的逐行相同。
func TestLightScanCapRetriesStalledTailsOnHistoryPool(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	const (
		base    = int64(86400 * 100)
		hours   = 60
		perTS   = 20
		tasks   = 50
		coarseH = 40
		tailM5  = 10 * 12
		tailM1  = 10 * 60
	)
	for task := uint64(1); task <= tasks; task++ {
		seedProbeTasks(t, s, task)
	}
	lv, _ := LevelByName("1h")
	from, to := base, base+hours*3600
	if est := scanEstimate(from, to-1, lv, 64); est > lightScanRows {
		t.Fatalf("estimate %d must stay under the light cutoff %d for the query to start on the light pool", est, lightScanRows)
	}
	upto1h := base + coarseH*3600
	upto5m := upto1h + tailM5*300
	coarse := fillProbeLevel(t, s, "probe_1h", id, base, 3600, coarseH, perTS, tasks)
	m5 := fillProbeLevel(t, s, "probe_5m", id, upto1h, 300, tailM5, perTS, tasks)
	fillProbeLevel(t, s, "probe_1m", id, upto5m, 60, tailM1, perTS, tasks)
	if coarse+m5 > lightScanRows || coarse+m5+tailM1*perTS <= lightScanRows {
		t.Fatalf("fixture must trip the cap on the third level: coarse %d + 5m %d, 1m %d", coarse, m5, tailM1*perTS)
	}
	setWatermark(t, s, "probe_1h", upto1h)
	setWatermark(t, s, "probe_5m", upto5m)

	trace := tracePools(t, s)
	got, err := s.QueryProbes(t.Context(), id, from, to, lv, 3600, 0)
	if err != nil {
		t.Fatal(err)
	}
	checkCappedRetry(t, trace.snapshot())
	if n := countLightCounts(trace.snapshot()); n != 3 {
		t.Errorf("light pool ran %d quota counts, want 3 (two coarse levels pass, the third trips)", n)
	}

	trace.reset()
	want, err := s.Evaluation().QueryProbes(t.Context(), id, from, to, lv, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("capped-and-retried result differs from the uncapped read: got %d rows, want %d", len(got), len(want))
	}
}

func countLightCounts(events []poolEvent) int {
	var n int
	for _, e := range events {
		if e.pool == "light" && e.kind == "query" && isQuotaCount(e.query) {
			n++
		}
	}
	return n
}

// 任务更替：节点当前只有 2 个任务（调用方按它估，30 天 1h 级 720 桶 × 2 = 1440 行，在分界之下），窗口里却留着 50 个
// 历史 task_id 的行（从这个节点撤下、仍存在的任务，历史照常可读；已删任务的行在清理完成前同样被读到，只是不出现在结果里）。三层（1h 主体 + 5m 尾 + 1m 尾）合计远超 lightScanRows：轻池上只计数、按跨层累计的余量封顶，
// 第三层触顶后回滚，聚合只在历史池执行；结果与同一数据按序列上限估、直接走历史池的结果逐行相同。
func TestLightScanCapCatchesTaskTurnover(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	const (
		base    = int64(86400 * 100)
		days    = 30
		perTS   = 4
		tasks   = 50
		current = 2
		coarseH = 20 * 24
		tailM5  = 6 * 12
		tailM1  = 12 * 60
	)
	for task := uint64(1); task <= tasks; task++ {
		seedProbeTasks(t, s, task)
	}
	lv, _ := LevelByName("1h")
	from, to := base, base+days*86400
	if est := scanEstimate(from, to-1, lv, current); est > lightScanRows || scanEstimate(from, to-1, lv, 64) <= lightScanRows {
		t.Fatalf("the current-task estimate %d must start on the light pool and the 64-series one %d on the history pool (cutoff %d)", est, scanEstimate(from, to-1, lv, 64), lightScanRows)
	}
	upto1h := base + coarseH*3600
	upto5m := upto1h + tailM5*300
	coarse := fillProbeLevel(t, s, "probe_1h", id, base, 3600, coarseH, perTS, tasks)
	m5 := fillProbeLevel(t, s, "probe_5m", id, upto1h, 300, tailM5, perTS, tasks)
	m1 := fillProbeLevel(t, s, "probe_1m", id, upto5m, 60, tailM1, perTS, tasks)
	if coarse+m5 > lightScanRows || coarse+m5+m1 <= lightScanRows {
		t.Fatalf("fixture must trip the cap on the third level: coarse %d + 5m %d, 1m %d", coarse, m5, m1)
	}
	setWatermark(t, s, "probe_1h", upto1h)
	setWatermark(t, s, "probe_5m", upto5m)

	trace := tracePools(t, s)
	got, err := s.QueryProbes(t.Context(), id, from, to, lv, 3600, current)
	if err != nil {
		t.Fatal(err)
	}
	events := trace.snapshot()
	checkCappedRetry(t, events)
	if n := countLightCounts(events); n != 3 {
		t.Errorf("light pool ran %d quota counts, want 3 (two coarse levels pass, the third trips)", n)
	}
	taskIDs := map[uint64]bool{}
	for _, r := range got {
		taskIDs[r.TaskID] = true
	}
	if len(taskIDs) != tasks {
		t.Errorf("result has %d distinct tasks, want the %d historical ones", len(taskIDs), tasks)
	}

	trace.reset()
	want, err := s.QueryProbes(t.Context(), id, from, to, lv, 3600, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range trace.snapshot() {
		if e.pool != "history" {
			t.Errorf("64-series estimate must go straight to the history pool, saw %+v", e)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capped-and-retried result differs from the direct history read: got %d rows, want %d", len(got), len(want))
	}
}
