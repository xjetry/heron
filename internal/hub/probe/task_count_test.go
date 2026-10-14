package probe

import (
	"testing"

	"github.com/xjetry/heron/internal/hub/store"
)

// TaskCount 数的是节点当前覆盖的全部任务，直接指定与按标签两种作用域都算，标签作用域随节点标签变化，删除任务即不再计入。
func TestTaskCountCountsEveryScope(t *testing.T) {
	r, _, ids := registryStore(t)
	counts := func(want ...int) {
		t.Helper()
		for i, id := range ids {
			if got := r.TaskCount(id); got != want[i] {
				t.Fatalf("TaskCount(node %d) = %d, want %d", id, got, want[i])
			}
		}
	}
	counts(0, 0)
	direct, _, err := r.Save(t.Context(), task("direct.example"), store.NodeSelector{AllNodes: false, NodeIDs: ids[:1]})
	if err != nil {
		t.Fatal(err)
	}
	counts(1, 0)
	if _, err := r.UpdateNode(t.Context(), ids[1], store.NodeEdit{Name: "two", TrafficResetDay: 1, Tags: []string{"db"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Save(t.Context(), task("tagged.example"), store.NodeSelector{AllNodes: false, Tags: []string{"db"}}); err != nil {
		t.Fatal(err)
	}
	counts(1, 1)
	if _, err := r.UpdateNode(t.Context(), ids[0], store.NodeEdit{Name: "one", TrafficResetDay: 1, Tags: []string{"db"}}); err != nil {
		t.Fatal(err)
	}
	counts(2, 1)
	if _, err := r.Delete(t.Context(), direct.Task.Id); err != nil {
		t.Fatal(err)
	}
	counts(1, 1)
}
