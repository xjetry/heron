package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
)

func openTraffic(t *testing.T) (*Store, int64) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	st, err := Open(filepath.Join(t.TempDir(), "t.db"), clk, slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, _, err := st.CreateNode(context.Background(), NewNode{Name: "n"}, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	return st, id
}

func perInterface(counters ...TrafficCounter) []TrafficCounter { return counters }

func trafficBaselineRows(t *testing.T, st *Store, id int64) int {
	t.Helper()
	var n int
	if err := st.r.QueryRow("SELECT count(*) FROM traffic_baseline WHERE node_id = ?", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func loadOne(t *testing.T, st *Store) TrafficRecord {
	t.Helper()
	got, err := st.LoadTraffic(t.Context())
	if err != nil || len(got) != 1 {
		t.Fatalf("load: %+v %v", got, err)
	}
	return got[0]
}

// 三种基线形态都能写入、读回，后写的记录整体替换先前的基线行（形态之间来回切换不留旧行）；配置快照恢复后原样回来。
func TestTrafficRoundTripKeepsEachBaselineForm(t *testing.T) {
	t.Parallel()
	st, id := openTraffic(t)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rec := TrafficRecord{NodeID: id, BootID: "b1", TotalRx: 1000, TotalTx: 2000, PeriodRx: 10, PeriodTx: 20, PeriodStart: start}
	for _, form := range []struct {
		name     string
		epoch    string
		baseline []TrafficCounter
	}{
		{"per_interface", "", perInterface(TrafficCounter{"eth0", 1, 2}, TrafficCounter{"eth1", 3, 4}, TrafficCounter{"wlan0", 5, 6})},
		{"totals", strings.Repeat("a", 64), []TrafficCounter{{Rx: 100, Tx: 200}}},
		{"legacy_totals", "", []TrafficCounter{{Rx: 150, Tx: 250}}},
		{"none", "", nil},
		{"per_interface_again", "", perInterface(TrafficCounter{"eth0", 7, 8})},
	} {
		rec.NetCounterEpoch, rec.Baseline = form.epoch, form.baseline
		rec.TotalRx++
		if written, err := st.WriteTraffic(t.Context(), []TrafficRecord{rec}); err != nil || len(written) != 1 || written[0] != id {
			t.Fatalf("%s: write: %v written=%v", form.name, err, written)
		}
		if got := loadOne(t, st); !reflect.DeepEqual(got, rec) {
			t.Fatalf("%s: got %+v, want %+v", form.name, got, rec)
		}
		if n := trafficBaselineRows(t, st, id); n != len(form.baseline) {
			t.Fatalf("%s: %d baseline rows in the table, want %d", form.name, n, len(form.baseline))
		}
	}
	config, metrics := filepath.Join(t.TempDir(), "config.db"), filepath.Join(t.TempDir(), "metrics.db")
	if err := st.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := st.SnapshotMetrics(t.Context(), metrics); err != nil {
		t.Fatal(err)
	}
	for _, layers := range [][2]string{{config, ""}, {config, metrics}} {
		target := filepath.Join(t.TempDir(), "restored.db")
		if _, err := Restore(t.Context(), target, layers[0], layers[1], "", time.Now(), slog.Default()); err != nil {
			t.Fatal(err)
		}
		restored, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
		if err != nil {
			t.Fatal(err)
		}
		got := loadOne(t, restored)
		restored.Close()
		if !reflect.DeepEqual(got, rec) {
			t.Fatalf("restored %v: got %+v, want %+v", layers, got, rec)
		}
	}
}

// 写入口按 CheckTrafficBaseline 拒收不合法的形态，整批不写：同批里合法的记录也不落盘，库里保留原样。
func TestTrafficWriteRejectsInvalidBaselineAtomically(t *testing.T) {
	t.Parallel()
	st, id := openTraffic(t)
	rec := TrafficRecord{NodeID: id, BootID: "boot", NetCounterEpoch: strings.Repeat("a", 64), Baseline: []TrafficCounter{{Rx: 1, Tx: 2}}, TotalRx: 100, PeriodStart: time.Unix(0, 0).UTC()}
	if _, err := st.WriteTraffic(t.Context(), []TrafficRecord{rec}); err != nil {
		t.Fatal(err)
	}
	var full []TrafficCounter
	for i := range agentwire.MaxNetInterfaces + 1 {
		full = append(full, TrafficCounter{Name: fmt.Sprintf("veth%03d", i)})
	}
	for _, tc := range []struct {
		name     string
		epoch    string
		baseline []TrafficCounter
		err      string
	}{
		{"invalid_epoch", "short", []TrafficCounter{{}}, "net_counter_epoch"},
		{"epoch_without_baseline", strings.Repeat("a", 64), nil, "must be empty without a baseline"},
		{"epoch_with_interfaces", strings.Repeat("a", 64), perInterface(TrafficCounter{Name: "eth0"}), "must be empty for a per-interface baseline"},
		{"mixed_forms", "", perInterface(TrafficCounter{}, TrafficCounter{Name: "eth0"}), "interface names must be non-empty"},
		{"unsorted", "", perInterface(TrafficCounter{Name: "eth1"}, TrafficCounter{Name: "eth0"}), "unique and sorted"},
		{"duplicate", "", perInterface(TrafficCounter{Name: "eth0"}, TrafficCounter{Name: "eth0"}), "unique and sorted"},
		{"control_character", "", perInterface(TrafficCounter{Name: "eth\x000"}), "interface names must be non-empty"},
		{"too_many", "", full, "at most 64 interfaces; got 65"},
		{"negative", "", perInterface(TrafficCounter{Name: "eth0", Rx: -1}), "non-negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valid := rec
			valid.TotalRx = 200
			invalid := rec
			invalid.NetCounterEpoch, invalid.Baseline = tc.epoch, tc.baseline
			if _, err := st.WriteTraffic(t.Context(), []TrafficRecord{valid, invalid}); err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("write error = %v, want %q", err, tc.err)
			}
			if got := loadOne(t, st); !reflect.DeepEqual(got, rec) {
				t.Fatalf("failed batch changed the stored record: %+v", got)
			}
		})
	}
}

// 读库与恢复预检用同一判定拒收手工改库或损坏快照留下的不合法形态，包括没有 traffic 行的基线行；被拒的恢复不改目标库。
func TestTrafficReadAndRestoreRejectInvalidBaseline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		corrupt []string
		err     string
	}{
		{"invalid_epoch", []string{"UPDATE traffic SET net_counter_epoch='invalid'"}, "net_counter_epoch"},
		{"mixed_forms", []string{"UPDATE traffic SET net_counter_epoch=''", "INSERT INTO traffic_baseline VALUES (%[1]d, 'eth0', 1, 1)"}, "interface names must be non-empty"},
		{"epoch_with_interfaces", []string{"DELETE FROM traffic_baseline", "INSERT INTO traffic_baseline VALUES (%[1]d, 'eth0', 1, 1)"}, "must be empty for a per-interface baseline"},
		{"epoch_without_baseline", []string{"DELETE FROM traffic_baseline"}, "must be empty without a baseline"},
		{"bad_name", []string{"DELETE FROM traffic_baseline", "UPDATE traffic SET net_counter_epoch=''", "INSERT INTO traffic_baseline VALUES (%[1]d, 'eth' || char(10), 1, 1)"}, "interface names must be non-empty"},
		{"orphan_rows", []string{"INSERT INTO traffic_baseline VALUES (%[1]d + 1000, '', 1, 1)"}, "traffic baseline rows without a traffic row"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, id := openTraffic(t)
			rec := TrafficRecord{NodeID: id, BootID: "boot", NetCounterEpoch: strings.Repeat("a", 64), Baseline: []TrafficCounter{{Rx: 1, Tx: 2}}, TotalRx: 100, PeriodStart: time.Unix(0, 0).UTC()}
			if _, err := st.WriteTraffic(t.Context(), []TrafficRecord{rec}); err != nil {
				t.Fatal(err)
			}
			if err := st.write(t.Context(), func(tx *sql.Tx) error {
				for _, q := range tc.corrupt {
					if _, err := tx.Exec(strings.ReplaceAll(q, "%[1]d", fmt.Sprint(id))); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.LoadTraffic(t.Context()); err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("load error = %v, want %q", err, tc.err)
			}
			config := filepath.Join(t.TempDir(), "config.db")
			if err := st.SnapshotConfig(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			target, err := Open(filepath.Join(t.TempDir(), "target.db"), clock.Real(), slog.Default(), MigrateSchema)
			if err != nil {
				t.Fatal(err)
			}
			targetID, _, err := target.CreateNode(t.Context(), NewNode{Name: "kept"}, []byte{2})
			if err != nil {
				t.Fatal(err)
			}
			kept := rec
			kept.NodeID = targetID
			if _, err := target.WriteTraffic(t.Context(), []TrafficRecord{kept}); err != nil {
				t.Fatal(err)
			}
			if err := target.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(t.Context(), target.path, config, "", "", time.Now(), slog.Default()); err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("restore error = %v, want %q", err, tc.err)
			}
			reopened, err := Open(target.path, clock.Real(), slog.Default(), RequireCurrentSchema)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if got := loadOne(t, reopened); !reflect.DeepEqual(got, kept) {
				t.Fatalf("rejected restore changed the target: %+v, want %+v", got, kept)
			}
		})
	}
}

// 快照不经 WriteTraffic，表上的 CHECK 约束也不随 CREATE TABLE AS 进快照：负计数只能由恢复预检挡住。
func TestTrafficRestoreRejectsNegativeSnapshotCounters(t *testing.T) {
	t.Parallel()
	st, id := openTraffic(t)
	if _, err := st.WriteTraffic(t.Context(), []TrafficRecord{{NodeID: id, Baseline: perInterface(TrafficCounter{"eth0", 1, 2}), PeriodStart: time.Unix(0, 0)}}); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := st.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", config)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := db.Exec("UPDATE traffic_baseline SET tx = -5"); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("corruption touched %d rows, want 1", n)
	}
	db.Close()
	if _, err := Restore(t.Context(), filepath.Join(t.TempDir(), "restored.db"), config, "", "", time.Now(), slog.Default()); err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("restore error = %v, want the non-negative rule", err)
	}
}

func TestTrafficWriteSkipsDeletedNodes(t *testing.T) {
	t.Parallel()
	st, id := openTraffic(t)
	if err := st.DeleteNode(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	written, err := st.WriteTraffic(t.Context(), []TrafficRecord{{NodeID: id, BootID: "b", Baseline: perInterface(TrafficCounter{"eth0", 1, 2}), PeriodStart: time.Unix(0, 0)}})
	if err != nil || len(written) != 0 {
		t.Fatalf("written=%v err=%v, want empty/nil", written, err)
	}
	got, _ := st.LoadTraffic(t.Context())
	if len(got) != 0 || trafficBaselineRows(t, st, id) != 0 {
		t.Fatalf("rows written for a deleted node: %+v", got)
	}
}

func TestTrafficResetDaysComeFromNodeTable(t *testing.T) {
	t.Parallel()
	st, id := openTraffic(t)
	err := st.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE node SET traffic_reset_day = 15 WHERE id = ?", id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	days, err := st.TrafficResetDays(t.Context())
	if err != nil || days[id] != 15 {
		t.Fatalf("days = %v err=%v, want {%d: 15}", days, err, id)
	}
}
