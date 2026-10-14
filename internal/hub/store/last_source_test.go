package store

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/metric"
)

// v12 的完整 DDL：v11 加上告警状态的上次恢复时刻。
var schemaV12 = append(slices.Clone(schemaV11), "ALTER TABLE alert_state ADD COLUMN recovered_at INTEGER")

// 旧库的节点升级后 last_source 为空串，即便它此前上报过：last_seen_at 保留着升级前最后一次上报的墙钟时刻，
// 空串只说明 hub 在这个版本之前不记录来源，不能被读成"这个节点从未上报"。
func TestMigrationFromV12AddsEmptyLastSource(t *testing.T) {
	t.Parallel()
	seed := func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		if _, err := db.Exec("UPDATE node SET last_seen_at = 1 WHERE id = 7"); err != nil {
			t.Fatal(err)
		}
	}
	migrated := migrateFrom(t, 12, seed)
	if v := userVersion(t, migrated.r); v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	n, err := migrated.GetNode(t.Context(), 7)
	if err != nil || n.Name != "kept" || n.LastSource != "" || n.LastSeenAt.IsZero() {
		t.Fatalf("node after migration: %+v %v", n, err)
	}
}

// 分钟行带着来源地址与 last_seen 一起落盘；空串的来源（那次上报取不到对端）只更新 last_seen_at，不清掉已有的地址。
func TestMinuteBatchWritesLastSource(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	write := func(ts int64, source string) {
		t.Helper()
		row := metric.Row{NodeID: id, TS: ts, CoverageStart: 600, Bucket: metric.NewBucket(), LastSeen: clk.Now(), Source: source}
		if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{row}}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() Node {
		t.Helper()
		n, err := s.GetNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := read(); n.LastSource != "" {
		t.Fatalf("new node last_source = %q", n.LastSource)
	}
	write(600, "203.0.113.7")
	if n := read(); n.LastSource != "203.0.113.7" {
		t.Fatalf("last_source = %q, want 203.0.113.7", n.LastSource)
	}
	clk.Advance(time.Minute)
	write(660, "")
	n := read()
	if n.LastSource != "203.0.113.7" || !n.LastSeenAt.Equal(time.Unix(clk.Now().Unix(), 0).UTC()) {
		t.Fatalf("after an empty source: last_source = %q last_seen_at = %v", n.LastSource, n.LastSeenAt)
	}
}
