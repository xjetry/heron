package store

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// 写后检查需要即时行数，直接计算而不经过 StorageStats 的复用窗口。
func rowCounts(t testing.TB, s *Store) map[string]int64 {
	t.Helper()
	stats, err := s.computeStorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, tr := range stats.Tables {
		out[tr.Name] = tr.Rows
	}
	return out
}

func openAt(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// 统计恰好覆盖 schemaStatements 建出的每一张表，不多不少，行数与表一一对应。新库的 sqlite_master 就是执行这些
// 语句建出来的，所以按它列表的实现自然满足；统计若改成手写的表名清单，以后新增表而漏改清单时这个用例会红。
func TestStorageStatsCoversEveryTableAndCountsRows(t *testing.T) {
	t.Parallel()
	s, _ := openAt(t)
	for i := range 3 {
		if _, _, err := s.CreateNode(t.Context(), NewNode{Name: fmt.Sprint("n", i)}, hash(byte(i))); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := s.StorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tr := range stats.Tables {
		names = append(names, tr.Name)
	}
	var want []string
	create := regexp.MustCompile(`^CREATE TABLE (\w+)`)
	for _, stmt := range schemaStatements() {
		if m := create.FindStringSubmatch(stmt); m != nil {
			want = append(want, m[1])
		}
	}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("tables = %v, want %v", names, want)
	}
	rows := rowCounts(t, s)
	if rows["node"] != 3 || rows["setting"] != 0 || rows["probe_meta"] != 1 || rows["api_token"] != 0 {
		t.Fatalf("rows = %v", rows)
	}
}

// db_bytes 是逻辑大小 page_count × page_size：WAL 检查点之前主文件可能远小于它，之后二者相等。
func TestStorageStatsReportsLogicalDatabaseSize(t *testing.T) {
	t.Parallel()
	s, path := openAt(t)
	// 用例的区分力来自检查点前主文件落后于逻辑大小，所以写入期间关掉自动检查点：否则 WAL 攒到 1000 页就会合回主文件，
	// 能否落后取决于每次写入改了几页（触发器、索引都会改变它）。写连接只有一条（Open 里 SetMaxOpenConns(1)）、
	// 不按空闲回收，这条 PRAGMA 对之后的每次写入都生效。
	if _, err := s.w.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		if _, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(byte(i))); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := s.StorageStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 检查点前主文件落后于逻辑大小：此时按主文件大小算的实现给不出 DBBytes。
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() >= stats.DBBytes {
		t.Fatalf("main file before checkpoint = %d, db_bytes = %d: the WAL is no longer ahead of the main file "+
			"(a checkpoint ran despite wal_autocheckpoint=0), so this test cannot tell logical from physical size",
			before.Size(), stats.DBBytes)
	}
	if _, err := s.w.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stats.DBBytes <= 4096 || stats.DBBytes != info.Size() {
		t.Fatalf("db_bytes = %d, main file after checkpoint = %d", stats.DBBytes, info.Size())
	}
}
