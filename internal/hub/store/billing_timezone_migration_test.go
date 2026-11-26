package store

import (
	"database/sql"
	"slices"
	"testing"
)

// schemaV55 冻结 v55 的完整 DDL：v54 加上 node 的重置时刻与计费时区两列，并按两列重建节点资料版本的更新触发器。
var schemaV55 = append(append(slices.Clone(schemaV54), migrationV55Config...), migrationV55Trigger...)

// removeV55Config 把当前配置库或快照退回 v54 的 node 结构，供从旧版本起步的迁移与恢复测试使用。
func removeV55Config(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("ALTER TABLE node DROP COLUMN billing_timezone; ALTER TABLE node DROP COLUMN traffic_reset_minute"); err != nil {
		t.Fatal(err)
	}
}

// v54 的行迁上来取列默认值：重置时刻 00:00、计费时区空串（沿用 hub 时区），周期规则与升级前相同；迁移重建的触发器
// 随即生效，改这两列推进节点资料版本。
func TestMigrationFromV54AddsResetMinuteAndBillingTimezone(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 54, seedMinuteRow)
	var minute int
	var timezone string
	if err := s.r.QueryRow("SELECT traffic_reset_minute, billing_timezone FROM node WHERE id = 7").Scan(&minute, &timezone); err != nil {
		t.Fatal(err)
	}
	if minute != 0 || timezone != "" {
		t.Fatalf("v54 row migrated to (%d, %q), want (0, \"\")", minute, timezone)
	}
	schedules, err := s.TrafficSchedules(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := schedules[7]; got != (TrafficSchedule{Day: DefaultTrafficResetDay}) {
		t.Fatalf("migrated schedule = %+v, want the column defaults", got)
	}
	before := revisionOf(t, s)
	mustExec(t, s, "UPDATE node SET billing_timezone = 'UTC' WHERE id = 7")
	if after := revisionOf(t, s); after == before {
		t.Fatal("changing billing_timezone after the migration did not bump the node list revision")
	}
}
