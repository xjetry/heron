package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/metric"
)

var schemaV31 = append(slices.Clone(schemaV30),
	`ALTER TABLE metric_1m ADD COLUMN reported INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE metric_1m ADD COLUMN observed INTEGER`,
	`ALTER TABLE metric_5m ADD COLUMN minutes INTEGER`,
	`ALTER TABLE metric_5m ADD COLUMN observed INTEGER`,
	`ALTER TABLE metric_5m ADD COLUMN both INTEGER`,
	`ALTER TABLE metric_1h ADD COLUMN minutes INTEGER`,
	`ALTER TABLE metric_1h ADD COLUMN observed INTEGER`,
	`ALTER TABLE metric_1h ADD COLUMN both INTEGER`,
	`CREATE TABLE node_coverage (node_id INTEGER PRIMARY KEY, start_ts INTEGER NOT NULL)`,
)

func insertMetricFixture(table string) string {
	names := append([]string{"node_id", "ts"}, metricColumnNames()...)
	return "INSERT INTO " + table + " (" + strings.Join(names, ",") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(names)), ",") + ")"
}

func TestCoverageSQLiteTruthTables(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	values := []any{nil, 0, 1}
	or := [][]any{{nil, nil, 1}, {nil, 0, 1}, {1, 1, 1}}
	and := [][]any{{nil, 0, nil}, {0, 0, 0}, {nil, 0, 1}}
	for i, a := range values {
		for j, b := range values {
			var gotOr, gotAnd any
			if err := s.r.QueryRow("SELECT "+coverageOr("?1", "?2")+","+coverageAnd("?1", "?2"), a, b).Scan(&gotOr, &gotAnd); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(gotOr) != fmt.Sprint(or[i][j]) || fmt.Sprint(gotAnd) != fmt.Sprint(and[i][j]) {
				t.Fatalf("a=%v b=%v OR=%v AND=%v", a, b, gotOr, gotAnd)
			}
		}
	}
	var scalar any
	if err := s.r.QueryRow("SELECT max(NULL,1)").Scan(&scalar); err != nil || scalar != nil {
		t.Fatalf("scalar max=%v err=%v", scalar, err)
	}
}

func TestMigrationFromV30Coverage(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 30, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		for _, q := range []string{
			"INSERT INTO metric_5m(node_id,ts) VALUES(7,0)",
			"INSERT INTO node(id,name,token_hash,created_at,last_seen_at) VALUES(8,'seen',x'08',0,125),(9,'never',x'09',0,NULL)",
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	})
	starts, err := s.CoverageStarts(t.Context())
	if err != nil || !reflect.DeepEqual(starts, map[int64]int64{7: 0, 8: 120}) {
		t.Fatalf("starts=%v err=%v", starts, err)
	}
	rows, err := s.ReadMinuteRows(t.Context(), 7, 0, 120)
	if err != nil || len(rows) != 1 || rows[0].Coverage.Minutes == nil || *rows[0].Coverage.Minutes != 1 || rows[0].Coverage.Observed != nil || rows[0].Coverage.ObservedReported != nil {
		t.Fatalf("legacy=%+v err=%v", rows, err)
	}
}

func TestCoverageMergeOrderAndNullRollup(t *testing.T) {
	t.Parallel()
	for _, order := range [][]bool{{false, true}, {true, false}, {false, true, true}, {true, false, false}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			s, clk := open(t)
			id, _, _ := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
			for _, observation := range order {
				r := metric.Row{NodeID: id, TS: 600, CoverageStart: 600, Bucket: metric.NewBucket(), ObservationOnly: observation, Observed: observation}
				if _, err := s.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{r}}); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := s.ReadMinuteRows(t.Context(), id, 600, 660)
			if err != nil || len(rows) != 1 {
				t.Fatal(rows, err)
			}
			c := rows[0].Coverage
			if *c.Minutes != 1 || *c.Observed != 1 || *c.ObservedReported != 1 {
				t.Fatalf("merged=%+v", c)
			}
			if _, err := s.w.Exec("INSERT INTO metric_1m(node_id,ts) VALUES(?,660)", id); err != nil {
				t.Fatal(err)
			}
			clk.SetWall(time.Unix(7200, 0))
			if err := s.Rollup(t.Context()); err != nil {
				t.Fatal(err)
			}
			coarse, err := s.QueryMetrics(t.Context(), id, 0, 3600, levels[2], 3600)
			if err != nil || len(coarse) != 1 {
				t.Fatal(coarse, err)
			}
			c = coarse[0].Coverage
			if c.Minutes == nil || *c.Minutes != 2 || c.Observed != nil || c.ObservedReported != nil {
				t.Fatalf("NULL rollup=%+v", c)
			}
		})
	}
}

func TestCoverageSummaryUsesCompleteSourcesAndRequestWindow(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	id, _, _ := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
	for ts := int64(600); ts < 1200; ts += 60 {
		if _, err := s.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: id, TS: ts, CoverageStart: 600, Bucket: metric.NewBucket(), Observed: true, ObservationOnly: ts >= 900}}}); err != nil {
			t.Fatal(err)
		}
	}
	clk.SetWall(time.Unix(1800, 0))
	for _, step := range []int64{60, 300, 3600} {
		_, c, err := s.QueryMetricsCoverage(t.Context(), id, 600, 3600, levels[0], step)
		if err != nil || c.CoverageStart == nil || *c.CoverageStart != 600 || c.EligibleMinutes != 20 || c.ObservedMinutes != 10 || c.ObservedReportedMinutes != 5 {
			t.Fatalf("step=%d summary=%+v err=%v", step, c, err)
		}
	}
	_, c, err := s.QueryMetricsCoverage(t.Context(), id, 630, 690, levels[0], 60)
	if err != nil || c.EligibleMinutes != 0 || c.ObservedMinutes != 0 {
		t.Fatal(c, err)
	}
	if err := s.Rollup(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, c, err = s.QueryMetricsCoverage(t.Context(), id, 660, 1140, levels[1], 300)
	if err != nil || c.EligibleMinutes != 8 || c.ObservedMinutes != 0 {
		t.Fatalf("partial source=%+v err=%v", c, err)
	}
	_, c, err = s.QueryMetricsCoverage(t.Context(), id, 600, 1200, levels[1], 300)
	if err != nil || c.ObservedMinutes != 10 || c.ObservedReportedMinutes != 5 {
		t.Fatal(c, err)
	}
}

func TestCoverageRestoreLayerAndResurrectedNode(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	oldConfig := filepath.Join(t.TempDir(), "empty.db")
	if err := s.SnapshotConfig(ctx, oldConfig); err != nil {
		t.Fatal(err)
	}
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	config := filepath.Join(t.TempDir(), "config.db")
	if err := s.SnapshotConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{{NodeID: id, TS: 600, CoverageStart: 600, Observed: true, Bucket: metric.NewBucket()}}}); err != nil {
		t.Fatal(err)
	}
	metrics := filepath.Join(t.TempDir(), "metrics.db")
	if err := s.SnapshotMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	_, before, err := s.QueryMetricsCoverage(ctx, id, 600, 1200, levels[0], 60)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, s.path, config, "", "", clk.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	r, err := Open(s.path, clk, slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	_, after, err := r.QueryMetricsCoverage(ctx, id, 600, 1200, levels[0], 60)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("config-only changed coverage: %+v / %+v %v", before, after, err)
	}
	if err := r.DeleteNode(ctx, id); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if _, err := Restore(ctx, s.path, config, "", "", clk.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	r, err = Open(s.path, clk, slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	_, after, err = r.QueryMetricsCoverage(ctx, id, 600, 1200, levels[0], 60)
	if err != nil || after.CoverageStart != nil || after.EligibleMinutes != 0 {
		t.Fatalf("resurrected node has coverage: %+v %v", after, err)
	}
	r.Close()
	result, err := Restore(ctx, s.path, oldConfig, metrics, "", clk.Now(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if result.Orphans["node_coverage"] != 1 || result.Orphans["metric_1m"] != 1 {
		t.Fatalf("orphan coverage not cleaned: %+v", result.Orphans)
	}
}

func TestCoverageStartBatchOrderAndRollbackData(t *testing.T) {
	t.Parallel()
	for _, order := range [][]int64{{660, 600}, {600, 660}} {
		s, _ := open(t)
		id, _, _ := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
		var batch metric.Batch
		for _, ts := range order {
			batch.Rows = append(batch.Rows, metric.Row{NodeID: id, TS: ts, CoverageStart: 540, Observed: true, Bucket: metric.NewBucket()})
		}
		if _, err := s.WriteMinuteBatch(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
		starts, err := s.CoverageStarts(t.Context())
		if err != nil || starts[id] != 540 {
			t.Fatalf("batch order start=%v err=%v", starts, err)
		}
		if _, err := s.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: id, TS: 480, CoverageStart: 540, Bucket: metric.NewBucket()}}}); err != nil {
			t.Fatal("rollback report rejected", err)
		}
		rows, err := s.ReadMinuteRows(t.Context(), id, 480, 540)
		if err != nil || len(rows) != 1 || *rows[0].Coverage.Minutes != 1 {
			t.Fatal(rows, err)
		}
		rejected, err := s.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: id, TS: 480, CoverageStart: 540, Observed: true, Bucket: metric.NewBucket()}}})
		if err != nil || rejected != 1 {
			t.Fatalf("observation before coverage start: rejected=%d err=%v", rejected, err)
		}
		rows, err = s.ReadMinuteRows(t.Context(), id, 480, 540)
		if err != nil || len(rows) != 1 || rows[0].Coverage.Observed == nil || *rows[0].Coverage.Observed != 0 {
			t.Fatal("observation before coverage start admitted", rows, err)
		}
	}
}

// 覆盖事实矛盾的行按行拒绝：同批其他节点的行照常写入，被拒的行既不留下指标行，也不留下覆盖起点。
func TestCoverageInconsistentRowsRejectedPerRow(t *testing.T) {
	t.Parallel()
	sampled := metric.NewBucket()
	sampled.AddSum(0, 1)
	for name, bad := range map[string]metric.Row{
		"zero start":                        {TS: 600, Bucket: metric.NewBucket()},
		"unaligned start":                   {TS: 600, CoverageStart: 630, Bucket: metric.NewBucket()},
		"observation precedes start":        {TS: 540, CoverageStart: 600, Observed: true, Bucket: metric.NewBucket()},
		"observation-only without observed": {TS: 600, CoverageStart: 600, ObservationOnly: true, Bucket: metric.NewBucket()},
		"observation-only with last seen":   {TS: 600, CoverageStart: 600, ObservationOnly: true, Observed: true, LastSeen: time.Unix(610, 0), Bucket: metric.NewBucket()},
		"observation-only with source":      {TS: 600, CoverageStart: 600, ObservationOnly: true, Observed: true, Source: "198.51.100.7", Bucket: metric.NewBucket()},
		"observation-only with samples":     {TS: 600, CoverageStart: 600, ObservationOnly: true, Observed: true, Bucket: sampled},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := open(t)
			good, _, _ := s.CreateNode(t.Context(), NewNode{Name: "good"}, hash(1))
			badID, _, _ := s.CreateNode(t.Context(), NewNode{Name: "bad"}, hash(2))
			bad.NodeID = badID
			rejected, err := s.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{bad, {NodeID: good, TS: 600, CoverageStart: 600, Observed: true, Bucket: metric.NewBucket()}}})
			if err != nil || rejected != 1 {
				t.Fatalf("rejected=%d err=%v", rejected, err)
			}
			if rows, err := s.ReadMinuteRows(t.Context(), good, 600, 660); err != nil || len(rows) != 1 {
				t.Fatalf("valid row in the same batch: %v err=%v", rows, err)
			}
			if rows, err := s.ReadMinuteRows(t.Context(), badID, 0, 1200); err != nil || len(rows) != 0 {
				t.Fatalf("rejected row written: %v err=%v", rows, err)
			}
			starts, err := s.CoverageStarts(t.Context())
			if _, ok := starts[badID]; err != nil || ok || starts[good] != 600 {
				t.Fatalf("coverage starts %v err=%v", starts, err)
			}
		})
	}
}

func TestCoverageEmptyAndPrunedShortWindowsRemainUnknown(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	id, _, _ := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
	_, c, err := s.QueryMetricsCoverage(t.Context(), id, 600, 1200, levels[0], 60)
	if err != nil || c.CoverageStart != nil || c.EligibleMinutes != 0 {
		t.Fatal("never reported", c, err)
	}
	if _, err := s.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: id, TS: 600, CoverageStart: 600, Observed: true, Bucket: metric.NewBucket()}}}); err != nil {
		t.Fatal(err)
	}
	clk.SetWall(time.Unix(10*86400, 0))
	if err := s.Rollup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(t.Context(), DefaultRetention); err != nil {
		t.Fatal(err)
	}
	for _, window := range [][2]int64{{600, 720}, {1200, 1320}} {
		rows, c, err := s.QueryMetricsCoverage(t.Context(), id, window[0], window[1], levels[0], 60)
		if err != nil || len(rows) != 0 || c.CoverageStart == nil || c.EligibleMinutes != 2 || c.ObservedMinutes != 0 || c.ObservedReportedMinutes != 0 {
			t.Fatalf("missing rows must remain unknown: %+v rows=%v err=%v", c, rows, err)
		}
	}
}
