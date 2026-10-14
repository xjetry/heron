package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type statsResult struct {
	stats StorageStats
	err   error
}

// Done 在 StorageStats 进入等待的 select 时调用，避免靠睡眠猜调用是否已加入同一轮计算。
type statsWaitContext struct {
	context.Context
	ready chan struct{}
	once  sync.Once
}

func (c *statsWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.ready) })
	return c.Context.Done()
}

func receiveStats[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for storage stats")
	}
	var zero T
	return zero
}

func startStats(t *testing.T, s *Store, ctx context.Context) <-chan statsResult {
	t.Helper()
	wait := &statsWaitContext{Context: ctx, ready: make(chan struct{})}
	result := make(chan statsResult, 1)
	go func() {
		stats, err := s.StorageStats(wait)
		result <- statsResult{stats, err}
	}()
	receiveStats(t, wait.ready)
	return result
}

func TestStorageStatsSharesConcurrentComputation(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	s.stats.afterBegin = func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var results []<-chan statsResult
	for range 12 {
		results = append(results, startStats(t, s, t.Context()))
	}
	receiveStats(t, entered)
	if got := s.r.Stats().InUse; got != 0 {
		t.Errorf("shared read pool InUse = %d, want 0", got)
	}
	if got := s.stats.db.Stats().InUse; got != 1 {
		t.Errorf("stats connection InUse = %d, want 1", got)
	}
	close(release)
	for _, result := range results {
		if got := receiveStats(t, result); got.err != nil {
			t.Fatal(got.err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("computations = %d, want 1", got)
	}
}

func TestStorageStatsReusesSQLButObservesWALEachTime(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	var calls atomic.Int32
	start := clk.Now().Unix()
	s.stats.afterBegin = func(context.Context) error {
		calls.Add(1)
		clk.Advance(10 * time.Second)
		return nil
	}
	walCalls := 0
	s.files = statFunc(func(string) (fs.FileInfo, error) { walCalls++; return nil, fs.ErrNotExist })
	first, err := s.StorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.SQLObservedAt != start {
		t.Fatalf("SQL observed at = %d, want transaction start %d", first.SQLObservedAt, start)
	}
	for _, elapsed := range []time.Duration{0, 59 * time.Second, 999 * time.Millisecond} {
		clk.Advance(elapsed)
		got, err := s.StorageStats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 1 || got.SQLObservedAt != first.SQLObservedAt {
			t.Fatalf("cache miss before 60 seconds after completion: computations=%d observed=%d, want 1/%d", calls.Load(), got.SQLObservedAt, first.SQLObservedAt)
		}
		if got.WAL.ObservedAt != clk.Now().Unix() {
			t.Fatalf("WAL observation = %d, want %d", got.WAL.ObservedAt, clk.Now().Unix())
		}
	}
	if walCalls != 4 {
		t.Fatalf("WAL stat calls = %d, want 4", walCalls)
	}
	clk.Advance(time.Millisecond)
	got, err := s.StorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || got.SQLObservedAt != start+70 {
		t.Fatalf("cache did not expire at 60 seconds after completion: computations=%d observed=%d, want 2/%d", calls.Load(), got.SQLObservedAt, start+70)
	}
}

func TestStorageStatsWaitCancellationDoesNotCancelComputation(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	s.stats.afterBegin = func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := startStats(t, s, ctx)
	receiveStats(t, entered)
	s.stats.mu.Lock()
	flight := s.stats.flight
	s.stats.mu.Unlock()
	cancel()
	if got := receiveStats(t, result); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v", got.err)
	}
	close(release)
	receiveStats(t, flight.done)
	if flight.err != nil {
		t.Fatalf("request cancellation canceled shared computation: %v", flight.err)
	}
	if _, err := s.StorageStats(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("computations after canceled waiter = %d, want 1", calls.Load())
	}
}

func TestStorageStatsCloseCancelsAndWaits(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	s.stats.afterBegin = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	}
	result := startStats(t, s, t.Context())
	receiveStats(t, entered)
	s.stats.mu.Lock()
	flight := s.stats.flight
	s.stats.mu.Unlock()
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	receiveStats(t, canceled)
	select {
	case err := <-closed:
		t.Fatalf("Close returned before computation exited: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	// 独立连接的 sql.Tx 可由 database/sql 因 ctx 取消先行回滚；Close 仍须等待统计协程自身。
	release <- struct{}{}
	if err := receiveStats(t, closed); err != nil {
		t.Fatal(err)
	}
	select {
	case <-flight.done:
	default:
		t.Fatal("Close returned with computation still running")
	}
	if got := receiveStats(t, result); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("closing computation error = %v", got.err)
	}
	if _, err := s.StorageStats(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("stats after Close = %v, want ErrClosed", err)
	}
}

func TestStorageStatsFailureIsSharedButNotCached(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	want := errors.New("statistics failed")
	var calls atomic.Int32
	release := make(chan struct{})
	s.stats.afterBegin = func(context.Context) error {
		if calls.Add(1) == 1 {
			<-release
			return want
		}
		return nil
	}
	first := startStats(t, s, t.Context())
	second := startStats(t, s, t.Context())
	close(release)
	for _, ch := range []<-chan statsResult{first, second} {
		if got := receiveStats(t, ch); got.err != want {
			t.Fatalf("waiter error = %v, want same error %v", got.err, want)
		}
	}
	if _, err := s.StorageStats(t.Context()); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("computations after failure = %d, want 2", calls.Load())
	}
}

func TestStorageStatsSnapshotMatchesDirectQueries(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for _, f := range families {
		for i, table := range f.tables {
			cols, vals := "node_id, ts", "1, %d"
			if f.tables[0] == "probe_1m" {
				cols, vals = "node_id, ts, task_id", "1, %d, 1"
			}
			for _, ts := range []int{60 * (i + 1), 3600 * (i + 1)} {
				execStmts(t, s, "INSERT INTO "+table+" ("+cols+") VALUES ("+fmt.Sprintf(vals, ts)+")")
			}
		}
	}
	execStmts(t, s, "INSERT INTO maintenance_state (name, finished_at) VALUES ('prune', 123), ('rollup', 456)")
	got, err := s.StorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range got.Tables {
		var count int64
		if err := s.r.QueryRow("SELECT count(*) FROM " + table.Name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if table.Rows != count {
			t.Errorf("%s rows = %d, direct count = %d", table.Name, table.Rows, count)
		}
	}
	for _, series := range got.Series {
		var oldest sql.NullInt64
		if err := s.r.QueryRow("SELECT min(ts) FROM " + series.Table).Scan(&oldest); err != nil {
			t.Fatal(err)
		}
		if !oldest.Valid || series.Oldest == nil || *series.Oldest != oldest.Int64 {
			t.Errorf("%s oldest = %v, direct min = %v", series.Table, deref(series.Oldest), oldest)
		}
	}
	var pages, size int64
	if err := s.r.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := s.r.QueryRow("PRAGMA page_size").Scan(&size); err != nil {
		t.Fatal(err)
	}
	if got.DBBytes != pages*size {
		t.Fatalf("logical size = %d, direct pages*size = %d", got.DBBytes, pages*size)
	}
	// 调用方可自由修改结果；后续读者不能看到这些修改。
	want, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	got.Tables[0].Rows = -1
	*got.Series[0].Oldest = -1
	*got.Series[1].Watermark = -1
	*got.LastPrune, *got.LastRollup = -1, -1
	next, err := s.StorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	nextJSON, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if string(nextJSON) != string(want) {
		t.Fatalf("caller mutated cached SQL snapshot: got %s, want %s", nextJSON, want)
	}
}

func TestStorageStatsConnectionIsReadOnly(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	if _, err := s.stats.db.Exec("DELETE FROM rollup_state"); err == nil {
		t.Fatal("stats connection allowed a write")
	}
	if max := s.stats.db.Stats().MaxOpenConnections; max != 1 {
		t.Fatalf("stats max connections = %d, want 1", max)
	}
}

func TestStorageStatsMissingSeriesTableIsNotEmpty(t *testing.T) {
	t.Parallel()
	for _, f := range families {
		for _, table := range f.tables {
			t.Run(table, func(t *testing.T) {
				s, _ := open(t)
				execStmts(t, s, "DROP TABLE "+table)
				if _, err := s.StorageStats(t.Context()); err == nil || !strings.Contains(err.Error(), table) {
					t.Fatalf("missing %s must fail statistics, got %v", table, err)
				}
			})
		}
	}
}

func TestStorageStatsQueryPlans(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	var version string
	if err := s.stats.db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("SQLite %s", version)
	for _, f := range families {
		for _, table := range f.tables {
			query := "EXPLAIN QUERY PLAN SELECT COUNT(*), min(ts) FROM " + table
			rows, err := s.stats.db.Query(query)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				t.Logf("%s: %d|%d|%d|%s", query, id, parent, unused, detail)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				t.Fatal(err)
			}
		}
	}
}
