package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// schemaV58 冻结 v58 的完整 DDL：v57 加上 node.billing_anchor_day。
var schemaV58 = append(slices.Clone(schemaV57), migrationV58Config...)

// removeV58Config 把当前配置库或快照退回 v57 的 node 结构，供从旧版本起步的迁移与恢复测试使用。
func removeV58Config(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("ALTER TABLE node DROP COLUMN billing_anchor_day"); err != nil {
		t.Fatal(err)
	}
}

// v57 升到 v58：现有合法 expires_on 的节点按其日号初始化 billing_anchor_day；没有到期日或手改的不可解析日期留 NULL。
func TestMigrationFromV57InitializesBillingAnchorDay(t *testing.T) {
	t.Parallel()
	rows := []struct {
		id         int64
		name       string
		expiresOn  string
		wantAnchor sql.NullInt64
	}{
		{1, "jan31", "2026-01-31", sql.NullInt64{Int64: 31, Valid: true}},
		{2, "leap29", "2024-02-29", sql.NullInt64{Int64: 29, Valid: true}},
		{3, "drifted28", "2026-02-28", sql.NullInt64{Int64: 28, Valid: true}},
		{4, "noexpiry", "", sql.NullInt64{}},
		{5, "unreadable", "2026-09-31", sql.NullInt64{}},
	}
	s := migrateFrom(t, 57, func(t *testing.T, db *sql.DB) {
		for _, r := range rows {
			if _, err := db.Exec("INSERT INTO node (id, name, token_hash, created_at, billing_cycle, expires_on) VALUES (?, ?, ?, 1, 'monthly', ?)",
				r.id, r.name, hash(byte(r.id)), r.expiresOn); err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, r := range rows {
		var anchor sql.NullInt64
		if err := s.r.QueryRow("SELECT billing_anchor_day FROM node WHERE id = ?", r.id).Scan(&anchor); err != nil {
			t.Fatal(err)
		}
		if anchor != r.wantAnchor {
			t.Errorf("%s (%q): billing_anchor_day = %+v, want %+v", r.name, r.expiresOn, anchor, r.wantAnchor)
		}
		n, err := s.GetNode(t.Context(), r.id)
		if err != nil {
			t.Fatal(err)
		}
		if n.Billing.AnchorDay != int(r.wantAnchor.Int64) {
			t.Errorf("%s: GetNode AnchorDay = %d, want %d", r.name, n.Billing.AnchorDay, r.wantAnchor.Int64)
		}
	}
}

// 写入口对计费锚定日号的推导、保留与拒绝：
//   - CreateNode 与 UpdateNodeTasks 在写入新到期日时按新日期的日号写入；
//   - 续期推进到短月（2026-02-28，锚定 31）后，UpdateNodeTasks 改备注等非到期字段时保留原锚定 31；
//   - 改到新到期日时改锚为新日号，清空到期日时写 NULL；
//   - 越界锚定日号、无到期日却带锚定日号或非法到期日文本全部拒绝且不改库。
func TestBillingAnchorDayWriteAdmissionAndPreservation(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()

	readAnchor := func(id int64) sql.NullInt64 {
		t.Helper()
		var v sql.NullInt64
		if err := s.r.QueryRow("SELECT billing_anchor_day FROM node WHERE id = ?", id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// 新建带到期日：锚定日号 = 31。
	id, _, err := s.CreateNode(ctx, NewNode{Name: "eom", Billing: Billing{Cycle: CycleMonthly, ExpiresOn: "2026-01-31"}}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAnchor(id); !got.Valid || got.Int64 != 31 {
		t.Fatalf("after CreateNode(2026-01-31): billing_anchor_day = %+v, want 31", got)
	}

	// 新建不带到期日：锚定日号 = NULL。
	plain, _, err := s.CreateNode(ctx, NewNode{Name: "plain"}, hash(2))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAnchor(plain); got.Valid {
		t.Fatalf("after CreateNode(no expiry): billing_anchor_day = %+v, want NULL", got)
	}

	// 续期推进到 2 月 28 日：到期日变，锚定日号留在 31。
	if err := s.RenewNodeBilling(ctx, id, CycleMonthly, "2026-01-31", 31, "2026-02-28"); err != nil {
		t.Fatal(err)
	}
	if got := readAnchor(id); !got.Valid || got.Int64 != 31 {
		t.Fatalf("after RenewNodeBilling: billing_anchor_day = %+v, want 31", got)
	}

	// 到期日未变、只改备注或名称（调用方不传 AnchorDay，与面板表单整体替换一致）：保留 31 且不报计费变化。
	res, err := s.UpdateNodeTasks(ctx, id, NodeEdit{
		Name: "eom-renamed", Note: "updated note", TrafficResetDay: 1,
		Billing: Billing{Cycle: CycleMonthly, ExpiresOn: "2026-02-28"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.BillingChanged {
		t.Fatal("editing only name and note on a clamped-month node reported BillingChanged = true")
	}
	if got := readAnchor(id); !got.Valid || got.Int64 != 31 {
		t.Fatalf("after unrelated UpdateNodeTasks: billing_anchor_day = %+v, want preserved 31", got)
	}

	// 显式改到期日为 2026-03-15（即使调用方带着旧的 AnchorDay=31）：锚定日号更新为 15。
	res, err = s.UpdateNodeTasks(ctx, id, NodeEdit{
		Name: "eom-renamed", TrafficResetDay: 1,
		Billing: Billing{Cycle: CycleMonthly, ExpiresOn: "2026-03-15", AnchorDay: 31},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.BillingChanged {
		t.Fatal("changing expires_on reported BillingChanged = false")
	}
	if got := readAnchor(id); !got.Valid || got.Int64 != 15 {
		t.Fatalf("after changing expires_on to 2026-03-15: billing_anchor_day = %+v, want 15", got)
	}

	// 清空到期日：锚定日号回到 NULL。
	res, err = s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "eom-renamed", TrafficResetDay: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !res.BillingChanged {
		t.Fatal("clearing expires_on reported BillingChanged = false")
	}
	if got := readAnchor(id); got.Valid {
		t.Fatalf("after clearing expires_on: billing_anchor_day = %+v, want NULL", got)
	}

	// 各类非法输入在 CreateNode 与 UpdateNodeTasks 两处都拒绝且不落盘。
	beforeNodes := nodeCount(t, s)
	for _, tc := range []struct {
		name string
		b    Billing
		want string
	}{
		{"anchor without expiry", Billing{AnchorDay: 15}, "billing_anchor_day must be empty when expires_on is empty"},
		{"negative anchor", Billing{ExpiresOn: "2026-01-31", AnchorDay: -1}, "billing_anchor_day must be between 1 and 31"},
		{"anchor over 31", Billing{ExpiresOn: "2026-01-31", AnchorDay: 32}, "billing_anchor_day must be between 1 and 31"},
		{"invalid expires_on", Billing{ExpiresOn: "2026-02-30"}, "billing.expires_on: must be an existing date in YYYY-MM-DD form"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.CreateNode(ctx, NewNode{Name: "bad", Billing: tc.b}, hash(9)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("CreateNode(%+v) err = %v, want %q", tc.b, err, tc.want)
			}
			if _, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "mutated", TrafficResetDay: 1, Billing: tc.b}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("UpdateNodeTasks(%+v) err = %v, want %q", tc.b, err, tc.want)
			}
		})
	}
	if after := nodeCount(t, s); after != beforeNodes {
		t.Fatalf("rejected CreateNode changed node count from %d to %d", beforeNodes, after)
	}
	if n, err := s.GetNode(ctx, id); err != nil || n.Name != "eom-renamed" || n.Billing != (Billing{}) {
		t.Fatalf("rejected UpdateNodeTasks modified node: %+v %v", n, err)
	}
}

// 快照恢复预检核对 billing_anchor_day 的范围与空值不变式，拒绝绕过写入口篡改出的非法快照；
// 合法的短月钳位快照原样恢复，v57 旧快照在迁移阶段按 expires_on 初始化后通过预检。
func TestRestoreBillingAnchorDayValidationAndMigration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		expiresOn string
		sqlSet    string
		want      string
	}{
		{"zero with expiry", "2026-01-31", "billing_anchor_day = 0", "billing_anchor_day must be between 1 and 31; got 0"},
		{"negative with expiry", "2026-01-31", "billing_anchor_day = -1", "billing_anchor_day must be between 1 and 31; got -1"},
		{"over 31 with expiry", "2026-01-31", "billing_anchor_day = 32", "billing_anchor_day must be between 1 and 31; got 32"},
		{"null with valid expiry", "2026-01-31", "billing_anchor_day = NULL", `billing_anchor_day must be between 1 and 31 when expires_on ("2026-01-31") is set; got empty`},
		{"non-null without expiry", "", "billing_anchor_day = 15", "billing_anchor_day must be empty when expires_on is empty; got 15"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source, _ := open(t)
			id, _, err := source.CreateNode(t.Context(), NewNode{Name: "ok", Billing: Billing{ExpiresOn: tc.expiresOn}}, hash(1))
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, source, "UPDATE node SET "+tc.sqlSet+" WHERE id = ?", id)
			config := filepath.Join(t.TempDir(), "config.db")
			if err := source.SnapshotConfig(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			source.Close()
			target := filepath.Join(t.TempDir(), "target.db")
			_, err = Restore(t.Context(), target, config, "", "", time.Now(), slog.Default())
			want := fmt.Sprintf("config node %d (\"ok\"): %s", id, tc.want)
			if err == nil || err.Error() != want {
				t.Fatalf("Restore err = %v, want %q", err, want)
			}
		})
	}

	t.Run("clamped snapshot preserves anchor day", func(t *testing.T) {
		t.Parallel()
		source, clk := open(t)
		ctx := t.Context()
		id, _, err := source.CreateNode(ctx, NewNode{Name: "eom", Billing: Billing{Cycle: CycleMonthly, ExpiresOn: "2026-01-31"}}, hash(1))
		if err != nil {
			t.Fatal(err)
		}
		if err := source.RenewNodeBilling(ctx, id, CycleMonthly, "2026-01-31", 31, "2026-02-28"); err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(t.TempDir(), "config.db")
		if err := source.SnapshotConfig(ctx, config); err != nil {
			t.Fatal(err)
		}
		source.Close()
		target := filepath.Join(t.TempDir(), "target.db")
		if _, err := Restore(ctx, target, config, "", "", clk.Now(), slog.Default()); err != nil {
			t.Fatal(err)
		}
		restored, err := Open(target, clk, slog.Default(), RequireCurrentSchema)
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		n, err := restored.GetNode(ctx, id)
		if err != nil || n.Billing.ExpiresOn != "2026-02-28" || n.Billing.AnchorDay != 31 {
			t.Fatalf("restored billing = %+v %v, want expires_on=2026-02-28 anchor=31", n.Billing, err)
		}
	})

	t.Run("v57 snapshot initializes anchor day on restore", func(t *testing.T) {
		t.Parallel()
		source, clk := open(t)
		ctx := t.Context()
		a, _, err := source.CreateNode(ctx, NewNode{Name: "dated", Billing: Billing{Cycle: CycleMonthly, ExpiresOn: "2026-01-31"}}, hash(1))
		if err != nil {
			t.Fatal(err)
		}
		b, _, err := source.CreateNode(ctx, NewNode{Name: "undated"}, hash(2))
		if err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(t.TempDir(), "config.db")
		if err := source.SnapshotConfig(ctx, config); err != nil {
			t.Fatal(err)
		}
		source.Close()
		db, err := sql.Open("sqlite", config)
		if err != nil {
			t.Fatal(err)
		}
		removeV58Config(t, db)
		if _, err := db.Exec("UPDATE snapshot_meta SET schema_version = 57"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		target := filepath.Join(t.TempDir(), "target.db")
		if _, err := Restore(ctx, target, config, "", "", clk.Now(), slog.Default()); err != nil {
			t.Fatal(err)
		}
		restored, err := Open(target, clk, slog.Default(), RequireCurrentSchema)
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		na, err := restored.GetNode(ctx, a)
		if err != nil || na.Billing.AnchorDay != 31 {
			t.Fatalf("restored dated node = %+v %v, want anchor=31", na.Billing, err)
		}
		nb, err := restored.GetNode(ctx, b)
		if err != nil || nb.Billing.AnchorDay != 0 {
			t.Fatalf("restored undated node = %+v %v, want anchor=0", nb.Billing, err)
		}
	})
}
