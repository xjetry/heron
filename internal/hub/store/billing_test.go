package store

import (
	"database/sql"
	"errors"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"
)

// v8 的完整 DDL：v7 加上 setting 表。
var schemaV8 = append(slices.Clone(schemaV7), "CREATE TABLE setting (\n  key TEXT PRIMARY KEY,\n  value TEXT NOT NULL\n)")

// 旧库里已有的节点、规则与状态升级后取列默认值：没有计费信息，规则没有提前天数，状态没有触发时的到期日；
// 升级后的库能照常写入新列。
func TestMigrationFromV8MatchesFreshSchemaAndKeepsRows(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 8, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		for _, stmt := range []string{
			"INSERT INTO alert_rule (id, name, kind, enabled, all_nodes, created_at) VALUES (3, 'r', 'offline', 1, 1, 1)",
			"INSERT INTO alert_state (rule_id, node_id, state, since_at) VALUES (3, 7, 'firing', 1)",
		} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
	})
	if v := userVersion(t, migrated.r); v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	n, err := migrated.GetNode(t.Context(), 7)
	if err != nil || n.Name != "kept" || n.Billing != (Billing{}) {
		t.Fatalf("node after migration: %+v %v", n, err)
	}
	rules, err := migrated.ListAlertRules(t.Context())
	if err != nil || len(rules) != 1 || rules[0].ID != 3 || rules[0].DaysBefore != 0 {
		t.Fatalf("rules after migration: %+v %v", rules, err)
	}
	wantStates := []StateRow{{RuleID: 3, NodeID: 7, State: StateFiring, SinceAt: time.Unix(1, 0).UTC(), FiredAt: time.Unix(1, 0).UTC()}}
	if states, err := migrated.ListAlertStates(t.Context()); err != nil || !reflect.DeepEqual(states, wantStates) {
		t.Fatalf("states after migration: %+v %v", states, err)
	}
	b := Billing{Price: "5", Currency: "EUR", Cycle: CycleYearly, ExpiresOn: "2027-01-01", AutoRenew: true}
	if _, err := migrated.UpdateNode(t.Context(), 7, NodeEdit{Name: "kept", TrafficResetDay: 1, Billing: b}); err != nil {
		t.Fatal(err)
	}
	if n, err := migrated.GetNode(t.Context(), 7); err != nil || n.Billing != b {
		t.Fatalf("billing on a migrated database: %+v %v", n, err)
	}
}

// 计费随建节点一次写入：CreateNode 落库的五项与 UpdateNode 写进去的读法一致；空值创建没有计费。
func TestCreateNodeStoresBilling(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	full := Billing{Price: "12.50", Currency: "USD", Cycle: CycleMonthly, ExpiresOn: "2026-10-01", AutoRenew: true}
	id, _, err := s.CreateNode(ctx, NewNode{Name: "billed", Billing: full}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := s.CreateNode(ctx, NewNode{Name: "plain"}, hash(2))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.GetNode(ctx, id); err != nil || n.Billing != full {
		t.Fatalf("billed node read back %+v %v, want %+v", n.Billing, err, full)
	}
	if n, err := s.GetNode(ctx, plain); err != nil || n.Billing != (Billing{}) {
		t.Fatalf("plain node read back %+v %v, want zero", n.Billing, err)
	}
}

// 计费五项随 UpdateNode 整体替换：零值即清除。billingChanged 只看这五项与库内原值是否不同，别的字段变不算。
func TestUpdateNodeReplacesBillingAndReportsChange(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	edit := NodeEdit{Name: "n", TrafficResetDay: 1}
	update := func(e NodeEdit) bool {
		t.Helper()
		changed, err := s.UpdateNode(ctx, id, e)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if update(edit) {
		t.Fatal("empty billing over empty billing reported a change")
	}
	full := Billing{Price: "12.50", Currency: "USD", Cycle: CycleMonthly, ExpiresOn: "2026-10-01", AutoRenew: true}
	edit.Billing = full
	if !update(edit) {
		t.Fatal("setting billing reported no change")
	}
	for _, read := range []func() ([]Node, error){
		func() ([]Node, error) { n, err := s.GetNode(ctx, id); return []Node{n}, err },
		func() ([]Node, error) { return s.ListNodes(ctx) },
	} {
		nodes, err := read()
		if err != nil || len(nodes) != 1 || nodes[0].Billing != full {
			t.Fatalf("read back %+v %v, want %+v", nodes, err, full)
		}
	}
	edit.Name, edit.Public = "renamed", true
	if update(edit) {
		t.Fatal("changing only name and public reported a billing change")
	}
	if nodes, err := s.ListPublicNodes(ctx); err != nil || len(nodes) != 1 || nodes[0].Billing != full {
		t.Fatalf("public nodes %+v %v", nodes, err)
	}
	edit.Billing.AutoRenew = false
	if !update(edit) {
		t.Fatal("turning off auto renew reported no change")
	}
	edit.Billing.Currency = "EUR"
	if !update(edit) {
		t.Fatal("changing only the currency reported no change")
	}
	edit.Billing.Cycle = CycleYearly
	if !update(edit) {
		t.Fatal("changing only the cycle reported no change")
	}
	edit.Billing.ExpiresOn = "2026-11-01"
	if !update(edit) {
		t.Fatal("changing only the expiry date reported no change")
	}
	edit.Billing.Price = "13.00"
	if !update(edit) {
		t.Fatal("changing only the price reported no change")
	}
	edit.Billing = Billing{}
	if !update(edit) {
		t.Fatal("clearing billing reported no change")
	}
	if n, err := s.GetNode(ctx, id); err != nil || n.Billing != (Billing{}) {
		t.Fatalf("cleared billing read back %+v %v", n.Billing, err)
	}
	if changed, err := s.UpdateNode(ctx, 999, edit); !errors.Is(err, ErrNotFound) || changed {
		t.Fatalf("unknown id: %v %v, want ErrNotFound and no change", changed, err)
	}
}

// RenewExpiry 只在该行仍是推后所依据的取值时写入：到期日、周期或自动续期任一已被改过都不写。
func TestRenewExpiryWritesOnlyOverTheValuesItWasComputedFrom(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	set := func(b Billing) {
		t.Helper()
		if _, err := s.UpdateNode(ctx, id, NodeEdit{Name: "n", TrafficResetDay: 1, Billing: b}); err != nil {
			t.Fatal(err)
		}
	}
	expiresOn := func() string {
		t.Helper()
		n, err := s.GetNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return n.Billing.ExpiresOn
	}
	monthly := Billing{Cycle: CycleMonthly, ExpiresOn: "2026-01-31", AutoRenew: true}
	for _, c := range []struct {
		name string
		row  Billing
		from string
	}{
		{"stale date", monthly, "2026-01-30"},
		{"cycle changed", Billing{Cycle: CycleYearly, ExpiresOn: "2026-01-31", AutoRenew: true}, "2026-01-31"},
		{"auto renew off", Billing{Cycle: CycleMonthly, ExpiresOn: "2026-01-31"}, "2026-01-31"},
	} {
		set(c.row)
		renewed, err := s.RenewExpiry(ctx, id, CycleMonthly, c.from, "2026-02-28")
		if err != nil || renewed || expiresOn() != "2026-01-31" {
			t.Fatalf("%s: renewed=%v err=%v expires_on=%s", c.name, renewed, err, expiresOn())
		}
	}
	set(monthly)
	renewed, err := s.RenewExpiry(ctx, id, CycleMonthly, "2026-01-31", "2026-02-28")
	if err != nil || !renewed || expiresOn() != "2026-02-28" {
		t.Fatalf("renew: renewed=%v err=%v expires_on=%s", renewed, err, expiresOn())
	}
	if renewed, err := s.RenewExpiry(ctx, 999, CycleMonthly, "2026-01-31", "2026-02-28"); err != nil || renewed {
		t.Fatalf("unknown node: renewed=%v err=%v", renewed, err)
	}
}

// 手动续期与自动续期共用条件写的谓词，但不要求开着自动续期：没开它的节点照样写入；周期或到期日在读到之后被改过时
// 不写并返回 ErrPrecondition，节点不存在返回 ErrNotFound。只写到期日，其余计费字段原样保留。
func TestRenewNodeBillingWritesOverTheValuesItWasComputedFromWithoutAutoRenew(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	set := func(b Billing) {
		t.Helper()
		if _, err := s.UpdateNode(ctx, id, NodeEdit{Name: "n", TrafficResetDay: 1, Billing: b}); err != nil {
			t.Fatal(err)
		}
	}
	billing := func() Billing {
		t.Helper()
		n, err := s.GetNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return n.Billing
	}
	manual := Billing{Price: "9.90", Currency: "USD", Cycle: CycleMonthly, ExpiresOn: "2026-01-31"}
	for _, c := range []struct {
		name string
		row  Billing
		from string
	}{
		{"stale date", manual, "2026-01-30"},
		{"cycle changed", Billing{Price: "9.90", Currency: "USD", Cycle: CycleYearly, ExpiresOn: "2026-01-31"}, "2026-01-31"},
	} {
		set(c.row)
		err := s.RenewNodeBilling(ctx, id, CycleMonthly, c.from, "2026-02-28")
		if !errors.Is(err, ErrPrecondition) || billing() != c.row {
			t.Fatalf("%s: err=%v billing=%+v, want ErrPrecondition and %+v", c.name, err, billing(), c.row)
		}
	}
	set(manual)
	if err := s.RenewNodeBilling(ctx, id, CycleMonthly, "2026-01-31", "2026-02-28"); err != nil {
		t.Fatalf("renew with auto renew off: %v", err)
	}
	want := manual
	want.ExpiresOn = "2026-02-28"
	if got := billing(); got != want {
		t.Fatalf("after renew: %+v, want %+v", got, want)
	}
	if err := s.RenewNodeBilling(ctx, 999, CycleMonthly, "2026-01-31", "2026-02-28"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown node: %v, want ErrNotFound", err)
	}
}

// days_before 只随到期规则落库；改它不改规则身份，状态保留；换成别的种类（同时清掉 days_before，组合合法）时，
// 列变回 NULL，状态随身份一起清除。
func TestAlertRuleDaysBeforeBelongsToExpiryRules(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	ctx := t.Context()
	r := saveRule(t, s, AlertRule{Name: "到期", Kind: KindExpiry, Enabled: true, AllNodes: true, DaysBefore: 7})
	column := func() sql.NullInt64 {
		t.Helper()
		var v sql.NullInt64
		if err := s.r.QueryRow("SELECT days_before FROM alert_rule WHERE id = ?", r.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := column(); r.DaysBefore != 7 || got != (sql.NullInt64{Int64: 7, Valid: true}) {
		t.Fatalf("saved %d, column %v", r.DaysBefore, got)
	}
	if rules, err := s.ListAlertRules(ctx); err != nil || len(rules) != 1 || rules[0].DaysBefore != 7 || rules[0].Kind != KindExpiry {
		t.Fatalf("listed %+v %v", rules, err)
	}
	if err := s.SetAlertState(ctx, r.ID, ids[0], StateFiring, s.clk.Now(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	r.DaysBefore = 30
	r = saveRule(t, s, r)
	assertAlertRows(t, s, "alert_state", "rule_id = 1", 1)
	r.Kind, r.DaysBefore = KindOffline, 0
	r = saveRule(t, s, r)
	if got := column(); got.Valid {
		t.Fatalf("offline rule kept the days_before column: %v", got)
	}
	assertAlertRows(t, s, "alert_state", "rule_id = 1", 0)
}

// 种类与专用字段的非法组合在存储层就被拒绝，新建与修改都不写入，已有的行保持原样；报错写明字段与约束。
func TestSaveAlertRuleRejectsFieldsOfOtherKinds(t *testing.T) {
	t.Parallel()
	s, _, _, task := alertFixture(t)
	existing := saveRule(t, s, AlertRule{Name: "离线", Kind: KindOffline, Enabled: true, AllNodes: true})
	for _, c := range []struct {
		name  string
		rule  AlertRule
		field string
	}{
		{"offline with task", AlertRule{Kind: KindOffline, TaskID: task}, "task_id"},
		{"offline with metric", AlertRule{Kind: KindOffline, Metric: MetricLossPct}, "metric"},
		{"offline with NaN threshold", AlertRule{Kind: KindOffline, Threshold: math.NaN()}, "threshold"},
		{"offline with for_minutes", AlertRule{Kind: KindOffline, ForMinutes: 3}, "for_minutes"},
		{"offline with days_before", AlertRule{Kind: KindOffline, DaysBefore: 7}, "days_before"},
		{"expiry with threshold", AlertRule{Kind: KindExpiry, DaysBefore: 7, Threshold: 5}, "threshold"},
		{"probe with days_before", AlertRule{Kind: KindProbe, TaskID: task, Metric: MetricLossPct, Threshold: 5, ForMinutes: 3, DaysBefore: 7}, "days_before"},
	} {
		for _, id := range []int64{0, existing.ID} {
			r := c.rule
			r.ID, r.Name, r.Enabled, r.AllNodes = id, "非法", true, true
			var kf KindFieldError
			if _, err := s.SaveAlertRule(t.Context(), r); !errors.As(err, &kf) || kf.Field != c.field {
				t.Fatalf("%s (id %d): err = %v, want KindFieldError on %s", c.name, id, err, c.field)
			}
		}
	}
	rules, err := s.ListAlertRules(t.Context())
	if err != nil || len(rules) != 1 || rules[0].Name != "离线" || rules[0].Kind != KindOffline {
		t.Fatalf("rules after rejected saves: %+v %v", rules, err)
	}
}

// 触发时的到期日只由写入它的那一次给出：每次写都整行替换，恢复与不带事件的写都把它清空，状态行不会带着上一次
// 触发的日期进入下一个状态。它存在库里，重新读状态（hub 重启走的就是这条路）拿到的是同一个值。
func TestAlertStateFiredExpiresOnFollowsEachWrite(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	ctx := t.Context()
	r := saveRule(t, s, AlertRule{Name: "到期", Kind: KindExpiry, Enabled: true, AllNodes: true, DaysBefore: 7})
	fired := func() string {
		t.Helper()
		states, err := s.ListAlertStates(ctx)
		if err != nil || len(states) != 1 {
			t.Fatalf("states = %+v %v", states, err)
		}
		return states[0].FiredExpiresOn
	}
	record := func(state AlertState, firedExpiresOn string, tr Transition) {
		t.Helper()
		if _, err := s.RecordTransition(ctx, r.ID, ids[0], state, firedExpiresOn, time.Time{}, AlertEvent{Transition: tr, At: s.clk.Now()}, nil); err != nil {
			t.Fatal(err)
		}
	}
	record(StateFiring, "2026-10-01", TransitionFiring)
	if got := fired(); got != "2026-10-01" {
		t.Fatalf("after firing: fired_expires_on = %q, want 2026-10-01", got)
	}
	record(StateOK, "", TransitionRecovered)
	if got := fired(); got != "" {
		t.Fatalf("after recovery: fired_expires_on = %q, want empty", got)
	}
	record(StateFiring, "2026-10-01", TransitionFiring)
	if err := s.SetAlertState(ctx, r.ID, ids[0], StateOK, s.clk.Now(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := fired(); got != "" {
		t.Fatalf("after SetAlertState: fired_expires_on = %q, want empty", got)
	}
}
