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
