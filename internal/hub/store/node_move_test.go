package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// moveNodesOrder 是当前全序：ListNodes 按 (sort_order, id) 升序返回，序号列显示的就是这条顺序。
func moveNodesOrder(t *testing.T, s *Store) []int64 {
	t.Helper()
	nodes, err := s.ListNodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(nodes))
	positions := make([]uint32, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
		positions[i] = n.Position
		if n.Position != uint32(i+1) {
			t.Fatalf("position of node %d = %d, want %d", n.ID, n.Position, i+1)
		}
	}
	return ids
}

func wantOrder(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestMoveNodesMovesSelectionAsAContiguousBlock(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	ids := map[string]int64{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		id, _, err := s.CreateNode(ctx, NewNode{Name: name}, hash(byte(len(ids))))
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	for _, c := range []struct {
		label    string
		move     []string
		position uint32
		want     []string
	}{
		// 被选节点全在目标位置之后：整体提到第 1 位。
		{"to the top", []string{"c"}, 1, []string{"c", "a", "b", "d", "e"}},
		// 被选节点全在目标位置之前：整体沉到最后一位（position = N-k+1）。
		{"to the bottom", []string{"a"}, 5, []string{"b", "c", "d", "e", "a"}},
		// 被选节点跨目标位置（b 在第 2、d 在第 4，目标第 3 位）：其余节点相对顺序不变。
		{"straddling the target", []string{"b", "d"}, 3, []string{"a", "c", "b", "d", "e"}},
		// 多个节点提到第 1 位，按现有先后连续排列。
		{"pair to the top", []string{"d", "e"}, 1, []string{"d", "e", "a", "b", "c"}},
		// k = N：移动全部节点，结果仍是现有全序。
		{"everything, position 1", []string{"e", "d", "c", "b", "a"}, 1, []string{"a", "b", "c", "d", "e"}},
	} {
		// 每个用例都从基准顺序 a..e 出发，断言只依赖本用例。
		if err := s.ReorderNodes(ctx, namedIDs(ids, "a", "b", "c", "d", "e")); err != nil {
			t.Fatal(err)
		}
		if err := s.MoveNodes(ctx, namedIDs(ids, c.move...), c.position); err != nil {
			t.Fatalf("%s: %v", c.label, err)
		}
		wantOrder(t, moveNodesOrder(t, s), namedIDs(ids, c.want...)...)
	}
}

func namedIDs(ids map[string]int64, names ...string) []int64 {
	out := make([]int64, len(names))
	for i, name := range names {
		out[i] = ids[name]
	}
	return out
}

func TestMoveNodesDedupesAndIgnoresRequestOrder(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	ids := map[string]int64{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		id, _, err := s.CreateNode(ctx, NewNode{Name: name}, hash(byte(len(ids))))
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	// 请求里 id 的先后与重复都不改变结果：被选节点按库内现有先后（c 在 d 前）连续放到第 2 位。
	for _, move := range [][]int64{namedIDs(ids, "d", "c", "d"), namedIDs(ids, "c", "d", "c")} {
		if err := s.MoveNodes(ctx, move, 2); err != nil {
			t.Fatal(err)
		}
		wantOrder(t, moveNodesOrder(t, s), namedIDs(ids, "a", "c", "d", "b", "e")...)
	}
}

// 故障注入：写事务在半途失败（触发器拒绝 sort_order 更新，等价磁盘满一类写入失败）时整批回滚，
// 库内排序与名次保持失败前的状态。
func TestMoveNodesWriteFailureLeavesOrderIntact(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	var ids []int64
	for _, name := range []string{"a", "b", "c", "d"} {
		id, _, err := s.CreateNode(ctx, NewNode{Name: name}, hash(byte(len(ids)+1)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TRIGGER reject_move BEFORE UPDATE ON node WHEN NEW.sort_order != OLD.sort_order BEGIN SELECT RAISE(ABORT, 'reject move'); END")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveNodes(ctx, ids[:2], 3); err == nil || !strings.Contains(err.Error(), "reject move") {
		t.Fatalf("injected write failure not returned: %v", err)
	}
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, node := range nodes {
		if node.ID != ids[i] || node.Position != uint32(i+1) {
			t.Fatalf("failed move changed order or rank: index=%d node=%+v", i, node)
		}
	}
}

func TestMoveNodesRejectsEmptyIDs(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	if _, _, err := s.CreateNode(context.Background(), NewNode{Name: "a"}, hash(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveNodes(t.Context(), nil, 1); !errors.Is(err, ErrEmptyMove) {
		t.Fatalf("MoveNodes(nil) = %v, want ErrEmptyMove", err)
	}
}

func TestMoveNodesRejectsOutOfRangePosition(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	ids := map[string]int64{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		id, _, err := s.CreateNode(ctx, NewNode{Name: name}, hash(byte(len(ids))))
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	// N = 5、k = 2：合法区间 1..4，两端之外都拒绝且不截断。
	for _, position := range []uint32{0, 4 + 1, 999} {
		err := s.MoveNodes(ctx, []int64{ids["b"], ids["c"]}, position)
		var outOfRange MoveRangeError
		if !errors.As(err, &outOfRange) {
			t.Fatalf("MoveNodes(position %d) = %v, want MoveRangeError", position, err)
		}
		if outOfRange.Total != 5 || outOfRange.Moving != 2 || outOfRange.Position != position {
			t.Fatalf("MoveRangeError = %+v, want {5 2 %d}", outOfRange, position)
		}
		if !strings.Contains(err.Error(), "between 1 and 4") {
			t.Fatalf("error %q must spell out the valid range", err)
		}
		wantOrder(t, moveNodesOrder(t, s), namedIDs(ids, "a", "b", "c", "d", "e")...)
	}
	// 单节点 k=1：N=5 时 5 合法、6 越界。
	if err := s.MoveNodes(ctx, []int64{ids["a"]}, 5); err != nil {
		t.Fatal(err)
	}
	wantOrder(t, moveNodesOrder(t, s), namedIDs(ids, "b", "c", "d", "e", "a")...)
	var last MoveRangeError
	if err := s.MoveNodes(ctx, []int64{ids["a"]}, 6); !errors.As(err, &last) || last != (MoveRangeError{Total: 5, Moving: 1, Position: 6}) {
		t.Fatalf("MoveNodes(6) = %v, want MoveRangeError{5 1 6}", err)
	}
}

func TestMoveNodesRejectsUnknownIDAndLeavesEveryOrderUntouched(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	ids := map[string]int64{}
	for _, name := range []string{"a", "b", "c"} {
		id, _, err := s.CreateNode(ctx, NewNode{Name: name}, hash(byte(len(ids))))
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	before := rawOrders(t, s)
	err := s.MoveNodes(ctx, []int64{ids["c"], 999, ids["a"]}, 1)
	var missing NotFoundError
	if !errors.As(err, &missing) || missing.ID != 999 || missing.Kind != ObjectNode {
		t.Fatalf("MoveNodes = %v, want NotFoundError{node, 999}", err)
	}
	if after := rawOrders(t, s); !slices.Equal(before, after) {
		t.Fatalf("rejected move must leave every sort_order untouched: %v -> %v", before, after)
	}
}

// rawOrders 直读 (id, sort_order)，连拒绝的写入没碰任何一行也能看出来。
func rawOrders(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.r.Query("SELECT id, sort_order FROM node ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		var order int
		if err := rows.Scan(&id, &order); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d:%d", id, order))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// 名次与移动都只看 (sort_order, id) 全序：删除留下的空洞与并发补号造成的并列都不影响——并列按 id 分先后，
// 名次仍然连续。MoveNodes 重写后空洞与并列一并消掉。
func TestMoveNodesOverTiesAndGaps(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	ids := map[string]int64{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		id, _, err := s.CreateNode(ctx, NewNode{Name: name}, hash(byte(len(ids))))
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	// sort_order：a=5、b=5（并列，a 的 id 更小排前）、c=2、d=2、e=10（空洞）。
	for name, order := range map[string]int{"a": 5, "b": 5, "c": 2, "d": 2, "e": 10} {
		if _, err := s.w.ExecContext(ctx, "UPDATE node SET sort_order = ? WHERE id = ?", order, ids[name]); err != nil {
			t.Fatal(err)
		}
	}
	wantOrder(t, moveNodesOrder(t, s), namedIDs(ids, "c", "d", "a", "b", "e")...)
	if err := s.MoveNodes(ctx, []int64{ids["e"]}, 3); err != nil {
		t.Fatal(err)
	}
	wantOrder(t, moveNodesOrder(t, s), namedIDs(ids, "c", "d", "e", "a", "b")...)
	for i, name := range []string{"c", "d", "e", "a", "b"} {
		var order int
		if err := s.r.QueryRowContext(ctx, "SELECT sort_order FROM node WHERE id = ?", ids[name]).Scan(&order); err != nil {
			t.Fatal(err)
		}
		if order != i {
			t.Fatalf("sort_order of %s = %d, want %d", name, order, i)
		}
	}
}

// 名次属于全部节点：标签交集、无标签与公开过滤只筛行，不重排名次。
func TestNodePositionIsRankOverAllNodes(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	ids := map[string]int64{}
	for _, name := range []string{"a", "b", "c", "d"} {
		id, _, err := s.CreateNode(ctx, NewNode{Name: name}, hash(byte(len(ids))))
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	setTags(t, s, ids["c"], "x")
	setTags(t, s, ids["d"], "y")
	// b 设为私有并挂上 x：标签交集、无标签与公开三张过滤各筛掉不同的行，名次都是全序名次。
	b, err := s.GetNode(ctx, ids["b"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateNode(ctx, ids["b"], NodeEdit{Name: b.Name, Public: false, TrafficResetDay: b.TrafficResetDay, Billing: b.Billing, Tags: []string{"x"}}); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range all {
		if want := uint32(i + 1); n.Position != want {
			t.Fatalf("ListNodes: position of %s = %d, want %d", n.Name, n.Position, want)
		}
	}
	byTag, err := filtered(ctx, s, NodeFilter{Tags: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(byTag) != 2 || byTag[0].Position != 2 || byTag[1].Position != 3 {
		t.Fatalf("tags [x] positions = %d, %d, want 2, 3", byTag[0].Position, byTag[1].Position)
	}
	untagged, err := filtered(ctx, s, NodeFilter{Untagged: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(untagged) != 1 || untagged[0].Position != 1 {
		t.Fatalf("untagged position = %+v, want a at 1", untagged)
	}
	public, err := s.ListPublicNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(public) != 3 || public[0].Position != 1 || public[1].Position != 3 || public[2].Position != 4 {
		t.Fatalf("ListPublicNodes positions = %d, %d, %d, want 1, 3, 4", public[0].Position, public[1].Position, public[2].Position)
	}

	// 重排之后名次随全序变化，过滤结果里的名次同步。
	if err := s.ReorderNodes(ctx, namedIDs(ids, "d", "a", "b", "c")); err != nil {
		t.Fatal(err)
	}
	byTag, err = filtered(ctx, s, NodeFilter{Tags: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if byTag[0].Position != 3 || byTag[1].Position != 4 {
		t.Fatalf("tags [x] positions after reorder = %d, %d, want 3, 4", byTag[0].Position, byTag[1].Position)
	}
}
