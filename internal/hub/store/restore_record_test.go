package store

import (
	"database/sql"
	"strings"
	"testing"
)

// 直接 INSERT 而不经 Restore：这里测的是读面对写入口约定的依赖（列形态与两个 JSON 列），
// 不需要一次真实恢复。写入形由 Restore 保证：id 是随机十六进制，orphans 与 themes 是 json.Marshal 的产物。
func insertRestoreRecord(t *testing.T, s *Store, id string, restoredAt, configAt int64, metrics sql.NullInt64, orphans, themes string) {
	t.Helper()
	err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO restore_record (id,restored_at,config_taken_at,metrics_taken_at,orphans,themes) VALUES (?,?,?,?,?,?)",
			id, restoredAt, configAt, metrics, orphans, themes)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestListRestoreRecords(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	insertRestoreRecord(t, s, "latest", 900, 800, sql.NullInt64{Int64: 850, Valid: true},
		`{"probe_1h":2,"metric_1m":10}`, `{"restored":["dark"],"missing":["old"],"ignored":["stray.zip"]}`)
	// 同一秒的两次恢复靠 id 打破平局（升序）；这次恢复没带指标层，orphans 是空对象。
	insertRestoreRecord(t, s, "a", 500, 400, sql.NullInt64{}, `{}`, `{"restored":["dark"]}`)
	insertRestoreRecord(t, s, "b", 500, 450, sql.NullInt64{Int64: 460, Valid: true}, `{"metric_1m":0}`, `{}`)
	records, err := s.ListRestoreRecords(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("records=%d, want 3: %+v", len(records), records)
	}
	if records[0].ID != "latest" || records[1].ID != "a" || records[2].ID != "b" {
		t.Fatalf("order by restored_at desc, id asc: got %s %s %s", records[0].ID, records[1].ID, records[2].ID)
	}
	if records[0].RestoredAt != 900 || records[0].ConfigTakenAt != 800 || records[0].MetricsTakenAt == nil || *records[0].MetricsTakenAt != 850 {
		t.Fatalf("latest=%+v", records[0])
	}
	if records[0].Orphans["metric_1m"] != 10 || records[0].Orphans["probe_1h"] != 2 {
		t.Fatalf("latest orphans=%+v", records[0].Orphans)
	}
	if got := records[0].Themes; len(got.Restored) != 1 || got.Restored[0] != "dark" || len(got.Missing) != 1 || got.Missing[0] != "old" || len(got.Ignored) != 1 || got.Ignored[0] != "stray.zip" {
		t.Fatalf("latest themes=%+v", got)
	}
	// 没带指标层的恢复：缺席是 nil 指针，不是 0；orphans 空对象解成空 map。
	if records[1].MetricsTakenAt != nil {
		t.Fatalf("config-only restore metrics_taken_at=%d, want absent", *records[1].MetricsTakenAt)
	}
	if len(records[1].Orphans) != 0 || len(records[2].Orphans) != 1 || records[2].Orphans["metric_1m"] != 0 {
		t.Fatalf("orphans: a=%+v b=%+v", records[1].Orphans, records[2].Orphans)
	}

	limited, err := s.ListRestoreRecords(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 || limited[0].ID != "latest" || limited[1].ID != "a" {
		t.Fatalf("limit 2 returned %+v", limited)
	}

	// 两个 JSON 列只有 Restore 一个写者且总是 json.Marshal 的产物；坏值是库损坏，读侧必须报错而不是吞掉一行。
	insertRestoreRecord(t, s, "broken", 1000, 900, sql.NullInt64{}, `{"metric_1m":`, `{}`)
	if _, err := s.ListRestoreRecords(t.Context(), 20); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("corrupt orphans JSON: err=%v, want error naming the record", err)
	}
}
