package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

// 面板的历史查询参数（web/src/components/History.tsx）：maxPoints 恒为 HISTORY_MAX_POINTS = 1000；窗口右端
// to = (⌊now/60⌋ + 1) × 60（useTimeWindow），左端 from = to − 跨度；api 层按 store.ChooseLevel(from, to, maxPoints) 选级与步长
// （internal/hub/api/history.go）。面板的跨度是 1h / 6h / 24h / 7d / 30d，这里取其中 6h、7d、30d 三档；365d 不是面板的档位，
// 是 API 允许的最长窗口（400 天）以内的一年历史，同用 maxPoints 1000，使四档可比。
const panelMaxPoints = 1000

var queryWindows = []struct {
	Name string
	Span int64
}{{"6h", 6 * 3600}, {"7d", 7 * 86400}, {"30d", 30 * 86400}, {"365d", 365 * 86400}}

// 并发写入条件的写入速率：生产每个分钟边界刷出一批（每节点一行指标 + 每任务一行探测，live.Flush），这里每 writePeriod
// 一批，倍数 = 60 s / writePeriod。默认 1 s 即 60 倍，目的是让被测操作与写入必然重叠，并量出写入侧被拖多久。
const productionBatchPeriod = time.Minute

type timeConfig struct {
	db, work     string
	reps         int
	conditions   []string
	ops          []string
	writePeriod  time.Duration
	pruneAdvance time.Duration
}

// timeSample 是一次被测操作。Overlapped 只在并发写入条件下给出：操作期间是否有分钟批写入在执行。
type timeSample struct {
	Rep        int            `json:"rep"`
	Ms         float64        `json:"ms"`
	Overlapped *bool          `json:"overlapped_write,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
}

type dist struct {
	N      int     `json:"n"`
	Median float64 `json:"median"`
	P95    float64 `json:"p95"`
	P99    float64 `json:"p99"`
	Max    float64 `json:"max"`
}

// summarize 用最近秩：第 ⌈p·n⌉ 个样本。
func summarize(v []float64) dist {
	if len(v) == 0 {
		return dist{}
	}
	s := slices.Sorted(slices.Values(v))
	at := func(p float64) float64 { return s[max(0, int(math.Ceil(p*float64(len(s))))-1)] }
	return dist{N: len(s), Median: at(0.5), P95: at(0.95), P99: at(0.99), Max: s[len(s)-1]}
}

type opResult struct {
	Condition string       `json:"condition"`
	Op        string       `json:"op"`
	Family    string       `json:"family,omitempty"`
	Window    string       `json:"window,omitempty"`
	Level     string       `json:"level,omitempty"`
	StepS     int64        `json:"step_s,omitempty"`
	Hooks     bool         `json:"writer_hooks"`
	Samples   []timeSample `json:"samples"`
	Ms        dist         `json:"ms"`
	// HoldMs 是 Prune 每个删除块的写者占用下界（首行改动 → 提交钩子，见 track.go），全部重复合并；CommitGapMs 是空闲条件下
	// 相邻提交的间隔（上界）。只在挂钩子的重复上给出。
	HoldMs      *dist           `json:"hold_lower_ms,omitempty"`
	HoldByTable map[string]dist `json:"hold_lower_ms_by_table,omitempty"`
	CommitGapMs *dist           `json:"commit_gap_upper_ms,omitempty"`
	// WriteMs 是并发写入条件下与被测操作重叠的分钟批写入耗时（WriteMinuteBatch 调用到返回，含排队），全部重复合并。
	WriteMs *dist `json:"write_batch_ms,omitempty"`
}

type writeRate struct {
	PeriodS           float64 `json:"batch_period_s"`
	ProductionPeriodS float64 `json:"production_batch_period_s"`
	Multiplier        float64 `json:"multiplier"`
	RowsPerBatch      int     `json:"metric_rows_per_batch"`
	ProbeRowsPerBatch int     `json:"probe_rows_per_batch"`
}

type timeReport struct {
	Env       envInfo     `json:"env"`
	DB        string      `json:"db"`
	Manifest  genManifest `json:"manifest"`
	NodeID    int64       `json:"query_node_id"`
	WriteRate writeRate   `json:"write_rate"`
	Results   []opResult  `json:"results"`
	// Writes 是每个并发写入条件下的全部分钟批（不只与操作重叠的那些）：实际达到的速率与被拒行数。
	Writes []writeSummary `json:"writes,omitempty"`
}

type writeSummary struct {
	Condition string  `json:"condition"`
	Op        string  `json:"op"`
	Batches   int     `json:"batches"`
	Rejected  int     `json:"rejected_rows"`
	Errors    int     `json:"errors"`
	RateHz    float64 `json:"achieved_batches_per_s"`
	Ms        dist    `json:"ms"`
}

func runTime(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("time", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg timeConfig
	fs.StringVar(&cfg.db, "db", "", "generated database (left untouched; every measurement runs on a fresh copy)")
	fs.StringVar(&cfg.work, "work", "", "directory for the copies (must exist; copies are removed after use)")
	fs.IntVar(&cfg.reps, "reps", 5, "repetitions per operation and condition")
	conditions := fs.String("conditions", "idle,writes", "comma-separated: idle, writes")
	ops := fs.String("ops", "rollup,query,prune", "comma-separated: rollup, query, prune")
	fs.DurationVar(&cfg.writePeriod, "write-period", time.Second, "period between minute batches in the writes condition (production: 1m)")
	fs.DurationVar(&cfg.pruneAdvance, "prune-advance", 24*time.Hour, "how far past the generation clock Prune runs")
	jsonPath := fs.String("json", "", "write the machine-readable report here")
	textPath := fs.String("text", "", "write the human-readable summary here (also printed)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.db == "" || cfg.work == "" || fs.NArg() != 0 || cfg.reps < 1 || cfg.writePeriod <= 0 {
		return errors.New("-db and -work are required, -reps must be positive, -write-period positive, and no positional arguments are accepted")
	}
	cfg.conditions, cfg.ops = strings.Split(*conditions, ","), strings.Split(*ops, ",")
	for _, c := range cfg.conditions {
		if c != "idle" && c != "writes" {
			return fmt.Errorf("unknown condition %q", c)
		}
	}
	for _, o := range cfg.ops {
		if o != "rollup" && o != "query" && o != "prune" {
			return fmt.Errorf("unknown op %q", o)
		}
	}
	rep, err := timeOps(ctx, cfg)
	if err != nil {
		return err
	}
	return writeOutputs(*jsonPath, *textPath, rep, rep.text(), stdout)
}

func timeOps(ctx context.Context, cfg timeConfig) (timeReport, error) {
	m, err := readManifest(cfg.db)
	if err != nil {
		return timeReport{}, fmt.Errorf("read manifest: %w", err)
	}
	// 生成结束时库已检查点、没有 -wal；有 -wal 说明库在 gen 之后被打开写过，复制主文件得不到同一份数据。
	if _, err := os.Stat(cfg.db + "-wal"); err == nil {
		return timeReport{}, fmt.Errorf("%s-wal exists; copies of the main file alone would not be the same data", cfg.db)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return timeReport{}, err
	}
	version, err := sqliteVersion(ctx, cfg.db)
	if err != nil {
		return timeReport{}, err
	}
	b := &bench{cfg: cfg, m: m, writes: map[string]*writeAgg{}}
	b.rep = timeReport{Env: currentEnv(version), DB: cfg.db, Manifest: m, NodeID: m.NodeIDs[len(m.NodeIDs)/2],
		WriteRate: writeRate{PeriodS: cfg.writePeriod.Seconds(), ProductionPeriodS: productionBatchPeriod.Seconds(),
			Multiplier: float64(productionBatchPeriod) / float64(cfg.writePeriod), RowsPerBatch: len(m.NodeIDs), ProbeRowsPerBatch: len(m.NodeIDs) * len(m.TaskIDs)}}
	for _, cond := range cfg.conditions {
		for _, op := range cfg.ops {
			var err error
			switch op {
			case "rollup":
				err = b.rollup(ctx, cond)
			case "query":
				err = b.query(ctx, cond)
			case "prune":
				if err = b.prune(ctx, cond, false); err == nil {
					err = b.prune(ctx, cond, true)
				}
			}
			if err != nil {
				return timeReport{}, fmt.Errorf("%s %s: %w", cond, op, err)
			}
		}
	}
	for _, key := range b.writeOrder {
		a := b.writes[key]
		ws := writeSummary{Condition: a.cond, Op: a.op, Batches: len(a.batches)}
		var lat []float64
		for _, x := range a.batches {
			lat = append(lat, ms(x.Dur))
			ws.Rejected += x.Rejected
			if x.Err != "" {
				ws.Errors++
			}
		}
		ws.Ms = summarize(lat)
		if a.elapsed > 0 {
			ws.RateHz = float64(len(a.batches)) / a.elapsed.Seconds()
		}
		b.rep.Writes = append(b.rep.Writes, ws)
	}
	return b.rep, nil
}

func sqliteVersion(ctx context.Context, path string) (string, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path, "_pragma=query_only(1)"))
	if err != nil {
		return "", err
	}
	defer db.Close()
	var v string
	return v, db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&v)
}

type bench struct {
	cfg timeConfig
	m   genManifest
	rep timeReport
	seq int
	// writes 按 条件/操作 累计各会话写入协程的全部批与运行时长，timeOps 结束时汇总成 rep.Writes。
	writes     map[string]*writeAgg
	writeOrder []string
}

type writeAgg struct {
	cond, op string
	batches  []batchSample
	elapsed  time.Duration
}

// session 是一份新副本上打开的 Store，并发写入条件下带一个写入协程。
type session struct {
	path string
	st   *store.Store
	w    *batchWriter
}

// open 复制库并在副本上打开 Store。hooks 决定这条写连接是否挂计时钩子（track.go），必须在 store.Open 之前设定。
func (b *bench) open(ctx context.Context, cond string, now time.Time, hooks bool) (*session, error) {
	b.seq++
	path := filepath.Join(b.cfg.work, fmt.Sprintf("copy-%03d.db", b.seq))
	if err := copyFile(b.cfg.db, path); err != nil {
		return nil, err
	}
	tracker.reset(hooks)
	st, err := store.Open(path, clock.NewFake(now), errorLogger(), store.RequireCurrentSchema)
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	s := &session{path: path, st: st}
	if cond == "writes" {
		s.w = startWriter(ctx, st, b.m, b.cfg.writePeriod)
		// 等第一批写完再开始被测操作：此后写入按周期持续进行，操作开始时写入已在稳态。
		if err := s.w.waitFirst(ctx); err != nil {
			s.close()
			return nil, err
		}
	}
	return s, nil
}

func (s *session) close() error {
	var errs []error
	if s.w != nil {
		errs = append(errs, s.w.stop())
	}
	errs = append(errs, s.st.Close())
	for _, p := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	// 副本就是被测的库文件。store 用 WAL + synchronous(NORMAL)（store.go 的 dsn），提交不 fsync 主库文件，
	// 每次 checkpoint 完成后才 fsync 它，而 fsync 要把这个文件此刻的全部脏页写回——刚复制完的副本可能还有
	// 几 GB 没写回。不在这里落盘，这笔写回就算进触发第一次 checkpoint 的那个被测操作（哪个操作先把 WAL 写过自动
	// checkpoint 的门槛，就算谁的），耗时测的是复制的写回，不是被测操作。
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// measure 执行一次被测操作，并发写入条件下附上它与写入的重叠情况。并发写入条件下操作在下一批写入开始的时刻发起：
// 毫秒级的查询若随意发起，多数落在两批之间，测到的就是空闲条件；对齐批的开始，每次操作都与一批写入同时开始。
func (s *session) measure(ctx context.Context, fn func() error) (timeSample, []batchSample, error) {
	if s.w != nil {
		if err := s.w.nextBatch(ctx); err != nil {
			return timeSample{}, nil, err
		}
	}
	start := time.Now()
	err := fn()
	end := time.Now()
	sample := timeSample{Ms: ms(end.Sub(start))}
	var overlapping []batchSample
	if s.w != nil {
		overlapping = s.w.overlapping(start, end)
		o := len(overlapping) > 0
		sample.Overlapped = &o
	}
	return sample, overlapping, err
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (b *bench) recordWrites(cond, op string, s *session) {
	if s.w == nil {
		return
	}
	all, elapsed := s.w.all()
	key := cond + "/" + op
	a := b.writes[key]
	if a == nil {
		a = &writeAgg{cond: cond, op: op}
		b.writes[key] = a
		b.writeOrder = append(b.writeOrder, key)
	}
	a.batches = append(a.batches, all...)
	a.elapsed += elapsed
}

func writeDist(batches []batchSample) *dist {
	if len(batches) == 0 {
		return nil
	}
	var v []float64
	for _, x := range batches {
		v = append(v, ms(x.Dur))
	}
	d := summarize(v)
	return &d
}

// rollup：时钟拨到 End + RollupLag，ceiling 恰为 End，Rollup 对两族各推进一个 5m 桶与一个 1h 桶（见 timeline）。
// 每次重复用新副本；操作之后核对水位确实到了 End、新桶的行数是节点数（探测再乘任务数），被测的那一步确实干了活。
func (b *bench) rollup(ctx context.Context, cond string) error {
	res := opResult{Condition: cond, Op: "rollup"}
	var writes []batchSample
	now := time.Unix(b.m.Config.End, 0).Add(store.RollupLag)
	for rep := range b.cfg.reps {
		s, err := b.open(ctx, cond, now, false)
		if err != nil {
			return err
		}
		sample, w, err := s.measure(ctx, func() error { return s.st.Rollup(ctx) })
		if err == nil {
			b.recordWrites(cond, "rollup", s)
		}
		if err := errors.Join(err, s.closeKeep()); err != nil {
			s.close()
			return err
		}
		detail, err := b.checkRollup(ctx, s.path)
		if err := errors.Join(err, s.removeFiles()); err != nil {
			return err
		}
		sample.Rep, sample.Detail = rep+1, detail
		res.Samples = append(res.Samples, sample)
		writes = append(writes, w...)
	}
	res.finish(writes)
	b.rep.Results = append(b.rep.Results, res)
	return nil
}

// closeKeep 关闭 Store 但保留副本，供操作之后的核对读取。
func (s *session) closeKeep() error {
	var errs []error
	if s.w != nil {
		errs = append(errs, s.w.stop())
		s.w = nil
	}
	errs = append(errs, s.st.Close())
	return errors.Join(errs...)
}

func (s *session) removeFiles() error {
	var errs []error
	for _, p := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *bench) checkRollup(ctx context.Context, path string) (map[string]any, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path, "_pragma=query_only(1)"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	end := b.m.Config.End
	nodes, tasks := int64(len(b.m.NodeIDs)), int64(len(b.m.TaskIDs))
	detail := map[string]any{}
	for _, c := range []struct {
		level, table string
		ts, rows     int64
	}{
		{"5m", "metric_5m", end - 300, nodes}, {"1h", "metric_1h", end - 3600, nodes},
		{"probe_5m", "probe_5m", end - 300, nodes * tasks}, {"probe_1h", "probe_1h", end - 3600, nodes * tasks},
	} {
		var upto, n int64
		if err := db.QueryRowContext(ctx, "SELECT upto_ts FROM rollup_state WHERE level = ?", c.level).Scan(&upto); err != nil {
			return nil, err
		}
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+c.table+" WHERE ts = ?", c.ts).Scan(&n); err != nil {
			return nil, err
		}
		if upto != end || n != c.rows {
			return nil, fmt.Errorf("after Rollup %s watermark is %d (want %d) and %s has %d rows at %d (want %d)", c.level, upto, end, c.table, n, c.ts, c.rows)
		}
		detail[c.table+"_rows_written"] = n
	}
	return detail, nil
}

// query：一份新副本上按面板的窗口与点数测单节点查询。QueryMetricsCoverage 是面板指标历史实际调用的入口
// （api 的 history.metrics），QueryMetrics 是同一读路径去掉覆盖汇总，两者都测；探测族经 QueryProbes，seriesEstimate
// 取节点的任务数（api 的 h.probes.TaskCount）。
func (b *bench) query(ctx context.Context, cond string) error {
	now := time.Unix(b.m.Now, 0)
	s, err := b.open(ctx, cond, now, false)
	if err != nil {
		return err
	}
	defer s.close()
	node := b.rep.NodeID
	to := (b.m.Now/60 + 1) * 60
	families := []string{"metrics_coverage", "metrics"}
	if len(b.m.TaskIDs) > 0 {
		families = append(families, "probes")
	}
	results := map[string]*opResult{}
	var order []string
	writes := map[string][]batchSample{}
	for rep := range b.cfg.reps {
		for _, w := range queryWindows {
			from := to - w.Span
			lv, step := store.ChooseLevel(from, to, panelMaxPoints)
			for _, fam := range families {
				key := fam + "/" + w.Name
				r := results[key]
				if r == nil {
					r = &opResult{Condition: cond, Op: "query", Family: fam, Window: w.Name, Level: lv.Name, StepS: step}
					results[key] = r
					order = append(order, key)
				}
				var points int
				sample, ws, err := s.measure(ctx, func() error {
					var err error
					switch fam {
					case "metrics_coverage":
						var rows []metric.Row
						rows, _, err = s.st.QueryMetricsCoverage(ctx, node, from, to, lv, step)
						points = len(rows)
					case "metrics":
						var rows []metric.Row
						rows, err = s.st.QueryMetrics(ctx, node, from, to, lv, step)
						points = len(rows)
					case "probes":
						var rows []metric.ProbeRow
						rows, err = s.st.QueryProbes(ctx, node, from, to, lv, step, len(b.m.TaskIDs))
						points = len(rows)
					}
					return err
				})
				if err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
				sample.Rep, sample.Detail = rep+1, map[string]any{"points": points}
				r.Samples = append(r.Samples, sample)
				writes[key] = append(writes[key], ws...)
			}
		}
	}
	b.recordWrites(cond, "query", s)
	for _, key := range order {
		r := results[key]
		r.finish(writes[key])
		b.rep.Results = append(b.rep.Results, *r)
	}
	return nil
}

// prune：时钟拨到生成时刻之后 pruneAdvance，Prune 按默认保留期删掉这段时间里过期的行。hooks 为真的重复挂计时钩子，
// 给出每个删除块的写者占用；为假的重复不挂，给出不含钩子开销的总耗时。
func (b *bench) prune(ctx context.Context, cond string, hooks bool) error {
	res := opResult{Condition: cond, Op: "prune", Hooks: hooks}
	var holds, gaps []float64
	byTable := map[string][]float64{}
	var writes []batchSample
	now := time.Unix(b.m.Now, 0).Add(b.cfg.pruneAdvance)
	for rep := range b.cfg.reps {
		s, err := b.open(ctx, cond, now, hooks)
		if err != nil {
			return err
		}
		var deleted int64
		sample, w, err := s.measure(ctx, func() error {
			var err error
			deleted, err = s.st.Prune(ctx, store.DefaultRetention)
			return err
		})
		if err == nil {
			b.recordWrites(cond, "prune", s)
		}
		spans, commits := tracker.snapshot()
		if err := errors.Join(err, s.close()); err != nil {
			return err
		}
		detail := map[string]any{"deleted_rows": deleted}
		if hooks {
			var chunks int
			for _, sp := range spans {
				if sp.Op == "delete" && (strings.HasPrefix(sp.Table, "metric_") || strings.HasPrefix(sp.Table, "probe_")) {
					holds = append(holds, ms(sp.End.Sub(sp.Start)))
					byTable[sp.Table] = append(byTable[sp.Table], ms(sp.End.Sub(sp.Start)))
					chunks++
				}
			}
			detail["chunks"] = chunks
			if cond == "idle" {
				for i := 1; i < len(commits); i++ {
					gaps = append(gaps, ms(commits[i].Sub(commits[i-1])))
				}
			}
		}
		sample.Rep, sample.Detail = rep+1, detail
		res.Samples = append(res.Samples, sample)
		writes = append(writes, w...)
	}
	res.finish(writes)
	if hooks {
		d := summarize(holds)
		res.HoldMs = &d
		res.HoldByTable = map[string]dist{}
		for t, v := range byTable {
			res.HoldByTable[t] = summarize(v)
		}
		if len(gaps) > 0 {
			g := summarize(gaps)
			res.CommitGapMs = &g
		}
	}
	b.rep.Results = append(b.rep.Results, res)
	return nil
}

func (r *opResult) finish(writes []batchSample) {
	var v []float64
	for _, s := range r.Samples {
		v = append(v, s.Ms)
	}
	r.Ms = summarize(v)
	r.WriteMs = writeDist(writes)
}

// batchWriter 以固定周期经 WriteMinuteBatch 写分钟批：每节点一行指标、每 (节点, 任务) 一行探测，行内容来自 gen 的
// 取值模型。ts 从生成时最新 1m 行之后的分钟起逐批 +60，所以每批都是新行（与生产刷出新分钟相同），且远在水位之后、
// 不被拒绝；被拒行数照样计数并报告。
type batchWriter struct {
	st     *store.Store
	m      genManifest
	period time.Duration
	r      *rand.Rand
	cols   [][]colReading
	probes [][]probeProfile
	ts     int64

	mu      sync.Mutex
	samples []batchSample
	first   chan struct{}
	// starting 在每批调用 WriteMinuteBatch 之前非阻塞地发一次信号，nextBatch 据此把被测操作对齐到批的开始。
	starting chan struct{}
	started  time.Time
	cancel   context.CancelFunc
	done     chan struct{}
}

type batchSample struct {
	Start    time.Time
	Dur      time.Duration
	Rejected int
	Err      string
}

func startWriter(ctx context.Context, st *store.Store, m genManifest, period time.Duration) *batchWriter {
	ctx, cancel := context.WithCancel(ctx)
	w := &batchWriter{st: st, m: m, period: period, r: rand.New(rand.NewPCG(m.Config.Seed, 3)), ts: m.Now - m.Now%60,
		first: make(chan struct{}), starting: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{}), started: time.Now()}
	prof := rand.New(rand.NewPCG(m.Config.Seed, 1))
	for range m.NodeIDs {
		p := newProfile(prof, m.Config.ContainerFrac)
		cols, err := p.readings()
		if err != nil {
			panic(err) // gen 已用同一模型生成过这份库，readings 在那里失败过就不会有 manifest
		}
		w.cols = append(w.cols, cols)
	}
	for range m.NodeIDs {
		var ps []probeProfile
		for range m.TaskIDs {
			ps = append(ps, newProbeProfile(prof))
		}
		w.probes = append(w.probes, ps)
	}
	go w.loop(ctx)
	return w
}

func (w *batchWriter) batch() metric.Batch {
	var b metric.Batch
	for n, id := range w.m.NodeIDs {
		b.Rows = append(b.Rows, metric.Row{NodeID: id, TS: w.ts, Bucket: newBucket(w.r, w.cols[n], bucketSamples(w.r, 1)),
			LastSeen: time.Unix(w.ts+55, 0), Observed: true, CoverageStart: w.m.CoverageStart})
		for k, task := range w.m.TaskIDs {
			b.Probes = append(b.Probes, metric.ProbeRow{NodeID: id, TS: w.ts, TaskID: task, Bucket: newProbeBucket(w.r, w.probes[n][k], 1)})
		}
	}
	return b
}

func (w *batchWriter) loop(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.period)
	defer ticker.Stop()
	for {
		batch := w.batch()
		select {
		case <-w.starting:
		default:
		}
		select {
		case w.starting <- struct{}{}:
		default:
		}
		start := time.Now()
		rejected, err := w.st.WriteMinuteBatch(context.Background(), batch)
		s := batchSample{Start: start, Dur: time.Since(start), Rejected: rejected}
		if err != nil {
			s.Err = err.Error()
		}
		w.mu.Lock()
		w.samples = append(w.samples, s)
		if len(w.samples) == 1 {
			close(w.first)
		}
		w.mu.Unlock()
		w.ts += 60
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *batchWriter) waitFirst(ctx context.Context) error {
	select {
	case <-w.first:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// nextBatch 等到下一批开始写入：先清掉已过时的信号，再等新的一次。
func (w *batchWriter) nextBatch(ctx context.Context) error {
	select {
	case <-w.starting:
	default:
	}
	select {
	case <-w.starting:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return errors.New("batch writer stopped")
	}
}

func (w *batchWriter) stop() error {
	w.cancel()
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	var errs []error
	for _, s := range w.samples {
		if s.Err != "" {
			errs = append(errs, errors.New(s.Err))
		}
	}
	return errors.Join(errs...)
}

// overlapping 返回执行区间与 [start, end] 相交的批。
func (w *batchWriter) overlapping(start, end time.Time) []batchSample {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []batchSample
	for _, s := range w.samples {
		if s.Start.Before(end) && s.Start.Add(s.Dur).After(start) {
			out = append(out, s)
		}
	}
	return out
}

func (w *batchWriter) all() ([]batchSample, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]batchSample(nil), w.samples...), time.Since(w.started)
}

func (r timeReport) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "environment: %s\n", r.Env)
	c := r.Manifest.Config
	fmt.Fprintf(&b, "database: %s (nodes=%d tasks=%d seed=%d end=%d churn=%v)\n", r.DB, c.Nodes, c.Tasks, c.Seed, c.End, c.Churn)
	fmt.Fprintf(&b, "writes condition: one batch (%d metric rows + %d probe rows) every %.3gs = %.0f× the production rate of one batch per %.0fs\n",
		r.WriteRate.RowsPerBatch, r.WriteRate.ProbeRowsPerBatch, r.WriteRate.PeriodS, r.WriteRate.Multiplier, r.WriteRate.ProductionPeriodS)
	fmt.Fprintf(&b, "query node id: %d; times in ms (median / p95 / max over n samples)\n\n", r.NodeID)
	for _, res := range r.Results {
		name := res.Op
		if res.Family != "" {
			name += " " + res.Family + " " + res.Window + fmt.Sprintf(" (level %s, step %ds)", res.Level, res.StepS)
		}
		if res.Op == "prune" {
			name += fmt.Sprintf(" (writer hooks %v)", res.Hooks)
		}
		fmt.Fprintf(&b, "%-6s %-60s %9.2f / %9.2f / %9.2f  n=%d", res.Condition, name, res.Ms.Median, res.Ms.P95, res.Ms.Max, res.Ms.N)
		if res.Condition == "writes" {
			o := 0
			for _, s := range res.Samples {
				if s.Overlapped != nil && *s.Overlapped {
					o++
				}
			}
			fmt.Fprintf(&b, "  overlapped=%d/%d", o, len(res.Samples))
		}
		fmt.Fprintln(&b)
		if len(res.Samples) > 0 && res.Samples[0].Detail != nil {
			fmt.Fprintf(&b, "         detail(rep 1): %v\n", res.Samples[0].Detail)
		}
		if res.HoldMs != nil {
			fmt.Fprintf(&b, "         per-chunk writer hold, lower bound: p50 %.3f  p99 %.3f  max %.3f  (n=%d chunks)\n", res.HoldMs.Median, res.HoldMs.P99, res.HoldMs.Max, res.HoldMs.N)
		}
		for _, t := range slices.Sorted(maps.Keys(res.HoldByTable)) {
			h := res.HoldByTable[t]
			fmt.Fprintf(&b, "           %-10s p50 %.3f  p99 %.3f  max %.3f  (n=%d)\n", t, h.Median, h.P99, h.Max, h.N)
		}
		if res.CommitGapMs != nil {
			fmt.Fprintf(&b, "         commit-to-commit gap, upper bound: p50 %.3f  p99 %.3f  max %.3f  (n=%d)\n", res.CommitGapMs.Median, res.CommitGapMs.P99, res.CommitGapMs.Max, res.CommitGapMs.N)
		}
		if res.WriteMs != nil {
			fmt.Fprintf(&b, "         overlapping batch writes: median %.2f  p95 %.2f  p99 %.2f  max %.2f  (n=%d)\n", res.WriteMs.Median, res.WriteMs.P95, res.WriteMs.P99, res.WriteMs.Max, res.WriteMs.N)
		}
	}
	for _, w := range r.Writes {
		fmt.Fprintf(&b, "writer during %-6s %-6s: %d batches at %.2f/s, rejected rows %d, errors %d, batch ms median %.2f p95 %.2f p99 %.2f max %.2f\n",
			w.Condition, w.Op, w.Batches, w.RateHz, w.Rejected, w.Errors, w.Ms.Median, w.Ms.P95, w.Ms.P99, w.Ms.Max)
	}
	fmt.Fprintln(&b, "\nraw samples are in the JSON report.")
	return b.String()
}
