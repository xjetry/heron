package store

import (
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// v17 的完整 DDL：v16 加上恢复审计记录。
var schemaV17 = append(slices.Clone(schemaV16),
	"CREATE TABLE restore_record (id TEXT PRIMARY KEY NOT NULL, restored_at INTEGER NOT NULL, config_taken_at INTEGER NOT NULL, metrics_taken_at INTEGER, orphans TEXT NOT NULL)")

// v18 冻结重建后的投递表与渠道节奏列，不引用生产迁移，防止输入和被测实现一起漂移。
var schemaV18 = append(slices.Clone(schemaV17),
	`DROP TABLE alert_delivery`,
	`CREATE TABLE alert_delivery (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  channel_id INTEGER NOT NULL,
  batch_id INTEGER NOT NULL CHECK (batch_id > 0),
  attempts INTEGER NOT NULL DEFAULT 0,
  ok INTEGER NOT NULL DEFAULT 0,
  done INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at INTEGER,
  failure TEXT NOT NULL DEFAULT '',
  http_status INTEGER,
  not_before INTEGER NOT NULL DEFAULT 0
)`,
	`CREATE INDEX alert_delivery_by_event ON alert_delivery(event_id)`,
	`CREATE INDEX alert_delivery_pending ON alert_delivery(done, batch_id, channel_id)`,
	`CREATE INDEX alert_delivery_by_batch ON alert_delivery(batch_id)`,
	`ALTER TABLE notify_channel ADD COLUMN rate_per_minute INTEGER NOT NULL DEFAULT 0 CHECK (rate_per_minute >= 0)`,
)

// 升级前每行各自发送过：旧行各成一批，未终态的仍按原行续投，其余各列原样保留；旧渠道按种类取缺省节奏。重建表带上
// 旧序列：id 50 的行已被清理，新行必须从 51 起，否则批次号会被复用。
func TestMigrationFromV17AddsBatchesAndChannelRates(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 17, func(t *testing.T, db *sql.DB) {
		for _, stmt := range []string{
			"INSERT INTO node (id, name, token_hash, created_at) VALUES (7, 'n', x'07', 1)",
			"INSERT INTO notify_channel (id, name, kind, config, created_at) VALUES (1, 'tg', 'telegram', '{}', 1), (2, 'hook', 'webhook', '{}', 1)",
			"INSERT INTO alert_event (id, rule_id, node_id, transition, at, summary, value) VALUES (5, 3, 7, 'firing', 1, 's', 0)",
			"INSERT INTO alert_delivery (id, event_id, channel_id, attempts, done, failure, http_status, last_error) VALUES (10, 5, 1, 1, 0, 'http_status', 503, 'busy'), (11, 5, 2, 3, 1, '', NULL, '')",
			"INSERT INTO alert_delivery (id, event_id, channel_id) VALUES (50, 5, 1)",
			"DELETE FROM alert_delivery WHERE id = 50",
		} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
	})
	if v := userVersion(t, migrated.r); v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	channels, err := migrated.ListNotifyChannels(t.Context())
	if err != nil || len(channels) != 2 || channels[0].RatePerMinute != 20 || channels[1].RatePerMinute != 0 {
		t.Fatalf("channels after migration: %+v %v, want telegram 20 and webhook 0", channels, err)
	}
	ev, err := migrated.GetAlertEvent(t.Context(), 5)
	want := []Delivery{
		{ID: 10, EventID: 5, ChannelID: 1, BatchID: 10, Attempts: 1, Failure: FailureHTTPStatus, HTTPStatus: 503, LastError: "busy"},
		{ID: 11, EventID: 5, ChannelID: 2, BatchID: 11, Attempts: 3, Done: true},
	}
	if err != nil || !reflect.DeepEqual(ev.Deliveries, want) {
		t.Fatalf("deliveries after migration: %+v %v, want %+v", ev.Deliveries, err, want)
	}
	if pending, err := migrated.PendingBatches(t.Context()); err != nil || !slices.Equal(pending, []PendingBatch{{ID: 10, ChannelID: 1}}) {
		t.Fatalf("pending batches after migration: %v %v, want [{10 1}]", pending, err)
	}
	var seq, leftover int64
	if err := migrated.r.QueryRow("SELECT seq FROM sqlite_sequence WHERE name = 'alert_delivery'").Scan(&seq); err != nil || seq != 50 {
		t.Fatalf("alert_delivery sequence after migration: %d %v, want 50", seq, err)
	}
	if err := migrated.r.QueryRow("SELECT COUNT(*) FROM sqlite_sequence WHERE name = 'alert_delivery_v18'").Scan(&leftover); err != nil || leftover != 0 {
		t.Fatalf("sequence rows left under the temporary name: %d %v", leftover, err)
	}
	r := saveRule(t, migrated, AlertRule{Kind: KindOffline})
	if d := recordTargets(t, migrated, r.ID, 7, DeliveryTarget{ChannelID: 1}).Deliveries[0]; d.ID != 51 || d.BatchID != 51 {
		t.Fatalf("first delivery after migration: %+v, want id and batch 51", d)
	}
}

// 不写批次号的插入在写时失败（新建库与迁移库都是）：之后每个读这一列的查询都依赖它非空。
func TestDeliveryInsertWithoutBatchFails(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 17, func(*testing.T, *sql.DB) {})
	fresh, _ := open(t)
	for _, s := range []*Store{fresh, migrated} {
		err := s.write(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.Exec("INSERT INTO alert_delivery (event_id, channel_id) VALUES (1, 1)")
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "NOT NULL constraint failed: alert_delivery.batch_id") {
			t.Errorf("insert without batch_id on a %s database: %v", map[bool]string{true: "migrated", false: "fresh"}[s == migrated], err)
		}
	}
}

func recordTargets(t *testing.T, s *Store, rule, node int64, targets ...DeliveryTarget) AlertEvent {
	t.Helper()
	ev, err := s.RecordTransition(t.Context(), rule, node, StateFiring, "", time.Time{}, AlertEvent{Transition: TransitionFiring, At: s.clk.Now(), Summary: "down"}, targets)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// 新行只加入还没开始尝试、且发往同一渠道的批次；其余情形新开一批（批次号即自己的 id）。
func TestRecordTransitionJoinsOnlyOpenBatchOfSameChannel(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	first := recordTargets(t, s, r.ID, ids[0], DeliveryTarget{ChannelID: cs[0].ID})
	batch := first.Deliveries[0].BatchID
	if batch != first.Deliveries[0].ID {
		t.Fatalf("new batch id = %d, want the row id %d", batch, first.Deliveries[0].ID)
	}
	joined := recordTargets(t, s, r.ID, ids[1], DeliveryTarget{ChannelID: cs[0].ID, Batch: batch}, DeliveryTarget{ChannelID: cs[1].ID, Batch: batch})
	if got := joined.Deliveries[0].BatchID; got != batch {
		t.Fatalf("same-channel row joined batch %d, want %d", got, batch)
	}
	if d := joined.Deliveries[1]; d.BatchID != d.ID {
		t.Fatalf("other-channel row joined batch %d, want its own batch %d", d.BatchID, d.ID)
	}
	if ghost := recordTargets(t, s, r.ID, ids[0], DeliveryTarget{ChannelID: cs[0].ID, Batch: 999}).Deliveries[0]; ghost.BatchID != ghost.ID {
		t.Fatalf("row asking for a missing batch got batch %d, want its own %d", ghost.BatchID, ghost.ID)
	}

	ds, err := s.BeginBatchAttempt(t.Context(), batch, []int64{first.Deliveries[0].ID, joined.Deliveries[0].ID})
	if err != nil || len(ds) != 2 || ds[0].ID != first.Deliveries[0].ID || ds[1].ID != joined.Deliveries[0].ID || ds[0].Attempts != 1 || ds[1].Attempts != 1 {
		t.Fatalf("batch attempt rows=%+v err=%v", ds, err)
	}
	if late := recordTargets(t, s, r.ID, ids[0], DeliveryTarget{ChannelID: cs[0].ID, Batch: batch}).Deliveries[0]; late.BatchID != late.ID {
		t.Fatalf("row joining a started batch got batch %d, want its own %d", late.BatchID, late.ID)
	}

	// 终态的批次同样不再接纳新行：它已不会再发送。
	ended := recordTargets(t, s, r.ID, ids[1], DeliveryTarget{ChannelID: cs[1].ID}).Deliveries[0]
	if err := s.UpdateBatch(t.Context(), ended.BatchID, DeliveryResult{Done: true, Failure: FailureChannelInvalid, Error: "bad"}); err != nil {
		t.Fatal(err)
	}
	if after := recordTargets(t, s, r.ID, ids[1], DeliveryTarget{ChannelID: cs[1].ID, Batch: ended.BatchID}).Deliveries[0]; after.BatchID != after.ID {
		t.Fatalf("row joining a finished batch got batch %d, want its own %d", after.BatchID, after.ID)
	}
}

// 一次尝试与它的结果写到整批的每一行，别的批次不受影响；批次读出的行与事件一一对应。
func TestBatchAttemptAndResultCoverWholeBatch(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	a := recordTargets(t, s, r.ID, ids[0], DeliveryTarget{ChannelID: cs[0].ID})
	batch := a.Deliveries[0].BatchID
	b := recordTargets(t, s, r.ID, ids[1], DeliveryTarget{ChannelID: cs[0].ID, Batch: batch})
	other := recordTargets(t, s, r.ID, ids[0], DeliveryTarget{ChannelID: cs[0].ID})
	rows := []int64{b.Deliveries[0].ID, a.Deliveries[0].ID}
	if _, err := s.BeginBatchAttempt(t.Context(), batch, rows); err != nil {
		t.Fatal(err)
	}
	next := s.clk.Now().Add(1500 * time.Millisecond)
	if err := s.UpdateBatch(t.Context(), batch, DeliveryResult{Failure: FailureHTTPStatus, HTTPStatus: 503, Error: "busy", NotBefore: next}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDeliveryBatch(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Deliveries) != 2 || len(got.Events) != 2 || got.ID != batch {
		t.Fatalf("batch=%+v", got)
	}
	for i, d := range got.Deliveries {
		// not_before 按秒向上取整：写成更早的时刻会让补回的批次提前重试。
		if d.Attempts != 1 || d.Done || d.Failure != FailureHTTPStatus || d.HTTPStatus != 503 || d.LastError != "busy" || d.BatchID != batch || !d.NotBefore.Equal(s.clk.Now().Add(2*time.Second)) {
			t.Fatalf("row %d after a failed attempt=%+v", i, d)
		}
		if got.Events[i].ID != d.EventID || got.Events[i].Summary != "down" || got.Events[i].Deliveries != nil {
			t.Fatalf("event %d=%+v does not belong to row %+v", i, got.Events[i], d)
		}
	}
	if got.Events[0].ID != a.ID || got.Events[1].ID != b.ID {
		t.Fatalf("batch events=%d,%d want %d,%d in row order", got.Events[0].ID, got.Events[1].ID, a.ID, b.ID)
	}
	at := s.clk.Now()
	if _, err := s.BeginBatchAttempt(t.Context(), batch, rows); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateBatch(t.Context(), batch, DeliveryResult{OK: true, Done: true, DeliveredAt: at}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetDeliveryBatch(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range got.Deliveries {
		if d.Attempts != 2 || !d.OK || !d.Done || d.Failure != FailureNone || d.HTTPStatus != 0 || d.LastError != "" || !d.DeliveredAt.Equal(at) || !d.NotBefore.IsZero() {
			t.Fatalf("row %d after success=%+v", i, d)
		}
	}
	untouched, err := s.GetAlertEvent(t.Context(), other.ID)
	if err != nil || !reflect.DeepEqual(untouched.Deliveries, other.Deliveries) {
		t.Fatalf("other batch=%+v err=%v, want untouched %+v", untouched.Deliveries, err, other.Deliveries)
	}
}

// 同批各行的次数或 not_before 不一致说明不变式已被破坏：报错并回滚，不按其中某行发送。比较的是批次全部行：已耗尽
// 名额（attempts = 3）却未终态的一行不会被计数的 UPDATE 命中，同样要被发现；not_before 不一致时按首行判断等不等，
// 会让另一行早于它自己的最早时刻被发出。
func TestBeginBatchAttemptRejectsNonUniformBatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, set string
		value     int64
		attempts  int // 被改写的那一行改写后的次数。
	}{
		{"attempts_1", "attempts = ?", 1, 1},
		{"attempts_exhausted", "attempts = ?", MaxDeliveryAttempts, MaxDeliveryAttempts},
		{"not_before", "not_before = ?", time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC).Unix(), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ids, cs, _ := alertFixture(t)
			r := saveRule(t, s, AlertRule{Kind: KindOffline})
			a := recordTargets(t, s, r.ID, ids[0], DeliveryTarget{ChannelID: cs[0].ID})
			batch := a.Deliveries[0].BatchID
			b := recordTargets(t, s, r.ID, ids[1], DeliveryTarget{ChannelID: cs[0].ID, Batch: batch})
			if err := s.write(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.Exec("UPDATE alert_delivery SET "+tc.set+" WHERE id = ?", tc.value, b.Deliveries[0].ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.BeginBatchAttempt(t.Context(), batch, []int64{a.Deliveries[0].ID, b.Deliveries[0].ID}); err == nil || !strings.Contains(err.Error(), "not uniform") {
				t.Fatalf("non-uniform batch attempt err=%v", err)
			}
			got, err := s.GetDeliveryBatch(t.Context(), batch)
			if err != nil || got.Deliveries[0].Attempts != 0 || got.Deliveries[1].Attempts != tc.attempts {
				t.Fatalf("rows after refused attempt=%+v err=%v, want the attempt rolled back", got.Deliveries, err)
			}
		})
	}
}

// 开始尝试只覆盖调用方拼消息时读到的那组行：少了后加入的行、多了已清理的行、或者一行都没给，都拒绝且不消耗名额；
// 调用方重读后用批次当前的行集合（顺序不限）才能开始。
func TestBeginBatchAttemptRejectsChangedRowSet(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	a := recordTargets(t, s, r.ID, ids[0], DeliveryTarget{ChannelID: cs[0].ID})
	batch := a.Deliveries[0].BatchID
	b := recordTargets(t, s, r.ID, ids[1], DeliveryTarget{ChannelID: cs[0].ID, Batch: batch})
	current := []int64{a.Deliveries[0].ID, b.Deliveries[0].ID}
	for _, stale := range [][]int64{{a.Deliveries[0].ID}, {a.Deliveries[0].ID, b.Deliveries[0].ID, 999}, nil} {
		if ds, err := s.BeginBatchAttempt(t.Context(), batch, stale); !errors.Is(err, ErrBatchChanged) || ds != nil {
			t.Fatalf("attempt with rows %v on batch rows %v: %+v %v, want ErrBatchChanged", stale, current, ds, err)
		}
	}
	got, err := s.GetDeliveryBatch(t.Context(), batch)
	if err != nil || got.Deliveries[0].Attempts != 0 || got.Deliveries[1].Attempts != 0 {
		t.Fatalf("rows after refused attempts=%+v err=%v, want no attempt consumed", got.Deliveries, err)
	}
	ds, err := s.BeginBatchAttempt(t.Context(), batch, []int64{b.Deliveries[0].ID, a.Deliveries[0].ID})
	if err != nil || len(ds) != 2 || ds[0].Attempts != 1 || ds[1].Attempts != 1 {
		t.Fatalf("attempt with the current rows=%+v err=%v", ds, err)
	}
}

// 下一次尝试的时刻只属于还会重试的失败：成功或终态的结果带着它被拒绝，不写库。
func TestDeliveryResultRejectsNotBeforeOnFinishedResult(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	ev := recordTargets(t, s, r.ID, ids[0], DeliveryTarget{ChannelID: cs[0].ID})
	later := s.clk.Now().Add(time.Minute)
	for _, res := range []DeliveryResult{
		{OK: true, Done: true, DeliveredAt: s.clk.Now(), NotBefore: later},
		{Done: true, Failure: FailureHTTPStatus, HTTPStatus: 400, Error: "bad", NotBefore: later},
	} {
		if err := s.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, res); err == nil || !strings.Contains(err.Error(), "next-attempt time") {
			t.Errorf("result %+v accepted: %v", res, err)
		}
	}
}

// 节奏上限原样往返，更新时改写。取值约束（非负）由列上的 CHECK 承载，见 TestEveryCheckConstraintRefusesItsViolation。
func TestNotifyChannelRateRoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	saved, err := s.SaveNotifyChannel(t.Context(), NotifyChannel{Name: "tg", Kind: ChannelTelegram, Config: `{}`, RatePerMinute: 7})
	if err != nil {
		t.Fatal(err)
	}
	saved.RatePerMinute = 0
	if _, err := s.SaveNotifyChannel(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	channels, err := s.ListNotifyChannels(t.Context())
	if err != nil || len(channels) != 1 || channels[0].RatePerMinute != 0 || channels[0].ID != saved.ID {
		t.Fatalf("rate after update=%+v %v, want 0", channels, err)
	}
}
