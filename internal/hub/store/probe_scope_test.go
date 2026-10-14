package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/probelimit"
)

// v9 的完整 DDL：v8 加上计费与到期的七次 ADD COLUMN。
var schemaV9 = append(slices.Clone(schemaV8),
	"ALTER TABLE node ADD COLUMN price TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE node ADD COLUMN currency TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE node ADD COLUMN billing_cycle TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE node ADD COLUMN expires_on TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE node ADD COLUMN auto_renew INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE alert_rule ADD COLUMN days_before INTEGER",
	"ALTER TABLE alert_state ADD COLUMN fired_expires_on TEXT NOT NULL DEFAULT ''",
)

// 旧库里的任务升级后 all_nodes 取默认值 0：仍按原来的分配行覆盖，不会被放宽到全部节点。
func TestMigrationFromV9MatchesFreshSchemaAndKeepsTaskScope(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 9, func(t *testing.T, db *sql.DB) {
		seedMinuteRow(t, db)
		for _, stmt := range []string{
			"INSERT INTO node (id, name, token_hash, created_at) VALUES (8, 'other', x'01', 1)",
			"INSERT INTO probe_task (id, kind, target, interval_s, timeout_ms, created_at) VALUES (4, 1, '127.0.0.1', 5, 100, 1)",
			"INSERT INTO probe_task (id, kind, target, interval_s, timeout_ms, created_at) VALUES (5, 1, '127.0.0.2', 5, 100, 1)",
			"INSERT INTO probe_task_node (task_id, node_id) VALUES (4, 7)",
			"UPDATE probe_meta SET version = 42",
		} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
	})
	if v := userVersion(t, migrated.r); v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	var flags []int
	rows, err := migrated.r.Query("SELECT all_nodes FROM probe_task ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var f int
		if err := rows.Scan(&f); err != nil {
			t.Fatal(err)
		}
		flags = append(flags, f)
	}
	rows.Close()
	if !reflect.DeepEqual(flags, []int{0, 0}) {
		t.Fatalf("all_nodes after migration = %v, want [0 0]", flags)
	}
	version, recs, err := migrated.LoadProbeTasks(t.Context())
	// 迁移给已有任务生成配置身份并推进清单版本，否则 agent 会继续拿着没有身份的旧清单。
	if err != nil || version <= 42 || len(recs) != 2 {
		t.Fatalf("tasks after migration: version=%d recs=%+v err=%v, want version bumped above 42", version, recs, err)
	}
	if recs[0].AllNodes || !reflect.DeepEqual(recs[0].NodeIDs, []int64{7}) || recs[1].AllNodes || recs[1].NodeIDs != nil {
		t.Fatalf("task scope after migration: %+v", recs)
	}
}

// all_nodes 任务不写分配行，覆盖在读取时从节点表展开：保存时已有的节点、之后新建的节点都在内，删除的节点不在；
// 请求只带一个作用域模式，不能混入显式分配。
func TestAllNodesTaskCoversEveryNodeWithoutAssignmentRows(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	a, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	b, _, _ := s.CreateNode(ctx, NewNode{Name: "b"}, hash(2))
	saved, version, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: true, NodeIDs: nil})
	if err != nil || !saved.AllNodes || !reflect.DeepEqual(saved.NodeIDs, []int64{a, b}) {
		t.Fatalf("save all_nodes: %+v err=%v", saved, err)
	}
	assertTasks(t, s, version, []ProbeTaskRecord{{Task: saved.Task, AllNodes: true, NodeIDs: []int64{a, b}}})
	c, created, err := s.CreateNode(ctx, NewNode{Name: "c"}, hash(3))
	if err != nil {
		t.Fatal(err)
	}
	version = created.Version
	assertTasks(t, s, version, []ProbeTaskRecord{{Task: saved.Task, AllNodes: true, NodeIDs: []int64{a, b, c}}})
	if ids, err := s.ProbeTaskNodeIDs(ctx, saved.Task.Id); err != nil || !reflect.DeepEqual(ids, []int64{a, b, c}) {
		t.Fatalf("ProbeTaskNodeIDs after create = %v %v, want [%d %d %d]", ids, err, a, b, c)
	}
	if err := s.DeleteNode(ctx, a); err != nil {
		t.Fatal(err)
	}
	assertTasks(t, s, version, []ProbeTaskRecord{{Task: saved.Task, AllNodes: true, NodeIDs: []int64{b, c}}})
	if ids, err := s.ProbeTaskNodeIDs(ctx, saved.Task.Id); err != nil || !reflect.DeepEqual(ids, []int64{b, c}) {
		t.Fatalf("ProbeTaskNodeIDs after delete = %v %v, want [%d %d]", ids, err, b, c)
	}
}

// all_nodes 为假时分配行就是全部覆盖：空集不覆盖任何节点，之后新建的节点也不纳入；从全部节点改回显式空集也不例外。
func TestExplicitEmptyScopeCoversNoNode(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	a, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	all, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: true, NodeIDs: nil})
	if err != nil {
		t.Fatal(err)
	}
	narrowed, _, err := s.SaveProbeTask(ctx, all.Task, NodeSelector{AllNodes: false, NodeIDs: nil})
	if err != nil || narrowed.AllNodes || narrowed.NodeIDs != nil {
		t.Fatalf("all_nodes switched off with no nodes: %+v err=%v", narrowed, err)
	}
	empty, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: nil})
	if err != nil {
		t.Fatal(err)
	}
	b, created, _ := s.CreateNode(ctx, NewNode{Name: "b"}, hash(2))
	assertTasks(t, s, created.Version, []ProbeTaskRecord{{Task: narrowed.Task}, {Task: empty.Task}})
	for _, task := range []uint64{narrowed.Task.Id, empty.Task.Id} {
		if ids, err := s.ProbeTaskNodeIDs(ctx, task); err != nil || ids != nil {
			t.Fatalf("task %d covers %v (%v), want no node among %d %d", task, ids, err, a, b)
		}
	}
}

// 建节点改变新节点的清单，推进版本（两个入口都推）；删节点不推。
func TestCreatingNodesBumpsProbeVersionDeletingDoesNot(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	stored := func() uint64 {
		t.Helper()
		v, _, err := s.LoadProbeTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := stored()
	a, createdNode, err := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	created := createdNode.Version
	if err != nil || created <= before || created != stored() {
		t.Fatalf("CreateNode version=%d stored=%d before=%d err=%v", created, stored(), before, err)
	}
	if err := s.SetRegisterWindow(ctx, hash(9), clk.Now().Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	_, registeredNode, err := s.RegisterNode(ctx, hash(9), "r", hash(2))
	registered := registeredNode.Version
	if err != nil || registered <= created || registered != stored() {
		t.Fatalf("RegisterNode version=%d stored=%d created=%d err=%v", registered, stored(), created, err)
	}
	if err := s.DeleteNode(ctx, a); err != nil {
		t.Fatal(err)
	}
	if v := stored(); v != registered {
		t.Fatalf("DeleteNode changed version %d -> %d", registered, v)
	}
}

// 两个建节点入口返回的 TaskIDs 就是提交后库里新节点的覆盖，升序，Version 就是库里的版本：all_nodes 任务在内，
// 显式分配给别的节点的任务与显式空集不在。注册表的建节点增量只取这份结果，它错了内存索引就跟着错。
func TestCreatingNodeReturnsItsCoverage(t *testing.T) {
	t.Parallel()
	for name, create := range map[string]func(t *testing.T, s *Store) (int64, NewNodeTasks, error){
		"create": func(t *testing.T, s *Store) (int64, NewNodeTasks, error) {
			return s.CreateNode(t.Context(), NewNode{Name: "new"}, hash(9))
		},
		"register": func(t *testing.T, s *Store) (int64, NewNodeTasks, error) {
			return s.RegisterNode(t.Context(), hash(8), "new", hash(9))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, clk := open(t)
			ctx := t.Context()
			if err := s.SetRegisterWindow(ctx, hash(8), clk.Now().Add(time.Hour), 1); err != nil {
				t.Fatal(err)
			}
			a, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
			first, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: true, NodeIDs: nil})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{a}}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: nil}); err != nil {
				t.Fatal(err)
			}
			second, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: true, NodeIDs: nil})
			if err != nil {
				t.Fatal(err)
			}
			id, created, err := create(t, s)
			if err != nil {
				t.Fatal(err)
			}
			version, recs, err := s.LoadProbeTasks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var covered []uint64
			for _, rec := range recs {
				if slices.Contains(rec.NodeIDs, id) {
					covered = append(covered, rec.Task.Id)
				}
			}
			want := NewNodeTasks{Version: version, TaskIDs: []uint64{first.Task.Id, second.Task.Id}}
			if !reflect.DeepEqual(covered, want.TaskIDs) {
				t.Fatalf("stored coverage of the new node = %v, want %v", covered, want.TaskIDs)
			}
			if !reflect.DeepEqual(created, want) {
				t.Fatalf("%s returned %+v, want %+v", name, created, want)
			}
		})
	}
}

func saveAllNodesTasks(t *testing.T, s *Store, n int) []ProbeTaskRecord {
	t.Helper()
	var out []ProbeTaskRecord
	for range n {
		rec, _, err := s.SaveProbeTask(t.Context(), taskForTest(), NodeSelector{AllNodes: true, NodeIDs: nil})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

// all_nodes 任务计入每个现有节点，与显式分配合计；第 65 个被拒并整体回滚。
func TestSavingAllNodesTaskCountsTowardEveryNodesLimit(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	a, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	b, _, _ := s.CreateNode(ctx, NewNode{Name: "b"}, hash(2))
	saveAllNodesTasks(t, s, probelimit.MaxTasksPerNode-1)
	if _, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{b}}); err != nil {
		t.Fatal(err)
	}
	version, before, err := s.LoadProbeTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: true, NodeIDs: nil})
	if !errors.Is(err, ErrNodeLimit) || err.Error() != fmt.Sprintf("node %d would have 65 probe tasks (maximum 64)", b) {
		t.Fatalf("65th task on node b error=%v", err)
	}
	assertTasks(t, s, version, before)
	// a 只有 63 个 all_nodes 任务，再来一个显式分配到 a 的任务是它的第 64 个，放行。
	if _, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{a}}); err != nil {
		t.Fatalf("64th task on node a rejected: %v", err)
	}
	if _, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: true, NodeIDs: nil}); !errors.Is(err, ErrNodeLimit) || !strings.Contains(err.Error(), fmt.Sprintf("node %d would have 65", a)) {
		t.Fatalf("all_nodes task past the limit of both nodes error=%v", err)
	}
}

// 没有节点时 all_nodes 任务可以多于 64 个（保存时没有节点可超限）；此时建节点与注册都失败并说明，事务整体回滚：
// 节点不建、版本不变、窗口名额不消耗。恰好 64 个时新节点正好到上限，放行。
func TestCreatingNodeRejectsInheritingMoreThanTheLimit(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	tasks := saveAllNodesTasks(t, s, probelimit.MaxTasksPerNode+1)
	if err := s.SetRegisterWindow(ctx, hash(9), clk.Now().Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	version, before, err := s.LoadProbeTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "a new node would inherit 65 all-nodes probe tasks (maximum 64 per node); assign some of them to explicit nodes or delete them first"
	for name, create := range map[string]func() error{
		"create":   func() error { _, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1)); return err },
		"register": func() error { _, _, err := s.RegisterNode(ctx, hash(9), "n", hash(1)); return err },
	} {
		if err := create(); !errors.Is(err, ErrNodeLimit) || err.Error() != want {
			t.Fatalf("%s error=%v, want %q", name, err, want)
		}
	}
	if nodes, err := s.ListNodes(ctx); err != nil || len(nodes) != 0 {
		t.Fatalf("rejected creation left nodes: %+v %v", nodes, err)
	}
	if w, ok, err := s.RegisterWindow(ctx); err != nil || !ok || w.Remaining != 1 {
		t.Fatalf("rejected registration consumed the window: %+v %v %v", w, ok, err)
	}
	assertTasks(t, s, version, before)

	if _, err := s.DeleteProbeTask(ctx, tasks[0].Task.Id); err != nil {
		t.Fatal(err)
	}
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatalf("node inheriting exactly 64 tasks rejected: %v", err)
	}
	_, recs, err := s.LoadProbeTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if !slices.Equal(rec.NodeIDs, []int64{id}) {
			t.Fatalf("task %d covers %v, want [%d]", rec.Task.Id, rec.NodeIDs, id)
		}
	}
}
