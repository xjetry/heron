package api

import (
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

func moveNodesHarness(t *testing.T, names ...string) (*harness, []int64) {
	t.Helper()
	h := newHarness(t, "")
	h.login(t)
	ids := make([]int64, len(names))
	for i, name := range names {
		id, _, err := h.auth.CreateNode(t.Context(), store.NewNode{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	return h, ids
}

// listedPositions 按 ListNodes 返回顺序给出 (name, position)，过滤条件可加 tags / untagged。
func listedPositions(t *testing.T, h *harness, req *heronv1.ListNodesRequest) []struct {
	name     string
	position uint32
} {
	t.Helper()
	list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]struct {
		name     string
		position uint32
	}, len(list.Msg.GetNodes()))
	for i, n := range list.Msg.GetNodes() {
		out[i].name, out[i].position = n.GetName(), n.GetPosition()
	}
	return out
}

func wantPositions(t *testing.T, got []struct {
	name     string
	position uint32
}, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("positions = %v, want %v", got, want)
	}
	for i, w := range want {
		name, pos, ok := strings.Cut(w, "@")
		n, _ := strconv.Atoi(pos)
		if !ok || got[i].name != name || got[i].position != uint32(n) {
			t.Fatalf("positions = %v, want %v", got, want)
		}
	}
}

// MoveNodes 走全序名次：被选节点按现有先后连续占位，其余节点相对顺序不变；ListNodes 回读名次，
// 标签过滤只筛行、不改名次。
func TestMoveNodesReportsPositions(t *testing.T) {
	t.Parallel()
	h, ids := moveNodesHarness(t, "a", "b", "c", "d", "e")
	// a、b 挂上标签：无标签列表只含 c、e、d，名次仍是全序名次。
	if _, err := h.svc.BatchUpdateNodeTags(t.Context(), connect.NewRequest(&heronv1.BatchUpdateNodeTagsRequest{NodeIds: ids[:2], AddTags: []string{"x"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.MoveNodes(t.Context(), connect.NewRequest(&heronv1.MoveNodesRequest{Ids: []int64{ids[2], ids[4]}, Position: 2})); err != nil {
		t.Fatal(err)
	}
	wantPositions(t, listedPositions(t, h, &heronv1.ListNodesRequest{}), "a@1", "c@2", "e@3", "b@4", "d@5")
	// 无标签过滤里的名次仍是全序名次。
	wantPositions(t, listedPositions(t, h, &heronv1.ListNodesRequest{Untagged: true}), "c@2", "e@3", "d@5")
}

func TestMoveNodesRejectsBadArguments(t *testing.T) {
	t.Parallel()
	h, ids := moveNodesHarness(t, "a", "b", "c")
	for _, c := range []struct {
		label    string
		ids      []int64
		position uint32
		code     connect.Code
		message  string
	}{
		{"empty ids", nil, 1, connect.CodeInvalidArgument, "ids: must list at least one node"},
		// N = 3、k = 2：合法区间 1..2，越界消息写明区间与推导，不截断。
		{"zero", []int64{ids[0], ids[1]}, 0, connect.CodeInvalidArgument, "position: must be between 1 and 2 (N = 3 nodes, k = 2 selected); got 0"},
		{"too large", []int64{ids[0], ids[1]}, 3, connect.CodeInvalidArgument, "position: must be between 1 and 2 (N = 3 nodes, k = 2 selected); got 3"},
		// 不存在的 id：含该 id，整批不改。
		{"unknown id", []int64{ids[0], 999}, 1, connect.CodeNotFound, "ids: node 999 does not exist; nothing was moved"},
	} {
		_, err := h.svc.MoveNodes(t.Context(), connect.NewRequest(&heronv1.MoveNodesRequest{Ids: c.ids, Position: c.position}))
		if codeOf(err) != c.code || err.Error() != c.code.String()+": "+c.message {
			t.Fatalf("%s: MoveNodes = %v, want %s: %s", c.label, err, c.code, c.message)
		}
	}
	wantPositions(t, listedPositions(t, h, &heronv1.ListNodesRequest{}), "a@1", "b@2", "c@3")
}

// 重复 id 去重、请求顺序不影响结果：c、d 按库内先后连续放到第 2 位。
func TestMoveNodesDedupesRequest(t *testing.T) {
	t.Parallel()
	h, ids := moveNodesHarness(t, "a", "b", "c", "d")
	if _, err := h.svc.MoveNodes(t.Context(), connect.NewRequest(&heronv1.MoveNodesRequest{Ids: []int64{ids[3], ids[2], ids[3]}, Position: 2})); err != nil {
		t.Fatal(err)
	}
	wantPositions(t, listedPositions(t, h, &heronv1.ListNodesRequest{}), "a@1", "c@2", "d@3", "b@4")
}

// MoveNodes 改的是全序：API token 只有读权限，写被拒。
func TestMoveNodesRejectsApiToken(t *testing.T) {
	t.Parallel()
	h, ids := moveNodesHarness(t, "a", "b")
	_, token := createToken(t, h, "ro")
	res := rawCall(t, h, "MoveNodes", `{"ids":[`+strconv.FormatInt(ids[0], 10)+`],"position":1}`, bearer(token))
	if res.code != "permission_denied" {
		t.Fatalf("MoveNodes by API token = %d %s %s, want permission_denied", res.status, res.code, res.message)
	}
}
