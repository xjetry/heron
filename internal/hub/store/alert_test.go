package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func alertFixture(t *testing.T) (*Store, []int64, []NotifyChannel, uint64) {
	t.Helper()
	s, _ := open(t)
	var ids []int64
	var channels []NotifyChannel
	for i := range 2 {
		id, _, err := s.CreateNode(t.Context(), NewNode{Name: fmt.Sprint(i)}, hash(byte(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		c, err := s.SaveNotifyChannel(t.Context(), NotifyChannel{Name: fmt.Sprint(i), Kind: ChannelWebhook, Config: `{}`})
		if err != nil {
			t.Fatal(err)
		}
		channels = append(channels, c)
	}
	task, _, err := s.SaveProbeTask(t.Context(), taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{ids[1], ids[0]}})
	if err != nil {
		t.Fatal(err)
	}
	return s, ids, channels, task.Task.Id
}

// newBatches 给每个渠道一个新批次的投递目标：不请求合并，各行自成一批。
func newBatches(channels ...int64) []DeliveryTarget {
	out := make([]DeliveryTarget, len(channels))
	for i, c := range channels {
		out[i] = DeliveryTarget{ChannelID: c}
	}
	return out
}

func saveRule(t *testing.T, s *Store, r AlertRule) AlertRule {
	t.Helper()
	r, err := s.SaveAlertRule(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func recordEvent(t *testing.T, s *Store, rule, node int64, channels []int64) AlertEvent {
	t.Helper()
	ev, err := s.RecordTransition(t.Context(), rule, node, StateFiring, "", time.Time{}, AlertEvent{
		Transition: TransitionFiring, At: s.clk.Now(), Summary: "offline", Value: 42,
	}, newBatches(channels...))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func assertAlertRows(t *testing.T, s *Store, table, where string, want int) {
	t.Helper()
	var got int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE " + where).Scan(&got); err != nil || got != want {
		t.Fatalf("%s WHERE %s count=%d err=%v, want %d", table, where, got, err, want)
	}
}

func TestAlertRuleRoundTripAndScope(t *testing.T) {
	t.Parallel()
	s, ids, cs, task := alertFixture(t)
	a := saveRule(t, s, AlertRule{Name: "offline", Kind: KindOffline, Enabled: true})
	b := saveRule(t, s, AlertRule{Name: "probe", Kind: KindProbe, Enabled: true, TaskID: task,
		Metric: MetricLossPct, Threshold: 25.5, ForMinutes: 3,
		NodeIDs: []int64{ids[1], ids[0], ids[1]}, ChannelIDs: []int64{cs[1].ID, cs[0].ID, cs[1].ID}})
	wantA := AlertRule{ID: 1, Name: "offline", Kind: KindOffline, Enabled: true, CreatedAt: s.clk.Now()}
	wantB := AlertRule{ID: 2, Name: "probe", Kind: KindProbe, Enabled: true, TaskID: task,
		Metric: MetricLossPct, Threshold: 25.5, ForMinutes: 3, NodeIDs: ids,
		ChannelIDs: []int64{cs[0].ID, cs[1].ID}, CreatedAt: s.clk.Now()}
	if !reflect.DeepEqual(a, wantA) || !reflect.DeepEqual(b, wantB) {
		t.Fatalf("saved rules=%+v/%+v, want %+v/%+v", a, b, wantA, wantB)
	}
	got, err := s.ListAlertRules(t.Context())
	if err != nil || !reflect.DeepEqual(got, []AlertRule{wantA, wantB}) {
		t.Fatalf("listed rules=%+v err=%v", got, err)
	}
	wantB.Name, wantB.Enabled, wantB.Metric, wantB.RttMode, wantB.Threshold, wantB.ForMinutes = "changed", false, MetricRttMs, RttThreshold, 100, 5
	wantB.NodeIDs, wantB.ChannelIDs = []int64{ids[1]}, []int64{cs[1].ID}
	b = saveRule(t, s, wantB)
	got, err = s.ListAlertRules(t.Context())
	if err != nil || !reflect.DeepEqual(got, []AlertRule{wantA, wantB}) || !reflect.DeepEqual(b, wantB) {
		t.Fatalf("replaced rules=%+v saved=%+v err=%v", got, b, err)
	}
	wantB.Kind, wantB.TaskID, wantB.Metric, wantB.RttMode, wantB.Threshold, wantB.ForMinutes = KindOffline, 0, "", "", 0, 0
	wantB.NodeIDs, wantB.ChannelIDs = nil, nil
	saveRule(t, s, wantB)
	got, err = s.ListAlertRules(t.Context())
	if err != nil || !reflect.DeepEqual(got, []AlertRule{wantA, wantB}) {
		t.Fatalf("cleared scope=%+v err=%v", got, err)
	}
}

func TestSaveAlertRuleRejectsMissingReferences(t *testing.T) {
	t.Parallel()
	s, _, _, task := alertFixture(t)
	for _, tc := range []struct {
		kind ObjectKind
		r    AlertRule
	}{
		{ObjectNode, AlertRule{Kind: KindOffline, NodeIDs: []int64{999}}},
		{ObjectNotifyChannel, AlertRule{Kind: KindOffline, ChannelIDs: []int64{999}}},
		{ObjectProbeTask, AlertRule{Kind: KindProbe, TaskID: 999}},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			_, err := s.SaveAlertRule(t.Context(), tc.r)
			var missing NotFoundError
			if !errors.Is(err, ErrNotFound) || !errors.As(err, &missing) || missing.Kind != tc.kind || missing.ID != 999 {
				t.Fatalf("reference error=%v, detail=%+v", err, missing)
			}
			assertAlertRows(t, s, "alert_rule", "1", 0)
		})
	}
	r := saveRule(t, s, AlertRule{Name: "keep", Kind: KindProbe, TaskID: task})
	bad := r
	bad.Name, bad.ChannelIDs = "lost", []int64{999}
	if _, err := s.SaveAlertRule(t.Context(), bad); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad update=%v", err)
	}
	got, err := s.ListAlertRules(t.Context())
	if err != nil || !reflect.DeepEqual(got, []AlertRule{r}) {
		t.Fatalf("rejected save changed rule: %+v %v", got, err)
	}
}

func TestDeleteAlertRuleCascadesButKeepsEvents(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline, NodeIDs: ids, ChannelIDs: []int64{cs[0].ID}})
	ev := recordEvent(t, s, r.ID, ids[0], []int64{cs[0].ID})
	if err := s.DeleteAlertRule(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"alert_rule", "alert_rule_node", "alert_rule_channel", "alert_state"} {
		assertAlertRows(t, s, table, "1", 0)
	}
	got, err := s.GetAlertEvent(t.Context(), ev.ID)
	if err != nil || !reflect.DeepEqual(got, ev) {
		t.Fatalf("event not retained: %+v %v", got, err)
	}
}

func TestDeleteNotifyChannelInUse(t *testing.T) {
	t.Parallel()
	s, _, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Name: "bound", Kind: KindOffline, ChannelIDs: []int64{cs[0].ID}})
	assertInUse(t, s.DeleteNotifyChannel(t.Context(), cs[0].ID), ObjectNotifyChannel, cs[0].ID)
	r.ChannelIDs = nil
	saveRule(t, s, r)
	if err := s.DeleteNotifyChannel(t.Context(), cs[0].ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListNotifyChannels(t.Context())
	if err != nil || !reflect.DeepEqual(got, cs[1:]) {
		t.Fatalf("remaining channels=%+v err=%v", got, err)
	}
}

func assertInUse(t *testing.T, err error, kind ObjectKind, id int64) {
	t.Helper()
	var used InUseError
	if !errors.Is(err, ErrInUse) || !errors.As(err, &used) || used.Kind != kind || used.ID != id || !reflect.DeepEqual(used.Rules, []RuleReference{{ID: 1, Name: "bound"}}) || err.Error() != fmt.Sprintf("%s %d is referenced by alert rules: bound (id 1)", kind, id) {
		t.Fatalf("reference deletion error=%v, detail=%+v", err, used)
	}
}

func TestDeleteProbeTaskInUseByRule(t *testing.T) {
	t.Parallel()
	s, _, _, task := alertFixture(t)
	r := saveRule(t, s, AlertRule{Name: "bound", Kind: KindProbe, TaskID: task})
	_, err := s.DeleteProbeTask(t.Context(), task)
	assertInUse(t, err, ObjectProbeTask, int64(task))
	if err := s.DeleteAlertRule(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteProbeTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
}

func TestRecordTransitionWritesStateEventAndDeliveries(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	ev := recordEvent(t, s, r.ID, ids[0], []int64{cs[0].ID, cs[1].ID})
	want := AlertEvent{ID: 1, RuleID: r.ID, NodeID: ids[0], Transition: TransitionFiring,
		At: s.clk.Now(), Summary: "offline", Value: 42, Deliveries: []Delivery{
			{ID: 1, EventID: 1, ChannelID: cs[0].ID, BatchID: 1}, {ID: 2, EventID: 1, ChannelID: cs[1].ID, BatchID: 2},
		}}
	if !reflect.DeepEqual(ev, want) {
		t.Fatalf("transition event=%+v want=%+v", ev, want)
	}
	got, err := s.GetAlertEvent(t.Context(), ev.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("persisted event=%+v err=%v", got, err)
	}
	states, err := s.ListAlertStates(t.Context())
	if err != nil || !reflect.DeepEqual(states, []StateRow{{RuleID: r.ID, NodeID: ids[0], State: StateFiring, SinceAt: ev.At, FiredAt: ev.At}}) {
		t.Fatalf("transition states=%+v err=%v", states, err)
	}
}

func TestRecordTransitionRollsBackOnDeliveryFailure(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TRIGGER reject_delivery BEFORE INSERT ON alert_delivery WHEN NEW.channel_id = 2 BEGIN SELECT RAISE(ABORT, 'delivery rejected'); END")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.RecordTransition(t.Context(), r.ID, ids[0], StateFiring, "", time.Time{}, AlertEvent{At: s.clk.Now()}, newBatches(cs[0].ID, cs[1].ID))
	if err == nil {
		t.Fatal("delivery failure was accepted")
	}
	for _, table := range []string{"alert_state", "alert_event", "alert_delivery"} {
		assertAlertRows(t, s, table, "1", 0)
	}
}

func TestUpdateBatchAndPending(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	ev := recordEvent(t, s, r.ID, ids[0], []int64{cs[0].ID, cs[1].ID})
	for _, attempts := range []int{1, 2, 3} {
		if _, err := s.BeginBatchAttempt(t.Context(), ev.Deliveries[0].BatchID, []int64{ev.Deliveries[0].ID}); err != nil {
			t.Fatal(err)
		}
		done := attempts == MaxDeliveryAttempts
		if err := s.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, DeliveryResult{Done: done, Failure: FailureTransport, Error: "failed"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.PendingBatches(t.Context())
		want := []PendingBatch{{ev.Deliveries[0].BatchID, cs[0].ID}, {ev.Deliveries[1].BatchID, cs[1].ID}}
		if done {
			want = want[1:]
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("pending batches after attempt %d=%v err=%v, want %v", attempts, got, err, want)
		}
		saved, err := s.GetAlertEvent(t.Context(), ev.ID)
		row := Delivery{ID: 1, EventID: ev.ID, ChannelID: cs[0].ID, BatchID: 1, Attempts: attempts, Done: done, Failure: FailureTransport, LastError: "failed"}
		if err != nil || !reflect.DeepEqual(saved.Deliveries, []Delivery{row, ev.Deliveries[1]}) {
			t.Fatalf("rows after attempt %d=%+v err=%v, want %+v", attempts, saved.Deliveries, err, []Delivery{row, ev.Deliveries[1]})
		}
	}
	at := s.clk.Now().Add(time.Minute)
	if _, err := s.BeginBatchAttempt(t.Context(), ev.Deliveries[1].BatchID, []int64{ev.Deliveries[1].ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateBatch(t.Context(), ev.Deliveries[1].BatchID, DeliveryResult{OK: true, Done: true, DeliveredAt: at}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingBatches(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("successful delivery still pending: %+v %v", pending, err)
	}
	got, err := s.GetAlertEvent(t.Context(), ev.ID)
	want := Delivery{ID: 2, EventID: ev.ID, ChannelID: cs[1].ID, BatchID: 2, Attempts: 1, OK: true, Done: true, DeliveredAt: at}
	if err != nil || len(got.Deliveries) != 2 || got.Deliveries[1] != want {
		t.Fatalf("delivered event=%+v err=%v", got, err)
	}
}

func TestDeliveryTerminalStateCannotBeReopened(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	ev := recordEvent(t, s, r.ID, ids[0], []int64{cs[0].ID})
	if err := s.DeleteNotifyChannel(t.Context(), cs[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, DeliveryResult{Failure: FailureHTTPStatus, HTTPStatus: 500, Error: "late HTTP failure"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAlertEvent(t.Context(), ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	d := got.Deliveries[0]
	if !d.Done || d.Failure != FailureChannelDeleted || d.LastError != "" || d.Attempts != 0 {
		t.Fatalf("terminal delivery reopened: %+v", d)
	}
}

// 续投按批次号装填，批次读写按批次号定位：两类查询各走自己的索引，续投不为去重与排序建临时 B 树。
func TestDeliveryBatchQueriesUseIndexes(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for _, tc := range []struct{ query, index string }{
		{selectPendingBatches, "USING COVERING INDEX alert_delivery_pending"},
		{selectDeliveries + " WHERE batch_id = 1 ORDER BY id", "USING INDEX alert_delivery_by_batch (batch_id=?)"},
		// done = 0 让它也能走 (done, batch_id) 的前缀，两个索引都是按批次号定位，不扫全表。
		{"UPDATE alert_delivery SET attempts = attempts + 1 WHERE batch_id = 1 AND done = 0", "batch_id=?)"},
		{"SELECT COUNT(*) FROM alert_delivery WHERE batch_id = 1", "USING COVERING INDEX alert_delivery_by_batch (batch_id=?)"},
	} {
		rows, err := s.r.Query("EXPLAIN QUERY PLAN " + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		var plan string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan += detail
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !strings.Contains(plan, tc.index) || strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("plan for %q = %s, want index %s without a temp b-tree", tc.query, plan, tc.index)
		}
	}
}

func TestListAlertEventsCursor(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	var events []AlertEvent
	for i := range 5 {
		events = append(events, recordEvent(t, s, r.ID, ids[i%2], []int64{cs[0].ID}))
	}
	for _, tc := range []struct {
		node, before int64
		want         []AlertEvent
	}{
		{0, 0, []AlertEvent{events[4], events[3]}},
		{0, events[3].ID, []AlertEvent{events[2], events[1]}},
		{ids[0], 0, []AlertEvent{events[4], events[2]}},
	} {
		got, err := s.ListAlertEvents(t.Context(), tc.node, tc.before, 2)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("cursor node=%d before=%d: got=%+v err=%v want=%+v", tc.node, tc.before, got, err, tc.want)
		}
	}
}

func TestDeleteNodeCleansAlertScopeAndState(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline, NodeIDs: ids})
	ev := recordEvent(t, s, r.ID, ids[0], []int64{cs[0].ID})
	if err := s.SetAlertState(t.Context(), r.ID, ids[1], StatePending, s.clk.Now(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode(t.Context(), ids[0]); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"alert_rule_node", "alert_state"} {
		assertAlertRows(t, s, table, fmt.Sprintf("node_id = %d", ids[0]), 0)
		assertAlertRows(t, s, table, fmt.Sprintf("node_id = %d", ids[1]), 1)
	}
	assertAlertRows(t, s, "alert_rule", "1", 1)
	got, err := s.GetAlertEvent(t.Context(), ev.ID)
	if err != nil || !reflect.DeepEqual(got, ev) {
		t.Fatalf("node deletion erased event: %+v %v", got, err)
	}
}

func TestNodeOfflineGraceRoundTrip(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	for _, grace := range []int{90, 0} {
		if _, err := s.UpdateNode(t.Context(), ids[0], NodeEdit{Name: "n", TrafficResetDay: 1, OfflineGraceS: grace}); err != nil {
			t.Fatal(err)
		}
		var raw sql.NullInt64
		if err := s.r.QueryRow("SELECT offline_grace_s FROM node WHERE id = ?", ids[0]).Scan(&raw); err != nil || raw.Valid != (grace != 0) || raw.Int64 != int64(grace) {
			t.Fatalf("grace column=%v err=%v want=%d", raw, err, grace)
		}
		n, err := s.GetNode(t.Context(), ids[0])
		if err != nil || n.OfflineGraceS != grace {
			t.Fatalf("grace node=%+v err=%v want=%d", n, err, grace)
		}
	}
}

func TestSaveAlertRulePrunesStatesWithScope(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	a := saveRule(t, s, AlertRule{Kind: KindOffline, Enabled: true, NodeIDs: ids})
	b := saveRule(t, s, AlertRule{Kind: KindOffline, Enabled: true, NodeIDs: ids})
	for _, rule := range []int64{a.ID, b.ID} {
		for _, node := range ids {
			if err := s.SetAlertState(t.Context(), rule, node, StateOK, s.clk.Now(), time.Time{}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetAlertState(t.Context(), rule, node, StatePending, s.clk.Now().Add(time.Second), time.Time{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	a.NodeIDs = []int64{ids[1], ids[1]}
	a = saveRule(t, s, a)
	pending := func(rule, node int64) StateRow {
		return StateRow{RuleID: rule, NodeID: node, State: StatePending, SinceAt: s.clk.Now().Add(time.Second)}
	}
	want := []StateRow{pending(a.ID, ids[1]), pending(b.ID, ids[0]), pending(b.ID, ids[1])}
	got, err := s.ListAlertStates(t.Context())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("kept states=%+v err=%v want=%+v", got, err, want)
	}
	a.NodeIDs = nil
	saveRule(t, s, a)
	got, err = s.ListAlertStates(t.Context())
	if err != nil || !reflect.DeepEqual(got, want[1:]) {
		t.Fatalf("clear rule states=%+v err=%v", got, err)
	}
	assertAlertRows(t, s, "alert_event", "1", 0)
}

func TestProbeTaskNodeIDs(t *testing.T) {
	t.Parallel()
	s, ids, _, task := alertFixture(t)
	got, err := s.ProbeTaskNodeIDs(t.Context(), task)
	if err != nil || !reflect.DeepEqual(got, ids) {
		t.Fatalf("assigned nodes=%v err=%v want=%v", got, err, ids)
	}
	got, err = s.ProbeTaskNodeIDs(t.Context(), task+1)
	if err != nil || len(got) != 0 {
		t.Fatalf("missing task nodes=%v err=%v", got, err)
	}
}

func TestNotifyChannelRoundTrip(t *testing.T) {
	t.Parallel()
	s, _, cs, _ := alertFixture(t)
	want := []NotifyChannel{
		{ID: cs[0].ID, Name: "0", Kind: ChannelWebhook, Config: `{}`, CreatedAt: s.clk.Now()},
		{ID: cs[1].ID, Name: "1", Kind: ChannelWebhook, Config: `{}`, CreatedAt: s.clk.Now()},
	}
	got, err := s.ListNotifyChannels(t.Context())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("channels=%+v err=%v", got, err)
	}
	want[0].Name, want[0].Kind, want[0].Config = "changed", ChannelTelegram, `{"chat_id":"42"}`
	updated, err := s.SaveNotifyChannel(t.Context(), want[0])
	if err != nil || updated != want[0] {
		t.Fatalf("updated channel=%+v err=%v", updated, err)
	}
	got, err = s.ListNotifyChannels(t.Context())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("updated channels=%+v err=%v", got, err)
	}
}

func TestAlertScopeDoesNotWidenAfterLastNodeDeletion(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	scoped := saveRule(t, s, AlertRule{Name: "scoped", Kind: KindOffline, Enabled: true, NodeIDs: ids[:1]})
	all := saveRule(t, s, AlertRule{Name: "all", Kind: KindOffline, Enabled: true, AllNodes: true})
	if !all.AllNodes || len(all.NodeIDs) != 0 {
		t.Fatalf("all scope=%+v", all)
	}
	assertAlertRows(t, s, "alert_rule_node", fmt.Sprintf("rule_id = %d", all.ID), 0)
	if err := s.DeleteNode(t.Context(), ids[0]); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListAlertRules(t.Context())
	scoped.NodeIDs = nil
	if err != nil || !reflect.DeepEqual(got, []AlertRule{scoped, all}) {
		t.Fatalf("scope widened after deletion: %+v err=%v", got, err)
	}
	all.AllNodes = false
	all.NodeIDs = ids[1:]
	all = saveRule(t, s, all)
	got, err = s.ListAlertRules(t.Context())
	if err != nil || !reflect.DeepEqual(got, []AlertRule{scoped, all}) {
		t.Fatalf("scope replacement=%+v err=%v", got, err)
	}
}

func assertAlertNotFound(t *testing.T, err error, kind ObjectKind, id int64) {
	t.Helper()
	var e NotFoundError
	if !errors.Is(err, ErrNotFound) || !errors.As(err, &e) || e.Kind != kind || e.ID != id {
		t.Fatalf("missing object: error=%v detail=%+v want=%s/%d", err, e, kind, id)
	}
}

func TestAlertWritesRejectDeletedReferences(t *testing.T) {
	t.Parallel()
	for _, missing := range []ObjectKind{ObjectNode, ObjectAlertRule, ObjectNotifyChannel} {
		t.Run(string(missing), func(t *testing.T) {
			s, ids, cs, _ := alertFixture(t)
			r := saveRule(t, s, AlertRule{Kind: KindOffline, Enabled: true, AllNodes: true})
			id := ids[0]
			var err error
			switch missing {
			case ObjectNode:
				err = s.DeleteNode(t.Context(), id)
			case ObjectAlertRule:
				id = r.ID
				err = s.DeleteAlertRule(t.Context(), id)
			case ObjectNotifyChannel:
				id = cs[1].ID
				err = s.DeleteNotifyChannel(t.Context(), id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if missing != ObjectNotifyChannel {
				t.Run("set", func(t *testing.T) {
					assertAlertNotFound(t, s.SetAlertState(t.Context(), r.ID, ids[0], StatePending, s.clk.Now(), time.Time{}), missing, id)
				})
			}
			t.Run("transition", func(t *testing.T) {
				_, err := s.RecordTransition(t.Context(), r.ID, ids[0], StateFiring, "", time.Time{}, AlertEvent{At: s.clk.Now()}, newBatches(cs[0].ID, cs[1].ID))
				assertAlertNotFound(t, err, missing, id)
			})
			for _, table := range []string{"alert_state", "alert_event", "alert_delivery"} {
				assertAlertRows(t, s, table, "1", 0)
			}
		})
	}
}

func TestSaveAlertRulePrunesDisabledButKeepsEnabledAll(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline, Enabled: true, AllNodes: true})
	if err := s.SetAlertState(t.Context(), r.ID, ids[0], StateFiring, s.clk.Now(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	saveRule(t, s, r)
	assertAlertRows(t, s, "alert_state", "1", 1)
	r.Enabled = false
	saveRule(t, s, r)
	assertAlertRows(t, s, "alert_state", "1", 0)
}

func TestSaveAlertRuleRollsBackWhenStatePruningFails(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Name: "kept", Kind: KindOffline, Enabled: true, NodeIDs: ids})
	if err := s.SetAlertState(t.Context(), r.ID, ids[0], StateFiring, s.clk.Now(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TRIGGER reject_state_delete BEFORE DELETE ON alert_state BEGIN SELECT RAISE(ABORT, 'state delete rejected'); END")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	changed := r
	changed.Name, changed.Enabled, changed.NodeIDs = "lost", false, nil
	_, err := s.SaveAlertRule(t.Context(), changed)
	if err == nil || !strings.Contains(err.Error(), "state delete rejected") {
		t.Fatalf("prune error=%v", err)
	}
	got, err := s.ListAlertRules(t.Context())
	if err != nil || !reflect.DeepEqual(got, []AlertRule{r}) {
		t.Fatalf("partially saved rule: %+v %v", got, err)
	}
	assertAlertRows(t, s, "alert_state", "1", 1)
}

func TestAlertMissingObjects(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for _, tc := range []struct {
		name string
		kind ObjectKind
		call func() error
	}{
		{"save_rule", ObjectAlertRule, func() error {
			_, err := s.SaveAlertRule(t.Context(), AlertRule{ID: 999, Kind: KindOffline})
			return err
		}},
		{"delete_rule", ObjectAlertRule, func() error { return s.DeleteAlertRule(t.Context(), 999) }},
		{"delete_channel", ObjectNotifyChannel, func() error { return s.DeleteNotifyChannel(t.Context(), 999) }},
		{"save_channel", ObjectNotifyChannel, func() error { _, err := s.SaveNotifyChannel(t.Context(), NotifyChannel{ID: 999}); return err }},
		{"update_batch", ObjectAlertDelivery, func() error {
			return s.UpdateBatch(t.Context(), 999, DeliveryResult{Failure: FailureTransport, Error: "failed"})
		}},
		{"begin_batch_attempt", ObjectAlertDelivery, func() error { _, err := s.BeginBatchAttempt(t.Context(), 999, []int64{999}); return err }},
		{"get_delivery_batch", ObjectAlertDelivery, func() error { _, err := s.GetDeliveryBatch(t.Context(), 999); return err }},
		{"get_event", ObjectAlertEvent, func() error { _, err := s.GetAlertEvent(t.Context(), 999); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) { assertAlertNotFound(t, tc.call(), tc.kind, 999) })
	}
}

func TestDeleteNotifyChannelTerminatesPendingDeliveries(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	ev := recordEvent(t, s, r.ID, ids[0], []int64{cs[0].ID, cs[0].ID, cs[1].ID})
	if _, err := s.BeginBatchAttempt(t.Context(), ev.Deliveries[1].BatchID, []int64{ev.Deliveries[1].ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateBatch(t.Context(), ev.Deliveries[1].BatchID, DeliveryResult{OK: true, Done: true, DeliveredAt: s.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNotifyChannel(t.Context(), cs[0].ID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingBatches(t.Context())
	if err != nil || !reflect.DeepEqual(pending, []PendingBatch{{ev.Deliveries[2].BatchID, cs[1].ID}}) {
		t.Fatalf("deleted channel pending=%+v err=%v", pending, err)
	}
	ev.Deliveries[0].Done, ev.Deliveries[0].Failure = true, FailureChannelDeleted
	ev.Deliveries[1].Attempts, ev.Deliveries[1].OK, ev.Deliveries[1].Done, ev.Deliveries[1].DeliveredAt = 1, true, true, s.clk.Now()
	got, err := s.GetAlertEvent(t.Context(), ev.ID)
	if err != nil || !reflect.DeepEqual(got, ev) {
		t.Fatalf("terminal deliveries=%+v err=%v want=%+v", got, err, ev)
	}
}

func TestListAlertEventsUsesNodeIndex(t *testing.T) {
	t.Parallel()
	s, _, _, _ := alertFixture(t)
	for _, before := range []int64{0, 99} {
		where, args := alertEventWindow(1, before, 2)
		rows, err := s.r.Query("EXPLAIN QUERY PLAN "+selectAlertEvents+where, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(plan, "\n"), "alert_event_by_node") {
			t.Fatalf("node filter plan=%v", plan)
		}
		t.Logf("before=%d plan=%v", before, plan)
	}
}
