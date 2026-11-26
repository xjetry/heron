package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
	"google.golang.org/protobuf/proto"
)

// logBuffer 收集一个库的日志；写协程与用例协程并发写读，所以带锁。
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func openLogged(t *testing.T, path string, clk clock.Clock) (*Store, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	s, err := Open(path, clk, slog.New(slog.NewTextHandler(logs, nil)), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	return s, logs
}

// logLine 返回第一条同时含 parts 全部片段的日志行，没有则为空串。
func logLine(logs string, parts ...string) string {
	for line := range strings.Lines(logs) {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			return line
		}
	}
	return ""
}

// logDuration 解析一行文本日志里 key=<time.Duration> 的值；缺这个键即失败。
func logDuration(t *testing.T, line, key string) time.Duration {
	t.Helper()
	for field := range strings.FieldsSeq(line) {
		if v, ok := strings.CutPrefix(field, key+"="); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				t.Fatalf("%s=%q in %q: %v", key, v, line, err)
			}
			return d
		}
	}
	t.Fatalf("no %s in log line %q", key, line)
	return 0
}

// number 把驱动读回的数值列统一成 float64：SQLite 按存储类返回 int64 或 float64，比较只关心数值。
func number(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	panic(fmt.Sprintf("not a number: %T %v", v, v))
}

// clockTask 是这组用例里探测行所属的任务。
const clockTask = 7

// minuteOf 造一分钟的两族行：指标 cpu = v，探测一次往返 v 微秒。
func minuteOf(id, ts int64, v float64) metric.Batch {
	b := metric.NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(v)})
	return metric.Batch{
		Rows:   []metric.Row{{NodeID: id, TS: ts, CoverageStart: ts, Bucket: b}},
		Probes: []metric.ProbeRow{probeRow(id, ts, clockTask, []uint32{uint32(v)}, 0, 0)},
	}
}

// runMinutes 按 hub 的节奏跑 n 分钟：每一分钟闭合后半秒刷出它的行（ingest 的刷出时刻），边界后 2 秒跑一轮上卷
// （nextMaintenanceAt）。时钟取的是墙钟，所以 from 就是这段运行时墙钟的读数。返回写入口拒绝的行数之和。
func runMinutes(t *testing.T, s *Store, clk *clock.Fake, id, from int64, n int, v float64) int {
	t.Helper()
	ctx := t.Context()
	rejected := 0
	for i := range int64(n) {
		ts := from + i*60
		clk.SetWall(time.Unix(ts+60, 500_000_000))
		r, err := s.WriteMinuteBatch(ctx, minuteOf(id, ts, v))
		if err != nil {
			t.Fatal(err)
		}
		rejected += r
		clk.SetWall(time.Unix(ts+62, 0))
		if err := s.Rollup(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return rejected
}

// familyState 是一个库在两族三级上的全部行与粗级水位，用来逐列比较两个库。
type familyState struct {
	rows       map[string][][]any
	watermarks map[string]int64
}

// readLevelRows 按原始列值读 f 在第 i 级的行（键列在前、按键排序）。指标族 1m 表的覆盖列是 reported、observed，
// 粗级是 minutes、observed、both，其余列两级相同。
func readLevelRows(t *testing.T, s *Store, f *family, i int, nodeID int64) [][]any {
	t.Helper()
	cols := append(f.keys(), f.values()...)
	if f == metricFamily && i == 0 {
		cols = append(f.keys(), append(metricColumnNames(), "reported", "observed")...)
	}
	rows, err := s.r.Query("SELECT "+strings.Join(cols, ", ")+" FROM "+f.tables[i]+" WHERE node_id=? ORDER BY "+strings.Join(f.keys(), ", "), nodeID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for j := range dest {
			dest[j] = &values[j]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		out = append(out, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func readFamilyState(t *testing.T, s *Store, id int64) familyState {
	t.Helper()
	out := familyState{rows: map[string][][]any{}, watermarks: map[string]int64{}}
	for _, f := range families {
		for i := range levels {
			out.rows[f.tables[i]] = readLevelRows(t, s, f, i, id)
			if i > 0 {
				out.watermarks[f.states[i]] = watermark(t, s, f.states[i])
			}
		}
	}
	return out
}

// 墙钟开机时快 8 小时：快钟下的上卷把两族的水位推到真实时刻之后。校回之后，修复前真实时刻的分钟行全部早于水位、
// 一律被拒（三级都没有这段数据），快钟打点的行留在未来、在那一分钟到来时与真实数据相加。修复之后：校回后两小时的
// 分钟行全部被接受，快钟打点的行被删除，各级结果与墙钟一直正确的对照库逐行一致，粗级等于逐分钟求和。
func TestFastClockRollupThenCorrectionKeepsMinutesAndRebuildsLevels(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	fastAt := t0 + 8*3600
	run := func(t *testing.T, fast bool) (familyState, string) {
		clk := clock.NewFake(time.Unix(t0-3600, 0))
		s, logs := openLogged(t, filepath.Join(t.TempDir(), "t.db"), clk)
		t.Cleanup(func() { s.Close() })
		seedProbeTasks(t, s, clockTask)
		id, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
		if err != nil {
			t.Fatal(err)
		}
		if n := runMinutes(t, s, clk, id, t0-3600, 60, 1); n != 0 {
			t.Fatalf("history before the clock fault: %d rows rejected", n)
		}
		if fast {
			// 快钟下的数值与真实分钟不同：若被并进真实分钟或粗级桶，求和对不上。
			if n := runMinutes(t, s, clk, id, fastAt, 10, 1000); n != 0 {
				t.Fatalf("rows under the fast clock: %d rejected", n)
			}
			for _, level := range []string{"5m", "probe_5m"} {
				if got := watermark(t, s, level); got <= t0 {
					t.Fatalf("setup: %s watermark %d did not move past the real time %d under the fast clock", level, got, t0)
				}
			}
		}
		if n := runMinutes(t, s, clk, id, t0, 120, 1); n != 0 {
			t.Fatalf("fast=%v: %d of the minute rows after the correction were rejected", fast, n)
		}
		return readFamilyState(t, s, id), logs.String()
	}
	control, controlLogs := run(t, false)
	got, logs := run(t, true)

	if !reflect.DeepEqual(got, control) {
		for table, rows := range control.rows {
			if !reflect.DeepEqual(got.rows[table], rows) {
				t.Errorf("%s differs from the control:\n got %v\nwant %v", table, got.rows[table], rows)
			}
		}
		t.Fatalf("watermarks %v, control %v", got.watermarks, control.watermarks)
	}
	// 粗级等于逐分钟求和：真实分钟每分钟 cpu = 1、往返 1 微秒，快钟的 1000 一点都不能混进来。
	for _, f := range families {
		for i := 1; i < len(levels); i++ {
			upto := got.watermarks[f.states[i]]
			minutes := int64(levels[i].Bucket / 60)
			want := (upto - (t0 - 3600)) / levels[i].Bucket
			if n := int64(len(got.rows[f.tables[i]])); n != want {
				t.Fatalf("%s has %d rows below watermark %d, want %d", f.tables[i], n, upto, want)
			}
			for _, row := range got.rows[f.tables[i]] {
				ts := row[1].(int64)
				if ts >= upto {
					t.Fatalf("%s row at %d is not below the watermark %d", f.tables[i], ts, upto)
				}
				// 指标族值列从 cpu_sum、cpu_n 起；探测族是 sent、lost、errors、rtt_sum_us。
				sum, n := number(row[2]), number(row[3])
				if f.extraKey != "" {
					n, sum = number(row[3]), number(row[6])
				}
				if sum != float64(minutes) || n != float64(minutes) {
					t.Fatalf("%s at %d: sum %v over %v, want %d over %d", f.tables[i], ts, sum, n, minutes, minutes)
				}
			}
		}
		for _, row := range got.rows[f.tables[0]] {
			if ts := row[1].(int64); ts >= fastAt {
				t.Fatalf("%s still holds the fast-clock minute %d", f.tables[0], ts)
			}
		}
	}
	// 每个粗级一条回退日志，带回退幅度：快钟推进到的水位减去按校回后的时刻算出的上界，至少是快出的 8 小时。
	for _, level := range []string{"5m", "1h", "probe_5m", "probe_1h"} {
		line := logLine(logs, "rollup watermark was ahead of the wall clock", "rollup_level="+level+" ")
		if d := logDuration(t, line, "rolled_back"); d < 8*time.Hour {
			t.Fatalf("rollback of %s logged as %v, want at least 8h:\n%s", level, d, logs)
		}
	}
	for _, table := range []string{"metric_1m", "probe_1m"} {
		line := logLine(logs, "minute rows ahead of the wall clock deleted", "table="+table+" ")
		if d := logDuration(t, line, "ahead"); d < 8*time.Hour {
			t.Fatalf("deletion from %s logged as %v ahead, want at least 8h:\n%s", table, d, logs)
		}
	}
	if strings.Contains(controlLogs, "ahead of the wall clock") {
		t.Fatalf("control with a correct clock repaired something:\n%s", controlLogs)
	}
}

// 检查只读库里的状态，不依赖进程内的任何记忆：水位超前的库换一个时钟正确的进程重新打开，第一次写入或第一次上卷
// 就把水位退回、删掉快钟打点的分钟行。
func TestWatermarkAheadIsRolledBackAfterReopen(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	for _, first := range []string{"write", "rollup"} {
		t.Run(first, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "t.db")
			fastClk := clock.NewFake(time.Unix(t0+8*3600, 0))
			fast, _ := openLogged(t, path, fastClk)
			seedProbeTasks(t, fast, clockTask)
			id, _, err := fast.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
			if err != nil {
				t.Fatal(err)
			}
			runMinutes(t, fast, fastClk, id, t0+8*3600, 10, 1000)
			if err := fast.Close(); err != nil {
				t.Fatal(err)
			}

			clk := clock.NewFake(time.Unix(t0+60, 500_000_000))
			s, logs := openLogged(t, path, clk)
			t.Cleanup(func() { s.Close() })
			switch first {
			case "write":
				if n, err := s.WriteMinuteBatch(t.Context(), minuteOf(id, t0, 1)); err != nil || n != 0 {
					t.Fatalf("first write after reopening: rejected=%d err=%v, want 0", n, err)
				}
			case "rollup":
				if err := s.Rollup(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			ceiling := clk.Now().Add(-RollupLag).Unix()
			for _, f := range families {
				for i := 1; i < len(levels); i++ {
					if got, want := watermark(t, s, f.states[i]), alignDown(ceiling, levels[i].Bucket); got != want {
						t.Fatalf("%s watermark = %d after the first %s, want %d", f.states[i], got, first, want)
					}
				}
				var ahead int
				if err := s.r.QueryRow("SELECT count(*) FROM "+f.tables[0]+" WHERE ts >= ?", t0+8*3600).Scan(&ahead); err != nil || ahead != 0 {
					t.Fatalf("%s keeps %d fast-clock rows (err %v)", f.tables[0], ahead, err)
				}
			}
			if line := logLine(logs.String(), "rollup watermark was ahead of the wall clock", "rollup_level=5m "); logDuration(t, line, "rolled_back") < 8*time.Hour {
				t.Fatalf("rollback not logged with its size:\n%s", logs)
			}
		})
	}
}

// 写入口的上端与时钟超前检查取同一个界（metric.LatestMinute）：当前分钟与其后一分钟被接受，再晚的两族行都被拒并计数；
// 接受的行不会被同一时刻的检查删掉。墙钟随后往回拨一分钟，原来在容差里的那一行就超前了，下一次写入删掉它，那一分钟
// 真正到来时写入的是一行新的，不与它相加——这一段水位并没有超前，靠的是对 1m 行的独立检查。
func TestWriteMinuteBatchRejectsRowsAfterTheCurrentMinute(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	seedProbeTasks(t, s, clockTask)
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	now := clk.Now().Truncate(time.Hour).Unix()
	clk.SetWall(time.Unix(now+30, 0))
	var batch metric.Batch
	for _, ts := range []int64{now, now + 60, now + 120} {
		m := minuteOf(id, ts, 5)
		batch.Rows = append(batch.Rows, m.Rows...)
		batch.Probes = append(batch.Probes, m.Probes...)
	}
	if n, err := s.WriteMinuteBatch(ctx, batch); err != nil || n != 2 {
		t.Fatalf("rejected=%d err=%v, want the metric and the probe row two minutes ahead rejected", n, err)
	}
	minutes := func() map[string][]int64 {
		out := map[string][]int64{}
		for _, f := range families {
			for _, row := range readLevelRows(t, s, f, 0, id) {
				out[f.tables[0]] = append(out[f.tables[0]], row[1].(int64))
			}
		}
		return out
	}
	want := map[string][]int64{"metric_1m": {now, now + 60}, "probe_1m": {now, now + 60}}
	if got := minutes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("1m rows = %v, want %v", got, want)
	}
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{}); err != nil {
		t.Fatal(err)
	}
	if got := minutes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the check at the same instant deleted a row the write path accepted: %v, want %v", got, want)
	}

	clk.SetWall(time.Unix(now-30, 0))
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{}); err != nil {
		t.Fatal(err)
	}
	want = map[string][]int64{"metric_1m": {now}, "probe_1m": {now}}
	if got := minutes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the clock stepped back a minute: 1m rows = %v, want %v", got, want)
	}
	if got := watermark(t, s, "5m"); got != 0 {
		t.Fatalf("setup: 5m watermark = %d, want it untouched", got)
	}
	clk.SetWall(time.Unix(now+120, 500_000_000))
	if n, err := s.WriteMinuteBatch(ctx, minuteOf(id, now+60, 7)); err != nil || n != 0 {
		t.Fatalf("rejected=%d err=%v", n, err)
	}
	rows, err := s.ReadMinuteRows(ctx, id, now+60, now+120)
	if err != nil || len(rows) != 1 || rows[0].Bucket.N[0] != 1 || rows[0].Bucket.Sum[0] != 7 {
		t.Fatalf("minute %d = %+v err=%v, want only the row written when it arrived", now+60, rows, err)
	}
}

// 时钟超前检查在每次写入与上卷里都跑：它的读与删都由节点集合驱动主键 (node_id, ts …) 的查找，不扫描整张表。
func TestClockAheadCheckSeeksPrimaryKeys(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for _, f := range families {
		for i, table := range f.tables {
			if i == 0 {
				if plan := strings.Join(queryPlan(t, s, newestRowSQL(table)), "\n"); !strings.Contains(plan, "SEARCH "+table+" USING PRIMARY KEY (node_id=?)") {
					t.Errorf("%s newest-row read does not seek by node:\n%s", table, plan)
				}
			}
			for _, op := range []string{">", ">="} {
				if plan := strings.Join(queryPlan(t, s, deleteFromSQL(table, op), 0), "\n"); !strings.Contains(plan, "SEARCH "+table+" USING PRIMARY KEY (node_id=? AND ts>?)") {
					t.Errorf("%s delete ts %s ? does not seek node and time range:\n%s", table, op, plan)
				}
			}
		}
	}
}

// 检查自身在正常运行时是空操作：水位不超前、没有超前的 1m 行时，不改任何行也不记日志。检查放在上一轮上卷之后
// 10 分钟：水位落后于上界是两轮上卷之间的常态，检查只往回退，不能替上卷把水位往前推。
func TestClockAheadCheckIsANoOpWhenNothingIsAhead(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, logs := openLogged(t, filepath.Join(t.TempDir(), "t.db"), clk)
	t.Cleanup(func() { s.Close() })
	seedProbeTasks(t, s, clockTask)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	runMinutes(t, s, clk, id, clk.Now().Unix(), 30, 1)
	clk.Advance(10 * time.Minute)
	before := readFamilyState(t, s, id)
	if err := s.write(context.Background(), func(tx *sql.Tx) error { return s.repairClockAhead(tx, clk.Now()) }); err != nil {
		t.Fatal(err)
	}
	if after := readFamilyState(t, s, id); !reflect.DeepEqual(after, before) {
		t.Fatalf("check changed a consistent database:\n before %v\n after %v", before, after)
	}
	if strings.Contains(logs.String(), "ahead of the wall clock") {
		t.Fatalf("check logged a repair on a consistent database:\n%s", logs)
	}
}
