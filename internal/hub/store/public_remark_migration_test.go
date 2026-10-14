package store

import (
	"slices"
	"testing"
)

// schemaV32 冻结 v32 的完整 DDL：v31 加上 node.public_remark。逐字冻结，生产 DDL 后续变化不影响它。
var schemaV32 = append(slices.Clone(schemaV31),
	`ALTER TABLE node ADD COLUMN public_remark TEXT NOT NULL DEFAULT ''`,
)

// 旧库升级前没有公开备注，升级后旧节点的 public_remark 取空串：空串即"没有"，公开端对空串不下发有意义的字段，
// 不把升级伪装成站长写过什么。seedMinuteRow 建的节点是 7 号。
func TestMigrationFromV31AddsPublicRemark(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 31, seedMinuteRow)
	var remark string
	if err := migrated.r.QueryRow("SELECT public_remark FROM node WHERE id = 7").Scan(&remark); err != nil {
		t.Fatal(err)
	}
	if remark != "" {
		t.Fatalf("legacy node public_remark = %q, want \"\"", remark)
	}
	// 迁移后的库上新写入经 UpdateNode 带上这一列，读回同一份值；结构比对由 TestFrozenSchemasFollowMigrations 承担。
	if _, err := migrated.UpdateNode(t.Context(), 7, NodeEdit{Name: "kept", TrafficResetDay: 1, PublicRemark: "联通 4837"}); err != nil {
		t.Fatal(err)
	}
	if err := migrated.r.QueryRow("SELECT public_remark FROM node WHERE id = 7").Scan(&remark); err != nil {
		t.Fatal(err)
	}
	if remark != "联通 4837" {
		t.Fatalf("public_remark after update = %q, want %q", remark, "联通 4837")
	}
}
