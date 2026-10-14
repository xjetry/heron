package main

import (
	"context"
	"database/sql"
	"testing"
)

// 逐列核算按 valueWidth 的宽度规则把每行记录长加总；抽样覆盖全部节点时，它必须恰好等于 dbstat 报出的载荷之和。
// WITHOUT ROWID 表是索引式 B 树：每行的记录恰好存一次，在叶页或内部页的单元格里（内部页的键不在叶里重复），所以全部
// 非溢出页的单元格数是行数、载荷和是记录长之和；行远小于溢出阈值，没有溢出页。宽度规则错一档，这里就对不上。
func TestColumnAccountingMatchesDBStat(t *testing.T) {
	path, m := generated(t)
	if len(m.NodeIDs) > columnSampleNodes {
		t.Fatalf("test database has %d nodes; the check needs every node sampled (at most %d)", len(m.NodeIDs), columnSampleNodes)
	}
	ctx := context.Background()
	got, _, err := measureSize(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, "_pragma=query_only(1)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if len(got.Columns) != 6 {
		t.Fatalf("column accounting covers %d tables, want 6", len(got.Columns))
	}
	for _, tc := range got.Columns {
		var payload, cells, overflow int64
		if err := db.QueryRowContext(ctx, "SELECT coalesce(sum(payload), 0), coalesce(sum(ncell), 0) FROM dbstat WHERE name = ? AND pagetype IN ('leaf', 'internal')", tc.Table).Scan(&payload, &cells); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM dbstat WHERE name = ? AND pagetype = 'overflow'", tc.Table).Scan(&overflow); err != nil {
			t.Fatal(err)
		}
		if overflow != 0 {
			t.Fatalf("%s has %d overflow pages; cell payloads would not be whole records", tc.Table, overflow)
		}
		if tc.SampledRows != cells || tc.RecordBytes != payload {
			t.Errorf("%s: accounted %d rows / %d record bytes, dbstat pages hold %d cells / %d payload bytes", tc.Table, tc.SampledRows, tc.RecordBytes, cells, payload)
		}
	}
	var pages int64
	for _, o := range got.Objects {
		pages += o.Pages
	}
	if pages+got.FreelistCount+lockBytePages(got.PageCount, got.PageSize) != got.PageCount || got.DBBytes != got.PageCount*got.PageSize {
		t.Errorf("objects %d pages + freelist %d vs page_count %d; db bytes %d vs %d×%d", pages, got.FreelistCount, got.PageCount, got.DBBytes, got.PageCount, got.PageSize)
	}
}

// 宽度规则本身：边界两侧各取一个值。REAL 列里的整值按整数存，2^47 起不再转换。
func TestValueWidth(t *testing.T) {
	for _, c := range []struct {
		v    any
		real bool
		want int
	}{
		{nil, false, 0}, {int64(0), false, 0}, {int64(1), false, 0}, {int64(2), false, 1}, {int64(-1), false, 1},
		{int64(127), false, 1}, {int64(128), false, 2}, {int64(32767), false, 2}, {int64(32768), false, 3},
		{int64(8388607), false, 3}, {int64(8388608), false, 4}, {int64(1<<31 - 1), false, 4}, {int64(1 << 31), false, 6},
		{int64(1<<47 - 1), false, 6}, {int64(1 << 47), false, 8},
		{0.0, true, 0}, {1.0, true, 0}, {0.5, true, 8}, {123456.0, true, 3}, {1e13, true, 6}, {float64(1 << 47), true, 8}, {1e15, true, 8},
		{2.0, false, 8},
	} {
		if got, _ := valueWidth(c.v, c.real); got != c.want {
			t.Errorf("valueWidth(%v, real=%v) = %d, want %d", c.v, c.real, got, c.want)
		}
	}
}

// 探测增量只在节点数相同、任务数不同、其余生成参数相同的库之间求，按任务数升序相邻配对。
func TestProbeIncrementsPairOnlyComparableDatabases(t *testing.T) {
	mk := func(path string, nodes, tasks int, seed uint64, probe int64) dbSize {
		return dbSize{Path: path, ProbeBytes: probe, DBBytes: 1000 + probe, Manifest: &genManifest{Config: genConfig{Nodes: nodes, Tasks: tasks, Seed: seed, End: 1, Churn: true}}}
	}
	got := probeIncrements([]dbSize{mk("t4", 10, 4, 1, 4000), mk("t0", 10, 0, 1, 0), mk("t1", 10, 1, 1, 1100), mk("other-seed", 10, 2, 2, 50), {Path: "no-manifest"}})
	if len(got) != 2 {
		t.Fatalf("%d increments, want 2 (t0→t1, t1→t4): %+v", len(got), got)
	}
	if got[0].From != "t0" || got[0].To != "t1" || got[0].PerNodeTask != 110 {
		t.Errorf("first increment %+v, want t0→t1 at 110 B per node per task", got[0])
	}
	if got[1].From != "t1" || got[1].To != "t4" || got[1].PerNodeTask != float64(4000-1100)/30 {
		t.Errorf("second increment %+v, want t1→t4 at %.2f", got[1], float64(4000-1100)/30)
	}
}

// 锁字节页在文件跨过 2^30 字节之后才出现：恰好 2^30 字节（页号 1..2^30/页长）的库还没有它。
func TestLockBytePages(t *testing.T) {
	for _, c := range []struct{ pages, size, want int64 }{{262144, 4096, 0}, {262145, 4096, 1}, {797051, 4096, 1}, {10, 4096, 0}, {65536, 16384, 0}, {65537, 16384, 1}} {
		if got := lockBytePages(c.pages, c.size); got != c.want {
			t.Errorf("lockBytePages(%d, %d) = %d, want %d", c.pages, c.size, got, c.want)
		}
	}
}
