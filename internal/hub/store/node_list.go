package store

import (
	"context"
	"database/sql"
	"errors"
)

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// nodeListRevision 读节点资料版本（含义见 schema.go 的 ddlNodeListRevision）。缺行是库损坏：把读不出来当成某个
// 固定值，会让条件读取把变化答成"未变"。
func nodeListRevision(ctx context.Context, q rowQuerier) (uint64, error) {
	var v int64
	err := q.QueryRowContext(ctx, "SELECT revision FROM node_list_revision WHERE id = 1").Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errors.New("node_list_revision: row missing")
	}
	if err != nil {
		return 0, err
	}
	return uint64(v), nil
}

// NodeFilter 是 ListNodes 的两种条件，都为空即全部；结果的顺序都与 ListNodes 相同（nodeOrder 是节点列表唯一的排序）。
//
// Tags 非空时取同时挂着其中全部标签的节点（交集），名字按 TagFold 比较。取交集而不是并集：多选过滤是逐步收窄——再选
// 一个标签，结果只会变少，"db 且 客户A"才是按多个维度定位一批机器；并集会让多选越选越多。实现是按节点分组、数命中的
// 不同标签：命中数等于所选的不同标签数，节点才拥有全部所选标签。所选标签数按折叠去重后计，否则 [db, DB] 会要求命中
// 两个标签而任何节点都只能命中一个；不存在的名字匹配不到任何标签，命中数凑不满，结果为空。
//
// Untagged 取没有任何标签的节点：调用方要的是"没有标签"而不是某个标签集合的交集。两者同时给出没有合法含义（交集
// 必然为空），where 报错而不是按其中一个放宽；api 在此之前已按参数错误拒绝，这里的检查让存储层的口径不依赖调用方。
type NodeFilter struct {
	Tags     []string
	Untagged bool
}

var errTagsWithUntagged = errors.New("node filter: tags and untagged must not be combined")

func (f NodeFilter) where() (string, []any, error) {
	if f.Untagged && len(f.Tags) > 0 {
		return "", nil, errTagsWithUntagged
	}
	if f.Untagged {
		return untaggedWhere, nil, nil
	}
	// 空条件匹配一切：没选任何标签表示不过滤，返回全部节点。这与"空即拒绝"的方向相反，所以显式分支，不让它
	// 从下面的 SQL 里掉出来——IN () 在 SQLite 里合法且不匹配任何行，空选择会静默变成空列表。
	if len(f.Tags) == 0 {
		return "", nil, nil
	}
	seen := map[string]bool{}
	var folds []any
	for _, name := range f.Tags {
		if fold := TagFold(name); !seen[fold] {
			seen[fold] = true
			folds = append(folds, fold)
		}
	}
	return tagFilterWhere(len(folds)), append(folds, len(folds)), nil
}

// NodeList 是一次 ReadNodeList 的结果。Revision 与 Nodes 读自同一个只读事务（WAL 下事务的第一次读定下快照），
// 所以 Nodes 恰是 Revision 时刻的内容：调用方拿 Revision 去问"之后变没变"，不会因版本号比数据新而漏掉一次变化。
type NodeList struct {
	Revision uint64
	Nodes    []Node
	// Unchanged 为真时没有读节点行（Nodes 为 nil）：unchanged(Revision) 返回了真。
	Unchanged bool
}

// ReadNodeList 读节点资料版本与按 filter、调用方可见范围筛出的节点。unchanged 在读出版本之后、读节点行之前调用
// （nil 视为恒假），返回真时不读节点行：条件读取省下整份查询与序列化。
func (s *Store) ReadNodeList(ctx context.Context, filter NodeFilter, unchanged func(revision uint64) bool) (NodeList, error) {
	where, args, err := filter.where()
	if err != nil {
		return NodeList{}, err
	}
	where = visibleWhere(ctx, where)
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return NodeList{}, err
	}
	defer tx.Rollback()
	rev, err := nodeListRevision(ctx, tx)
	if err != nil {
		return NodeList{}, err
	}
	if unchanged != nil && unchanged(rev) {
		return NodeList{Revision: rev, Unchanged: true}, nil
	}
	nodes, err := queryNodesTx(ctx, tx, where, args...)
	if err != nil {
		return NodeList{}, err
	}
	return NodeList{Revision: rev, Nodes: nodes}, nil
}
