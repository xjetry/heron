package probe

import (
	"errors"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/probelimit"
	"slices"
	"strings"
	"testing"
)

func TestDynamicSelectorUpdatesAllReadersAndRejectsReferencedTags(t *testing.T) {
	r, st, ids := registryStore(t)
	edit := func(id int64, tags ...string) {
		t.Helper()
		_, err := r.UpdateNode(t.Context(), id, store.NodeEdit{Name: "n", TrafficResetDay: 1, Tags: tags})
		if err != nil {
			t.Fatal(err)
		}
	}
	edit(ids[0], "DB", "west")
	edit(ids[1], "DB")
	d, version, err := r.Save(t.Context(), task("dynamic.example"), store.NodeSelector{AllNodes: false, NodeIDs: nil, Tags: []string{"db", "west"}})
	if err != nil {
		t.Fatal(err)
	}
	check := func(want []int64) {
		t.Helper()
		actual, err := st.ProbeTaskNodeIDs(t.Context(), d.Task.Id)
		if err != nil || !slices.Equal(actual, want) {
			t.Fatalf("stored coverage=%v want=%v err=%v", actual, want, err)
		}
		_, list := r.List()
		if !slices.Equal(list[0].NodeIDs, want) {
			t.Fatalf("list=%+v want=%v", list, want)
		}
		for _, id := range ids {
			covered := slices.Contains(want, id)
			_, _, public := r.TargetFor(id, d.Task.Id)
			if r.Assigned(id, d.Task.Id) != covered || public != covered || (len(r.TasksFor(id).Tasks) == 1) != covered {
				t.Fatalf("node %d coverage readers disagree; want %v", id, covered)
			}
		}
	}
	check(ids[:1])
	if err := st.DeleteTag(t.Context(), "db"); !errors.Is(err, store.ErrInUse) || !strings.Contains(err.Error(), "dynamic.example") {
		t.Fatalf("delete referenced tag=%v", err)
	}
	edit(ids[1], "DB", "west")
	if r.Version() <= version {
		t.Fatal("tag change did not advance task version")
	}
	check(ids)
	edit(ids[0], "west")
	check(ids[1:])
	edit(ids[1], "DB")
	check(nil)
	if err := r.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	check(nil)
	newID, err := r.CreateNode(t.Context(), store.NewNode{Name: "new"}, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, newID)
	edit(newID, "DB", "west")
	check([]int64{newID})
}

func TestDynamicSelectorLimitRollsBackNodeAndVersion(t *testing.T) {
	r, st, ids := registryStore(t)
	if _, err := r.UpdateNode(t.Context(), ids[0], store.NodeEdit{Name: "tagged", TrafficResetDay: 1, Tags: []string{"group"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < probelimit.MaxTasksPerNode; i++ {
		save(t, r, "full.example", ids[1:])
	}
	d, _, err := r.Save(t.Context(), task("dynamic.example"), store.NodeSelector{AllNodes: false, NodeIDs: nil, Tags: []string{"group"}})
	if err != nil {
		t.Fatal(err)
	}
	version := r.Version()
	_, err = r.UpdateNode(t.Context(), ids[1], store.NodeEdit{Name: "must rollback", TrafficResetDay: 1, Tags: []string{"group"}})
	if !errors.Is(err, store.ErrNodeLimit) {
		t.Fatalf("limit error=%v", err)
	}
	node, err := st.GetNode(t.Context(), ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if node.Name != "two" || len(node.Tags) != 0 || r.Version() != version || r.Assigned(ids[1], d.Task.Id) {
		t.Fatalf("failed edit published: node=%+v version=%d want=%d", node, r.Version(), version)
	}
}

func TestSelectorModesRejectAmbiguityAndEmptyTag(t *testing.T) {
	r, _, ids := registryStore(t)
	for _, c := range []struct {
		all   bool
		nodes []int64
		tags  []string
	}{
		{true, ids, nil}, {true, nil, []string{"x"}}, {false, ids, []string{"x"}}, {false, nil, []string{""}},
	} {
		if _, _, err := r.Save(t.Context(), task("invalid.example"), store.NodeSelector{AllNodes: c.all, NodeIDs: c.nodes, Tags: c.tags}); err == nil {
			t.Fatalf("accepted ambiguous selector %+v", c)
		}
	}
}
