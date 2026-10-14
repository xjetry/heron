package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
)

// 测试用的库：节点与任务取最小的非平凡规模，时间轴与保留期是默认值，所以每节点的行数与 N=500 时相同。
var testConfig = genConfig{Nodes: 2, Tasks: 1, End: mustUnix(defaultEnd), Seed: 7, ContainerFrac: 0.5, Churn: true}

func mustUnix(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.Unix()
}

// sharedDB 只生成一次，供只读的测试共用；需要改库的测试（time 的冒烟）在副本上做。
var sharedDB struct {
	once sync.Once
	path string
	m    genManifest
	err  error
	dir  string
}

func generated(t *testing.T) (string, genManifest) {
	t.Helper()
	sharedDB.once.Do(func() {
		sharedDB.dir, sharedDB.err = os.MkdirTemp("", "storebench-test-")
		if sharedDB.err != nil {
			return
		}
		sharedDB.path = filepath.Join(sharedDB.dir, "a.db")
		sharedDB.m, sharedDB.err = gen(context.Background(), sharedDB.path, testConfig)
	})
	if sharedDB.err != nil {
		t.Fatal(sharedDB.err)
	}
	return sharedDB.path, sharedDB.m
}

func TestMain(m *testing.M) {
	code := m.Run()
	if sharedDB.dir != "" {
		os.RemoveAll(sharedDB.dir)
	}
	os.Exit(code)
}

// 同一 genConfig 两次生成的库文件逐字节相同，且生成结束时没有 -wal：time 只复制主文件，-wal 里的内容会丢。
func TestGenIsDeterministic(t *testing.T) {
	a, _ := generated(t)
	b := filepath.Join(t.TempDir(), "b.db")
	if _, err := gen(context.Background(), b, testConfig); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p + "-wal"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s-wal after gen: %v; the generated file must be self-contained", p, err)
		}
	}
	ra, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ra, rb) {
		n := 0
		for n < min(len(ra), len(rb)) && ra[n] == rb[n] {
			n++
		}
		t.Fatalf("two generations differ: sizes %d and %d, first differing byte at offset %d (page %d)", len(ra), len(rb), n, n/4096+1)
	}
}

// gen 只写到空路径：已有的库不被覆盖。
func TestGenRefusesExistingPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.db")
	if err := os.WriteFile(p, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gen(context.Background(), p, testConfig); err == nil {
		t.Fatal("gen overwrote an existing file")
	}
	if raw, _ := os.ReadFile(p); string(raw) != "keep" {
		t.Fatalf("existing file changed to %q", raw)
	}
}

// 期望值由保留期与时间轴的定义直接推出，不读 manifest：生成器把某一项算错时，两边不会一起错。
//   - 墙钟 now = End + 242（最后一个分钟边界 End + 240 之后的维护时刻）。
//   - 最老行是各级 Prune 的截止点 alignDown(now − 保留期, 桶长)；5m 水位 alignDown(now − RollupLag, 300) = End − 300，
//     1h 水位 alignDown(now − RollupLag, 3600) = End − 3600；最新 1m 行是 End + 180。
//   - 覆盖起点是节点首报的分钟：重放从 1h 级的最老行再往前一个保留期开始。
type expectation struct {
	now, newest1m   int64
	oldest, wm      [3]int64
	coverageStart   int64
	rowsPerNode     [3]int64
	nodes, tasks    int64
	queryTo         int64
	retentionSecond [3]int64
}

func expect(cfg genConfig) expectation {
	var e expectation
	e.now = cfg.End + 242
	e.newest1m = cfg.End + 180
	ret := [3]time.Duration{store.DefaultRetention.M1, store.DefaultRetention.M5, store.DefaultRetention.H1}
	buckets := [3]int64{60, 300, 3600}
	lag := int64(store.RollupLag / time.Second)
	e.wm = [3]int64{0, alignDown(e.now-lag, 300), alignDown(e.now-lag, 3600)}
	newest := [3]int64{e.newest1m, e.wm[1] - 300, e.wm[2] - 3600}
	for i := range 3 {
		e.retentionSecond[i] = int64(ret[i] / time.Second)
		e.oldest[i] = alignDown(e.now-e.retentionSecond[i], buckets[i])
		e.rowsPerNode[i] = (newest[i]-e.oldest[i])/buckets[i] + 1
	}
	e.coverageStart = e.oldest[2] - e.retentionSecond[2]
	e.nodes, e.tasks = int64(cfg.Nodes), int64(cfg.Tasks)
	e.queryTo = (e.now/60 + 1) * 60
	return e
}

func TestExpectationMatchesDesignScale(t *testing.T) {
	// 保留期估算按每节点 1m 10080 行、5m 8640 行、1h 8760 行计：保留期内每桶一行。实际留存比它各少 0、1、1 行
	// （5m、1h 的最新桶在水位处尚未上卷）。这条把期望值的推导钉在可以手算的数上。
	e := expect(testConfig)
	if e.rowsPerNode != [3]int64{10080, 8639, 8759} {
		t.Fatalf("rows per node %v, want [10080 8639 8759]", e.rowsPerNode)
	}
}

// 用 store 自己的读接口回读生成的库：存储统计的最老桶、水位与行数，覆盖起点，以及四个窗口的指标与探测查询。
func TestGeneratedDatabaseReadsBackThroughStore(t *testing.T) {
	path, _ := generated(t)
	e := expect(testConfig)
	ctx := context.Background()
	copyPath := filepath.Join(t.TempDir(), "readback.db")
	if err := copyFile(path, copyPath); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(copyPath, clock.NewFake(time.Unix(e.now, 0)), errorLogger(), store.RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stats, err := st.StorageStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]int64{}
	for _, tr := range stats.Tables {
		rows[tr.Name] = tr.Rows
	}
	for i, name := range metricTables {
		if want := e.nodes * e.rowsPerNode[i]; rows[name] != want {
			t.Errorf("%s has %d rows, want %d", name, rows[name], want)
		}
		if want := e.nodes * e.tasks * e.rowsPerNode[i]; rows[probeTables[i]] != want {
			t.Errorf("%s has %d rows, want %d", probeTables[i], rows[probeTables[i]], want)
		}
	}
	if rows["node"] != e.nodes || rows["probe_task"] != e.tasks || rows["probe_task_node"] != e.nodes*e.tasks || rows["node_coverage"] != e.nodes {
		t.Errorf("subjects: node=%d probe_task=%d probe_task_node=%d node_coverage=%d, want %d, %d, %d, %d",
			rows["node"], rows["probe_task"], rows["probe_task_node"], rows["node_coverage"], e.nodes, e.tasks, e.nodes*e.tasks, e.nodes)
	}
	if len(stats.Series) != 6 {
		t.Fatalf("%d series, want 6", len(stats.Series))
	}
	for _, s := range stats.Series {
		i := levelIndexOf(t, s.Level.Name)
		if s.Oldest == nil || *s.Oldest != e.oldest[i] {
			t.Errorf("%s oldest = %v, want %d", s.Table, deref(s.Oldest), e.oldest[i])
		}
		if i == 0 {
			if s.Watermark != nil {
				t.Errorf("%s has a watermark %d; the finest level has none", s.Table, *s.Watermark)
			}
			continue
		}
		if s.Watermark == nil || *s.Watermark != e.wm[i] {
			t.Errorf("%s watermark = %v, want %d", s.Table, deref(s.Watermark), e.wm[i])
		}
		// 存储健康的判定与面板同一份：生成的库应当是健康的（最老行没有超期、水位没有落后）。
		if st := s.Staleness(time.Unix(e.now, 0), store.DefaultRetention); st.Oldest || st.Watermark {
			t.Errorf("%s reads as stale: %+v", s.Table, st)
		}
	}
	if stats.LastPrune == nil || *stats.LastPrune != e.now || stats.LastRollup == nil || *stats.LastRollup != e.now {
		t.Errorf("maintenance completions prune=%v rollup=%v, want %d", deref(stats.LastPrune), deref(stats.LastRollup), e.now)
	}

	node := int64(1)
	for _, w := range queryWindows {
		from := e.queryTo - w.Span
		lv, step := store.ChooseLevel(from, e.queryTo, panelMaxPoints)
		got, summary, err := st.QueryMetricsCoverage(ctx, node, from, e.queryTo, lv, step)
		if err != nil {
			t.Fatalf("%s: %v", w.Name, err)
		}
		// 历史从最老留存行到最新 1m 行连续、按水位拼接后每分钟恰被一级覆盖一次：窗口内被覆盖的分钟数是
		// [max(窗口起点, 最老的 1h 行), 最新 1m 行 + 60) 的分钟数，桶数是这段跨越的步长桶数，中间的桶各覆盖整整一个步长。
		fromAligned := alignDown(from, step)
		first := max(fromAligned, e.oldest[2])
		wantMinutes := (e.newest1m + 60 - first) / 60
		wantPoints := (alignDown(e.newest1m, step)-alignDown(first, step))/step + 1
		if int64(len(got)) != wantPoints {
			t.Errorf("%s (level %s step %d): %d points, want %d", w.Name, lv.Name, step, len(got), wantPoints)
		}
		var minutes int64
		for k, r := range got {
			if r.Coverage.Minutes == nil {
				t.Fatalf("%s: point %d has unknown coverage", w.Name, r.TS)
			}
			minutes += int64(*r.Coverage.Minutes)
			if k > 0 && k < len(got)-1 && int64(*r.Coverage.Minutes) != step/60 {
				t.Errorf("%s: interior point %d covers %d minutes, want %d", w.Name, r.TS, *r.Coverage.Minutes, step/60)
			}
		}
		if minutes != wantMinutes {
			t.Errorf("%s: points cover %d minutes, want %d", w.Name, minutes, wantMinutes)
		}
		if summary.CoverageStart == nil || *summary.CoverageStart != e.coverageStart {
			t.Errorf("%s: coverage start %v, want %d", w.Name, deref(summary.CoverageStart), e.coverageStart)
		}
		nowMinute := alignDown(e.now, 60)
		if want := uint64((min(alignDown(e.queryTo, 60), nowMinute) - max(from, e.coverageStart)) / 60); summary.EligibleMinutes != want {
			t.Errorf("%s: eligible minutes %d, want %d", w.Name, summary.EligibleMinutes, want)
		}

		probes, err := st.QueryProbes(ctx, node, from, e.queryTo, lv, step, int(e.tasks))
		if err != nil {
			t.Fatalf("%s probes: %v", w.Name, err)
		}
		// 每个任务一条序列、点数与指标相同；默认任务一分钟探测一次，所以每条序列的 sent 之和等于覆盖的分钟数。
		perTask := map[uint64][2]int64{}
		for _, p := range probes {
			c := perTask[p.TaskID]
			perTask[p.TaskID] = [2]int64{c[0] + 1, c[1] + int64(p.Bucket.Sent)}
		}
		if int64(len(perTask)) != e.tasks {
			t.Errorf("%s probes: %d task series, want %d", w.Name, len(perTask), e.tasks)
		}
		for task, c := range perTask {
			if c[0] != wantPoints || c[1] != wantMinutes {
				t.Errorf("%s probes task %d: %d points and %d probes sent, want %d and %d", w.Name, task, c[0], c[1], wantPoints, wantMinutes)
			}
		}
	}
}

func levelIndexOf(t *testing.T, name string) int {
	t.Helper()
	switch name {
	case "1m":
		return 0
	case "5m":
		return 1
	case "1h":
		return 2
	}
	t.Fatalf("unknown level %q", name)
	return -1
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
