package store

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"google.golang.org/protobuf/proto"
)

var schemaV23 = append(slices.Clone(schemaV22), `ALTER TABLE node_facts ADD COLUMN network TEXT NOT NULL DEFAULT '{}'`)

func TestNetworkWritesRejectInvalidTime(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "bounded"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{253402300800, 9223372036854775807} {
		facts := &heronv1.Facts{Network: &heronv1.NetworkInfo{Ipv4: &heronv1.AddressDetection{State: 2, CheckedAt: at}}}
		if err := s.UpsertFacts(t.Context(), id, 1, facts); err == nil || !strings.Contains(err.Error(), "checked_at") {
			t.Fatalf("invalid time stored: %v", err)
		}
	}
}

func TestNetworkSnapshotsRejectInvalidData(t *testing.T) {
	t.Parallel()
	for _, network := range []string{`{"ipv4":{"state":2,"checkedAt":"9223372036854775807"}}`, `{"ipv6":{"state":1,"address":"8.8.8.8","checkedAt":"100"}}`, `{invalid`} {
		t.Run(network, func(t *testing.T) {
			s, _ := open(t)
			id, _, err := s.CreateNode(t.Context(), NewNode{Name: "bounded"}, hash(1))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.UpsertFacts(t.Context(), id, 1, &heronv1.Facts{}); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(t.TempDir(), "config.db")
			if err := s.SnapshotConfig(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE node_facts SET network=?", network); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(t.Context(), s.path, config, "", "", time.Now(), slog.Default()); err == nil || !strings.Contains(err.Error(), "network") {
				t.Fatalf("invalid snapshot restored: %v", err)
			}
			node, err := s.GetNode(t.Context(), id)
			if err != nil || node.Facts.Network != nil {
				t.Fatalf("rejected restore changed node: %v %v", node.Facts, err)
			}
		})
	}
}

func TestNetworkFactsRoundTripAndRestore(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "network"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	facts := &heronv1.Facts{Hostname: "kept", Network: &heronv1.NetworkInfo{
		Ipv4: &heronv1.AddressDetection{State: 1, Address: "8.8.4.4", CheckedAt: 100},
		Ipv6: &heronv1.AddressDetection{State: 2, CheckedAt: 101},
	}}
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
	defer restored.Close()
	check(restored)
	facts.Network = &heronv1.NetworkInfo{Ipv4: &heronv1.AddressDetection{State: 3, CheckedAt: 200}, Ipv6: &heronv1.AddressDetection{State: 1, Address: "2606:4700::1111", CheckedAt: 200}}
	if err := s.UpsertFacts(t.Context(), id, 43, facts); err != nil {
		t.Fatal(err)
	}
	check(s)
	facts.Network = nil
	if err := s.UpsertFacts(t.Context(), id, 44, facts); err != nil {
		t.Fatal(err)
	}
	check(s)
}

func TestNetworkMigrationFromV22PreservesUnknown(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 22, func(t *testing.T, db *sql.DB) {
		for _, q := range []string{`INSERT INTO node(id,name,token_hash,created_at) VALUES(1,'old',x'01',0)`, `INSERT INTO node_facts VALUES(1,42,'old-host','','','','','',0,'old-agent',0,123)`} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	})
	n, err := s.GetNode(t.Context(), 1)
	if err != nil || n.Facts == nil || n.Facts.Hostname != "old-host" || n.Facts.Network != nil {
		t.Fatalf("old facts=%v err=%v", n.Facts, err)
	}
}

func TestNetworkOldSnapshotRestoresWithoutInventingAddresses(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "old"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertFacts(t.Context(), id, 1, &heronv1.Facts{Hostname: "old"}); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "old-config.db")
	if err := s.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", config)
	if err != nil {
		t.Fatal(err)
	}
	removeV41Config(t, db)
	removeV40Config(t, db)
	removeV39Config(t, db)
	for _, q := range []string{"ALTER TABLE node DROP COLUMN rented", "DROP TABLE cleanup_job", "ALTER TABLE node DROP COLUMN traffic_quota_bytes", "ALTER TABLE node DROP COLUMN traffic_quota_mode", "DROP TABLE probe_cert_presented", "DROP TABLE probe_cert", "ALTER TABLE probe_task DROP COLUMN cert_spki_sha256", "ALTER TABLE probe_task DROP COLUMN config_id", "ALTER TABLE probe_task DROP COLUMN dns_server", "ALTER TABLE node_facts DROP COLUMN execution", "ALTER TABLE node_facts DROP COLUMN facts_rev", "ALTER TABLE node_facts DROP COLUMN diagnostics", "ALTER TABLE traffic DROP COLUMN net_counter_epoch", "ALTER TABLE node_facts DROP COLUMN network", "ALTER TABLE api_token DROP COLUMN permissions", "ALTER TABLE api_token DROP COLUMN all_nodes", "DROP TABLE api_token_node", "DROP TABLE operation", "ALTER TABLE node DROP COLUMN maintenance", "ALTER TABLE node DROP COLUMN public_remark", "ALTER TABLE alert_event DROP COLUMN silenced", "ALTER TABLE alert_state DROP COLUMN fired_silenced", "DROP TABLE silence", "DROP TABLE silence_node", "DROP TABLE silence_tag", "UPDATE snapshot_meta SET schema_version=22"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
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
	defer restored.Close()
	n, err := restored.GetNode(t.Context(), id)
	if err != nil || n.Facts == nil || n.Facts.Hostname != "old" || n.Facts.Network != nil {
		t.Fatalf("old snapshot facts=%v err=%v", n.Facts, err)
	}
}
