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

// NodeFilter 是 ListNodes 的两种条件：Tags 非空取交集（ListNodesByTags 的口径），Untagged 取没有任何标签的节点，
// 都为空即全部。两者同时给出由 api 按参数错误拒绝，这里不组合：Untagged 优先。
type NodeFilter struct {
	Tags     []string
	Untagged bool
}

func (f NodeFilter) where() (string, []any) {
	if f.Untagged {
		return untaggedWhere, nil
	}
	// 空条件匹配一切：没选任何标签表示不过滤，返回全部节点。这与"空即拒绝"的方向相反，所以显式分支，不让它
	// 从下面的 SQL 里掉出来——IN () 在 SQLite 里合法且不匹配任何行，空选择会静默变成空列表。
	if len(f.Tags) == 0 {
		return "", nil
	}
	seen := map[string]bool{}
	var folds []any
	for _, name := range f.Tags {
		if fold := TagFold(name); !seen[fold] {
			seen[fold] = true
			folds = append(folds, fold)
		}
	}
	return tagFilterWhere(len(folds)), append(folds, len(folds))
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
	where, args := filter.where()
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
