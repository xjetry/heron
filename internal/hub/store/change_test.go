package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

var schemaV25 = append(slices.Clone(schemaV24),
	"ALTER TABLE api_token ADD COLUMN permissions TEXT NOT NULL DEFAULT '[]'",
	"ALTER TABLE api_token ADD COLUMN all_nodes INTEGER NOT NULL DEFAULT 1 CHECK (all_nodes IN (0, 1))",
	`CREATE TABLE api_token_node (token_id INTEGER NOT NULL,node_id INTEGER NOT NULL,PRIMARY KEY(token_id,node_id)) WITHOUT ROWID`,
	`CREATE TABLE operation (owner_key TEXT NOT NULL,owner_id INTEGER NOT NULL,request_id TEXT NOT NULL,request_hash TEXT NOT NULL,action TEXT NOT NULL,resource_id INTEGER NOT NULL,before_json TEXT NOT NULL,after_json TEXT NOT NULL,committed_at INTEGER NOT NULL,PRIMARY KEY(owner_key,request_id)) WITHOUT ROWID`,
	`CREATE INDEX operation_by_owner ON operation(owner_id,committed_at DESC,request_id)`,
	`CREATE INDEX operation_details_by_time ON operation(committed_at) WHERE before_json!='' OR after_json!=''`,
	"ALTER TABLE register_window RENAME TO register_window_old",
	`CREATE TABLE register_window (owner_id INTEGER PRIMARY KEY,key_hash BLOB NOT NULL UNIQUE,expires_at INTEGER NOT NULL,remaining INTEGER NOT NULL)`,
	"INSERT INTO register_window SELECT 0,key_hash,expires_at,remaining FROM register_window_old",
	"DROP TABLE register_window_old",
	"ALTER TABLE node_update ADD COLUMN owner_id INTEGER NOT NULL DEFAULT 0",
)

// permitPolicy 放行一切：本文件的用例验证写边界自身的状态机，授权裁决由 authz 包的用例钉住（store 不能 import
// 实现了策略的 authz）。
type permitPolicy struct{}

func (permitPolicy) Principal(context.Context, ChangeReader, *Change) (*APIToken, error) {
	return nil, nil
}
func (permitPolicy) Scope(context.Context, ChangeReader, *Change, *APIToken) error    { return nil }
func (permitPolicy) Resource(context.Context, ChangeReader, *Change, *APIToken) error { return nil }

func TestAgenticMigrationKeepsLegacyReadOnly(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 24, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO api_token(name,token_hash,created_at) VALUES('legacy',?,1)", hash(1)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO register_window(id,key_hash,expires_at,remaining) VALUES(1,?,2000000000,7)", hash(2)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO node(id,name,token_hash,created_at) VALUES(42,'legacy-node',?,1); INSERT INTO node_update(node_id,data) VALUES(42,'{"supported":true,"version":"v0.1.0"}')`, hash(3)); err != nil {
			t.Fatal(err)
		}
	})
	list, err := s.ListAPITokens(t.Context())
	if err != nil || len(list) != 1 || !list[0].AllNodes || len(list[0].Permissions) != 0 {
		t.Fatalf("legacy grant changed: %+v %v", list, err)
	}
	var owner, expires, remaining int64
	var key []byte
	if err := s.r.QueryRow("SELECT owner_id,key_hash,expires_at,remaining FROM register_window").Scan(&owner, &key, &expires, &remaining); err != nil || owner != 0 || !slices.Equal(key, hash(2)) || expires != 2000000000 || remaining != 7 {
		t.Fatalf("legacy window changed: owner=%d key=%x expires=%d remaining=%d err=%v", owner, key, expires, remaining, err)
	}
	var data string
	if err := s.r.QueryRow("SELECT owner_id,data FROM node_update WHERE node_id=42").Scan(&owner, &data); err != nil || owner != 0 || data != `{"supported":true,"version":"v0.1.0"}` {
		t.Fatalf("legacy update changed: owner=%d data=%s err=%v", owner, data, err)
	}
}

func TestAgenticDeferredCommitFailure(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for _, stmt := range []string{
		"PRAGMA foreign_keys=ON",
		"CREATE TABLE commit_parent(id INTEGER PRIMARY KEY)",
		"CREATE TABLE commit_guard(id INTEGER REFERENCES commit_parent(id) DEFERRABLE INITIALLY DEFERRED)",
		"CREATE TRIGGER reject_commit AFTER INSERT ON operation BEGIN INSERT INTO commit_guard VALUES(1); END",
	} {
		if _, err := s.w.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	var enabled int
	if err := s.w.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("deferred constraint not enabled: %d %v", enabled, err)
	}
	probe, err := s.w.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Exec("INSERT INTO commit_guard VALUES(1)"); err != nil {
		probe.Rollback()
		t.Fatalf("constraint failed before commit: %v", err)
	}
	if err := probe.Commit(); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("constraint did not fail at commit: %v", err)
	}
	newChange := func() *Change {
		t.Helper()
		c := &Change{Operation: Operation{RequestID: "commit-retry", RequestHash: "same"}, Action: ActionCreateNode, Policy: permitPolicy{}}
		var err error
		c.ExpectedVersion, err = s.ChangeVersion(t.Context(), c)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := newChange()
	if _, _, err := s.CreateNode(WithChange(t.Context(), c), NewNode{Name: "pending"}, hash(1)); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("deferred commit constraint did not fail: %v", err)
	}
	if c.ID == "" || c.CommittedAt != 0 || !c.claimed {
		t.Fatalf("failed commit reported applied change: %+v", c)
	}
	for _, table := range []string{"node", "operation", "commit_guard"} {
		var count int
		if err := s.r.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed commit retained %s rows: %d %v", table, count, err)
		}
	}
	if _, err := s.w.Exec("DROP TRIGGER reject_commit"); err != nil {
		t.Fatal(err)
	}
	c = newChange()
	if _, _, err := s.CreateNode(WithChange(t.Context(), c), NewNode{Name: "pending"}, hash(1)); err != nil || c.CommittedAt == 0 {
		t.Fatalf("retry after commit failure did not recover: %+v %v", c, err)
	}
	retry := newChange()
	if _, _, err := s.CreateNode(WithChange(t.Context(), retry), NewNode{Name: "pending"}, hash(1)); !errors.Is(err, ErrReplay) || retry.ID != c.ID {
		t.Fatalf("recovered commit did not replay: %+v %v", retry, err)
	}
}

func TestQueuedUpdateRevocationAndRegisterOwnership(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	p, err := s.CreateAPIToken(t.Context(), "writer", sha256.Sum256([]byte("writer")), clk.Now(), 100, &TokenGrant{Permissions: []Permission{PermissionRegister, PermissionUpdate}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPrincipal(t.Context(), p)
	if err := s.SetRegisterWindow(ctx, hash(2), clk.Now().Add(60000000000), 2); err != nil {
		t.Fatal(err)
	}
	node, _, err := s.RegisterNode(t.Context(), hash(2), "registered", hash(3))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListNodes(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != node {
		t.Fatalf("registration did not grant node: %+v %v", rows, err)
	}
	c := &Change{Operation: Operation{OwnerID: p.ID, RequestID: "update", RequestHash: "x", ResourceID: node}, Action: ActionStartUpdate, Policy: permitPolicy{}}
	c.ExpectedVersion, err = s.ChangeVersion(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	status := &heronv1.UpdateStatus{Supported: true, Task: &heronv1.UpdateTask{Id: "update", State: "queued"}}
	if err := s.StartNodeUpdate(WithChange(ctx, c), node, status); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAPIToken(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	status.Task.State = "dispatched"
	if err := s.SaveNodeUpdate(t.Context(), node, status); err != nil {
		t.Fatal(err)
	}
	if status.Task.State != "cancelled" {
		t.Fatalf("revoked update dispatched: %v", status)
	}
	if _, _, err := s.RegisterNode(t.Context(), hash(2), "late", hash(4)); !errors.Is(err, ErrNoWindow) {
		t.Fatalf("revoked registration allowed: %v", err)
	}
}

func TestAgenticReceiptsSurviveRestartRestoreAndRetention(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	create := func(id string, b byte) *Change {
		t.Helper()
		c := &Change{Operation: Operation{RequestID: id, RequestHash: id}, Action: ActionCreateNode, Policy: permitPolicy{}}
		var err error
		c.ExpectedVersion, err = s.ChangeVersion(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.CreateNode(WithChange(ctx, c), NewNode{Name: id}, hash(b)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	first := create("first", 1)
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := s.SnapshotConfig(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	clk.Advance(91 * 24 * time.Hour)
	create("after-snapshot", 2)
	old, err := s.FindOperation(ctx, 0, "first")
	if err != nil || old.BeforeJSON != "" || old.AfterJSON != "" {
		t.Fatalf("details retained beyond retention: %+v %v", old, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, s.path, snapshot, "", "", clk.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.path, clk, slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	operations, err := reopened.ListOperations(ctx, 0, "", 100)
	if err != nil || len(operations) != 2 {
		t.Fatalf("restore erased existing receipts: %+v %v", operations, err)
	}
	old, err = reopened.FindOperation(ctx, 0, "first")
	if err != nil || old.BeforeJSON != "" || old.AfterJSON != "" {
		t.Fatalf("restore resurrected expired audit details: %+v %v", old, err)
	}
	retry := &Change{Operation: Operation{RequestID: "first", RequestHash: "first"}, Action: ActionCreateNode, Policy: permitPolicy{}, ExpectedVersion: first.ExpectedVersion}
	if _, _, err := reopened.CreateNode(WithChange(ctx, retry), NewNode{Name: "duplicate"}, hash(3)); !errors.Is(err, ErrReplay) {
		t.Fatalf("durable retry was executed: %v", err)
	}
	nodes, err := reopened.ListNodes(ctx)
	if err != nil || len(nodes) != 1 || nodes[0].Name != "first" {
		t.Fatalf("retry mutated restored configuration: %+v %v", nodes, err)
	}
}

// targetFixture 是一个节点与一个读它原始状态的探针：名字、凭据哈希与回执行数。
type targetFixture struct {
	s    *Store
	node int64
}

func newTargetFixture(t *testing.T) targetFixture {
	t.Helper()
	s, _ := open(t)
	node, _, err := s.CreateNode(t.Context(), NewNode{Name: "original"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	return targetFixture{s: s, node: node}
}

func (f targetFixture) state(t *testing.T) (name string, token []byte, receipts int) {
	t.Helper()
	if err := f.s.r.QueryRow("SELECT name, token_hash FROM node WHERE id=?", f.node).Scan(&name, &token); err != nil {
		t.Fatal(err)
	}
	if err := f.s.r.QueryRow("SELECT COUNT(*) FROM operation").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	return name, token, receipts
}

func (f targetFixture) change(t *testing.T, action ChangeAction, preview bool) *Change {
	t.Helper()
	c := &Change{Operation: Operation{RequestID: action.String(), RequestHash: "h", ResourceID: f.node}, Action: action, Policy: permitPolicy{}, Preview: preview}
	if action == ActionCreateNode {
		c.ResourceID = 0
	}
	var err error
	if c.ExpectedVersion, err = f.s.ChangeVersion(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func rename(name string) NodeEdit { return NodeEdit{Name: name, TrafficResetDay: 1} }

// 同一节点上的兄弟操作：轮换 token 的变更下先来一次改节点目标的写。只比种类与资源时它会被当成主写审计提交，
// 节点快照不含凭据哈希，回执差异为空，随后的凭据写绕过审计。按操作比对时它在开事务之前被拒绝，库里没有任何
// 行变化，主写机会随之用掉，后面真正的凭据写也被拒绝。
func TestChangeTargetRejectsSiblingFirstWrite(t *testing.T) {
	t.Parallel()
	for _, preview := range []bool{false, true} {
		t.Run(fmt.Sprintf("preview=%t", preview), func(t *testing.T) {
			f := newTargetFixture(t)
			c := f.change(t, ActionRotateNodeToken, preview)
			ctx := WithChange(t.Context(), c)
			if _, err := f.s.UpdateNode(ctx, f.node, rename("renamed")); !errors.Is(err, ErrChangeTarget) {
				t.Fatalf("sibling write under rotate: %v, want ErrChangeTarget", err)
			}
			if !errors.Is(c.Err, ErrChangeTarget) || !c.claimed {
				t.Fatalf("mismatched first write not recorded on the change: err=%v claimed=%t", c.Err, c.claimed)
			}
			if err := f.s.SetTokenHash(ctx, f.node, hash(1), hash(2)); !errors.Is(err, ErrChangeTarget) {
				t.Fatalf("primary write after a mismatched one: %v, want ErrChangeTarget", err)
			}
			if name, token, receipts := f.state(t); name != "original" || !slices.Equal(token, hash(1)) || receipts != 0 {
				t.Fatalf("refused writes changed rows: name=%q token=%x receipts=%d", name, token, receipts)
			}
		})
	}
}

func TestChangeTargetMatchesActionAndResource(t *testing.T) {
	t.Parallel()
	f := newTargetFixture(t)
	other, _, err := f.s.CreateNode(t.Context(), NewNode{Name: "other"}, hash(3))
	if err != nil {
		t.Fatal(err)
	}
	c := f.change(t, ActionUpdateNode, false)
	if _, err := f.s.UpdateNode(WithChange(t.Context(), c), other, rename("wrong")); !errors.Is(err, ErrChangeTarget) {
		t.Fatalf("write on another resource: %v", err)
	}
	create := f.change(t, ActionCreateNode, false)
	create.ResourceID = f.node
	if _, _, err := f.s.CreateNode(WithChange(t.Context(), create), NewNode{Name: "x"}, hash(4)); !errors.Is(err, ErrChangeTarget) {
		t.Fatalf("create carrying a resource id: %v", err)
	}
	if _, err := f.s.GetNode(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if name, _, receipts := f.state(t); name != "original" || receipts != 0 {
		t.Fatalf("refused writes changed rows: name=%q receipts=%d", name, receipts)
	}
	// 没有变更的 ctx：目标写方法就是普通写。
	if _, err := f.s.UpdateNode(t.Context(), f.node, rename("direct")); err != nil {
		t.Fatal(err)
	}
	if name, _, receipts := f.state(t); name != "direct" || receipts != 0 {
		t.Fatalf("direct write: name=%q receipts=%d", name, receipts)
	}
}

// 普通写永不消费变更：主写之前的普通写照常提交、不审计，主写仍是被审计的那一次；主写之后的派生写同样照常。
func TestPlainWritesNeverConsumeTheChange(t *testing.T) {
	t.Parallel()
	f := newTargetFixture(t)
	c := f.change(t, ActionUpdateNode, false)
	ctx := WithChange(t.Context(), c)
	if err := f.s.ReorderNodes(ctx, []int64{f.node}); err != nil {
		t.Fatal(err)
	}
	if c.claimed {
		t.Fatal("plain write consumed the change")
	}
	if _, err := f.s.UpdateNode(ctx, f.node, rename("audited")); err != nil || c.CommittedAt == 0 || !strings.Contains(c.AfterJSON, "audited") {
		t.Fatalf("primary write after a plain one: receipt=%+v err=%v", c.Operation, err)
	}
	if err := f.s.ReorderNodes(ctx, []int64{f.node}); err != nil {
		t.Fatalf("derived plain write after commit: %v", err)
	}
	if name, _, receipts := f.state(t); name != "audited" || receipts != 1 {
		t.Fatalf("name=%q receipts=%d", name, receipts)
	}
}

func TestChangeTargetPreviewRollsBack(t *testing.T) {
	t.Parallel()
	f := newTargetFixture(t)
	c := f.change(t, ActionUpdateNode, true)
	if _, err := f.s.UpdateNode(WithChange(t.Context(), c), f.node, rename("previewed")); !errors.Is(err, ErrPreview) {
		t.Fatalf("preview: %v", err)
	}
	if c.Version == "" || c.Version != c.ExpectedVersion || c.BeforeJSON == c.AfterJSON || !strings.Contains(c.AfterJSON, "previewed") || c.Operation.Action != "update_node" || c.CommittedAt != 0 {
		t.Fatalf("preview receipt: version=%q expected=%q op=%+v", c.Version, c.ExpectedVersion, c.Operation)
	}
	if name, _, receipts := f.state(t); name != "original" || receipts != 0 {
		t.Fatalf("preview changed rows: name=%q receipts=%d", name, receipts)
	}
}

// 一次变更只有一个主写：第二次 writeChange 被拒绝，第一次已提交的业务写与回执保留。
func TestChangeTargetRejectsSecondPrimaryWrite(t *testing.T) {
	t.Parallel()
	f := newTargetFixture(t)
	c := f.change(t, ActionUpdateNode, false)
	ctx := WithChange(t.Context(), c)
	if _, err := f.s.UpdateNode(ctx, f.node, rename("first")); err != nil {
		t.Fatal(err)
	}
	committed := c.Operation
	if _, err := f.s.UpdateNode(ctx, f.node, rename("second")); !errors.Is(err, ErrChangeTarget) {
		t.Fatalf("second primary write: %v", err)
	}
	if c.Err != nil || c.Operation != committed || c.CommittedAt == 0 {
		t.Fatalf("refused second write disturbed the committed change: err=%v op=%+v", c.Err, c.Operation)
	}
	if name, _, receipts := f.state(t); name != "first" || receipts != 1 {
		t.Fatalf("name=%q receipts=%d", name, receipts)
	}
	o, err := f.s.FindOperation(t.Context(), 0, c.RequestID)
	if err != nil || o.ID != committed.ID || !strings.Contains(o.AfterJSON, "first") {
		t.Fatalf("committed receipt: %+v %v", o, err)
	}
}

// 业务拒绝发生在主写之前：变更没有被使用，库与回执都不变。
func TestBusinessRejectionLeavesChangeUnclaimed(t *testing.T) {
	t.Parallel()
	f := newTargetFixture(t)
	c := &Change{Operation: Operation{RequestID: "probe", RequestHash: "h"}, Action: ActionSaveProbeTask, Policy: permitPolicy{}}
	var err error
	if c.ExpectedVersion, err = f.s.ChangeVersion(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.SaveProbeTask(WithChange(t.Context(), c), &heronv1.ProbeTask{}, NodeSelector{AllNodes: true, NodeIDs: []int64{f.node}}); err == nil {
		t.Fatal("contradictory selector accepted")
	}
	if c.claimed || c.Err != nil || c.CommittedAt != 0 {
		t.Fatalf("rejected request used the change: %+v", c)
	}
	if _, _, receipts := f.state(t); receipts != 0 {
		t.Fatalf("receipts=%d", receipts)
	}
}

func TestWithChangeRejectsMisassembly(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]*Change{
		"nil policy":          {Action: ActionDeleteTag},
		"unregistered action": {Policy: permitPolicy{}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("misassembled change accepted")
				}
			}()
			WithChange(t.Context(), c)
		})
	}
}

// 开始与取消更新共用 saveNodeUpdate，但各自声明目标：取消的变更下调用开始更新的写被拒绝，节点更新状态不变；
// 后台对账的 SaveNodeUpdate 不声明目标，不会用掉变更。
func TestNodeUpdateSiblingsDeclareDistinctTargets(t *testing.T) {
	t.Parallel()
	f := newTargetFixture(t)
	initial := &heronv1.UpdateStatus{Supported: true, Version: "v0.1.0", Task: &heronv1.UpdateTask{Id: "queued", State: "queued"}}
	if err := f.s.SaveNodeUpdate(t.Context(), f.node, initial); err != nil {
		t.Fatal(err)
	}
	c := f.change(t, ActionCancelUpdate, false)
	ctx := WithChange(t.Context(), c)
	if err := f.s.SaveNodeUpdate(ctx, f.node, initial); err != nil || c.claimed {
		t.Fatalf("reconcile write under a change: claimed=%t err=%v", c.claimed, err)
	}
	started := &heronv1.UpdateStatus{Supported: true, Version: "v0.1.0", Task: &heronv1.UpdateTask{Id: "other", State: "queued"}}
	if err := f.s.StartNodeUpdate(ctx, f.node, started); !errors.Is(err, ErrChangeTarget) {
		t.Fatalf("start under cancel: %v, want ErrChangeTarget", err)
	}
	states, err := f.s.NodeUpdates(t.Context())
	if err != nil || states[f.node].GetTask().GetId() != "queued" {
		t.Fatalf("refused start changed the update state: %v %v", states[f.node], err)
	}
	if _, _, receipts := f.state(t); receipts != 0 {
		t.Fatalf("receipts=%d", receipts)
	}
	cancel := f.change(t, ActionCancelUpdate, false)
	cancelled := &heronv1.UpdateStatus{Supported: true, Version: "v0.1.0", Task: &heronv1.UpdateTask{Id: "queued", State: "cancelled"}}
	if err := f.s.CancelNodeUpdate(WithChange(t.Context(), cancel), f.node, cancelled); err != nil || cancel.CommittedAt == 0 {
		t.Fatalf("matching cancel: receipt=%+v err=%v", cancel.Operation, err)
	}
}
