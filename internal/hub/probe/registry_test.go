package probe

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/probelimit"
	"google.golang.org/protobuf/proto"
)

func registryStore(t *testing.T) (*Registry, *store.Store, []int64) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), clock.NewFake(time.Unix(0, 0)), log, store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	r := New(st, log)
	if err := r.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, name := range []string{"one", "two"} {
		id, err := r.CreateNode(t.Context(), store.NewNode{Name: name}, []byte(name))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return r, st, ids
}

// setupVersion 是 registryStore 返回时的任务版本：时钟停在 Unix 0，每次建节点推进 1，两个节点即 2。
const setupVersion = 2

func task(target string) *heronv1.ProbeTask {
	return &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: target, IntervalS: 5, TimeoutMs: 1000}
}

func save(t *testing.T, r *Registry, target string, ids []int64) Detail {
	t.Helper()
	d, _, err := r.Save(t.Context(), task(target), store.NodeSelector{AllNodes: false, NodeIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func assertDetails(t *testing.T, got, want []Detail) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("details count=%d want=%d: %+v", len(got), len(want), got)
		return
	}
	for i := range want {
		if !proto.Equal(got[i].Task, want[i].Task) || got[i].AllNodes != want[i].AllNodes || !slices.Equal(got[i].NodeIDs, want[i].NodeIDs) {
			t.Errorf("detail[%d]=%v all=%v nodes=%v want=%v all=%v nodes=%v", i, got[i].Task, got[i].AllNodes, got[i].NodeIDs, want[i].Task, want[i].AllNodes, want[i].NodeIDs)
		}
	}
}

func assertState(t *testing.T, r *Registry, version uint64, want []Detail, nodes ...int64) {
	t.Helper()
	if v := r.Version(); v != version {
		t.Errorf("Version=%d want=%d", v, version)
	}
	v, got := r.List()
	if v != version {
		t.Errorf("List version=%d want=%d", v, version)
	}
	assertDetails(t, got, want)
	for _, node := range nodes {
		wt := &heronv1.ProbeTasks{Version: version}
		for _, d := range want {
			assigned := slices.Contains(d.NodeIDs, node)
			if r.Assigned(node, d.Task.Id) != assigned {
				t.Errorf("Assigned(%d,%d) want=%v", node, d.Task.Id, assigned)
			}
			if assigned {
				wt.Tasks = append(wt.Tasks, d.Task)
			}
		}
		if tasks := r.TasksFor(node); !proto.Equal(tasks, wt) {
			t.Errorf("TasksFor(%d)=%v want=%v", node, tasks, wt)
		}
	}
}

func TestRegistrySaveDeleteVersionAndAssignments(t *testing.T) {
	r, _, ids := registryStore(t)
	a, v, err := r.Save(t.Context(), task("a.example"), store.NodeSelector{AllNodes: false, NodeIDs: []int64{ids[1], ids[0], ids[0]}})
	if err != nil {
		t.Fatal(err)
	}
	if v != setupVersion+1 || !slices.Equal(a.NodeIDs, ids) || a.Task.Id == 0 {
		t.Errorf("saved=%+v version=%d", a, v)
	}
	assertState(t, r, setupVersion+1, []Detail{a}, ids...)
	b, v, err := r.Save(t.Context(), task("b.example"), store.NodeSelector{AllNodes: false, NodeIDs: nil})
	if err != nil {
		t.Fatal(err)
	}
	if v != setupVersion+2 {
		t.Errorf("save B version=%d want=%d", v, setupVersion+2)
	}
	assertState(t, r, setupVersion+2, []Detail{a, b}, ids...)
	v, err = r.Delete(t.Context(), a.Task.Id)
	if err != nil || v != setupVersion+3 {
		t.Errorf("delete version=%d err=%v", v, err)
	}
	if r.Assigned(ids[0], a.Task.Id) || r.Assigned(ids[1], a.Task.Id) {
		t.Error("deleted task remains assigned")
	}
	assertState(t, r, setupVersion+3, []Detail{b}, ids...)
}

func TestRegistryRejectsInvalidTaskWithoutTouchingStore(t *testing.T) {
	r, st, ids := registryStore(t)
	bad := task("example.com")
	bad.IntervalS = 1
	_, _, err := r.Save(t.Context(), bad, store.NodeSelector{AllNodes: false, NodeIDs: ids})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "interval_s must be between") {
		t.Errorf("invalid error=%v", err)
	}
	assertState(t, r, setupVersion, nil, ids...)
	v, recs, err := st.LoadProbeTasks(t.Context())
	if err != nil || v != setupVersion || len(recs) != 0 {
		t.Errorf("invalid task reached store: version=%d recs=%v err=%v", v, recs, err)
	}
}

func TestRegistryPassesThroughNotFoundAndLimit(t *testing.T) {
	r, _, ids := registryStore(t)
	_, _, err := r.Save(t.Context(), task("example.com"), store.NodeSelector{AllNodes: false, NodeIDs: []int64{42}})
	if !errors.Is(err, store.ErrNotFound) || !strings.Contains(err.Error(), "node 42 does not exist") {
		t.Errorf("missing node error=%v", err)
	}
	assertState(t, r, setupVersion, nil, ids...)
	var want []Detail
	for range probelimit.MaxTasksPerNode {
		want = append(want, save(t, r, "example.com", ids[:1]))
	}
	_, _, err = r.Save(t.Context(), task("example.com"), store.NodeSelector{AllNodes: false, NodeIDs: ids[:1]})
	if !errors.Is(err, store.ErrNodeLimit) || !strings.Contains(err.Error(), "node 1 would have 65 probe tasks (maximum 64)") {
		t.Errorf("limit error=%v", err)
	}
	assertState(t, r, setupVersion+64, want, ids...)
	if _, err := r.Delete(t.Context(), 999); !errors.Is(err, store.ErrNotFound) || !strings.Contains(err.Error(), "probe task 999 does not exist") {
		t.Errorf("missing task delete error=%v", err)
	}
	assertState(t, r, setupVersion+64, want, ids...)
}

func TestRegistryReloadMatchesMemory(t *testing.T) {
	r, st, ids := registryStore(t)
	a := save(t, r, "a.example", ids)
	b := save(t, r, "b.example", nil)
	c := save(t, r, "c.example", ids)
	d := save(t, r, "d.example", nil)
	edit := proto.Clone(a.Task).(*heronv1.ProbeTask)
	edit.Target = "changed.example"
	a, _, err := r.Save(t.Context(), edit, store.NodeSelector{AllNodes: false, NodeIDs: ids[1:]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Delete(t.Context(), b.Task.Id); err != nil {
		t.Fatal(err)
	}
	assertState(t, r, setupVersion+6, []Detail{a, c, d}, ids...)
	next := New(st, r.log)
	if err := next.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertState(t, next, setupVersion+6, []Detail{a, c, d}, ids...)
}

func TestRegistryLoadWaitsForPublication(t *testing.T) {
	r, st, ids := registryStore(t)
	// writeMu 覆盖从存储访问到发布的整个区间；重载不能在持锁写者结束前读取旧版本。
	r.writeMu.Lock()
	loaded := make(chan error, 1)
	go func() { loaded <- r.Load(t.Context()) }()
	var loadErr error
	early := false
	select {
	case loadErr = <-loaded:
		early = true
	// 负向窗口：持锁期间 Load 不应返回。窗口短只会漏掉稍晚才提前返回的缺陷，不会把仍在等锁的 Load 判失败。
	case <-time.After(100 * time.Millisecond):
	}
	saved, version, err := st.SaveProbeTask(t.Context(), task("new.example"), store.NodeSelector{AllNodes: false, NodeIDs: ids})
	r.writeMu.Unlock()
	if !early {
		loadErr = <-loaded
	}
	if err != nil || loadErr != nil {
		t.Fatalf("save error=%v load error=%v", err, loadErr)
	}
	if early {
		t.Error("Load returned before the writer released writeMu")
	}
	assertState(t, r, version, []Detail{{Task: saved.Task, NodeIDs: ids}}, ids...)
}

func TestRegistrySnapshotsDoNotAliasCache(t *testing.T) {
	for _, source := range []string{"save", "list", "tasks"} {
		t.Run(source, func(t *testing.T) {
			r, _, ids := registryStore(t)
			d := save(t, r, "a.example", ids)
			want := Detail{Task: proto.Clone(d.Task).(*heronv1.ProbeTask), NodeIDs: slices.Clone(ids)}
			switch source {
			case "save":
				d.Task.Target = "corrupted"
				d.NodeIDs[0] = 999
			case "list":
				_, list := r.List()
				list[0].Task.Target = "corrupted"
				list[0].NodeIDs[0] = 999
			case "tasks":
				r.TasksFor(ids[0]).Tasks[0].Target = "corrupted"
			}
			assertState(t, r, setupVersion+1, []Detail{want}, ids...)
		})
	}
}

func TestRegistryForgetPreservesOtherAssignmentsAndVersion(t *testing.T) {
	r, st, ids := registryStore(t)
	a := save(t, r, "a.example", ids)
	if err := st.DeleteNode(t.Context(), ids[0]); err != nil {
		t.Fatal(err)
	}
	r.Forget(ids[0])
	a.NodeIDs = ids[1:]
	assertState(t, r, setupVersion+1, []Detail{a}, ids...)
	next := New(st, r.log)
	if err := next.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertState(t, next, setupVersion+1, []Detail{a}, ids...)
}

// all_nodes 任务在内存索引里展开：保存时的全部节点、之后经两个建节点入口新建的节点都拿到它（TasksFor、Assigned），
// 版本随建节点前进；Forget 只摘掉被删的节点；重载得到同一份展开结果。
func TestRegistryExpandsAllNodesTasksOntoCreatedNodes(t *testing.T) {
	r, st, ids := registryStore(t)
	all, _, err := r.Save(t.Context(), task("all.example"), store.NodeSelector{AllNodes: true, NodeIDs: nil})
	if err != nil || !all.AllNodes || !slices.Equal(all.NodeIDs, ids) {
		t.Fatalf("save all_nodes: %+v err=%v", all, err)
	}
	one := save(t, r, "one.example", ids[:1])
	three, err := r.CreateNode(t.Context(), store.NewNode{Name: "three"}, []byte("three"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRegisterWindow(t.Context(), []byte("key"), time.Unix(3600, 0), 1); err != nil {
		t.Fatal(err)
	}
	four, err := r.RegisterNode(t.Context(), []byte("key"), "four", []byte("four"))
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := st.LoadProbeTasks(t.Context())
	if err != nil || stored != setupVersion+4 {
		t.Fatalf("stored version=%d err=%v, want %d", stored, err, setupVersion+4)
	}
	nodes := append(slices.Clone(ids), three, four)
	all.NodeIDs = nodes
	assertState(t, r, stored, []Detail{all, one}, nodes...)

	if err := st.DeleteNode(t.Context(), ids[0]); err != nil {
		t.Fatal(err)
	}
	r.Forget(ids[0])
	all.NodeIDs, one.NodeIDs = nodes[1:], nil
	assertState(t, r, stored, []Detail{all, one}, nodes...)
	next := New(st, r.log)
	if err := next.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertState(t, next, stored, []Detail{all, one}, nodes...)
}

// 显式空分配不覆盖任何节点，新建的节点也拿不到它。
func TestRegistryExplicitEmptyScopeReachesNoNode(t *testing.T) {
	r, _, ids := registryStore(t)
	empty := save(t, r, "none.example", nil)
	three, err := r.CreateNode(t.Context(), store.NewNode{Name: "three"}, []byte("three"))
	if err != nil {
		t.Fatal(err)
	}
	assertState(t, r, setupVersion+2, []Detail{{Task: empty.Task}}, append(slices.Clone(ids), three)...)
}

// 新节点的覆盖只取库在建节点事务里按 probeCoverage 读出的结果，不看内存的 allNodes 集合。这里绕过注册表直接在
// store 里改两个任务的作用域（任务体不变），让库的覆盖与内存的 allNodes 集合朝两个方向分叉：一个在库里改成全部
// 节点、内存仍是显式分配；另一个在库里改回显式分配、内存仍是全部节点。两个建节点入口建出的节点，清单都必须与库一致。
func TestRegistryCreatedNodeCoverageFollowsStore(t *testing.T) {
	for name, create := range map[string]func(t *testing.T, r *Registry, st *store.Store) (int64, error){
		"create": func(t *testing.T, r *Registry, _ *store.Store) (int64, error) {
			return r.CreateNode(t.Context(), store.NewNode{Name: "new"}, []byte("new"))
		},
		"register": func(t *testing.T, r *Registry, st *store.Store) (int64, error) {
			if err := st.SetRegisterWindow(t.Context(), []byte("key"), time.Unix(3600, 0), 1); err != nil {
				return 0, err
			}
			return r.RegisterNode(t.Context(), []byte("key"), "new", []byte("new"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, st, ids := registryStore(t)
			widened := save(t, r, "widened.example", ids[:1])
			narrowed, _, err := r.Save(t.Context(), task("narrowed.example"), store.NodeSelector{AllNodes: true, NodeIDs: nil})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.SaveProbeTask(t.Context(), widened.Task, store.NodeSelector{AllNodes: true, NodeIDs: nil}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.SaveProbeTask(t.Context(), narrowed.Task, store.NodeSelector{AllNodes: false, NodeIDs: ids[:1]}); err != nil {
				t.Fatal(err)
			}
			node, err := create(t, r, st)
			if err != nil {
				t.Fatal(err)
			}
			version, recs, err := st.LoadProbeTasks(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			stored := &heronv1.ProbeTasks{Version: version}
			for _, rec := range recs {
				if slices.Contains(rec.NodeIDs, node) {
					stored.Tasks = append(stored.Tasks, rec.Task)
				}
			}
			// 前提：分叉确实落在新节点上，库给它的是 widened 而不是 narrowed；否则下面的比较证明不了什么。
			if want := (&heronv1.ProbeTasks{Version: version, Tasks: []*heronv1.ProbeTask{widened.Task}}); !proto.Equal(stored, want) {
				t.Fatalf("store coverage of the new node = %v, want %v", stored, want)
			}
			if got := r.TasksFor(node); !proto.Equal(got, stored) {
				t.Errorf("TasksFor(new node)=%v, store has %v", got, stored)
			}
			if !r.Assigned(node, widened.Task.Id) || r.Assigned(node, narrowed.Task.Id) {
				t.Errorf("Assigned(new node): widened=%v narrowed=%v, want true false", r.Assigned(node, widened.Task.Id), r.Assigned(node, narrowed.Task.Id))
			}
		})
	}
}

// CertPolicy 与 CheckTask、agent 用同一个 https 判据：scheme 写成大写的目标同样是 https 任务。
func TestRegistryCertPolicyFollowsTheParsedScheme(t *testing.T) {
	r, _, ids := registryStore(t)
	d, _, err := r.Save(t.Context(), &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_HTTP, Target: "HTTPS://example.com/", IntervalS: 60, TimeoutMs: 1000}, store.NodeSelector{NodeIDs: ids[:1]})
	if err != nil {
		t.Fatal(err)
	}
	https, pinned, configID, ok := r.CertPolicy(d.Task.Id)
	if !ok || !https || pinned || len(configID) != 16 {
		t.Fatalf("CertPolicy = https=%v pinned=%v config_id=%x ok=%v, want an unpinned https task", https, pinned, configID, ok)
	}
}
