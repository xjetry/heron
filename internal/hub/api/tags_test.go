package api

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func updateTags(t *testing.T, h *harness, id int64, name string, public bool, tags ...string) (*heronv1.Node, error) {
	t.Helper()
	req := &heronv1.UpdateNodeRequest{Id: id, Name: name, Public: public, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), Tags: tags}
	resp, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNode(), nil
}

func mustUpdateTags(t *testing.T, h *harness, id int64, name string, tags ...string) *heronv1.Node {
	t.Helper()
	n, err := updateTags(t, h, id, name, false, tags...)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func listByTags(t *testing.T, h *harness, tags ...string) ([]string, error) {
	t.Helper()
	resp, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{Tags: tags}))
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, n := range resp.Msg.GetNodes() {
		names = append(names, n.GetName())
	}
	return names, nil
}

func listTags(t *testing.T, h *harness) []string {
	t.Helper()
	resp, err := h.admin.ListTags(t.Context(), connect.NewRequest(&heronv1.ListTagsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, tag := range resp.Msg.GetTags() {
		out = append(out, fmt.Sprintf("%s:%d", tag.GetName(), tag.GetNodeCount()))
	}
	return out
}

// 三个节点分别挂 {a}、{a,b}、{b}：多选取交集，空选择返回全部，不存在的标签让结果为空。
func TestListNodesFiltersByTagIntersection(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, tags := range [][]string{{"a"}, {"a", "b"}, {"b"}} {
		name := strings.Join(tags, "+")
		id, _ := h.createNode(t, name)
		mustUpdateTags(t, h, id, name, tags...)
	}
	for _, c := range []struct {
		filter []string
		want   []string
	}{
		{[]string{"a", "b"}, []string{"a+b"}},
		{[]string{"a"}, []string{"a", "a+b"}},
		{nil, []string{"a", "a+b", "b"}},
		{[]string{"nope"}, []string{}},
		{[]string{"A", " b "}, []string{"a+b"}},
	} {
		got, err := listByTags(t, h, c.filter...)
		if err != nil {
			t.Fatalf("filter %q: %v", c.filter, err)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("filter %q: nodes %q, want %q", c.filter, got, c.want)
		}
	}
}

// 过滤条件按标签名的规则校验；不同的标签多于 16 个时注定为空，按参数错误拒绝（折叠后重复的只算一个）。
func TestListNodesTagFilterValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	var seventeen, sixteenTwice []string
	for i := range 17 {
		seventeen = append(seventeen, fmt.Sprintf("t%d", i))
	}
	for i := range 16 {
		sixteenTwice = append(sixteenTwice, fmt.Sprintf("t%d", i), fmt.Sprintf("T%d", i))
	}
	for _, c := range []struct {
		filter []string
		code   connect.Code
		detail string
	}{
		{seventeen, connect.CodeInvalidArgument, "tags: at most 16 distinct tags"},
		{[]string{"a\x07b"}, connect.CodeInvalidArgument, "tags[0]: must not contain control characters"},
		{[]string{"ok", "  "}, connect.CodeInvalidArgument, "tags[1]: must be 1–64 characters"},
	} {
		_, err := listByTags(t, h, c.filter...)
		if codeOf(err) != c.code || !strings.Contains(err.Error(), c.detail) {
			t.Errorf("filter %q: %v, want %v mentioning %q", c.filter, err, c.code, c.detail)
		}
	}
	if got, err := listByTags(t, h, sixteenTwice...); err != nil || len(got) != 0 {
		t.Errorf("16 tags each spelled twice: %q %v, want an empty result", got, err)
	}
}

// untagged 只返回无标签节点，与 ListNodes 的节点顺序一致；与 tags 同时给出返回 InvalidArgument，错误信息同时点出
// 两个字段；untagged 为假时 tags 仍按交集过滤、空 tags 仍返回全部。
func TestListNodesUntagged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, tags := range [][]string{{"a"}, {"a", "b"}, {"b"}} {
		name := strings.Join(tags, "+")
		id, _ := h.createNode(t, name)
		mustUpdateTags(t, h, id, name, tags...)
	}
	h.createNode(t, "free1")
	h.createNode(t, "free2")
	untagged := func(tags ...string) ([]string, error) {
		resp, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{Tags: tags, Untagged: true}))
		if err != nil {
			return nil, err
		}
		names := []string{}
		for _, n := range resp.Msg.GetNodes() {
			names = append(names, n.GetName())
		}
		return names, nil
	}

	if got, err := untagged(); err != nil || !slices.Equal(got, []string{"free1", "free2"}) {
		t.Fatalf("untagged: nodes %q %v, want the two tagless nodes", got, err)
	}
	_, err := untagged("a")
	if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "tags") || !strings.Contains(err.Error(), "untagged") {
		t.Fatalf("untagged with tags: %v, want InvalidArgument naming both fields", err)
	}
	if got, err := listByTags(t, h, "a"); err != nil || !slices.Equal(got, []string{"a", "a+b"}) {
		t.Fatalf("tags still filter by intersection: %q %v", got, err)
	}
	if got, err := listByTags(t, h); err != nil || !slices.Equal(got, []string{"a", "a+b", "b", "free1", "free2"}) {
		t.Fatalf("empty tags still list every node: %q %v", got, err)
	}
}

// db 与 DB 是同一个标签，沿用先建的写法；同一请求里折叠后重复的只留第一个；回显按折叠后的名字排序。
func TestTagsAreCaseInsensitiveAndKeepTheFirstSpelling(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	first, _ := h.createNode(t, "first")
	second, _ := h.createNode(t, "second")
	if n := mustUpdateTags(t, h, first, "first", "db", "Web", "  api  "); !slices.Equal(n.GetTags(), []string{"api", "db", "Web"}) {
		t.Fatalf("first node tags = %q", n.GetTags())
	}
	if n := mustUpdateTags(t, h, second, "second", "DB", "客户A", "客户a", "WEB"); !slices.Equal(n.GetTags(), []string{"db", "Web", "客户A"}) {
		t.Fatalf("second node tags = %q, want the first spellings", n.GetTags())
	}
	if got, want := listTags(t, h), []string{"api:1", "db:2", "Web:2", "客户A:1"}; !slices.Equal(got, want) {
		t.Fatalf("ListTags = %q, want %q", got, want)
	}
}

// 名字只去首尾空白，中间的空格是名字的一部分：原样存下与回显，过滤按折叠比较同样命中；去掉空格后的写法是另一个名字。
func TestTagNamesKeepInnerSpaces(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	h.createNode(t, "other")
	if n := mustUpdateTags(t, h, id, "n", "web server", "Customer A", "  客户 甲  "); !slices.Equal(n.GetTags(), []string{"Customer A", "web server", "客户 甲"}) {
		t.Fatalf("tags = %q", n.GetTags())
	}
	for _, c := range []struct {
		filter []string
		want   []string
	}{
		{[]string{"WEB SERVER"}, []string{"n"}},
		{[]string{" customer a ", "客户 甲"}, []string{"n"}},
		{[]string{"webserver"}, []string{}},
	} {
		got, err := listByTags(t, h, c.filter...)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("filter %q: nodes %q %v, want %q", c.filter, got, err, c.want)
		}
	}
}

// 每节点至多 16 个（按去重后计），名字 1–64 个字符、不含控制字符；被拒的更新什么都不写。
func TestUpdateNodeTagValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	mustUpdateTags(t, h, id, "n", "kept")
	var sixteen []string
	for i := range 16 {
		sixteen = append(sixteen, fmt.Sprintf("t%02d", i))
	}
	long := strings.Repeat("é", 64)
	for _, c := range []struct {
		tags   []string
		detail string
	}{
		{append(slices.Clone(sixteen), "t16"), "tags: at most 16 distinct tags (case-insensitive); got 17"},
		{[]string{long + "x"}, "tags[0]: must be 1–64 characters after trimming whitespace; got 65"},
		{[]string{"ok", ""}, "tags[1]: must be 1–64 characters after trimming whitespace; got 0"},
		{[]string{"a\x00b"}, "tags[0]: must not contain control characters"},
		{[]string{"a\tb"}, "tags[0]: must not contain control characters"},
		{[]string{"a\u202eb"}, "tags[0]: must not contain control characters"},
		{[]string{"a\u0085b"}, "tags[0]: must not contain control characters"},
	} {
		_, err := updateTags(t, h, id, "renamed", false, c.tags...)
		if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), c.detail) {
			t.Errorf("tags %q: %v, want InvalidArgument mentioning %q", c.tags, err, c.detail)
		}
	}
	if n := listedNode(t, h); n.GetName() != "n" || !slices.Equal(n.GetTags(), []string{"kept"}) {
		t.Fatalf("a rejected update wrote something: %v", n)
	}
	// 边界：恰好 16 个、16 个再加一个只差大小写的、中间带空格与大小写混写的、64 个字符（首尾空白不计）都接受。
	for _, tags := range [][]string{sixteen, append(slices.Clone(sixteen), "T00"), {"web server", "Customer A"}, {"  " + long + "\t"}} {
		if _, err := updateTags(t, h, id, "n", false, tags...); err != nil {
			t.Errorf("tags %q rejected: %v", tags, err)
		}
	}
	if n := listedNode(t, h); !slices.Equal(n.GetTags(), []string{long}) {
		t.Fatalf("tags after the last accepted update = %q", n.GetTags())
	}
}

// 删除标签只解除关联：节点仍在、Node.tags 不再含它；大小写不敏感；不存在的标签 NotFound。
func TestDeleteTagDetachesItFromNodes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	mustUpdateTags(t, h, id, "n", "db", "web")
	if _, err := h.admin.DeleteTag(t.Context(), connect.NewRequest(&heronv1.DeleteTagRequest{Name: " DB "})); err != nil {
		t.Fatal(err)
	}
	if n := listedNode(t, h); n.GetId() != id || !slices.Equal(n.GetTags(), []string{"web"}) {
		t.Fatalf("node after deleting a tag: %v", n)
	}
	if got := listTags(t, h); !slices.Equal(got, []string{"web:1"}) {
		t.Fatalf("ListTags = %q", got)
	}
	_, err := h.admin.DeleteTag(t.Context(), connect.NewRequest(&heronv1.DeleteTagRequest{Name: "db"}))
	if codeOf(err) != connect.CodeNotFound || !strings.Contains(err.Error(), `tag "db" does not exist`) {
		t.Fatalf("deleting a missing tag: %v", err)
	}
	_, err = h.admin.DeleteTag(t.Context(), connect.NewRequest(&heronv1.DeleteTagRequest{Name: ""}))
	if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "name: must be 1–64 characters") {
		t.Fatalf("deleting an empty name: %v", err)
	}
}

// 删节点后 node_tag 没有它的行，标签本身留着。
func TestDeleteNodeLeavesNoTagRows(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	gone, _ := h.createNode(t, "gone")
	kept, _ := h.createNode(t, "kept")
	mustUpdateTags(t, h, gone, "gone", "a", "b")
	mustUpdateTags(t, h, kept, "kept", "a")
	if _, err := h.admin.DeleteNode(t.Context(), connect.NewRequest(&heronv1.DeleteNodeRequest{Id: gone})); err != nil {
		t.Fatal(err)
	}
	if rows := rowCounts(t, h.dbPath); rows["node_tag"] != 1 || rows["tag"] != 2 {
		t.Fatalf("node_tag rows = %d, tag rows = %d; want 1 and 2", rows["node_tag"], rows["tag"])
	}
	if got := listTags(t, h); !slices.Equal(got, []string{"a:1", "b:0"}) {
		t.Fatalf("ListTags = %q", got)
	}
}

// 公开节点的标签随快照公开，私有节点的标签不出现：快照只含 public = 1 的节点（store.ListPublicNodes），
// 标签跟着节点行读出，节点不在快照里它的标签就无处可出。两个方向同库断言，只测一个方向分不出
// "标签被过滤掉"与"标签根本没被读出"。
func TestPublicSnapshotCarriesTagsOfPublicNodesOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	pub, _ := h.createNode(t, "pub")
	priv, _ := h.createNode(t, "priv")
	if _, err := updateTags(t, h, pub, "pub", true, "web", "客户A"); err != nil {
		t.Fatal(err)
	}
	if _, err := updateTags(t, h, priv, "priv", false, "internal-billing-db"); err != nil {
		t.Fatal(err)
	}
	resp, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	nodes := resp.Msg.GetNodes()
	if len(nodes) != 1 || nodes[0].GetId() != pub {
		t.Fatalf("snapshot nodes = %v, want only the public node", nodes)
	}
	// 与管理端 Node.tags 同一口径：先建的写法，按折叠排序。不用 mustUpdateTags 取值：它会把节点改回私有。
	adminNodes, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, n := range adminNodes.Msg.GetNodes() {
		if n.GetId() == pub {
			want = n.GetTags()
		}
	}
	if !slices.Equal(nodes[0].GetTags(), want) || len(want) != 2 {
		t.Fatalf("public tags = %v, admin tags = %v", nodes[0].GetTags(), want)
	}
	raw := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	if bytes.Contains(raw.body, []byte("internal-billing-db")) {
		t.Fatalf("private node's tag leaked into the public snapshot: %s", raw.body)
	}
}

// PublicSnapshot.tags 是公开节点标签的并集，顺序与 ListTags 相同（按 name_fold）：公开页的标签栏照它渲染，
// 页面不复刻 TagFold。名字选成码元序与折叠序不同的一组（'Z' < '_' < 'a'，而折叠后 "ALPHA" < "ZETA" < "_X"），
// 按码元排序的实现在这里红；私有节点独有的标签不在其中。
func TestPublicSnapshotTagsFollowTagFoldOrder(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	p1, _ := h.createNode(t, "p1")
	p2, _ := h.createNode(t, "p2")
	priv, _ := h.createNode(t, "priv")
	for _, u := range []struct {
		id     int64
		name   string
		public bool
		tags   []string
	}{{p1, "p1", true, []string{"Zeta", "_x"}}, {p2, "p2", true, []string{"alpha", "Zeta"}}, {priv, "priv", false, []string{"secret", "alpha"}}} {
		if _, err := updateTags(t, h, u.id, u.name, u.public, u.tags...); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resp.Msg.GetTags(), []string{"alpha", "Zeta", "_x"}; !slices.Equal(got, want) {
		t.Fatalf("snapshot tags = %q, want %q", got, want)
	}
	// 与管理端同一顺序：ListTags 去掉只挂在私有节点上的 secret 后，应与快照逐项相同。
	all, err := h.admin.ListTags(t.Context(), connect.NewRequest(&heronv1.ListTagsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var admin []string
	for _, tg := range all.Msg.GetTags() {
		if tg.GetName() != "secret" {
			admin = append(admin, tg.GetName())
		}
	}
	if !slices.Equal(resp.Msg.GetTags(), admin) {
		t.Fatalf("snapshot tags = %q, ListTags order = %q", resp.Msg.GetTags(), admin)
	}
}
