package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

const MaxTagsPerNode = 16

var ErrTagLimit = errors.New("tag limit per node exceeded")

// TagFold 是标签名的比较键，tag.name_fold 列的唯一来源：每个字符取它在 Unicode 简单大小写折叠等价类（unicode.SimpleFold
// 的轨道）里码点最小的成员。两个名字键相同，当且仅当 strings.EqualFold 认为它们相等——同是简单折叠，所以 db 与 DB、
// σ 与 ς 相同，ß 与 ss 不同（那是完全折叠）。面板搜索框用的 JavaScript iu 正则也是简单折叠，两边口径一致。
func TagFold(name string) string {
	var b strings.Builder
	for _, r := range name {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		b.WriteRune(least)
	}
	return b.String()
}

// Tag 是一个标签与挂着它的节点数。
type Tag struct {
	Name  string
	Nodes int
}

// 按条件查找 node_tag 行的语句（setNodeTags 里插入关联的那条不在此列：它只查 tag）。它们各走哪个索引写在
// ddlNodeTag 的注释里，TestNodeTagQueryPlans 对这些语句本身跑 EXPLAIN QUERY PLAN 核对。
const (
	// setNodeTags 整体替换标签集合前清掉节点的全部旧关联。
	clearNodeTags = "DELETE FROM node_tag WHERE node_id = ?"
	// detachTag 解除一个标签的全部关联（DeleteTag）。
	detachTag = "DELETE FROM node_tag WHERE tag_id = ?"
	// listTagsQuery 是 ListTags 的查询。
	listTagsQuery = `SELECT t.name, COUNT(nt.node_id) FROM tag t LEFT JOIN node_tag nt ON nt.tag_id = t.id
	GROUP BY t.id ORDER BY t.name_fold`
)

// nodeTagsQuery 读出 where（queryNodes 的节点条件）选中的节点各自的标签名，按 name_fold 排序。
func nodeTagsQuery(where string) string {
	return `SELECT nt.node_id, t.name FROM node_tag nt JOIN tag t ON t.id = nt.tag_id
	WHERE nt.node_id IN (SELECT n.id FROM node n` + where + `) ORDER BY t.name_fold`
}

// tagFilterWhere 是 ListNodesByTags 交给 queryNodes 的节点条件：n 个折叠后的名字，外加所选标签数，共 n+1 个参数。
func tagFilterWhere(n int) string {
	return ` WHERE n.id IN (SELECT nt.node_id FROM node_tag nt JOIN tag t ON t.id = nt.tag_id
	WHERE t.name_fold IN (` + strings.TrimSuffix(strings.Repeat("?, ", n), ", ") + `)
	GROUP BY nt.node_id HAVING COUNT(DISTINCT nt.tag_id) = ?)`
}

// untaggedWhere 是 ListUntaggedNodes 交给 queryVisibleNodes 的节点条件：节点在 node_tag 里没有任何关联行。
// 判据是关联行而不是 tag 行：节点被去掉最后一个标签后 tag 行仍在（setNodeTags 的注释），此时节点没有标签；DeleteTag
// 删标签时关联行也一并删除。NOT EXISTS 对每个节点只判一次关联存在性，不需要聚合去重。
const untaggedWhere = ` WHERE NOT EXISTS (SELECT 1 FROM node_tag nt WHERE nt.node_id = n.id)`

// setNodeTags 在 UpdateNode 的写事务里把节点的标签集合整体替换成 names：已存在的标签（按 name_fold）沿用先建的写法，
// 不存在的新建。标签行不随最后一个关联消失：删标签只经 DeleteTag，ListTags 因此能列出没挂在任何节点上的标签。
func setNodeTags(tx *sql.Tx, node int64, names []string) error {
	if _, err := tx.Exec(clearNodeTags, node); err != nil {
		return err
	}
	for _, name := range names {
		if err := addNodeTag(tx, node, name); err != nil {
			return err
		}
	}
	return nil
}

func addNodeTag(tx *sql.Tx, node int64, name string) error {
	fold := TagFold(name)
	if _, err := tx.Exec("INSERT INTO tag (name, name_fold) VALUES (?, ?) ON CONFLICT (name_fold) DO NOTHING", name, fold); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT INTO node_tag (node_id, tag_id) SELECT ?, id FROM tag WHERE name_fold = ? ON CONFLICT DO NOTHING", node, fold)
	return err
}

// BatchUpdateNodeTags 只改变请求点名的关联；关联增删与最终数量检查共用写事务，不用客户端的旧集合覆盖其它标签。
func (s *Store) BatchUpdateNodeTags(ctx context.Context, ids []int64, add, remove []string) (result NodeUpdateResult, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		// 空集合不代表全部节点；存储入口也显式拒绝没有目标的修改。
		if len(ids) == 0 {
			return errors.New("node_ids must not be empty")
		}
		for _, id := range ids {
			exists, err := nodeExistsTx(tx, id)
			if err != nil {
				return err
			}
			if !exists {
				return NotFoundError{Kind: ObjectNode, ID: id}
			}
			for _, name := range remove {
				if _, err := tx.Exec("DELETE FROM node_tag WHERE node_id = ? AND tag_id IN (SELECT id FROM tag WHERE name_fold = ?)", id, TagFold(name)); err != nil {
					return err
				}
			}
			for _, name := range add {
				if err := addNodeTag(tx, id, name); err != nil {
					return err
				}
			}
			var count int
			if err := tx.QueryRow("SELECT COUNT(*) FROM node_tag WHERE node_id = ?", id).Scan(&count); err != nil {
				return err
			}
			if count > MaxTagsPerNode {
				return fmt.Errorf("%w: node %d would have %d tags (maximum %d)", ErrTagLimit, id, count, MaxTagsPerNode)
			}
		}
		result, err = s.nodeScopesAfterUpdate(ctx, tx, ids)
		return err
	})
	if err != nil {
		return NodeUpdateResult{}, err
	}
	return result, nil
}

// nodeTags 在 queryNodes 的只读事务里按 nodeTagsQuery 读标签，按节点分组。
func nodeTags(ctx context.Context, tx *sql.Tx, where string, args []any) (map[int64][]string, error) {
	rows, err := tx.QueryContext(ctx, nodeTagsQuery(where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

// ListNodesByTags 返回同时挂着 names 里全部标签的节点（交集），名字按 TagFold 比较。
//
// 取交集而不是并集：多选过滤是逐步收窄——再选一个标签，结果只会变少，"db 且 客户A"才是按多个维度定位一批机器；
// 并集会让多选越选越多。实现是按节点分组、数命中的不同标签：命中数等于所选的不同标签数，节点才拥有全部所选标签。
// 所选标签数按折叠去重后计，否则 [db, DB] 会要求命中两个标签而任何节点都只能命中一个；不存在的名字匹配不到任何标签，
// 命中数凑不满，结果为空。
func (s *Store) ListNodesByTags(ctx context.Context, names []string) ([]Node, error) {
	// 空条件匹配一切：没选任何标签表示不过滤，返回全部节点。这与"空即拒绝"的方向相反，所以显式分支，不让它
	// 从下面的 SQL 里掉出来——IN () 在 SQLite 里合法且不匹配任何行，空选择会静默变成空列表。
	if len(names) == 0 {
		return s.queryVisibleNodes(ctx, "")
	}
	seen := map[string]bool{}
	var folds []any
	for _, name := range names {
		fold := TagFold(name)
		if !seen[fold] {
			seen[fold] = true
			folds = append(folds, fold)
		}
	}
	return s.queryVisibleNodes(ctx, tagFilterWhere(len(folds)), append(folds, len(folds))...)
}

// ListUntaggedNodes 返回没有任何标签的节点，顺序与 ListNodes 相同（nodeOrder 是节点列表唯一的排序）。
// 与 ListNodesByTags 各管一种条件：调用方要的是"没有标签"而不是某个标签集合的交集，两者不组合。
func (s *Store) ListUntaggedNodes(ctx context.Context) ([]Node, error) {
	return s.queryVisibleNodes(ctx, untaggedWhere)
}

// ListTags 列出全部标签与各自的节点数，按 name_fold 排序；没挂在任何节点上的标签节点数为 0。
func (s *Store) ListTags(ctx context.Context) ([]Tag, error) {
	query := listTagsQuery
	if p, ok := Principal(ctx); ok && !p.AllNodes {
		query = "SELECT t.name, COUNT(nt.node_id) FROM tag t JOIN node_tag nt ON nt.tag_id=t.id WHERE " + nodeScopeSQL(ctx, "nt.node_id") + " GROUP BY t.id ORDER BY t.name_fold"
	}
	rows, err := s.r.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.Name, &t.Nodes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) TagID(ctx context.Context, name string) (int64, error) {
	var id int64
	err := s.r.QueryRowContext(ctx, "SELECT id FROM tag WHERE name_fold=?", TagFold(name)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return id, err
}

// DeleteTag 删除按 TagFold 与 name 相同的标签并解除它的全部关联，节点本身不动。没有这个标签时返回 ErrNotFound。
//
// 它是删标签的类型化变更目标，目标要在写事务之前声明，所以标签身份先从读连接解析；写事务只删这个身份，
// 并核对它仍叫这个名字：两次读取之间标签被删掉或换了身份时按不存在回答，不会删掉另一个同名的新标签。
func (s *Store) DeleteTag(ctx context.Context, name string) error {
	fold := TagFold(name)
	id, err := s.TagID(ctx, name)
	if err != nil {
		return err
	}
	return s.writeChange(ctx, ChangeTarget{Action: ActionDeleteTag, ResourceID: id}, func(tx *sql.Tx) error {
		var same bool
		if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM tag WHERE id = ? AND name_fold = ?)", id, fold).Scan(&same); err != nil {
			return err
		}
		if !same {
			return ErrNotFound
		}
		rows, err := tx.Query(`SELECT 'probe task', p.id, p.target FROM probe_task_tag st JOIN probe_task p ON p.id = st.task_id WHERE st.tag_id = ?
UNION ALL SELECT 'alert rule', r.id, r.name FROM alert_rule_tag st JOIN alert_rule r ON r.id = st.rule_id WHERE st.tag_id = ?
UNION ALL SELECT 'silence', s.id, s.name FROM silence_tag st JOIN silence s ON s.id = st.silence_id WHERE st.tag_id = ?`, id, id, id)
		if err != nil {
			return err
		}
		var refs []string
		for rows.Next() {
			var kind, label string
			var owner int64
			if err := rows.Scan(&kind, &owner, &label); err != nil {
				rows.Close()
				return err
			}
			refs = append(refs, fmt.Sprintf("%s %d (%s)", kind, owner, label))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(refs) > 0 {
			return fmt.Errorf("%w: tag %q referenced by %s", ErrInUse, name, strings.Join(refs, ", "))
		}
		if _, err := tx.Exec(detachTag, id); err != nil {
			return err
		}
		_, err = tx.Exec("DELETE FROM tag WHERE id = ?", id)
		return err
	})
}
