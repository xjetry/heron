package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"google.golang.org/protobuf/proto"
	"modernc.org/sqlite"
)

func historyMinutes(id, base int64, minutes int) metric.Batch {
	var batch metric.Batch
	for i := range minutes {
		b := metric.NewBucket()
		for j := 0; j <= i%3; j++ {
			b.Add(&heronv1.Metrics{CpuPct: proto.Float64(float64(i + j)), MemUsed: proto.Uint64(uint64(i * 10))})
			b.AddSum(metric.RxBytes, float64(i+j+1))
		}
		ts := base + int64(i)*60
		batch.Rows = append(batch.Rows, metric.Row{NodeID: id, TS: ts, CoverageStart: base, Bucket: b})
		var rtts []uint32
		for j := 0; j < i%4; j++ {
			rtts = append(rtts, uint32(i*100+j))
		}
		batch.Probes = append(batch.Probes, probeRow(id, ts, 7, rtts, uint32(i%2), 1), probeRow(id, ts, 9, nil, 1, 0))
	}
	return batch
}

// 真值只从原始分钟在内存中合并，不调用查询 SQL 或上卷 SQL。
func assertHistoryMinutes(t *testing.T, s *Store, f *family, batch metric.Batch, from, to int64, lv Level, step int64) {
	t.Helper()
	id := batch.Rows[0].NodeID
	if f == metricFamily {
		want := map[int64]*metric.Bucket{}
		for _, r := range batch.Rows {
			ts := r.TS / step * step
			if ts < from/step*step || ts > (to-1)/step*step {
				continue
			}
			if want[ts] == nil {
				want[ts] = metric.NewBucket()
			}
			want[ts].Merge(r.Bucket)
		}
		got, err := s.QueryMetrics(t.Context(), id, from, to, lv, step)
		if err != nil || len(got) != len(want) {
			t.Fatalf("metric continuity: rows=%d want=%d err=%v", len(got), len(want), err)
		}
		for _, r := range got {
			if !reflect.DeepEqual(r.Bucket, want[r.TS]) {
				t.Fatalf("metric minute truth at %d: got=%+v want=%+v", r.TS, r.Bucket, want[r.TS])
			}
			delete(want, r.TS)
		}
		return
	}
	type key struct {
		task uint64
		ts   int64
	}
	want := map[key]*metric.ProbeBucket{}
	for _, r := range batch.Probes {
		ts := r.TS / step * step
		if ts < from/step*step || ts > (to-1)/step*step {
			continue
		}
		k := key{r.TaskID, ts}
		if want[k] == nil {
			want[k] = &metric.ProbeBucket{}
		}
		want[k].Merge(r.Bucket)
	}
	got, err := s.QueryProbes(t.Context(), id, from, to, lv, step, 0)
	if err != nil || len(got) != len(want) {
		t.Fatalf("probe continuity: rows=%d want=%d err=%v", len(got), len(want), err)
	}
	for _, r := range got {
		k := key{r.TaskID, r.TS}
		if !reflect.DeepEqual(r.Bucket, want[k]) {
			t.Fatalf("probe minute truth at %+v: got=%+v want=%+v", k, r.Bucket, want[k])
		}
		delete(want, k)
	}
}

func TestHistoryTailMatchesMinuteTruth(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	seedProbeTasks(t, s, 7, 9)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "history"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	base := clk.Now().Truncate(6 * time.Hour).Unix()
	batch := historyMinutes(id, base, 197)
	if _, err := s.WriteMinuteBatch(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	for i, f := range families {
		five := base + int64(90+i*60)*60
		hour := base + int64(60+i*60)*60
		if _, err := s.rollupLevel(t.Context(), f, 1, five); err != nil {
			t.Fatal(err)
		}
		if _, err := s.rollupLevel(t.Context(), f, 2, hour); err != nil {
			t.Fatal(err)
		}
		// 留一行已消费的分钟，既覆盖重叠风险，也保证中段不能靠回退到全部 1m 蒙混通过。
		if _, err := s.deleteRange(t.Context(), f.tables[0], id, base, five-60); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			lv   Level
			step int64
		}{{levels[1], 300}, {levels[1], 1200}, {levels[2], 3600}, {levels[2], 10800}} {
			t.Run(fmt.Sprintf("%s/%s/%d", f.name, tc.lv.Name, tc.step), func(t *testing.T) {
				assertHistoryMinutes(t, s, f, batch, base+30, base+197*60, tc.lv, tc.step)
			})
		}
	}
}

// 在真实 SQLite 驱动关闭水位查询后同步提交上卷和清理，不依赖 goroutine 调度。
type historyReadDriver struct{ afterRead func() error }
type historyReadConn struct {
	driver.Conn
	afterRead func() error
}

func (d historyReadDriver) Open(name string) (driver.Conn, error) {
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &historyReadConn{Conn: c, afterRead: d.afterRead}, nil
}
func (c *historyReadConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *historyReadConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err == nil && strings.HasPrefix(q, "SELECT upto_ts FROM rollup_state") {
		return &snapshotRows{Rows: rows, afterVersion: c.afterRead}, nil
	}
	return rows, err
}

func TestHistoryTailReadsOneSnapshot(t *testing.T) {
	t.Parallel()
	for _, f := range families {
		for _, lv := range levels[1:] {
			t.Run(f.name+"/"+lv.Name, func(t *testing.T) {
				t.Parallel()
				s, clk := open(t)
				seedProbeTasks(t, s, 7, 9)
				id, _, err := s.CreateNode(t.Context(), NewNode{Name: "snapshot"}, hash(1))
				if err != nil {
					t.Fatal(err)
				}
				base := clk.Now().Truncate(6 * time.Hour).Unix()
				batch := historyMinutes(id, base, 197)
				if _, err := s.WriteMinuteBatch(t.Context(), batch); err != nil {
					t.Fatal(err)
				}
				if _, err := s.rollupLevel(t.Context(), f, 1, base+90*60); err != nil {
					t.Fatal(err)
				}
				if _, err := s.rollupLevel(t.Context(), f, 2, base+60*60); err != nil {
					t.Fatal(err)
				}
				var once sync.Once
				var advanceErr error
				advanced := false
				driverName := fmt.Sprintf("history-snapshot-%d", traceID.Add(1))
				sql.Register(driverName, historyReadDriver{afterRead: func() error {
					once.Do(func() {
						advanceErr = s.write(t.Context(), func(tx *sql.Tx) error {
							for i := 1; i < len(levels); i++ {
								if _, err := tx.Exec(f.rollupSQL(i), base, base+180*60); err != nil {
									return err
								}
								if _, err := tx.Exec("UPDATE rollup_state SET upto_ts = ? WHERE level = ?", base+180*60, f.states[i]); err != nil {
									return err
								}
								if _, err := tx.Exec("DELETE FROM "+f.tables[i-1]+" WHERE ts < ?", base+180*60); err != nil {
									return err
								}
							}
							return nil
						})
						advanced = advanceErr == nil
					})
					return advanceErr
				}})
				reopenReadPools(t, s, driverName)
				assertHistoryMinutes(t, s, f, batch, base, base+197*60, lv, 3*lv.Bucket)
				if !advanced {
					t.Fatal("concurrent rollup and prune did not run")
				}
				if got := watermark(t, s, f.states[2]); got != base+180*60 {
					t.Fatalf("committed watermark=%d", got)
				}
			})
		}
	}
}
