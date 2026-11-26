package store

import (
	"bytes"
	"database/sql"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/metric"
)

// schemaV58 冻结 v58 的完整 DDL：v57 加上告警事件的单位一列。
var schemaV58 = append(slices.Clone(schemaV57),
	`ALTER TABLE alert_event ADD COLUMN value_unit TEXT NOT NULL DEFAULT 'unknown'`,
)

// removeV58Config 把当前配置库或快照退回 v57 的 alert_event 结构，供从旧版本起步的迁移与恢复测试使用。
func removeV58Config(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("ALTER TABLE alert_event DROP COLUMN value_unit"); err != nil {
		t.Fatal(err)
	}
}

// 系统事件的 transition 全集：与 SystemEventKind 的分支逐个对应，迁移 58 按这张表写 n/a。
var systemTransitions = []Transition{TransitionLoginSuccess, TransitionLoginLocked, TransitionLoginFailed, TransitionAuthChanged,
	TransitionBackupSuccess, TransitionBackupRestored, TransitionBackupFailed, TransitionBackupRecovered, TransitionBackupDisabled,
	TransitionTrafficReport}

func TestSystemTransitionsMatchSystemEventKind(t *testing.T) {
	t.Parallel()
	for _, tr := range systemTransitions {
		if _, ok := SystemEventKind(tr); !ok {
			t.Errorf("%q is listed as a system transition but SystemEventKind rejects it", tr)
		}
	}
	for _, tr := range []Transition{TransitionFiring, TransitionRecovered} {
		if _, ok := SystemEventKind(tr); ok {
			t.Errorf("rule transition %q is classified as a system event", tr)
		}
	}
}

// 写入口：规则事件只收单位词表与 n/a，不收 unknown 与表外的名字；系统事件一律落成 0 与 n/a，带了值的被拒。
func TestRecordEventValueUnitAdmission(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	ctx := t.Context()
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	accepted := append(slices.Clone(metric.Units()), ValueUnitNotApplicable)
	for i, unit := range accepted {
		state := StateFiring
		tr := TransitionFiring
		if i%2 == 1 {
			state, tr = StateOK, TransitionRecovered
		}
		ev, err := s.RecordTransition(ctx, r.ID, ids[0], state, "", time.Time{}, AlertEvent{Transition: tr, At: s.clk.Now(), Value: 3, ValueUnit: unit}, nil)
		if err != nil {
			t.Fatalf("unit %q rejected: %v", unit, err)
		}
		events, err := s.ListAlertEvents(ctx, 0, 0, 1)
		if err != nil || len(events) != 1 || events[0].ID != ev.ID || events[0].ValueUnit != unit {
			t.Fatalf("unit %q read back as %+v err=%v", unit, events, err)
		}
	}
	for _, unit := range []string{ValueUnitUnknown, "Mbps", "ms", "percent "} {
		_, err := s.RecordTransition(ctx, r.ID, ids[0], StateFiring, "", time.Time{}, AlertEvent{Transition: TransitionFiring, At: s.clk.Now(), Value: 3, ValueUnit: unit}, nil)
		if err == nil || !strings.Contains(err.Error(), "value unit") {
			t.Fatalf("unit %q err=%v, want a value unit rejection", unit, err)
		}
	}
	before := len(mustEvents(t, s))

	login, err := s.RecordLoginEvent(ctx, AlertEvent{Transition: TransitionLoginSuccess, At: s.clk.Now(), Summary: "登录"})
	if err != nil || login.ValueUnit != ValueUnitNotApplicable || login.Value != 0 {
		t.Fatalf("login event = %+v err=%v, want value 0 with unit n/a", login, err)
	}
	if _, err := s.RecordLoginEvent(ctx, AlertEvent{Transition: TransitionLoginLocked, At: s.clk.Now(), Value: 5}); err == nil {
		t.Fatal("system event with a value was accepted")
	}
	backup, err := s.RecordBackupEvent(ctx, TransitionBackupFailed, "备份失败", s.clk.Now())
	if err != nil || backup.ValueUnit != ValueUnitNotApplicable {
		t.Fatalf("backup event = %+v err=%v", backup, err)
	}
	if err := s.RecordBackupSuccess(ctx, "config"); err != nil {
		t.Fatal(err)
	}
	events := mustEvents(t, s)
	if len(events) != before+3 {
		t.Fatalf("got %d events, want %d", len(events), before+3)
	}
	for _, ev := range events[:3] {
		if ev.ValueUnit != ValueUnitNotApplicable || ev.Value != 0 {
			t.Errorf("system event %q stored value %v %q, want 0 n/a", ev.Transition, ev.Value, ev.ValueUnit)
		}
	}
}

func mustEvents(t *testing.T, s *Store) []AlertEvent {
	t.Helper()
	events, err := s.ListAlertEvents(t.Context(), 0, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// v58 回填：系统事件写 n/a；规则仍在的按规则身份推出单位（与 alert.valueUnit 同一张表），到期规则值为 0 的恢复是清除
// 到期日、写 n/a；规则已删除的保持 unknown。回填后的每一行都满足恢复预检的判定。
func TestMigrationFromV57BackfillsEventValueUnit(t *testing.T) {
	t.Parallel()
	type rule struct {
		id                     int64
		kind, metric, resource string
	}
	rules := []rule{
		{1, "offline", "", ""},
		{2, "probe", "loss_pct", ""},
		{3, "probe", "rtt_ms", ""},
		{4, "resource", "", "memory_used_pct"},
		{5, "resource", "", "disk_used_pct"},
		{6, "resource", "", "cpu_pct"},
		{7, "resource", "", "load1_per_core"},
		{8, "resource", "", "net_rx_bps"},
		{9, "resource", "", "net_tx_bps"},
		{10, "traffic", "", ""},
		{11, "expiry", "", ""},
		{12, "cert_expiry", "", ""},
	}
	type event struct {
		rule       int64
		transition string
		summary    string
		value      float64
		want       string
	}
	events := []event{
		{1, "firing", "", 120, "seconds"},
		{2, "firing", "", 30, "percent"},
		{3, "recovered", "", 80, "milliseconds"},
		{4, "firing", "", 91, "percent"},
		{5, "firing", "", 95, "percent"},
		{6, "firing", "", 99, "percent"},
		{7, "firing", "", 1.5, ""},
		{8, "firing", "", 12500000, "bytes/s"},
		{9, "recovered", "", 125000, "bytes/s"},
		{10, "firing", "", 90, "percent"},
		{10, "recovered", "节点 n 本周期流量用量 0.00% 已低于阈值 80.00%（规则 流量）", 0, "percent"},
		{10, "recovered", "节点 n 已清除流量配额（规则 流量）", 0, "n/a"},
		{11, "firing", "", 0, "days"},
		{11, "firing", "", -3, "days"},
		{11, "recovered", "", 20, "days"},
		{11, "recovered", "", 0, "n/a"},
		{12, "firing", "", 5, "days"},
		{12, "recovered", "", 40, "days"},
		{99, "firing", "", 12500000, "unknown"},
		{99, "recovered", "", 0, "unknown"},
	}
	for _, tr := range systemTransitions {
		events = append(events, event{0, string(tr), "", 0, "n/a"})
	}
	migrated := migrateFrom(t, 57, func(t *testing.T, db *sql.DB) {
		for _, r := range rules {
			if _, err := db.Exec("INSERT INTO alert_rule (id, name, kind, created_at, metric, resource_metric) VALUES (?, ?, ?, 1, NULLIF(?, ''), NULLIF(?, ''))", r.id, r.kind, r.kind, r.metric, r.resource); err != nil {
				t.Fatal(err)
			}
		}
		for i, e := range events {
			node := int64(1)
			if e.rule == 0 {
				node = 0
			}
			if _, err := db.Exec("INSERT INTO alert_event (id, rule_id, node_id, transition, at, summary, value) VALUES (?, ?, ?, ?, 1, ?, ?)", i+1, e.rule, node, e.transition, e.summary, e.value); err != nil {
				t.Fatal(err)
			}
		}
	})
	got := map[int64]AlertEvent{}
	for _, ev := range mustEvents(t, migrated) {
		got[ev.ID] = ev
	}
	if len(got) != len(events) {
		t.Fatalf("got %d events after migration, want %d", len(got), len(events))
	}
	for i, e := range events {
		ev := got[int64(i+1)]
		if ev.ValueUnit != e.want || ev.Value != e.value {
			t.Errorf("event %d (rule %d %s %v) migrated to %v %q, want %v %q", i+1, e.rule, e.transition, e.value, ev.Value, ev.ValueUnit, e.value, e.want)
		}
		if err := checkStoredEventValueUnit(ev.Transition, ev.Value, ev.ValueUnit); err != nil {
			t.Errorf("event %d: migrated unit fails the stored-event check: %v", i+1, err)
		}
	}
}

// 恢复：当前版本快照里事件的单位原样搬回；v57 快照经迁移 58 回填；单位非法的快照在任何写入之前被拒、目标库不变。
func TestEventValueUnitSurvivesConfigSnapshot(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	ctx := t.Context()
	offline := saveRule(t, s, AlertRule{Kind: KindOffline})
	if _, err := s.RecordTransition(ctx, offline.ID, ids[0], StateFiring, "", time.Time{}, AlertEvent{Transition: TransitionFiring, At: s.clk.Now(), Value: 75, ValueUnit: metric.UnitSeconds}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordLoginEvent(ctx, AlertEvent{Transition: TransitionLoginSuccess, At: s.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	snapshot := func(t *testing.T, mutate func(*sql.DB)) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.db")
		if err := s.SnapshotConfig(ctx, path); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		mutate(db)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	restoredUnits := func(t *testing.T, config string) map[Transition]string {
		t.Helper()
		target := filepath.Join(t.TempDir(), "restored.db")
		if _, err := Restore(ctx, target, config, "", "", s.clk.Now(), slog.Default()); err != nil {
			t.Fatal(err)
		}
		restored, err := Open(target, s.clk, slog.Default(), RequireCurrentSchema)
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		out := map[Transition]string{}
		for _, ev := range mustEvents(t, restored) {
			out[ev.Transition] = ev.ValueUnit
		}
		return out
	}
	want := map[Transition]string{TransitionFiring: metric.UnitSeconds, TransitionLoginSuccess: ValueUnitNotApplicable, TransitionBackupRestored: ValueUnitNotApplicable}

	current := snapshot(t, func(*sql.DB) {})
	if got := restoredUnits(t, current); !maps.Equal(got, want) {
		t.Fatalf("restored units %v, want %v", got, want)
	}
	old := snapshot(t, func(db *sql.DB) {
		removeV58Config(t, db)
		if _, err := db.Exec("UPDATE snapshot_meta SET schema_version = 57"); err != nil {
			t.Fatal(err)
		}
	})
	if got := restoredUnits(t, old); !maps.Equal(got, want) {
		t.Fatalf("restored v57 units %v, want %v", got, want)
	}

	for _, tc := range []struct{ name, query, want string }{
		{"rule_event_outside_vocabulary", "UPDATE alert_event SET value_unit = 'Mbps' WHERE transition = 'firing'", `value unit "Mbps" is not in the unit vocabulary`},
		{"system_event_with_unit", "UPDATE alert_event SET value_unit = 'seconds' WHERE transition = 'login_success'", "carries no value"},
		{"system_event_unknown", "UPDATE alert_event SET value_unit = 'unknown' WHERE transition = 'login_success'", "carries no value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := snapshot(t, func(db *sql.DB) {
				if _, err := db.Exec(tc.query); err != nil {
					t.Fatal(err)
				}
			})
			seed, target := openAt(t)
			seed.Close()
			before, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			_, restoreErr := Restore(ctx, target, config, "", "", s.clk.Now(), slog.Default())
			if restoreErr == nil || !strings.Contains(restoreErr.Error(), "config alert event") || !strings.Contains(restoreErr.Error(), tc.want) {
				t.Fatalf("restore err=%v, want a rejection naming the alert event and %q", restoreErr, tc.want)
			}
			after, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("rejected restore changed the target database: %v", err)
			}
		})
	}
	// 规则事件的 unknown 是迁移写下的合法历史，恢复照收。
	legacy := snapshot(t, func(db *sql.DB) {
		if _, err := db.Exec("UPDATE alert_event SET value_unit = 'unknown' WHERE transition = 'firing'"); err != nil {
			t.Fatal(err)
		}
	})
	if got := restoredUnits(t, legacy); got[TransitionFiring] != ValueUnitUnknown {
		t.Fatalf("restored legacy rule event unit %q, want unknown", got[TransitionFiring])
	}
}
