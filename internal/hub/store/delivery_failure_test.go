package store

import (
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestUpdateDeliveryRejectsInconsistentResult(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 每个用例只违反一条规则，其余字段取合法值，才能说明被拒是因为那一条。
	for _, tc := range []struct {
		name string
		r    DeliveryResult
	}{
		{"ok_with_failure", DeliveryResult{OK: true, Done: true, DeliveredAt: at, Failure: FailureTransport}},
		{"ok_with_status", DeliveryResult{OK: true, Done: true, DeliveredAt: at, HTTPStatus: 500}},
		{"ok_with_error", DeliveryResult{OK: true, Done: true, DeliveredAt: at, Error: "leftover"}},
		{"ok_not_done", DeliveryResult{OK: true, DeliveredAt: at}},
		{"ok_without_delivered_at", DeliveryResult{OK: true, Done: true}},
		{"failure_with_delivered_at", DeliveryResult{Done: true, Failure: FailureTransport, Error: "refused", DeliveredAt: at}},
		{"failure_without_category", DeliveryResult{Done: true, Error: "HTTP 500"}},
		{"http_status_without_status", DeliveryResult{Done: true, Failure: FailureHTTPStatus, Error: "body"}},
		{"http_status_below_range", DeliveryResult{Done: true, Failure: FailureHTTPStatus, HTTPStatus: 99}},
		{"http_status_above_range", DeliveryResult{Done: true, Failure: FailureHTTPStatus, HTTPStatus: 1000}},
		{"status_without_http_status", DeliveryResult{Done: true, Failure: FailureTransport, HTTPStatus: 502}},
		{"out_of_range_status_without_http_status", DeliveryResult{Done: true, Failure: FailureTransport, HTTPStatus: 42}},
		{"negative_status_without_http_status", DeliveryResult{Done: true, Failure: FailureTransport, HTTPStatus: -1}},
		{"channel_deleted_with_error", DeliveryResult{Done: true, Failure: FailureChannelDeleted, Error: "channel deleted"}},
		{"result_unrecorded_with_error", DeliveryResult{Done: true, Failure: FailureResultUnrecorded, Error: "lost"}},
		{"channel_deleted_not_done", DeliveryResult{Failure: FailureChannelDeleted}},
		{"result_unrecorded_not_done", DeliveryResult{Failure: FailureResultUnrecorded}},
		{"channel_invalid_not_done", DeliveryResult{Failure: FailureChannelInvalid, Error: "invalid"}},
		{"request_not_done", DeliveryResult{Failure: FailureRequest, Error: "template"}},
		{"unclassified_not_done", DeliveryResult{Failure: FailureUnclassified, Error: "odd"}},
		{"unknown_category", DeliveryResult{Done: true, Failure: "timeout", Error: "late"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ids, cs, _ := alertFixture(t)
			r := saveRule(t, s, AlertRule{Kind: KindOffline})
			ev := recordEvent(t, s, r.ID, ids[0], []int64{cs[0].ID})
			if err := s.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, tc.r); err == nil {
				t.Fatalf("inconsistent result accepted: %+v", tc.r)
			}
			got, err := s.GetAlertEvent(t.Context(), ev.ID)
			if err != nil || !reflect.DeepEqual(got.Deliveries, ev.Deliveries) {
				t.Fatalf("rejected result was written: %+v err=%v", got.Deliveries, err)
			}
		})
	}
}

// 可重试的两类（transport、http_status）允许非终态写入，其余拒绝的只是上面列出的组合。
func TestUpdateDeliveryAcceptsRetryableFailureBeforeDone(t *testing.T) {
	t.Parallel()
	for _, r := range []DeliveryResult{
		{Failure: FailureTransport, Error: "refused"},
		{Failure: FailureHTTPStatus, HTTPStatus: 503, Error: "busy"},
		{Failure: FailureHTTPStatus, HTTPStatus: 100},
		{Failure: FailureHTTPStatus, HTTPStatus: 999},
	} {
		s, ids, cs, _ := alertFixture(t)
		rule := saveRule(t, s, AlertRule{Kind: KindOffline})
		ev := recordEvent(t, s, rule.ID, ids[0], []int64{cs[0].ID})
		if err := s.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, r); err != nil {
			t.Fatalf("%+v rejected: %v", r, err)
		}
	}
}

func TestUpdateDeliveryStoresFailureAndClearsItOnSuccess(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	ev := recordEvent(t, s, r.ID, ids[0], []int64{cs[0].ID})
	id := ev.Deliveries[0].ID
	if err := s.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, DeliveryResult{Failure: FailureHTTPStatus, HTTPStatus: 503, Error: "busy"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAlertEvent(t.Context(), ev.ID)
	if d := got.Deliveries[0]; err != nil || d.Failure != FailureHTTPStatus || d.HTTPStatus != 503 || d.LastError != "busy" || d.Done {
		t.Fatalf("failed attempt=%+v err=%v", d, err)
	}
	at := s.clk.Now()
	if err := s.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, DeliveryResult{OK: true, Done: true, DeliveredAt: at}); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAlertEvent(t.Context(), ev.ID)
	want := Delivery{ID: id, EventID: ev.ID, ChannelID: cs[0].ID, BatchID: id, OK: true, Done: true, DeliveredAt: at}
	if err != nil || got.Deliveries[0] != want {
		t.Fatalf("delivered=%+v err=%v, want %+v", got.Deliveries[0], err, want)
	}
	var status sql.NullInt64
	if err := s.r.QueryRow("SELECT http_status FROM alert_delivery WHERE id = ?", id).Scan(&status); err != nil || status.Valid {
		t.Fatalf("http_status after success=%v err=%v, want NULL", status, err)
	}
}

func TestGetDeliveryError(t *testing.T) {
	t.Parallel()
	s, ids, cs, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Kind: KindOffline})
	ev := recordEvent(t, s, r.ID, ids[0], []int64{cs[0].ID})
	if err := s.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, DeliveryResult{Done: true, Failure: FailureHTTPStatus, HTTPStatus: 401, Error: "echo secret"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetDeliveryError(t.Context(), ev.Deliveries[0].ID); err != nil || got != "echo secret" {
		t.Fatalf("raw error=%q err=%v", got, err)
	}
	_, err := s.GetDeliveryError(t.Context(), 999)
	assertAlertNotFound(t, err, ObjectAlertDelivery, 999)
}

// schemaV6 冻结加入投递失败类别之前的完整 DDL。
var schemaV6 = append(slices.Clone(schemaV5), "CREATE TABLE api_token (\n  -- AUTOINCREMENT：id 永不复用。吊销按 id 进行，复用会让针对旧 token 的吊销落到新 token 上。\n  id INTEGER PRIMARY KEY AUTOINCREMENT,\n  name TEXT NOT NULL,\n  -- 整串明文（含前缀）的 SHA-256；明文不落库。\n  token_hash BLOB NOT NULL UNIQUE,\n  created_at INTEGER NOT NULL,\n  -- NULL 表示从未使用。只供展示：距已落库值满一分钟才刷新。\n  last_used_at INTEGER\n)")

func TestMigrationFromV6ClassifiesDeliveryFailures(t *testing.T) {
	t.Parallel()
	type seedRow struct {
		ok, done  bool
		lastError string
	}
	type row struct {
		failure   DeliveryFailure
		status    sql.NullInt64
		lastError string
	}
	none := sql.NullInt64{}
	for id, tc := range map[int64]struct {
		seed seedRow
		want row
	}{
		1: {seedRow{false, true, "channel deleted"}, row{FailureChannelDeleted, none, ""}},
		2: {seedRow{false, true, "attempts exhausted but last result was not recorded"}, row{FailureResultUnrecorded, none, ""}},
		3: {seedRow{false, true, `HTTP 401 Unauthorized: echoed {"token":"x: y"}`}, row{FailureHTTPStatus, sql.NullInt64{Int64: 401, Valid: true}, `echoed {"token":"x: y"}`}},
		4: {seedRow{false, true, "Post: dial tcp: connection refused"}, row{FailureUnclassified, none, "Post: dial tcp: connection refused"}},
		5: {seedRow{true, true, ""}, row{FailureNone, none, ""}},
		6: {seedRow{false, false, ""}, row{FailureNone, none, ""}},
		// 成功行不论 last_error 写着什么都不碰。
		7:  {seedRow{true, true, "channel deleted"}, row{FailureNone, none, "channel deleted"}},
		8:  {seedRow{true, true, "attempts exhausted but last result was not recorded"}, row{FailureNone, none, "attempts exhausted but last result was not recorded"}},
		9:  {seedRow{true, true, "HTTP 502 Bad Gateway: x"}, row{FailureNone, none, "HTTP 502 Bad Gateway: x"}},
		10: {seedRow{true, true, "leftover"}, row{FailureNone, none, "leftover"}},
		// 没有 ": " 的不是旧 HTTP 格式写出的。
		11: {seedRow{false, true, "HTTP 401 Unauthorized"}, row{FailureUnclassified, none, "HTTP 401 Unauthorized"}},
		// http.StatusText 为空时旧格式是 "HTTP 599 : body"。
		12: {seedRow{false, true, "HTTP 599 : body"}, row{FailureHTTPStatus, sql.NullInt64{Int64: 599, Valid: true}, "body"}},
		// 终态失败却没有原文：不能迁成"没有失败"。
		13: {seedRow{false, true, ""}, row{FailureUnclassified, none, ""}},
		// 旧格式按 %d 写状态码，三位且首位为 0 的不是它写出的。
		14: {seedRow{false, true, "HTTP 099 x: y"}, row{FailureUnclassified, none, "HTTP 099 x: y"}},
	} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			t.Parallel()
			seed := func(t *testing.T, db *sql.DB) {
				seedMinuteRow(t, db)
				if _, err := db.Exec("INSERT INTO alert_delivery (id, event_id, channel_id, attempts, ok, done, last_error) VALUES (?, 1, 1, 1, ?, ?, ?)", id, tc.seed.ok, tc.seed.done, tc.seed.lastError); err != nil {
					t.Fatal(err)
				}
			}
			migrated := migrateFrom(t, 6, seed)
			if v := userVersion(t, migrated.r); v != schemaVersion {
				t.Fatalf("user_version = %d, want %d", v, schemaVersion)
			}
			var got row
			if err := migrated.r.QueryRow("SELECT failure, http_status, last_error FROM alert_delivery WHERE id = ?", id).Scan(&got.failure, &got.status, &got.lastError); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("delivery %d %+v = %+v, want %+v", id, tc.seed, got, tc.want)
			}
		})
	}
}
