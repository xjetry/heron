package store

import (
	"context"
	"database/sql"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/testwait"
)

type logWriterFunc func([]byte) (int, error)

func (f logWriterFunc) Write(b []byte) (int, error) { return f(b) }

func TestMaintenanceAfterRollupErrorKeepsSeriesButPrunesEvents(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk.SetWall(clk.Now().Add(59*time.Second + 999*time.Millisecond))
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	r := saveRule(t, s, AlertRule{Name: "offline", Kind: KindOffline, AllNodes: true})
	if _, err := s.RecordTransition(ctx, r.ID, id, StateFiring, "", time.Time{}, AlertEvent{At: clk.Now().Add(-91 * 24 * time.Hour), Transition: TransitionFiring}, nil); err != nil {
		t.Fatal(err)
	}
	ts := clk.Now().Add(-400 * 24 * time.Hour).Truncate(time.Hour).Unix()
	if err := s.write(ctx, func(tx *sql.Tx) error {
		args := append([]any{id, ts}, bucketArgs(bucket(3))...)
		if _, err := tx.Exec(insertMetricFixture("metric_1h"), args...); err != nil {
			return err
		}
		_, err := tx.Exec("CREATE TRIGGER block_rollup BEFORE UPDATE ON rollup_state BEGIN SELECT RAISE(ABORT, 'rollup blocked'); END")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s.log = slog.New(slog.NewTextHandler(logWriterFunc(func(b []byte) (int, error) {
		if strings.Contains(string(b), "rollup failed") {
			cancel()
		}
		return len(b), nil
	}), nil))
	done := make(chan struct{})
	go func() { s.RunMaintenance(ctx, DefaultRetention); close(done) }()
	select {
	case <-done:
	case <-time.After(testwait.Bound):
		cancel()
		<-done
		t.Fatal("maintenance did not report rollup failure")
	}
	if len(readLevel(t, s, levels[2], id)) != 1 {
		t.Fatal("maintenance pruned after rollup failure")
	}
	assertAlertRows(t, s, "alert_event", "1 = 1", 0)
}

func TestPruneWaitsForConsumer(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	ts := clk.Now().Add(-8 * 24 * time.Hour).Truncate(time.Hour).Unix()
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: minuteRows(id, ts, ts+60)}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Prune(ctx, DefaultRetention); err != nil || n != 0 || len(readLevel(t, s, levels[0], id)) != 1 {
		t.Fatalf("unconsumed 1m row deleted: n=%d err=%v", n, err)
	}
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Prune(ctx, DefaultRetention); err != nil || n != 1 || len(readLevel(t, s, levels[0], id)) != 0 {
		t.Fatalf("consumed 1m row retained: n=%d err=%v", n, err)
	}
	// 5m 也必须等待自己的消费者；不把 5m 水位误用作 1h 消费凭据。
	if err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE rollup_state SET upto_ts = 0 WHERE level = '1h'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := Retention{M1: 6 * time.Hour, M5: 7 * 24 * time.Hour, H1: 365 * 24 * time.Hour}
	if n, err := s.Prune(ctx, r); err != nil || n != 0 || len(readLevel(t, s, levels[1], id)) != 1 {
		t.Fatalf("unconsumed 5m row deleted: n=%d err=%v", n, err)
	}
	if err := s.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Prune(ctx, r); err != nil || n != 1 || len(readLevel(t, s, levels[1], id)) != 0 {
		t.Fatalf("consumed 5m row retained: n=%d err=%v", n, err)
	}
}

func TestRollupUsesPrimaryKeyRanges(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for _, f := range families {
		for i, lv := range levels {
			t.Run(f.name+"/"+lv.Name, func(t *testing.T) {
				if i > 0 {
					assertPrimaryKeyRange(t, s, f.tables[i-1], f.rollupSQL(i), 0, 86400)
				}
				var sources []string
				args := []any{lv.Bucket}
				for j := i; j >= 0; j-- {
					sources = append(sources, f.rangeSQL(j, queryShape{keyWhere: "node_id = ?", seriesLimit: 1}))
					args = append(args, 1, 0, 86400)
				}
				groupKey := ""
				if f.extraKey != "" {
					groupKey = "task_id"
				}
				for j := i; j >= 0; j-- {
					assertPrimaryKeyRange(t, s, f.tables[j], f.aggregateSQL("("+strings.Join(sources, " UNION ALL ")+")", groupKey), args...)
				}
			})
		}
	}
}

func assertPrimaryKeyRange(t *testing.T, s *Store, table, query string, args ...any) {
	t.Helper()
	rows, err := s.r.Query("EXPLAIN QUERY PLAN "+query, args...)
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
	// 主键必须同时约束节点和时间范围；只 SEARCH node_id 仍会扫描该节点全部历史。
	p := strings.Join(plan, "\n")
	t.Logf("%s query plan:\n%s", table, p)
	if !strings.Contains(p, "SEARCH "+table+" USING PRIMARY KEY (node_id=? AND ts>? AND ts<?)") {
		t.Fatalf("query must seek node and time primary key range:\n%s", p)
	}
}

func familyUpsert(f *family, table string, nodeID, ts int64) (string, []any) {
	if f.extraKey != "" {
		return strings.Replace(upsertProbeMinute, "INSERT INTO probe_1m", "INSERT INTO "+table, 1),
			probeArgs(probeRow(nodeID, ts, 1, []uint32{300, 100}, 1, 1))
	}
	return insertMetricFixture(table), append([]any{nodeID, ts}, bucketArgs(bucket(3))...)
}

// 按原始列值比较分片与整段聚合，NULL 也参与比较，不受展示或扫描结构变化影响。
func readFamilyLevel(t *testing.T, s *Store, f *family, i int, nodeID int64) [][]any {
	t.Helper()
	cols := append(f.keys(), f.values()...)
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

func TestRollupCatchupCommitsBoundedSlices(t *testing.T) {
	t.Parallel()
	for _, f := range families {
		for i := 1; i < len(levels); i++ {
			lv := levels[i]
			t.Run(f.name+"/"+lv.Name, func(t *testing.T) {
				s, _ := open(t)
				ctx := t.Context()
				id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
				if err != nil {
					t.Fatal(err)
				}
				width := int64(86400)
				if lv.Name == "1h" {
					width *= 7
				}
				base := int64(1_767_225_600)
				limit := base + 2*width + lv.Bucket
				if err := s.write(ctx, func(tx *sql.Tx) error {
					for ts := base; ts < limit; ts += lv.Bucket {
						query, args := familyUpsert(f, f.tables[i-1], id, ts)
						if _, err := tx.Exec(query, args...); err != nil {
							return err
						}
					}
					_, err := tx.Exec(f.rollupSQL(i), base, limit)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				want := readFamilyLevel(t, s, f, i, id)
				if err := s.write(ctx, func(tx *sql.Tx) error { _, err := tx.Exec("DELETE FROM " + f.tables[i]); return err }); err != nil {
					t.Fatal(err)
				}
				trace := traceWrites(t, s)
				upto, err := s.rollupLevel(ctx, f, i, limit)
				if err != nil {
					t.Fatal(err)
				}
				commits := trace.snapshot()
				t.Logf("%s %s catchup committed %d transactions, watermark=%d", f.name, lv.Name, len(commits), upto)
				if len(commits) < 2 {
					t.Fatalf("catchup used %d transactions, want multiple", len(commits))
				}
				for _, statements := range commits {
					for _, stmt := range statements {
						if strings.HasPrefix(stmt.query, "INSERT OR REPLACE INTO "+f.tables[i]) {
							span := stmt.args[1].Value.(int64) - stmt.args[0].Value.(int64)
							if span > width {
								t.Fatalf("rollup slice span=%d exceeds %d", span, width)
							}
						}
					}
				}
				persisted := watermark(t, s, f.states[i])
				if upto != limit || persisted != limit {
					t.Fatalf("catchup watermark returned=%d persisted=%d, want %d", upto, persisted, limit)
				}
				got := readFamilyLevel(t, s, f, i, id)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("chunked result differs from single range: got %v want %v", got, want)
				}
				if _, err := s.rollupLevel(ctx, f, i, limit); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(readFamilyLevel(t, s, f, i, id), got) {
					t.Fatal("catchup result is not idempotent")
				}
			})
		}
	}
}

func TestNodeCreationAppendsAfterReorder(t *testing.T) {
	t.Parallel()
	for _, registered := range []bool{false, true} {
		t.Run(map[bool]string{false: "CreateNode", true: "RegisterNode"}[registered], func(t *testing.T) {
			s, clk := open(t)
			ctx := context.Background()
			a, _, err := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
			if err != nil {
				t.Fatal(err)
			}
			b, _, err := s.CreateNode(ctx, NewNode{Name: "b"}, hash(2))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ReorderNodes(ctx, []int64{b, a}); err != nil {
				t.Fatal(err)
			}
			var c int64
			if registered {
				if err := s.SetRegisterWindow(ctx, hash(9), clk.Now().Add(time.Hour), 1); err != nil {
					t.Fatal(err)
				}
				c, _, err = s.RegisterNode(ctx, hash(9), "c", hash(3))
			} else {
				c, _, err = s.CreateNode(ctx, NewNode{Name: "c"}, hash(3))
			}
			if err != nil {
				t.Fatal(err)
			}
			nodes, err := s.ListNodes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, n := range nodes {
				ids = append(ids, n.ID)
			}
			if !reflect.DeepEqual(ids, []int64{b, a, c}) {
				t.Fatalf("new node displaced reordered nodes: got %v want %v", ids, []int64{b, a, c})
			}
		})
	}
}
