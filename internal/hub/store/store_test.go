package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/testwait"
	"google.golang.org/protobuf/proto"
)

func open(t *testing.T) (*Store, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, err := Open(filepath.Join(t.TempDir(), "t.db"), clk, slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, clk
}

func hash(b byte) []byte { h := make([]byte, 32); h[0] = b; return h }

func TestWriteAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprint(async), func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "closed.db"), clock.Real(), slog.Default(), MigrateSchema)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			fn := func(*sql.Tx) error { t.Error("closed store executed a write"); return nil }
			if async {
				result := make(chan error, 1)
				s.writeAsync(fn, func(e error) { result <- e })
				select {
				case err = <-result:
				case <-time.After(testwait.Bound):
					t.Fatal("closed async write did not notify")
				}
			} else {
				err = s.write(context.Background(), fn)
			}
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("write after close = %v, want ErrClosed", err)
			}
		})
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestOpenCreatesSchemaAtCurrentVersion(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	var v int
	if err := s.r.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	var seq int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_sequence'").Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatal("node.id must be AUTOINCREMENT: SQLite creates sqlite_sequence only when some table uses it")
	}
}

func TestReopenKeepsData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clk := clock.NewFake(time.Unix(0, 0))
	s, err := Open(filepath.Join(dir, "t.db"), clk, slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s.CreateNode(context.Background(), NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(filepath.Join(dir, "t.db"), clk, slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	nodes, err := s.ListNodes(context.Background())
	if err != nil || len(nodes) != 1 || nodes[0].ID != id {
		t.Fatalf("nodes = %+v err = %v", nodes, err)
	}
}

func TestDeletedNodeIDIsNotReused(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	a, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	if err := s.DeleteNode(ctx, a); err != nil {
		t.Fatal(err)
	}
	b, _, _ := s.CreateNode(ctx, NewNode{Name: "b"}, hash(2))
	if b == a {
		t.Fatalf("id %d was reused after delete", a)
	}
}

func TestTokenHashesAndRotate(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	if err := s.SetTokenHash(ctx, id, hash(1), hash(9)); err != nil {
		t.Fatal(err)
	}
	m, err := s.TokenHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var k [32]byte
	copy(k[:], hash(9))
	if m[k] != id || len(m) != 1 {
		t.Fatalf("hashes = %v", m)
	}
	if err := s.SetTokenHash(ctx, id+100, hash(9), hash(3)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate on missing node: err = %v, want ErrNotFound", err)
	}
}

// 比较并换发：期望值不是库里的当前凭据（调用方的映射落后于库）时不改库，与节点不存在区分开。
func TestSetTokenHashComparesAndSwaps(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	stored := func() []byte {
		t.Helper()
		var h []byte
		if err := s.r.QueryRow("SELECT token_hash FROM node WHERE id = ?", id).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, expected := range [][]byte{hash(7), nil} {
		if err := s.SetTokenHash(ctx, id, expected, hash(2)); !errors.Is(err, ErrCredentialChanged) {
			t.Fatalf("swap from stale credential %x: err = %v, want ErrCredentialChanged", expected, err)
		}
		if got := stored(); !slices.Equal(got, hash(1)) {
			t.Fatalf("stale swap changed the credential to %x", got)
		}
	}
	if err := s.SetTokenHash(ctx, id, hash(1), hash(2)); err != nil {
		t.Fatal(err)
	}
	if got := stored(); !slices.Equal(got, hash(2)) {
		t.Fatalf("credential = %x after swap, want %x", got, hash(2))
	}
	if err := s.SetTokenHash(ctx, id, hash(1), hash(3)); !errors.Is(err, ErrCredentialChanged) {
		t.Fatalf("replaying the consumed credential: err = %v, want ErrCredentialChanged", err)
	}
}

func TestRegisterNodeConsumesWindow(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	if _, _, err := s.RegisterNode(ctx, hash(5), "x", hash(1)); !errors.Is(err, ErrNoWindow) {
		t.Fatalf("no window: err = %v", err)
	}
	if err := s.SetRegisterWindow(ctx, hash(5), clk.Now().Add(time.Hour), 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterNode(ctx, hash(6), "x", hash(1)); !errors.Is(err, ErrBadKey) {
		t.Fatalf("wrong key: err = %v", err)
	}
	if _, _, err := s.RegisterNode(ctx, hash(5), "x", hash(1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterNode(ctx, hash(5), "y", hash(2)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterNode(ctx, hash(5), "z", hash(3)); !errors.Is(err, ErrNoWindow) {
		t.Fatalf("exhausted window must read as closed, err = %v", err)
	}
	w, ok, _ := s.RegisterWindow(ctx)
	if !ok || w.Remaining != 0 {
		t.Fatalf("window = %+v ok=%v", w, ok)
	}
}

func TestExpiredWindowIsClosed(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := context.Background()
	_ = s.SetRegisterWindow(ctx, hash(5), clk.Now().Add(time.Minute), 5)
	clk.Advance(2 * time.Minute)
	if _, _, err := s.RegisterNode(ctx, hash(5), "x", hash(1)); !errors.Is(err, ErrNoWindow) {
		t.Fatalf("expired: err = %v", err)
	}
}

func TestFacts(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	f := &heronv1.Facts{Hostname: "h", Os: "o", CpuCores: 4}
	if err := s.UpsertFacts(ctx, id, 77, f); err != nil {
		t.Fatal(err)
	}
	f.Hostname = "h2"
	done := make(chan error, 1)
	s.UpsertFactsAsync(id, 78, f, func(err error) { done <- err })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m, _ := s.FactsHashes(ctx)
	if m[id] != 78 {
		t.Fatalf("facts hash = %d, want 78", m[id])
	}
}

func bucket(cpu float64) *metric.Bucket {
	b := metric.NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(cpu)})
	return b
}

func TestHalfBucketsMergeAdditively(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: 600, CoverageStart: 600, Bucket: bucket(10)}}}); err != nil {
		t.Fatal(err)
	}
	b := bucket(30)
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(50)})
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: 600, CoverageStart: 600, Bucket: b}}}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ReadMinuteRows(ctx, id, 0, 1000)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v err = %v", rows, err)
	}
	if rows[0].Bucket.Sum[0] != 90 || rows[0].Bucket.N[0] != 3 || rows[0].Bucket.Max[0] != 50 {
		t.Fatalf("merged = %v/%d/%v, want 90/3/50", rows[0].Bucket.Sum[0], rows[0].Bucket.N[0], rows[0].Bucket.Max[0])
	}
}

func TestMissingMetricReadsBackAsNoData(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	_, _ = s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: 600, CoverageStart: 600, Bucket: bucket(10)}}})
	rows, _ := s.ReadMinuteRows(ctx, id, 0, 1000)
	for i, c := range metric.Columns {
		_, ok := rows[0].Bucket.Mean(i)
		if c.Name == "cpu" && !ok {
			t.Fatal("cpu had a reading")
		}
		if c.Name != "cpu" && ok {
			t.Fatalf("%s had no readings but reads back as data", c.Name)
		}
	}
}

func TestWriterRejectsRowsBeforeRollupWatermark(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	if err := s.setRollupWatermark(ctx, "5m", 900); err != nil {
		t.Fatal(err)
	}
	rejected, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{
		{NodeID: id, TS: 600, CoverageStart: 600, Bucket: bucket(1)},
		{NodeID: id, TS: 900, CoverageStart: 600, Bucket: bucket(2)},
	}})
	if err != nil || rejected != 1 {
		t.Fatalf("rejected = %d err = %v, want 1 nil", rejected, err)
	}
	rows, _ := s.ReadMinuteRows(ctx, id, 0, 2000)
	if len(rows) != 1 || rows[0].TS != 900 {
		t.Fatalf("rows = %+v, want only ts 900", rows)
	}
}

func TestStoreDoesNotExposeWatermarkMutation(t *testing.T) {
	t.Parallel()
	var s any = (*Store)(nil)
	if _, ok := s.(interface {
		SetRollupWatermark(context.Context, string, int64) error
	}); ok {
		t.Fatal("Store exposes arbitrary rollup watermark mutation")
	}
}

func TestWriteMinuteBatchUpdatesLastSeen(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	seen := time.Unix(1234, 0).UTC()
	_, _ = s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: 1200, CoverageStart: 1200, Bucket: bucket(1), LastSeen: seen}}})
	nodes, _ := s.ListNodes(ctx)
	if !nodes[0].LastSeenAt.Equal(seen) {
		t.Fatalf("last_seen_at = %v, want %v", nodes[0].LastSeenAt, seen)
	}
}

func TestDeleteNodeRemovesDependentRows(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	_ = s.UpsertFacts(ctx, id, 1, &heronv1.Facts{})
	_, _ = s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: 600, CoverageStart: 600, Bucket: bucket(1)}}})
	if err := s.DeleteNode(ctx, id); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.FactsHashes(ctx); len(m) != 0 {
		t.Fatalf("facts survived delete: %v", m)
	}
	if _, err := s.CleanupDeleted(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.ReadMinuteRows(ctx, id, 0, 1000); len(rows) != 0 {
		t.Fatalf("metric rows survived delete: %v", rows)
	}
}

// keptOnNodeDelete 是带 node_id 列、删节点时按设计保留的表。alert_event 是告警历史，只按保留期
// （deleteExpiredAlertEvents）清理：节点删了，它当时发生过的告警仍可查（TestDeleteNodeCleansAlertScopeAndState）。
// cleanup_job 的 node_id 指向已删节点本身，是删节点时登记的待办，作业完成才删。
var keptOnNodeDelete = []string{"alert_event", "cleanup_job"}

// DeleteNode 在事务里删 nodeConfigTables、登记作业，nodeHistoryTables 由清理作业删；这里逐表写入两节点的行，
// 验证删除事务只动配置层、清理之后时序行也没了，且 keptOnNodeDelete 中的历史与另一个节点的行都保留。
// cleanup_job 不预先写：它的行由 DeleteNode 登记、由清理在完成时删，正是要观察的对象。
// schema 与清单的集合完备性由 TestNodeDependentTablesComplete 校验。
func TestDeleteNodeCoversEveryTableWithNodeID(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	gone, _, _ := s.CreateNode(ctx, NewNode{Name: "gone"}, hash(1))
	kept, _, _ := s.CreateNode(ctx, NewNode{Name: "kept"}, hash(2))
	tables := tablesWithNodeID(t, s)
	t.Logf("tables with node_id: %v", tables)
	for _, name := range keptOnNodeDelete {
		if !slices.Contains(tables, name) {
			t.Fatalf("kept table %s has no node_id column; tables with node_id: %v", name, tables)
		}
	}
	for _, table := range tables {
		if table == "cleanup_job" {
			continue
		}
		for _, id := range []int64{gone, kept} {
			insertRowFor(t, s, table, id)
		}
	}
	check := func(stage string, want func(table string) int64) {
		t.Helper()
		for _, table := range tables {
			if n := nodeRows(t, s, table, gone); n != want(table) {
				t.Fatalf("%s: %s has %d rows of the deleted node, want %d", stage, table, n, want(table))
			}
			wantKept := int64(1)
			if table == "cleanup_job" {
				wantKept = 0
			}
			if n := nodeRows(t, s, table, kept); n != wantKept {
				t.Fatalf("%s: %s has %d rows of the other node, want %d", stage, table, n, wantKept)
			}
		}
	}
	if err := s.DeleteNode(ctx, gone); err != nil {
		t.Fatal(err)
	}
	check("after delete", func(table string) int64 {
		if slices.Contains(nodeConfigTables, table) {
			return 0
		}
		return 1 // 时序行等清理，alert_event 保留，cleanup_job 是刚登记的作业
	})
	if _, err := s.CleanupDeleted(ctx); err != nil {
		t.Fatal(err)
	}
	check("after cleanup", func(table string) int64 {
		if table == "alert_event" {
			return 1
		}
		return 0
	})
}

func tablesWithNodeID(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.r.QueryContext(t.Context(), "SELECT m.name FROM sqlite_master m JOIN pragma_table_info(m.name) c WHERE m.type = 'table' AND c.name = 'node_id' ORDER BY m.name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no table has a node_id column")
	}
	return out
}

// insertRowFor 给 table 写一行属于 node 的数据：node_id 取 node，其余 NOT NULL 且无默认值的列按声明类型给零值，
// 可空或有默认值的列不写。同一张表的两行因此只在 node_id 上不同：主键含 node_id 的表不冲突，
// 主键是自增 id 的表（alert_event）各得一个新 id。
func insertRowFor(t *testing.T, s *Store, table string, node int64) {
	t.Helper()
	rows, err := s.r.QueryContext(t.Context(), "SELECT name, type, \"notnull\", dflt_value IS NOT NULL FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	var args []any
	for rows.Next() {
		var name, typ string
		var notNull, hasDefault bool
		if err := rows.Scan(&name, &typ, &notNull, &hasDefault); err != nil {
			t.Fatal(err)
		}
		switch {
		case name == "node_id":
			args = append(args, node)
		case !notNull || hasDefault:
			continue
		case typ == "TEXT":
			args = append(args, "")
		case typ == "REAL":
			args = append(args, 0.0)
		case typ == "BLOB":
			args = append(args, []byte{})
		default:
			args = append(args, 0)
		}
		cols = append(cols, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO "+table+" ("+strings.Join(cols, ", ")+") VALUES ("+strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")+")", args...)
		return err
	}); err != nil {
		t.Fatalf("insert into %s: %v", table, err)
	}
}

func nodeRows(t *testing.T, s *Store, table string, node int64) int64 {
	t.Helper()
	var n int64
	if err := s.r.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table+" WHERE node_id = ?", node).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 删除节点在同一事务里删掉 traffic 行与它的基线行（nodeConfigTables）：基线行不能比 traffic 行活得久，否则读库按孤儿行拒收。
func TestDeleteNodeRemovesTraffic(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "traffic"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	rec := TrafficRecord{NodeID: id, BootID: "boot", Baseline: []TrafficCounter{{"eth0", 5, 7}, {"eth1", 1, 1}}, TotalRx: 5, TotalTx: 7, PeriodStart: time.Unix(0, 0)}
	if _, err := s.WriteTraffic(t.Context(), []TrafficRecord{rec}); err != nil {
		t.Fatal(err)
	}
	if n := rowCounts(t, s); n["traffic"] != 1 || n["traffic_baseline"] != 2 {
		t.Fatalf("rows before delete = %d traffic, %d traffic_baseline; want 1 and 2", n["traffic"], n["traffic_baseline"])
	}
	if err := s.DeleteNode(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"traffic", "traffic_baseline"} {
		var n int64
		if err := s.r.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE node_id = ?", id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s survived delete: %d rows (%v)", table, n, err)
		}
	}
	if got, err := s.LoadTraffic(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("load after delete: %+v %v", got, err)
	}
}

func TestAsyncCallbackObservesCommittedWrite(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	if err := s.UpsertFacts(ctx, id, 77, &heronv1.Facts{}); err != nil {
		t.Fatal(err)
	}
	seen := make(chan uint64, 1)
	s.UpsertFactsAsync(id, 78, &heronv1.Facts{}, func(err error) {
		if err != nil {
			t.Error(err)
		}
		m, _ := s.FactsHashes(ctx)
		seen <- m[id]
	})
	if got := <-seen; got != 78 {
		t.Fatalf("callback observed facts hash %d, want 78: it must run only after the transaction committed", got)
	}
}

// 已入队但尚未开始的事务在 ctx 取消后不执行，调用方得到 ctx.Err() 且库无变化。
func TestCancelBeforeStartSkipsTransaction(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	gate := make(chan struct{})
	started := make(chan struct{})
	go s.write(context.Background(), func(*sql.Tx) error { close(started); <-gate; return nil })
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() { _, _, err := s.CreateNode(ctx, NewNode{Name: "queued"}, hash(1)); res <- err }()
	// 第一个事务占住写协程，队列非空只能来自 CreateNode；确认入队后取消，
	// 才能检出写协程遗漏取消预检，而不是只覆盖入队前的取消分支。
	deadline := time.Now().Add(testwait.Bound)
	for len(s.writes) == 0 {
		if time.Now().After(deadline) {
			cancel()
			close(gate)
			<-res
			t.Fatal("CreateNode did not enter the write queue")
		}
		runtime.Gosched()
	}
	cancel()
	close(gate)
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	// 同步写屏障保证即使等待方错误地提前返回，节点事务也已结束。
	if err := s.SetRegisterWindow(context.Background(), hash(3), clk.Now().Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("node was created despite cancellation before start: %+v", nodes)
	}
}

// 已开始的事务不受取消影响，调用方必须拿到真实结果：nil 且库里有行。
func TestCancelDuringTransactionStillReportsCommit(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inside := make(chan struct{})
	res := make(chan error, 1)
	go func() {
		res <- s.write(ctx, func(tx *sql.Tx) error {
			close(inside)
			<-ctx.Done() // 事务进行中 ctx 被取消
			_, err := tx.Exec("INSERT INTO node (name, token_hash, created_at) VALUES ('mid', ?, 0)", hash(2))
			return err
		})
	}()
	<-inside
	cancel()
	if err := <-res; err != nil {
		t.Fatalf("err = %v, want nil: the transaction committed", err)
	}
	nodes, err := s.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("committed row missing: %+v", nodes)
	}
}

func TestListNodesCarriesFactsAndOrder(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	a, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	b, _, _ := s.CreateNode(ctx, NewNode{Name: "b"}, hash(2))
	if err := s.UpsertFacts(ctx, b, 7, &heronv1.Facts{Hostname: "hb", CpuCores: 4, IcmpAvailable: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReorderNodes(ctx, []int64{b, a}); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].ID != b || nodes[1].ID != a {
		t.Fatalf("order = %+v, want b before a", nodes)
	}
	if nodes[0].Facts == nil || nodes[0].Facts.Hostname != "hb" || nodes[0].Facts.CpuCores != 4 || !nodes[0].Facts.IcmpAvailable || nodes[0].FactsUpdatedAt.IsZero() {
		t.Fatalf("facts not joined: %+v", nodes[0])
	}
	if nodes[1].Facts != nil {
		t.Fatalf("node without facts must carry nil, got %+v", nodes[1].Facts)
	}
	if nodes[0].SortOrder != 0 || nodes[1].SortOrder != 1 {
		t.Fatalf("sort_order = %d, %d", nodes[0].SortOrder, nodes[1].SortOrder)
	}
}

func TestReorderNodesRejectsAnythingButAFullPermutation(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	a, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	b, _, _ := s.CreateNode(ctx, NewNode{Name: "b"}, hash(2))
	for _, ids := range [][]int64{{a}, {a, b, 999}, {a, a}, {b, 999}} {
		if err := s.ReorderNodes(ctx, ids); !errors.Is(err, ErrBadOrder) {
			t.Fatalf("ReorderNodes(%v) = %v, want ErrBadOrder", ids, err)
		}
	}
	nodes, _ := s.ListNodes(ctx)
	if nodes[0].ID != a || nodes[1].ID != b {
		t.Fatalf("rejected reorder must leave order untouched: %+v", nodes)
	}
}

func TestUpdateNodeReplacesEditableFields(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "old"}, hash(1))
	if _, err := s.UpdateNode(ctx, id, NodeEdit{Name: "new", Public: true, Note: "note", TrafficResetDay: 1}); err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNode(ctx, id)
	if err != nil || n.Name != "new" || !n.Public || n.Note != "note" {
		t.Fatalf("GetNode = %+v, %v", n, err)
	}
	if _, err := s.UpdateNode(ctx, 999, NodeEdit{Name: "x", TrafficResetDay: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v, want ErrNotFound", err)
	}
	if _, err := s.GetNode(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetNode(999) = %v, want ErrNotFound", err)
	}
}

func TestDeleteNodeClearsEveryLevel(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	keep, _, _ := s.CreateNode(ctx, NewNode{Name: "keep"}, hash(2))
	seedProbeLevels(t, s, []int64{id, keep})
	for _, tbl := range metricTables {
		for _, node := range []int64{id, keep} {
			args := append([]any{node, int64(600)}, bucketArgs(metric.NewBucket())...)
			if _, err := s.w.ExecContext(ctx, insertMetricFixture(tbl), args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.DeleteNode(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CleanupDeleted(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range append(append([]string{}, metricTables...), probeTables...) {
		var n int64
		if err := s.r.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl+" WHERE node_id = ?", id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s still has %d rows for deleted node (%v)", tbl, n, err)
		}
		if err := s.r.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl+" WHERE node_id = ?", keep).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s lost the other node's row: %d (%v)", tbl, n, err)
		}
	}
}

func TestUpdateNodePersistsResetDayAndCreateUsesTheDefault(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if n, _ := s.GetNode(ctx, id); n.TrafficResetDay != 1 {
		t.Fatalf("default reset day = %d, want 1", n.TrafficResetDay)
	}
	if _, err := s.UpdateNode(ctx, id, NodeEdit{Name: "n", TrafficResetDay: 15}); err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNode(ctx, id)
	if err != nil || n.TrafficResetDay != 15 {
		t.Fatalf("after update: %+v %v", n, err)
	}
	list, _ := s.ListNodes(ctx)
	if list[0].TrafficResetDay != 15 {
		t.Fatalf("ListNodes reset day = %d", list[0].TrafficResetDay)
	}
}

// SQLite 打开失败的报错不带文件名；容器里 /data 不可写时，路径是报错里唯一能指向原因的线索。
func TestOpenErrorNamesTheDatabasePath(t *testing.T) {
	t.Parallel()
	// 父路径是普通文件：谁来运行测试都打不开，失败不依赖权限位。
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "t.db")
	s, err := Open(path, clock.NewFake(time.Unix(0, 0)), slog.Default(), MigrateSchema)
	if err == nil {
		s.Close()
		t.Fatalf("Open(%s) succeeded", path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("Open(%s) = %q, want the path in the error", path, err)
	}
}

func TestPublicNodeQueriesSeeOnlyPublicNodes(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	var ids []int64
	for i, name := range []string{"a", "b", "c"} {
		id, _, err := s.CreateNode(ctx, NewNode{Name: name}, hash(byte(i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// 管理端建节点默认公开，这里显式把中间节点设为不公开，检验公开查询只返回 public=1 的节点。
	for i, public := range []bool{true, false, true} {
		if _, err := s.UpdateNode(ctx, ids[i], NodeEdit{Name: []string{"a", "b", "c"}[i], Public: public, TrafficResetDay: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReorderNodes(ctx, []int64{ids[2], ids[1], ids[0]}); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.ListPublicNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	if strings.Join(names, ",") != "c,a" {
		t.Fatalf("public nodes = %v, want c,a in panel order", names)
	}
	for id, want := range map[int64]bool{ids[0]: true, ids[1]: false, ids[2]: true, 999999: false} {
		if got, err := s.NodeIsPublic(ctx, id); err != nil || got != want {
			t.Errorf("NodeIsPublic(%d) = %v %v, want %v", id, got, err, want)
		}
	}
}
