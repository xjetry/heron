package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

var schemaV21 = func() []string {
	out := append(slices.Clone(schemaV20), `ALTER TABLE probe_task ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`)
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		out = append(out,
			"ALTER TABLE "+table+" ADD COLUMN net_rx_bps_sum INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE "+table+" ADD COLUMN net_rx_bps_n INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE "+table+" ADD COLUMN net_rx_bps_max INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE "+table+" ADD COLUMN net_tx_bps_sum INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE "+table+" ADD COLUMN net_tx_bps_n INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE "+table+" ADD COLUMN net_tx_bps_max INTEGER NOT NULL DEFAULT 0")
	}
	return out
}()

func TestMigrationFromV20KeepsHistoryAndTaskOrder(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 20, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		for _, statement := range []string{
			`INSERT INTO probe_task (id,kind,target,interval_s,timeout_ms,created_at) VALUES (8,1,'a',60,1000,1),(3,1,'b',60,1000,1)`,
			`INSERT INTO metric_5m (node_id,ts,rx_bytes_sum,rx_bytes_n) VALUES (7,300,600,1)`,
			`INSERT INTO metric_1h (node_id,ts,rx_bytes_sum,rx_bytes_n) VALUES (7,3600,1200,1)`,
		} {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, table := range metricTables {
		var rxN, txN, cpu int
		if err := s.r.QueryRow("SELECT net_rx_bps_n,net_tx_bps_n,cpu_sum FROM "+table+" WHERE node_id=7").Scan(&rxN, &txN, &cpu); err != nil {
			t.Fatal(err)
		}
		if rxN != 0 || txN != 0 {
			t.Fatalf("%s fabricated old peaks: %d/%d", table, rxN, txN)
		}
		if table == "metric_1m" && cpu != 50 {
			t.Fatalf("lost old cpu: %d", cpu)
		}
	}
	_, records, err := s.LoadProbeTasks(t.Context())
	if err != nil || len(records) != 2 || records[0].Task.Id != 3 || records[1].Task.Id != 8 {
		t.Fatalf("migration changed task order: %+v %v", records, err)
	}
}

func TestRestoreV20AndV21PreservesOrderAndPeakSemantics(t *testing.T) {
	t.Parallel()
	for _, version := range []int{20, 21} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path, db := frozenSchemaFixture(t, version)
			seedMinuteRow(t, db)
			if _, err := db.Exec(`INSERT INTO probe_task (id,kind,target,interval_s,timeout_ms,created_at) VALUES (8,1,'a',60,1000,1),(3,1,'b',60,1000,1)`); err != nil {
				t.Fatal(err)
			}
			if version == 21 {
				for _, q := range []string{
					`UPDATE probe_task SET sort_order = CASE id WHEN 8 THEN 0 ELSE 1 END`,
					`UPDATE metric_1m SET net_rx_bps_sum=900,net_rx_bps_n=2,net_rx_bps_max=800,net_tx_bps_n=2`,
				} {
					if _, err := db.Exec(q); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			config, metrics := filepath.Join(t.TempDir(), "config.db"), filepath.Join(t.TempDir(), "metrics.db")
			legacySnapshot(t, path, config, "config", version, true)
			legacySnapshot(t, path, metrics, "metrics", version, true)
			target := filepath.Join(t.TempDir(), "restored.db")
			if _, err := Restore(t.Context(), target, config, metrics, "", time.Now(), slog.Default()); err != nil {
				t.Fatal(err)
			}
			restored, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			_, records, err := restored.LoadProbeTasks(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := []uint64{3, 8}
			if version == 21 {
				want = []uint64{8, 3}
			}
			var ids []uint64
			for _, r := range records {
				ids = append(ids, r.Task.Id)
			}
			if !slices.Equal(ids, want) {
				t.Fatalf("restored order=%v want=%v", ids, want)
			}
			var rxN, txN, peak, sum, cpu int
			if err := restored.r.QueryRow(`SELECT net_rx_bps_n,net_tx_bps_n,net_rx_bps_max,net_rx_bps_sum,cpu_sum FROM metric_1m WHERE node_id=7`).Scan(&rxN, &txN, &peak, &sum, &cpu); err != nil {
				t.Fatal(err)
			}
			wantN, wantPeak, wantSum := 0, 0, 0
			if version == 21 {
				wantN, wantPeak, wantSum = 2, 800, 900
			}
			if cpu != 50 || rxN != wantN || txN != wantN || peak != wantPeak || sum != wantSum {
				t.Fatalf("restored cpu/n/n/peak/sum=%d/%d/%d/%d/%d", cpu, rxN, txN, peak, sum)
			}
		})
	}
}
