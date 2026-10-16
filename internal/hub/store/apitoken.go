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
	ExpiresAt  time.Time // 失效的第一个时刻；零值表示不过期
}

var ErrAPITokenLimit = errors.New("API token limit reached")

// MaxAPITokenExpiresAt 是 expires_at 的上限：9999-12-31T23:59:59Z。更晚的时刻在 RFC 3339 与面板的日期格式里写不成四位年份。
const MaxAPITokenExpiresAt int64 = 253402300799

// grantOrDefault 是建与改共用的缺省：没有给出 grant 即全站只读。
func grantOrDefault(g *TokenGrant) TokenGrant {
	if g == nil {
		return TokenGrant{AllNodes: true}
	}
	return *g
}

// unixOrZero 把时刻截到秒并转成 UTC，零值保持零值：与 scanAPIToken 从库里读回的值逐字段相等。
func unixOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return time.Unix(t.Unix(), 0).UTC()
}

// nullableUnix 把零值时刻存成 NULL：expires_at 的 NULL 表示不过期，0 会被列上的 CHECK 拒绝。
func nullableUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

// CreateAPIToken 在同一写事务里计数并插入：写协程串行执行事务，并发创建不会都看到
// limit−1 而一起越过上限。
func (s *Store) CreateAPIToken(ctx context.Context, name string, hash [32]byte, now time.Time, limit int, grant *TokenGrant, expiresAt time.Time) (APIToken, error) {
	var out APIToken
	g := grantOrDefault(grant)
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
		res, err := tx.Exec("INSERT INTO api_token (name, token_hash, created_at, permissions, all_nodes, expires_at) VALUES (?, ?, ?, ?, ?, ?)",
			name, hash[:], now.Unix(), string(permissions), g.AllNodes, nullableUnix(expiresAt))
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
		out = APIToken{TokenGrant: g, Identity: fmt.Sprintf("%X", hash), ID: id, Name: name, CreatedAt: time.Unix(now.Unix(), 0).UTC(), ExpiresAt: unixOrZero(expiresAt)}
		return nil
	})
	return out, err
}

// UpdateAPIToken 整体替换 token 的名称、授权与失效时刻（零值表示不过期）。明文哈希、身份、created_at 与 last_used_at
// 不变：持有者不用换配置，按身份归属的回执随之延续；鉴权每次都查库，下一个请求即按新授权。
//   - 授权与新建经同一个 validate；节点集合整体替换，包括它自己创建或经自己窗口接入而自动纳入的节点。
//   - 新授权不含注册权限时，同一事务删掉它的注册窗口，与 DeleteAPIToken 一致；认领时仍按 token 当前状态重查。
//   - expiresAt 不要求晚于当前时间：早于或等于即保存为已过期，晚于即有效（已过期的随之恢复）。
//
// 不存在返回 ErrNotFound。
func (s *Store) UpdateAPIToken(ctx context.Context, id int64, name string, grant *TokenGrant, expiresAt time.Time) (APIToken, error) {
	g := grantOrDefault(grant)
	var out APIToken
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := g.validate(tx); err != nil {
			return err
		}
		permissions, err := json.Marshal(g.Permissions)
		if err != nil {
			return err
		}
		res, err := tx.Exec("UPDATE api_token SET name = ?, permissions = ?, all_nodes = ?, expires_at = ? WHERE id = ?",
			name, string(permissions), g.AllNodes, nullableUnix(expiresAt), id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec("DELETE FROM api_token_node WHERE token_id = ?", id); err != nil {
			return err
		}
		for _, node := range g.NodeIDs {
			if err := grantNode(tx, id, node); err != nil {
				return err
			}
		}
		if !g.Allows(PermissionRegister) {
			if _, err := tx.Exec("DELETE FROM register_window WHERE owner_id = ?", id); err != nil {
				return err
			}
		}
		out, _, err = lookupAPIToken(tx.QueryRow, "id = ?", id, s.clk.Now())
		return err
	})
	return out, err
}

func scanAPIToken(scan func(...any) error) (APIToken, error) {
	var t APIToken
	var created int64
	var used, expires sql.NullInt64
	var permissions, nodes string
	if err := scan(&t.ID, &t.Name, &created, &used, &permissions, &t.AllNodes, &nodes, &t.Identity, &expires); err != nil {
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
	if expires.Valid {
		t.ExpiresAt = time.Unix(expires.Int64, 0).UTC()
	}
	return t, nil
}

const selectAPIToken = `SELECT id, name, created_at, last_used_at, permissions, all_nodes,
 (SELECT json_group_array(node_id) FROM (SELECT node_id FROM api_token_node WHERE token_id = api_token.id ORDER BY node_id)), hex(token_hash), expires_at FROM api_token`

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

// TokenState 是一枚 token 在某一时刻的状态，由 lookupAPIToken 给出。
type TokenState int

const (
	TokenMissing TokenState = iota // 没有这一行：从未存在或已吊销
	TokenExpired                   // 行在，但 now 已到 expires_at
	TokenLive
)

// Expired 报告 now 时 token 是否已过期：ExpiresAt 是失效的第一个时刻，零值表示不过期。
func (t APIToken) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// lookupAPIToken 是"token 此刻是否有效"的唯一判定。请求入口（APITokenByHash）、ExecuteChange 事务内的重读
// （txReader.APIToken）、注册窗口认领（RegisterNode）与更新下发（saveNodeUpdate）都只经它取 token，过期判定写在这里，
// 四处同时生效；后两处发生在之后、不经过该 token 的请求，入口判过一次替代不了它们。
// TokenExpired 时返回的仍是完整的行，入口据此在错误里写出过期时刻；其余读者把过期当作不存在。
// now 由调用方从 Store 的时钟取；零值会让每一枚 token 都显得未过期，按错误拒绝。
// TestScanAPITokenOnlyInLookupAndList 在源码上守着"只经它取"：scanAPIToken 只出现在这里与 ListAPITokens。
func lookupAPIToken(row func(query string, args ...any) *sql.Row, where string, arg any, now time.Time) (APIToken, TokenState, error) {
	if now.IsZero() {
		return APIToken{}, TokenMissing, errors.New("lookupAPIToken: zero time would treat every token as unexpired")
	}
	t, err := scanAPIToken(row(selectAPIToken+" WHERE "+where, arg).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, TokenMissing, nil
	}
	if err != nil {
		return APIToken{}, TokenMissing, err
	}
	if t.Expired(now) {
		return t, TokenExpired, nil
	}
	return t, TokenLive, nil
}

// APITokenByHash 每次鉴权都直接读库，不经缓存：删行即吊销，且对另一进程的删除同样立即生效；过期按 Store 的时钟判定。
func (s *Store) APITokenByHash(ctx context.Context, hash [32]byte) (APIToken, TokenState, error) {
	return lookupAPIToken(func(q string, args ...any) *sql.Row { return s.r.QueryRowContext(ctx, q, args...) }, "token_hash = ?", hash[:], s.clk.Now())
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
