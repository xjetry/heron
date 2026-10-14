package store

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"google.golang.org/protobuf/proto"
)

var schemaV26 = append(slices.Clone(schemaV25),
	`ALTER TABLE node_facts ADD COLUMN diagnostics TEXT NOT NULL DEFAULT 'null'`,
	`ALTER TABLE traffic ADD COLUMN net_counter_epoch TEXT NOT NULL DEFAULT ''`,
)

func TestDiagnosticsFactsRoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "diagnostics"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	facts := &heronv1.Facts{Hostname: "kept", Diagnostics: &heronv1.AgentDiagnostics{
		NetInclude: []string{"eth*"}, NetInterfaces: []string{"eth0"}, NetInterfacesTotal: 1,
		ReportIntervalMs: 10000,
	}}
	for _, diagnostics := range []*heronv1.AgentDiagnostics{
		facts.Diagnostics,
		{NetExclude: []string{"lo"}, FailedCollectors: []heronv1.CollectionComponent{heronv1.CollectionComponent_COLLECTION_COMPONENT_NET}},
		{}, nil,
	} {
		facts.Diagnostics = diagnostics
		if err := s.UpsertFacts(t.Context(), id, 42, facts); err != nil {
			t.Fatal(err)
		}
		check := func(st *Store) {
			t.Helper()
			n, err := st.GetNode(t.Context(), id)
			if err != nil || !proto.Equal(n.Facts, facts) {
				t.Fatalf("facts=%v err=%v want=%v", n.Facts, err, facts)
			}
		}
		check(s)
		config := filepath.Join(t.TempDir(), "config.db")
		if err := s.SnapshotConfig(t.Context(), config); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "restored.db")
		if _, err := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default()); err != nil {
			t.Fatal(err)
		}
		restored, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
		if err != nil {
			t.Fatal(err)
		}
		check(restored)
		if err := restored.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiagnosticsWritesRejectInvalidData(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "diagnostics"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []*heronv1.AgentDiagnostics{
		{FailedCollectors: []heronv1.CollectionComponent{99}},
		{NetInterfaces: []string{"eth0"}},
	} {
		if err := s.UpsertFacts(t.Context(), id, 1, &heronv1.Facts{Diagnostics: d}); err == nil {
			t.Fatal("invalid diagnostics stored")
		}
		var count int
		if err := s.r.QueryRow("SELECT COUNT(*) FROM node_facts").Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected facts changed storage: count=%d err=%v", count, err)
		}
	}
}

func TestDiagnosticsReadAndRestoreRejectInvalidData(t *testing.T) {
	t.Parallel()
	for _, text := range []string{`{"failedCollectors":[99]}`, `{"netInterfaces":["eth0"]}`, `{"secret":"not allowed"}`, `{invalid`, "\u00a0null"} {
		t.Run(text, func(t *testing.T) {
			s, _ := open(t)
			id, _, err := s.CreateNode(t.Context(), NewNode{Name: "diagnostics"}, hash(1))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.UpsertFacts(t.Context(), id, 1, &heronv1.Facts{Hostname: "kept"}); err != nil {
				t.Fatal(err)
			}
			if err := s.write(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.Exec("UPDATE node_facts SET diagnostics=?", text)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var stored string
			if err := s.r.QueryRow("SELECT diagnostics FROM node_facts WHERE node_id=?", id).Scan(&stored); err != nil || stored != text {
				t.Fatalf("invalid fixture did not land: %q %v", stored, err)
			}
			if _, err := s.GetNode(t.Context(), id); err == nil || !strings.Contains(err.Error(), "diagnostics") {
				t.Fatalf("invalid diagnostics read: %v", err)
			}
			config := filepath.Join(t.TempDir(), "config.db")
			if err := s.SnapshotConfig(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			if err := s.UpsertFacts(t.Context(), id, 2, &heronv1.Facts{Hostname: "kept"}); err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(t.Context(), s.path, config, "", "", time.Now(), slog.Default()); err == nil || !strings.Contains(err.Error(), "diagnostics") {
				t.Fatalf("invalid diagnostics restored: %v", err)
			}
			n, err := s.GetNode(t.Context(), id)
			if err != nil || n.Facts.Diagnostics != nil || n.Facts.Hostname != "kept" {
				t.Fatalf("rejected restore changed facts: %v %v", n.Facts, err)
			}
		})
	}
}

func TestObservabilityMigrationAndOldSnapshotsPreserveUnknown(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 25, func(t *testing.T, db *sql.DB) {
		for _, q := range []string{
			`INSERT INTO node(id,name,token_hash,created_at) VALUES(1,'old',x'01',0)`,
			`INSERT INTO node_facts VALUES(1,42,'old-host','','','','','',0,'old-agent',0,123,'{}')`,
			`INSERT INTO traffic VALUES(1,'boot',10,20,100,200,30,40,0,123)`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	})
	check := func(st *Store) {
		t.Helper()
		n, err := st.GetNode(t.Context(), 1)
		if err != nil || n.Facts.Diagnostics != nil || n.Facts.Hostname != "old-host" {
			t.Fatalf("old facts=%v err=%v", n.Facts, err)
		}
		recs, err := st.LoadTraffic(t.Context())
		want := TrafficRecord{NodeID: 1, BootID: "boot", Baseline: []TrafficCounter{{Rx: 10, Tx: 20}}, TotalRx: 100, TotalTx: 200, PeriodRx: 30, PeriodTx: 40, PeriodStart: time.Unix(0, 0).UTC()}
		if err != nil || len(recs) != 1 || !reflect.DeepEqual(recs[0], want) {
			t.Fatalf("old traffic=%+v err=%v want=%+v", recs, err, want)
		}
	}
	check(s)
	config, metrics := filepath.Join(t.TempDir(), "config.db"), filepath.Join(t.TempDir(), "metrics.db")
	if err := s.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotMetrics(t.Context(), metrics); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{config, metrics} {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if path == config {
			removeV41Config(t, db)
			removeV40Config(t, db)
			removeV39Config(t, db)
			for _, q := range []string{
				"ALTER TABLE node DROP COLUMN rented",
				"ALTER TABLE node DROP COLUMN traffic_quota_bytes", "ALTER TABLE node DROP COLUMN traffic_quota_mode",
				"DROP TABLE probe_cert_presented",
				"DROP TABLE probe_cert",
				"ALTER TABLE probe_task DROP COLUMN cert_spki_sha256",
				"ALTER TABLE probe_task DROP COLUMN config_id",
				"ALTER TABLE probe_task DROP COLUMN dns_server",
				"ALTER TABLE node_facts DROP COLUMN execution", "ALTER TABLE node_facts DROP COLUMN facts_rev",
				"ALTER TABLE node_facts DROP COLUMN diagnostics", "ALTER TABLE traffic DROP COLUMN net_counter_epoch",
				"ALTER TABLE node DROP COLUMN maintenance", "ALTER TABLE node DROP COLUMN public_remark", "ALTER TABLE alert_event DROP COLUMN silenced", "ALTER TABLE alert_state DROP COLUMN fired_silenced",
				"DROP TABLE silence", "DROP TABLE silence_node", "DROP TABLE silence_tag",
			} {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			dropTCPRateColumns(t, db)
			for _, q := range []string{
				"ALTER TABLE metric_1m DROP COLUMN load1_per_core_sum", "ALTER TABLE metric_1m DROP COLUMN load1_per_core_n",
				"ALTER TABLE metric_5m DROP COLUMN load1_per_core_sum", "ALTER TABLE metric_5m DROP COLUMN load1_per_core_n",
				"ALTER TABLE metric_1h DROP COLUMN load1_per_core_sum", "ALTER TABLE metric_1h DROP COLUMN load1_per_core_n",
				"DROP TABLE node_coverage", "ALTER TABLE metric_1m DROP COLUMN reported", "ALTER TABLE metric_1m DROP COLUMN observed",
				"ALTER TABLE metric_5m DROP COLUMN minutes", "ALTER TABLE metric_5m DROP COLUMN observed", "ALTER TABLE metric_5m DROP COLUMN both",
				"ALTER TABLE metric_1h DROP COLUMN minutes", "ALTER TABLE metric_1h DROP COLUMN observed", "ALTER TABLE metric_1h DROP COLUMN both",
			} {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			// v25 的指标表还没有这四个指标的列；快照要真像 v25，迁移 28 才不会撞上重复列。
			for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
				for _, column := range []string{"disk_read_bps", "disk_write_bps", "cpu_steal_pct", "cpu_iowait_pct"} {
					for _, suffix := range []string{"_sum", "_n", "_max"} {
						if _, err := db.Exec("ALTER TABLE " + table + " DROP COLUMN " + column + suffix); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		}
		if _, err := db.Exec("DROP TABLE IF EXISTS cleanup_job; UPDATE snapshot_meta SET schema_version=25"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(t.TempDir(), "restored.db")
	if _, err := Restore(t.Context(), target, config, metrics, "", time.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	check(restored)
}
