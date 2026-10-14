package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type APIToken struct {
	TokenGrant
	Identity   string
	ID         int64
	Name       string
	CreatedAt  time.Time
	LastUsedAt time.Time // 零值表示从未使用
}

var ErrAPITokenLimit = errors.New("API token limit reached")

// CreateAPIToken 在同一写事务里计数并插入：写协程串行执行事务，并发创建不会都看到
// limit−1 而一起越过上限。
func (s *Store) CreateAPIToken(ctx context.Context, name string, hash [32]byte, now time.Time, limit int, grant *TokenGrant) (APIToken, error) {
	var out APIToken
	g := TokenGrant{AllNodes: true}
	if grant != nil {
		g = *grant
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := g.validate(tx); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM api_token").Scan(&n); err != nil {
			return err
		}
		if n >= limit {
			return ErrAPITokenLimit
		}
		permissions, err := json.Marshal(g.Permissions)
		if err != nil {
			return err
		}
		res, err := tx.Exec("INSERT INTO api_token (name, token_hash, created_at, permissions, all_nodes) VALUES (?, ?, ?, ?, ?)", name, hash[:], now.Unix(), string(permissions), g.AllNodes)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		for _, node := range g.NodeIDs {
			if err := grantNode(tx, id, node); err != nil {
				return err
			}
		}
		out = APIToken{TokenGrant: g, Identity: fmt.Sprintf("%X", hash), ID: id, Name: name, CreatedAt: time.Unix(now.Unix(), 0).UTC()}
		return nil
	})
	return out, err
}

func scanAPIToken(scan func(...any) error) (APIToken, error) {
	var t APIToken
	var created int64
	var used sql.NullInt64
	var permissions, nodes string
	if err := scan(&t.ID, &t.Name, &created, &used, &permissions, &t.AllNodes, &nodes, &t.Identity); err != nil {
		return APIToken{}, err
	}
	if err := json.Unmarshal([]byte(permissions), &t.Permissions); err != nil {
		return APIToken{}, err
	}
	if err := json.Unmarshal([]byte(nodes), &t.NodeIDs); err != nil {
		return APIToken{}, err
	}
	t.CreatedAt = time.Unix(created, 0).UTC()
	if used.Valid {
		t.LastUsedAt = time.Unix(used.Int64, 0).UTC()
	}
	return t, nil
}

const selectAPIToken = `SELECT id, name, created_at, last_used_at, permissions, all_nodes,
 (SELECT json_group_array(node_id) FROM (SELECT node_id FROM api_token_node WHERE token_id = api_token.id ORDER BY node_id)), hex(token_hash) FROM api_token`

func (s *Store) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.r.QueryContext(ctx, selectAPIToken+" ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// APITokenByHash 每次鉴权都直接读库，不经缓存：删行即吊销，且对另一进程的删除同样立即生效。
func (s *Store) APITokenByHash(ctx context.Context, hash [32]byte) (APIToken, bool, error) {
	row := s.r.QueryRowContext(ctx, selectAPIToken+" WHERE token_hash = ?", hash[:])
	t, err := scanAPIToken(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, false, nil
	}
	if err != nil {
		return APIToken{}, false, err
	}
	return t, true, nil
}

func (s *Store) DeleteAPIToken(ctx context.Context, id int64) (bool, error) {
	var found bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{"DELETE FROM api_token_node WHERE token_id = ?", "DELETE FROM register_window WHERE owner_id = ?"} {
			if _, err := tx.Exec(q, id); err != nil {
				return err
			}
		}
		res, err := tx.Exec("DELETE FROM api_token WHERE id = ?", id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		found = n == 1
		return err
	})
	return found, err
}

func (s *Store) DeleteAllAPITokens(ctx context.Context) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{"DELETE FROM api_token_node", "DELETE FROM register_window WHERE owner_id != 0"} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		res, err := tx.Exec("DELETE FROM api_token")
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// TouchAPITokenAsync 只 UPDATE 已存在的行：吊销之后才落库的刷新不会把 token 写回来。
func (s *Store) TouchAPITokenAsync(id int64, now time.Time, done func(error)) {
	s.writeAsync(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE api_token SET last_used_at = ? WHERE id = ?", now.Unix(), id)
		return err
	}, done)
}
