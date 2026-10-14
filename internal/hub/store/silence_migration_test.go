package store

import (
	"slices"
	"testing"
)

// schemaV27 冻结 v27 的完整 DDL：v26 加上维护静默的存储结构。逐字冻结，生产 DDL 后续变化不影响它。
var schemaV27 = append(slices.Clone(schemaV26),
	`ALTER TABLE node ADD COLUMN maintenance INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE silence (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  all_nodes INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL,
  start_hhmm TEXT NOT NULL DEFAULT '',
  end_hhmm TEXT NOT NULL DEFAULT '',
  from_at INTEGER NOT NULL DEFAULT 0,
  until_at INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
)`,
	`CREATE TABLE silence_node (
  silence_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  PRIMARY KEY (silence_id, node_id)
)`,
	`CREATE INDEX silence_node_by_node ON silence_node(node_id)`,
	`CREATE TABLE silence_tag (
  silence_id INTEGER NOT NULL,
  tag_id INTEGER NOT NULL,
  PRIMARY KEY (silence_id, tag_id)
) WITHOUT ROWID`,
	`CREATE INDEX silence_tag_by_tag ON silence_tag (tag_id)`,
	`ALTER TABLE alert_event ADD COLUMN silenced INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE alert_state ADD COLUMN fired_silenced INTEGER NOT NULL DEFAULT 0`,
)

// 旧行的静默列一律取默认值：升级前的节点未被置于维护、历史事件与状态也不得被标成已静默。
func TestMigrationFromV26AddsSilenceStructure(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 26, seedMinuteRow)
	var maintenance int
	if err := migrated.r.QueryRow("SELECT maintenance FROM node WHERE id = 7").Scan(&maintenance); err != nil {
		t.Fatal(err)
	}
	if maintenance != 0 {
		t.Fatalf("legacy node maintenance = %d, want 0", maintenance)
	}
	for _, table := range []string{"silence", "silence_node", "silence_tag"} {
		var n int
		if err := migrated.r.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: count=%d err=%v, want an empty new table", table, n, err)
		}
	}
	for _, column := range []struct{ table, name string }{
		{"alert_event", "silenced"}, {"alert_state", "fired_silenced"},
	} {
		var n int
		if err := migrated.r.QueryRow("SELECT count(*) FROM pragma_table_info(?) WHERE name=?", column.table, column.name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s.%s present=%d err=%v, want the added column", column.table, column.name, n, err)
		}
	}
}
