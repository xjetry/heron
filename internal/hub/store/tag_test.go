package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// v14 的完整 DDL：v13 加上节点的国家三列。
var schemaV14 = append(slices.Clone(schemaV13),
	"ALTER TABLE node ADD COLUMN country TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE node ADD COLUMN country_ip TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE node ADD COLUMN country_pin TEXT NOT NULL DEFAULT ''")

// 旧库升级后与新建库结构相同，节点原样保留且没有标签；升级后的库能挂标签。
func TestMigrationFromV14AddsTagTables(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 14, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO node (id, name, token_hash, created_at, country_pin) VALUES (7, 'kept', x'00', 1, 'JP')"); err != nil {
			t.Fatal(err)
		}
	})
	if v := userVersion(t, migrated.r); v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	n, err := migrated.GetNode(t.Context(), 7)
	if err != nil || n.Name != "kept" || n.CountryPin != "JP" || n.Tags != nil {
		t.Fatalf("node after migration: %+v %v", n, err)
	}
	setTags(t, migrated, 7, "db")
	if n, err := migrated.GetNode(t.Context(), 7); err != nil || !slices.Equal(n.Tags, []string{"db"}) {
		t.Fatalf("tags on a migrated database: %+v %v", n, err)
	}
}

func setTags(t *testing.T, s *Store, id int64, tags ...string) {
	t.Helper()
	n, err := s.GetNode(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	edit := NodeEdit{Name: n.Name, Public: n.Public, Note: n.Note, TrafficResetDay: n.TrafficResetDay, OfflineGraceS: n.OfflineGraceS,
		Billing: n.Billing, CountryPin: n.CountryPin, Tags: tags}
	if _, err := s.UpdateNode(t.Context(), id, edit); err != nil {
		t.Fatal(err)
	}
}

func nodeNames(nodes []Node) []string {
	out := []string{}
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

// 三个节点分别挂 {a}、{a,b}、{b}：多选取交集，空选择返回全部，不存在的标签让结果为空。
func TestListNodesByTagsIsAnIntersection(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	for i, tags := range [][]string{{"a"}, {"a", "b"}, {"b"}} {
		id, _, err := s.CreateNode(ctx, NewNode{Name: strings.Join(tags, "+")}, hash(byte(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		setTags(t, s, id, tags...)
	}
	for _, c := range []struct {
		filter []string
		want   []string
	}{
		{[]string{"a", "b"}, []string{"a+b"}},
		{[]string{"b", "a"}, []string{"a+b"}},
		{[]string{"a"}, []string{"a", "a+b"}},
		{[]string{"b"}, []string{"a+b", "b"}},
		{nil, []string{"a", "a+b", "b"}},
		{[]string{}, []string{"a", "a+b", "b"}},
		{[]string{"nope"}, []string{}},
		{[]string{"a", "nope"}, []string{}},
		// 折叠后重复的名字只算一个所选标签。
		{[]string{"a", "A"}, []string{"a", "a+b"}},
	} {
		nodes, err := s.ListNodesByTags(ctx, c.filter)
		if err != nil {
			t.Fatal(err)
		}
		if got := nodeNames(nodes); !slices.Equal(got, c.want) {
			t.Errorf("filter %q: nodes %q, want %q", c.filter, got, c.want)
		}
	}
}

// ListUntaggedNodes 返回没有任何标签的节点：判据是 node_tag 关联行而不是 tag 行，标签行失去最后一个关联后仍在、节点
// 已算无标签；顺序与 ListNodes 相同；带主体范围时只含范围内的节点。
func TestListUntaggedNodes(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	var ids []int64
	for i, tags := range [][]string{{"solo"}, nil, {"a", "b"}, nil} {
		id, _, err := s.CreateNode(ctx, NewNode{Name: fmt.Sprint("n", i)}, hash(byte(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if tags != nil {
			setTags(t, s, id, tags...)
		}
	}
	nodes, err := s.ListUntaggedNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := nodeNames(nodes), []string{"n1", "n3"}; !slices.Equal(got, want) {
		t.Fatalf("ListUntaggedNodes = %q, want %q", got, want)
	}
	// 顺序与 ListNodes 一致：并集里标签为空的节点按 nodeOrder 取出后应与无标签结果逐项相同。
	all, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, n := range all {
		if len(n.Tags) == 0 {
			want = append(want, n.Name)
		}
	}
	if got := nodeNames(nodes); !slices.Equal(got, want) {
		t.Fatalf("ListUntaggedNodes = %q, want the untagged nodes of ListNodes in the same order %q", got, want)
	}

	// 去掉最后一个标签：n0 的 solo 只属于它，清空后关联行为空，但 tag 行仍在（ListTags 列出 solo 且计数为 0），
	// 节点回到无标签结果里。计数 0 才测到"标签行不随最后一个关联消失"，若 solo 也被别的节点挂着，计数就来自别人。
	setTags(t, s, ids[0])
	if tags, err := s.ListTags(ctx); err != nil || !slices.Contains(tags, Tag{Name: "solo", Nodes: 0}) {
		t.Errorf("ListTags after clearing the last tag = %v %v, want the solo row kept with no nodes", tags, err)
	}
	var rows int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM node_tag WHERE node_id = ?", ids[0]).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("node_tag rows of the cleared node = %d %v, want 0", rows, err)
	}
	if nodes, err = s.ListUntaggedNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := nodeNames(nodes), []string{"n0", "n1", "n3"}; !slices.Equal(got, want) {
		t.Errorf("after clearing the last tag: ListUntaggedNodes = %q, want %q", got, want)
	}

	// 带主体范围：范围 {n0,n1,n2} 含两个无标签节点与一个已挂标签的节点，范围外的无标签节点 n3 不出现。
	p, err := s.CreateAPIToken(t.Context(), "reader", sha256.Sum256([]byte("reader")), clk.Now(), 100, &TokenGrant{NodeIDs: ids[:3]})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := s.ListUntaggedNodes(WithPrincipal(t.Context(), p))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := nodeNames(scoped), []string{"n0", "n1"}; !slices.Equal(got, want) {
		t.Fatalf("scoped ListUntaggedNodes = %q, want the in-scope untagged nodes %q", got, want)
	}
}

// db 与 DB 是同一个标签，沿用先建的写法；节点的标签按折叠后的名字排序。
func TestTagsFoldCaseAndKeepTheFirstSpelling(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	first, _, _ := s.CreateNode(ctx, NewNode{Name: "first"}, hash(1))
	second, _, _ := s.CreateNode(ctx, NewNode{Name: "second"}, hash(2))
	setTags(t, s, first, "db", "Web", "api")
	setTags(t, s, second, "DB", "web")
	for _, c := range []struct {
		id   int64
		want []string
	}{{first, []string{"api", "db", "Web"}}, {second, []string{"db", "Web"}}} {
		if n, err := s.GetNode(ctx, c.id); err != nil || !slices.Equal(n.Tags, c.want) {
			t.Errorf("node %d tags = %q %v, want %q", c.id, n.Tags, err, c.want)
		}
	}
	tags, err := s.ListTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Tag{{"api", 1}, {"db", 2}, {"Web", 2}}; !slices.Equal(tags, want) {
		t.Fatalf("ListTags = %v, want %v", tags, want)
	}
	nodes, err := s.ListNodesByTags(ctx, []string{"dB", "WEB"})
	if err != nil || !slices.Equal(nodeNames(nodes), []string{"first", "second"}) {
		t.Fatalf("filter by other spellings: %q %v", nodeNames(nodes), err)
	}
}

// 整体替换：新集合替掉旧集合，空集合清空；标签行不随最后一个关联消失。
func TestUpdateNodeReplacesTheTagSet(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	setTags(t, s, id, "a", "b")
	setTags(t, s, id, "b", "c")
	if n, _ := s.GetNode(ctx, id); !slices.Equal(n.Tags, []string{"b", "c"}) {
		t.Fatalf("after replacement: %q", n.Tags)
	}
	setTags(t, s, id)
	if n, _ := s.GetNode(ctx, id); n.Tags != nil {
		t.Fatalf("after clearing: %q", n.Tags)
	}
	tags, err := s.ListTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Tag{{"a", 0}, {"b", 0}, {"c", 0}}; !slices.Equal(tags, want) {
		t.Fatalf("ListTags = %v, want %v", tags, want)
	}
}

// 删除标签只解除关联：节点仍在、别的标签不动；按任一写法都能删，删不存在的标签得 ErrNotFound。
func TestDeleteTagOnlyDetachesIt(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	setTags(t, s, id, "db", "web")
	if err := s.DeleteTag(ctx, "DB"); err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNode(ctx, id)
	if err != nil || !slices.Equal(n.Tags, []string{"web"}) {
		t.Fatalf("node after deleting its tag: %+v %v", n, err)
	}
	if tags, _ := s.ListTags(ctx); !slices.Equal(tags, []Tag{{"web", 1}}) {
		t.Fatalf("ListTags = %v", tags)
	}
	if err := s.DeleteTag(ctx, "db"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a missing tag: %v", err)
	}
	var rows int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM node_tag").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("node_tag rows = %d %v, want 1", rows, err)
	}
}

// 删节点后它的关联行一行不剩，别的节点的关联与标签本身都在。
func TestDeleteNodeRemovesItsTagRows(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	gone, _, _ := s.CreateNode(ctx, NewNode{Name: "gone"}, hash(1))
	kept, _, _ := s.CreateNode(ctx, NewNode{Name: "kept"}, hash(2))
	setTags(t, s, gone, "a", "b")
	setTags(t, s, kept, "a")
	if err := s.DeleteNode(ctx, gone); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM node_tag WHERE node_id = ?", gone).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("node_tag rows of the deleted node = %d %v", rows, err)
	}
	if tags, _ := s.ListTags(ctx); !slices.Equal(tags, []Tag{{"a", 1}, {"b", 0}}) {
		t.Fatalf("ListTags = %v", tags)
	}
}

// TagFold 的等价关系与 strings.EqualFold（简单折叠）相同。
func TestTagFoldMatchesEqualFold(t *testing.T) {
	t.Parallel()
	words := []string{"db", "DB", "Db", "σ", "ς", "Σ", "k", "K", "K", "ß", "ss", "SS", "ẞ", "客户A", "客户a", "İ", "i", "I", "ı"}
	for _, a := range words {
		for _, b := range words {
			if same, want := TagFold(a) == TagFold(b), strings.EqualFold(a, b); same != want {
				t.Errorf("%q vs %q: same key %v, EqualFold %v", a, b, same, want)
			}
		}
	}
}

// ddlNodeTag 注释里每条访问路径的依据：对 tag.go 里的语句本身跑 EXPLAIN QUERY PLAN，库里没有统计信息（store 从不跑
// ANALYZE，与生产一致）。计划的措辞属于所钉的 SQLite 版本；换驱动版本后这里红了，按新输出重新核对注释，不只改字符串。
func TestNodeTagQueryPlans(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	for i := range 30 {
		id, _, err := s.CreateNode(ctx, NewNode{Name: fmt.Sprint("n", i)}, hash(byte(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		setTags(t, s, id, fmt.Sprint("a", i%5), fmt.Sprint("b", i%3), "all")
	}
	filter := tagFilterWhere(2)
	filterArgs := []any{TagFold("a1"), TagFold("all"), 2}
	byTag := "SEARCH nt USING COVERING INDEX node_tag_by_tag (tag_id=?)"
	byNode := "SEARCH nt USING PRIMARY KEY (node_id=?)"
	for _, c := range []struct {
		label string
		query string
		args  []any
		want  []string
	}{
		{"ListNodesByTags", selectNodes + filter + nodeOrder, filterArgs, []string{byTag, "USE TEMP B-TREE FOR GROUP BY"}},
		// 无标签过滤：外层扫节点，相关性来自 NOT EXISTS 的关联存在性判断，走主键。节点行经全序名次的
		// 内层子查询（selectNodes）给出，NOT EXISTS 的子查询编号随之从 1 变为 2。
		{"ListUntaggedNodes", selectNodes + untaggedWhere + nodeOrder, nil, []string{"CORRELATED SCALAR SUBQUERY 2", byNode}},
		// 无标签节点的标签集：结果为空，但这条语句每次调用都会跑。只钉 NOT EXISTS 子查询出现与 node_tag 主键访问，
		// 不钉 node 表自动索引的选择（那是无关的偶然）。
		{"tags of the untagged nodes", nodeTagsQuery(untaggedWhere), nil, []string{"CORRELATED SCALAR SUBQUERY 1", byNode}},
		{"tags of the filtered nodes", nodeTagsQuery(filter), filterArgs, []string{byNode, byTag}},
		{"tags of all nodes", nodeTagsQuery(""), nil, []string{byNode}},
		{"ListTags", listTagsQuery, nil, []string{byTag + " LEFT-JOIN"}},
		{"DeleteTag", detachTag, []any{1}, []string{"SEARCH node_tag USING COVERING INDEX node_tag_by_tag (tag_id=?)"}},
		{"clearing a node", clearNodeTags, []any{1}, []string{"SEARCH node_tag USING PRIMARY KEY (node_id=?)"}},
	} {
		plan := queryPlan(t, s, c.query, c.args...)
		for _, w := range c.want {
			if !slices.Contains(plan, w) {
				t.Errorf("%s: plan %q lacks %q", c.label, plan, w)
			}
		}
	}
}

func queryPlan(t *testing.T, s *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := s.r.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// 节点行与它的标签来自同一个快照：写者每次同时改名字与标签（n<i> 配 t<i>），读者读到的名字后缀必须与标签后缀相同。
// 标签若在另一个只读事务里读，两次读之间提交的写会让名字与标签错配，这条用例随之变红。
//
// 不并行：单处理器的机器上它把进程级的 GOMAXPROCS 提到 2，并行用例会在这期间被改变调度。
func TestNodeRowAndTagsComeFromOneSnapshot(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n0"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	setTags(t, s, id, "t0")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	defer func() { close(stop); wg.Wait() }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.UpdateNode(ctx, id, NodeEdit{Name: fmt.Sprint("n", i), TrafficResetDay: 1, Tags: []string{fmt.Sprint("t", i)}}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	// 核对只有在写者真的插进两次读之间才验证了东西。单个处理器上调度可能让写者在整段读期间一次都没提交，
	// 所以用例期间把 GOMAXPROCS 提到至少 2，并且读满 minReads 次之后还要见过两个名字才停；到时限仍只见一个名字判为空过。
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(max(2, runtime.GOMAXPROCS(0))))
	const minReads = 500
	deadline := time.Now().Add(10 * time.Second)
	names := map[string]bool{}
	reads, mismatches := 0, 0
	for reads < minReads || (len(names) < 2 && time.Now().Before(deadline)) {
		n, err := s.GetNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		reads++
		names[n.Name] = true
		if want := []string{"t" + strings.TrimPrefix(n.Name, "n")}; !slices.Equal(n.Tags, want) {
			mismatches++
			if mismatches <= 3 {
				t.Errorf("node row %q read with tags %q", n.Name, n.Tags)
			}
		}
	}
	if mismatches > 0 {
		t.Errorf("%d of %d reads paired the node row with tags from another moment", mismatches, reads)
	}
	if len(names) < 2 {
		t.Fatalf("the reads saw %d distinct names in %d reads; the writer never interleaved with them", len(names), reads)
	}
}
