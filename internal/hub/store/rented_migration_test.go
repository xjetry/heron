package store

import (
	"errors"
	"slices"
	"testing"
)

// schemaV44 冻结 v44 的完整 DDL：v43 加上 node 表末尾的 rented 列（迁移 44 的那一条，列序与新建库一致）。
// 逐字冻结，生产 DDL 后续变化不影响它。
var schemaV44 = append(slices.Clone(schemaV43),
	`ALTER TABLE node ADD COLUMN rented INTEGER NOT NULL DEFAULT 0`,
)

// v44 给 node 表加 rented；迁上来的行都是 0（未出租），之后经 UpdateNodeTasks 打开。
func TestMigrationFromV43AddsRentedColumn(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 43, seedMinuteRow)

	var rented bool
	if err := s.r.QueryRow("SELECT rented FROM node WHERE id = 7").Scan(&rented); err != nil {
		t.Fatal(err)
	}
	if rented {
		t.Error("v43 的行迁到 44 后 rented 应为 0：既有节点都未出租")
	}
	res, err := s.UpdateNodeTasks(t.Context(), 7, NodeEdit{Name: "kept", Rented: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.RentedChanged || res.BillingChanged {
		t.Errorf("只翻转 rented：RentedChanged=%v BillingChanged=%v，应为 true/false", res.RentedChanged, res.BillingChanged)
	}
	if err := s.r.QueryRow("SELECT rented FROM node WHERE id = 7").Scan(&rented); err != nil {
		t.Fatal(err)
	}
	if !rented {
		t.Error("UpdateNodeTasks(Rented: true) 之后库里应为已出租")
	}
	nodes, err := s.ListNodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || !nodes[0].Rented {
		t.Fatalf("ListNodes 读出的 rented = %+v，要唯一节点已出租", nodes)
	}
}

// 维护中与已出租互斥：UpdateNodeTasks 是全列写入口，冲突在这里裁决，一行都不改。
func TestUpdateNodeRejectsRentedMaintenanceConflict(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	// 建节点时直接带出租标记合法（maintenance 恒为假）。
	rentedID, _, err := s.CreateNode(ctx, NewNode{Name: "r", Rented: true}, hash(2))
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNode(ctx, rentedID)
	if err != nil || !n.Rented {
		t.Fatalf("CreateNode(Rented: true) 读回 Rented=%v, err=%v", n.Rented, err)
	}
	// 单独开维护合法。
	if _, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "n", Maintenance: true}); err != nil {
		t.Fatal(err)
	}
	// 一次编辑把两个开关同时打开：冲突，返回 RentedConflictError，一行都不改。
	_, err = s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "n", Maintenance: true, Rented: true})
	var conflict RentedConflictError
	if !errors.As(err, &conflict) || !conflict.Maintenance {
		t.Fatalf("UpdateNodeTasks err = %v，要 RentedConflictError{Maintenance: true}", err)
	}
	var maintenance, rented bool
	if err := s.r.QueryRow("SELECT maintenance, rented FROM node WHERE id = ?", id).Scan(&maintenance, &rented); err != nil {
		t.Fatal(err)
	}
	if !maintenance || rented {
		t.Errorf("冲突须什么都不写：maintenance=%v rented=%v，要 true/false", maintenance, rented)
	}
	// 同一次更新里关掉维护并标出租（NodeEdit 是整体替换，结果对只有一面为真）：合法。
	if _, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "n", Maintenance: false, Rented: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.r.QueryRow("SELECT maintenance, rented FROM node WHERE id = ?", id).Scan(&maintenance, &rented); err != nil {
		t.Fatal(err)
	}
	if maintenance || !rented {
		t.Errorf("关维护并开出租后 maintenance=%v rented=%v，要 false/true", maintenance, rented)
	}
}
