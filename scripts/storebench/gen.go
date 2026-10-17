package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/probelimit"
)

// genConfig 是 gen 的全部输入；同一份 genConfig 生成的库逐字节相同（gen_test 守住）。
type genConfig struct {
	Nodes int `json:"nodes"`
	Tasks int `json:"tasks"`
	// End 是时间轴终点（Unix 秒，整点）。生成结束时 hub 的墙钟是 End + 242（见 timeline），水位、最老行都由它推出。
	End  int64  `json:"end"`
	Seed uint64 `json:"seed"`
	// ContainerFrac 是环境范围（LXC 一类）节点的比例，这些节点的 steal、iowait、负载与磁盘速率列 n = 0（见 nodeProfile）。
	ContainerFrac float64 `json:"container_frac"`
	// Churn 为真时按生产节奏重放写入与清理：每张表先写满一个保留期、再在第二个保留期里边写边删，最终布局是持续运行一年以上
	// 的稳态（见 timeline）。为假时只写最终留存的行、从不删除，作对照。
	Churn bool `json:"churn"`
}

// genManifest 写在库旁（<db>.manifest.json），time 与测试据此知道库的时间轴与主体；它不进库，确定性只针对库文件。
type genManifest struct {
	Config        genConfig        `json:"config"`
	SchemaVersion int              `json:"schema_version"`
	Now           int64            `json:"now"`
	CreatedAt     int64            `json:"created_at"`
	CoverageStart int64            `json:"coverage_start"`
	NodeIDs       []int64          `json:"node_ids"`
	TaskIDs       []uint64         `json:"task_ids"`
	Watermarks    map[string]int64 `json:"watermarks"`
	Rows          map[string]int64 `json:"rows"`
	Inserted      map[string]int64 `json:"inserted"`
	Deleted       map[string]int64 `json:"deleted"`
}

func manifestPath(db string) string { return db + ".manifest.json" }

func readManifest(db string) (genManifest, error) {
	var m genManifest
	raw, err := os.ReadFile(manifestPath(db))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(raw, &m)
}

// timeline 是 hub 在生成结束时刻的状态，全部由 End 推出，与生产的维护节奏一一对应：
//   - 分钟边界 B 之后 0.5 s 刷出 ts = B − 60 的 1m 行（live.Flush），+2 s 跑一轮维护（store.nextMaintenanceAt）：
//     上卷到 ceiling = B + 2 − RollupLag，再按保留期清理。
//   - 最后一个边界 last = End + 240，墙钟 now = last + 2：最新 1m 行 ts = End + 180；ceiling = End − 58，5m 水位
//     alignDown(ceiling, 300) = End − 300，1h 水位 alignDown(ceiling, 3600) = End − 3600。水位因此在 1m 最老行之后、
//     落后墙钟 RollupLag 左右，与运行中的 hub 相同。
//   - 在此墙钟下把时钟拨到 End + RollupLag 跑一次 Rollup，ceiling 恰为 End：5m 与 1h 各推进一个桶，两族都有活（time 的上卷）。
type timeline struct {
	end, last, now int64
	// start 是每张表最早写入的桶，cut 是生成结束时的清理截止点（最老留存行）；按 1m、5m、1h。
	start, cut [3]int64
}

var levelBuckets = [3]int64{60, 300, 3600}

func retentionSeconds(i int) int64 {
	r := []time.Duration{store.DefaultRetention.M1, store.DefaultRetention.M5, store.DefaultRetention.H1}[i]
	return int64(r / time.Second)
}

func alignDown(ts, b int64) int64 { return ts - ts%b }

func newTimeline(end int64, churn bool) (timeline, error) {
	if end%3600 != 0 {
		return timeline{}, fmt.Errorf("end %d is not on an hour boundary", end)
	}
	t := timeline{end: end, last: end + 240}
	t.now = t.last + 2
	for i := range 3 {
		t.cut[i] = alignDown(t.now-retentionSeconds(i), levelBuckets[i])
	}
	// Prune 对细一级只清到粗一级的水位（rollup.go 的 consumed）；默认保留期下水位远在截止点之后，min 不起作用，写出来
	// 是为了与 Prune 同一口径。
	t.cut[0] = min(t.cut[0], t.wm5())
	t.cut[1] = min(t.cut[1], t.wm1h())
	for i := range 3 {
		t.start[i] = t.cut[i]
		if churn {
			t.start[i] -= retentionSeconds(i)
		}
	}
	return t, nil
}

func (t timeline) wm5() int64  { return alignDown(t.now-int64(store.RollupLag/time.Second), 300) }
func (t timeline) wm1h() int64 { return alignDown(t.now-int64(store.RollupLag/time.Second), 3600) }

// coverageStart 是节点首次上报的分钟：最早写入的桶（1h 级的 start）。早于它的分钟不计入覆盖率分母。
func (t timeline) coverageStart() int64 { return t.start[2] }

// createdAt 是建节点与建任务的时刻：首报前一天。
func (t timeline) createdAt() int64 { return t.coverageStart() - 86400 }

// sqliteDSN 与 store 的 dsn 同一编码：路径按 URL 路径部分转义，# ? % 不会改变实际打开的文件。
// _dqs=0 与被测的 store 连接同规则（关掉双引号字符串回退），量出来的才是生产连接的行为。
func sqliteDSN(path, pragmas string) string {
	u := url.URL{Path: path}
	return "file:" + u.EscapedPath() + "?_dqs=0&" + pragmas
}

// errorLogger 只放出 store 的错误日志：生成与测量期间的告警（例如写协程的排队提示）不是结果的一部分。
func errorLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// gen 建出一份库。表结构只经 store.Open 的真实迁移建出；节点经 CreateNode、探测任务经 SaveProbeTask 建出（准入、上限与
// 任务版本都走生产路径）；时序行与覆盖起点、水位绕开写协程直接写入（生成不是被测对象），但每条 INSERT 的列清单都先与
// 迁移出的表逐列核对（checkColumns），表里多出或少了一列都直接失败，不靠列默认值补齐。
func gen(ctx context.Context, path string, cfg genConfig) (genManifest, error) {
	if cfg.Nodes < 1 {
		return genManifest{}, fmt.Errorf("nodes must be at least 1; got %d", cfg.Nodes)
	}
	if cfg.Tasks < 0 || cfg.Tasks > probelimit.MaxTasksPerNode {
		return genManifest{}, fmt.Errorf("tasks must be between 0 and %d; got %d", probelimit.MaxTasksPerNode, cfg.Tasks)
	}
	if cfg.ContainerFrac < 0 || cfg.ContainerFrac > 1 {
		return genManifest{}, fmt.Errorf("container fraction must be in [0, 1]; got %v", cfg.ContainerFrac)
	}
	tl, err := newTimeline(cfg.End, cfg.Churn)
	if err != nil {
		return genManifest{}, err
	}
	// 只生成到空路径：覆盖已有库会让一份库的数字混进另一份的内容，-wal / -shm 残留同理。
	for _, p := range []string{path, path + "-wal", path + "-shm", manifestPath(path)} {
		if _, err := os.Stat(p); err == nil {
			return genManifest{}, fmt.Errorf("%s already exists; gen only writes to a fresh path", p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return genManifest{}, err
		}
	}
	m := genManifest{Config: cfg, Now: tl.now, CreatedAt: tl.createdAt(), CoverageStart: tl.coverageStart(),
		Watermarks: map[string]int64{"5m": tl.wm5(), "1h": tl.wm1h(), "probe_5m": tl.wm5(), "probe_1h": tl.wm1h()},
		Inserted:   map[string]int64{}, Deleted: map[string]int64{}, Rows: map[string]int64{}}
	if err := m.createSubjects(ctx, path); err != nil {
		return genManifest{}, err
	}
	if err := m.fillSeries(ctx, path, tl); err != nil {
		return genManifest{}, err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return genManifest{}, err
	}
	return m, os.WriteFile(manifestPath(path), append(raw, '\n'), 0o644)
}

// createSubjects 经 store 的写接口建库、节点与探测任务。
func (m *genManifest) createSubjects(ctx context.Context, path string) error {
	clk := clock.NewFake(time.Unix(m.CreatedAt, 0))
	st, err := store.Open(path, clk, errorLogger(), store.MigrateSchema)
	if err != nil {
		return err
	}
	defer st.Close()
	for i := range m.Config.Nodes {
		id, _, err := st.CreateNode(ctx, store.NewNode{Name: fmt.Sprintf("node-%04d", i+1)}, derivedBytes(m.Config.Seed, "token", uint64(i), sha256.Size))
		if err != nil {
			return fmt.Errorf("create node %d: %w", i+1, err)
		}
		m.NodeIDs = append(m.NodeIDs, id)
	}
	for k := range m.Config.Tasks {
		task := &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: fmt.Sprintf("198.51.100.%d", k+1), IntervalS: probeIntervalS, TimeoutMs: 1000}
		rec, _, err := st.SaveProbeTask(ctx, task, store.NodeSelector{NodeIDs: m.NodeIDs})
		if err != nil {
			return fmt.Errorf("save probe task %d: %w", k+1, err)
		}
		m.TaskIDs = append(m.TaskIDs, rec.Task.GetId())
	}
	return st.Close()
}

// probeIntervalS 与面板新建任务的默认间隔一致（见 probeProfile），1m 桶 sent = 60 / 60 = 1。
const probeIntervalS = 60

// derivedBytes 从种子派生 n 字节，让需要随机字节的列（token_hash、config_id）在同一 genConfig 下逐字节可复现。
func derivedBytes(seed uint64, label string, i uint64, n int) []byte {
	var out []byte
	for block := uint64(0); len(out) < n; block++ {
		var in [24]byte
		binary.BigEndian.PutUint64(in[0:], seed)
		binary.BigEndian.PutUint64(in[8:], i)
		binary.BigEndian.PutUint64(in[16:], block)
		sum := sha256.Sum256(append([]byte(label), in[:]...))
		out = append(out, sum[:]...)
	}
	return out[:n]
}

var metricTables = [3]string{"metric_1m", "metric_5m", "metric_1h"}
var probeTables = [3]string{"probe_1m", "probe_5m", "probe_1h"}

// metricInsertColumns 是生成写入的指标列，顺序与 metricValues 的输出一致；1m 级另写 reported、observed，粗级另写
// minutes、observed、both（store 的 metricDDL）。
func metricInsertColumns(level int) []string {
	cols := []string{"node_id", "ts"}
	for _, c := range metric.Columns {
		cols = append(cols, c.Name+"_sum", c.Name+"_n")
		if c.Kind == metric.MeanMax {
			cols = append(cols, c.Name+"_max")
		}
	}
	if level == 0 {
		return append(cols, "reported", "observed")
	}
	return append(cols, "minutes", "observed", "both")
}

var probeInsertColumns = []string{"node_id", "ts", "task_id", "sent", "lost", "errors", "rtt_sum_us", "rtt_min_us", "rtt_max_us"}

// checkColumns 要求 cols 与迁移出的表的列集合完全相同。
func checkColumns(ctx context.Context, db *sql.DB, table string, cols []string) error {
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return err
	}
	defer rows.Close()
	var have []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		have = append(have, n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	want := slices.Clone(cols)
	slices.Sort(have)
	slices.Sort(want)
	if !slices.Equal(have, want) {
		return fmt.Errorf("table %s has columns %v; the generator writes %v", table, have, want)
	}
	return nil
}

// inserter 把行攒成多行 VALUES 一次执行：每行一条 Exec 时 database/sql 的逐次开销比 SQLite 写一行还大。攒批不改变
// 写入顺序，B 树里的行与逐行 INSERT 相同。
type inserter struct {
	tx    *sql.Tx
	head  string
	width int
	rows  int
	args  []any
	stmts map[int]*sql.Stmt
	count *int64
}

const insertBatchRows = 20

func newInserter(table string, cols []string, count *int64) *inserter {
	return &inserter{head: "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES ", width: len(cols), stmts: map[int]*sql.Stmt{}, count: count}
}

func (in *inserter) add(ctx context.Context, args []any) error {
	if len(args) != in.width {
		return fmt.Errorf("%s: %d values for %d columns", in.head, len(args), in.width)
	}
	in.args = append(in.args, args...)
	in.rows++
	if in.rows == insertBatchRows {
		return in.flush(ctx)
	}
	return nil
}

func (in *inserter) flush(ctx context.Context) error {
	if in.rows == 0 {
		return nil
	}
	st, ok := in.stmts[in.rows]
	if !ok {
		row := "(" + strings.TrimSuffix(strings.Repeat("?, ", in.width), ", ") + ")"
		var err error
		if st, err = in.tx.PrepareContext(ctx, in.head+strings.TrimSuffix(strings.Repeat(row+", ", in.rows), ", ")); err != nil {
			return err
		}
		in.stmts[in.rows] = st
	}
	if _, err := st.ExecContext(ctx, in.args...); err != nil {
		return err
	}
	*in.count += int64(in.rows)
	in.rows, in.args = 0, in.args[:0]
	return nil
}

func (in *inserter) close() error {
	var errs []error
	for _, st := range in.stmts {
		errs = append(errs, st.Close())
	}
	in.stmts = map[int]*sql.Stmt{}
	return errors.Join(errs...)
}

// fillSeries 写时序行、覆盖起点、水位与维护簿记，最后检查点并关闭，使库文件自包含（没有 -wal）。
func (m *genManifest) fillSeries(ctx context.Context, path string, tl timeline) error {
	// synchronous(OFF)、journal_mode(OFF) 与大页缓存只为生成速度：省掉 WAL 的二次写入与检查点；中途失败的库作废重生成，
	// 不需要日志保护。它们不改变页内容：同一配置分别在 WAL 与 OFF 下生成（N=3、T=1），两份文件只差文件头第 24–27 与
	// 92–95 字节（变更计数与 version-valid-for），其余字节全同。结束时切回 WAL 并检查点，文件头的日志模式字节与 hub
	// 建出的库相同。
	db, err := sql.Open("sqlite", sqliteDSN(path, "_pragma=busy_timeout(5000)&_pragma=synchronous(OFF)&_pragma=cache_size(-262144)&_pragma=journal_mode(OFF)"))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for i := range 3 {
		if err := checkColumns(ctx, db, metricTables[i], metricInsertColumns(i)); err != nil {
			return err
		}
		if err := checkColumns(ctx, db, probeTables[i], probeInsertColumns); err != nil {
			return err
		}
	}
	if err := m.replay(ctx, db, tl); err != nil {
		return err
	}
	if err := m.finish(ctx, db, tl); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	return db.Close()
}

// replay 按 timeline 的节奏逐分钟边界重放：刷出 1m 行，上卷出新闭合的 5m、1h 桶，再清理过期行。行内容来自取值模型，
// 粗级不由细级聚合而来（重放只为得到生产的写入与删除次序，从而得到生产的页布局）。次序与生产一致：
//   - 同一时刻跨节点（每个分钟批、每个上卷桶含全部节点），所以每个节点的键区间都在尾部追加，页分裂发生在区间之间；
//   - 清理从每个节点区间的头部删，与尾部追加交替，空页进 freelist 再被后来的写入复用。
//
// 两族的次序同 store：分钟批先指标后探测（WriteMinuteBatch），上卷与清理按 families × levels。事务按模拟的整点提交；
// SQLite 的页分配与 B 树平衡逐语句进行，与事务边界无关，所以合并事务不改变布局。
func (m *genManifest) replay(ctx context.Context, db *sql.DB, tl timeline) error {
	cfg := m.Config
	profRNG := rand.New(rand.NewPCG(cfg.Seed, 1))
	cols := make([][]colReading, len(m.NodeIDs))
	for i := range m.NodeIDs {
		p := newProfile(profRNG, cfg.ContainerFrac)
		var err error
		if cols[i], err = p.readings(); err != nil {
			return err
		}
	}
	probes := make([][]probeProfile, len(m.NodeIDs))
	for i := range m.NodeIDs {
		for range m.TaskIDs {
			probes[i] = append(probes[i], newProbeProfile(profRNG))
		}
	}
	r := rand.New(rand.NewPCG(cfg.Seed, 2))

	var tx *sql.Tx
	var ins [2][3]*inserter
	counts := map[string]*int64{}
	for _, t := range append(metricTables[:], probeTables[:]...) {
		counts[t] = new(int64)
	}
	begin := func() error {
		var err error
		if tx, err = db.BeginTx(ctx, nil); err != nil {
			return err
		}
		for i := range 3 {
			ins[0][i] = newInserter(metricTables[i], metricInsertColumns(i), counts[metricTables[i]])
			ins[0][i].tx = tx
			ins[1][i] = newInserter(probeTables[i], probeInsertColumns, counts[probeTables[i]])
			ins[1][i].tx = tx
		}
		return nil
	}
	commit := func() error {
		var errs []error
		for f := range 2 {
			for i := range 3 {
				errs = append(errs, ins[f][i].flush(ctx), ins[f][i].close())
			}
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
		return tx.Commit()
	}
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()
	// coarse 是每级每桶覆盖的分钟数：1m 行的 reported / observed 恒为 1（live.take 的有效上报行），粗级的 minutes、
	// observed、both 是其覆盖分钟的和；生成的节点全程在线、全程被观测，三者相等。
	minutesPer := [3]int{1, 5, 60}
	writeLevel := func(i int, ts int64) error {
		for n, id := range m.NodeIDs {
			args := []any{id, ts}
			args = bucketValues(newBucket(r, cols[n], bucketSamples(r, minutesPer[i])), args)
			if i == 0 {
				args = append(args, int64(1), int64(1))
			} else {
				mins := int64(minutesPer[i])
				args = append(args, mins, mins, mins)
			}
			if err := ins[0][i].add(ctx, args); err != nil {
				return err
			}
		}
		return nil
	}
	writeProbes := func(i int, ts int64) error {
		sent := minutesPer[i] * 60 / probeIntervalS
		for n, id := range m.NodeIDs {
			for k, task := range m.TaskIDs {
				args := probeBucketValues(newProbeBucket(r, probes[n][k], sent), []any{id, ts, int64(task)})
				if err := ins[1][i].add(ctx, args); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// prune 删 [from, to) 的行：一条语句按节点升序逐个定位主键区间，与 Prune 逐节点 deleteRange 删的是同一批行、同一次序。
	prune := func(table string, from, to int64) error {
		for _, ins := range ins {
			for _, in := range ins {
				if err := in.flush(ctx); err != nil {
					return err
				}
			}
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE node_id IN (SELECT id FROM node) AND ts >= ? AND ts < ?", from, to)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		m.Deleted[table] += n
		return nil
	}

	if err := begin(); err != nil {
		return err
	}
	lag := int64(store.RollupLag / time.Second)
	wm := [3]int64{0, tl.start[1], tl.start[2]}
	pruned := tl.start
	first := alignDown(min(tl.start[0], tl.start[1], tl.start[2]), 60)
	for b := first; b <= tl.last; b += 60 {
		if ts := b - 60; ts >= tl.start[0] {
			if err := writeLevel(0, ts); err != nil {
				return err
			}
			if err := writeProbes(0, ts); err != nil {
				return err
			}
		}
		ceiling := b + 2 - lag
		limit := [3]int64{0, alignDown(ceiling, 300), alignDown(ceiling, 3600)}
		for f := range 2 {
			for i := 1; i < 3; i++ {
				for ts := wm[i]; ts < limit[i]; ts += levelBuckets[i] {
					var err error
					if f == 0 {
						err = writeLevel(i, ts)
					} else {
						err = writeProbes(i, ts)
					}
					if err != nil {
						return err
					}
				}
			}
		}
		for i := 1; i < 3; i++ {
			wm[i] = max(wm[i], limit[i])
		}
		cut := [3]int64{
			min(alignDown(b+2-retentionSeconds(0), 60), wm[1]),
			min(alignDown(b+2-retentionSeconds(1), 300), wm[2]),
			alignDown(b+2-retentionSeconds(2), 3600),
		}
		for _, tables := range [][3]string{metricTables, probeTables} {
			for i := range 3 {
				if cut[i] > pruned[i] {
					if err := prune(tables[i], pruned[i], cut[i]); err != nil {
						return err
					}
				}
			}
		}
		for i := range 3 {
			pruned[i] = max(pruned[i], cut[i])
		}
		if b%3600 == 0 {
			if err := commit(); err != nil {
				return err
			}
			if err := begin(); err != nil {
				return err
			}
		}
	}
	if err := commit(); err != nil {
		return err
	}
	tx = nil
	for t, n := range counts {
		m.Inserted[t] = *n
	}
	if pruned != tl.cut || wm[1] != tl.wm5() || wm[2] != tl.wm1h() {
		return fmt.Errorf("replay ended at cut %v, watermarks %d/%d; timeline says %v, %d/%d", pruned, wm[1], wm[2], tl.cut, tl.wm5(), tl.wm1h())
	}
	return nil
}

// finish 写入运行中的 hub 在 tl.now 时刻应有的其余状态：覆盖起点（writeCoverageStart 写的那一行）、四个水位
// （rollupLevel 推进的值）、维护簿记（recordMaintenance）、节点最后上报时刻（WriteMinuteBatch 随分钟批写）。
// 探测任务的 config_id 由 SaveProbeTask 随机生成，是库里唯一不可复现的字节；换成种子派生的同长字节，行大小不变，
// 而它只用于相等比较（schema.go 的 probe_task.config_id）。
func (m *genManifest) finish(ctx context.Context, db *sql.DB, tl timeline) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range m.NodeIDs {
		if _, err := tx.ExecContext(ctx, "INSERT INTO node_coverage (node_id, start_ts) VALUES (?, ?)", id, m.CoverageStart); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE node SET last_seen_at = ? WHERE id = ?", tl.last-5, id); err != nil {
			return err
		}
	}
	// 按键序更新：同一页里几行的值变长时，单元格在页内的落点随更新次序变，乱序会让两次生成的页字节不同。
	for _, level := range slices.Sorted(maps.Keys(m.Watermarks)) {
		res, err := tx.ExecContext(ctx, "UPDATE rollup_state SET upto_ts = ? WHERE level = ?", m.Watermarks[level], level)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("rollup_state has no row for level %s", level)
		}
	}
	for _, name := range []string{store.MaintenanceRollup, store.MaintenancePrune} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO maintenance_state (name, finished_at) VALUES (?, ?)", name, tl.now); err != nil {
			return err
		}
	}
	for _, id := range m.TaskIDs {
		res, err := tx.ExecContext(ctx, "UPDATE probe_task SET config_id = ? WHERE id = ?", derivedBytes(m.Config.Seed, "config_id", id, probelimit.ConfigIDLen), int64(id))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("probe task %d vanished", id)
		}
	}
	for _, tables := range [][3]string{metricTables, probeTables} {
		for _, t := range tables {
			var n int64
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+t).Scan(&n); err != nil {
				return err
			}
			m.Rows[t] = n
		}
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&m.SchemaVersion); err != nil {
		return err
	}
	return tx.Commit()
}
