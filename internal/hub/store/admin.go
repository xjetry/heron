package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Session struct {
	TokenHash  [32]byte
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  time.Time
	// 签发该会话的登录请求的来源地址（auth.SourceText 规范文本）；空串 = 未知：
	// 迁移前已存在的会话，以及没有请求的签发路径。
	Source string
}

// SetAdminPassword 在同一事务里写入哈希并清空全部会话：改密码即登出所有人，
// 同事务保证不存在"密码已换、旧会话仍活"的窗口。
func (s *Store) SetAdminPassword(ctx context.Context, phc string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var existed int
		if err := tx.QueryRow(`SELECT count(*) FROM admin WHERE id=1`).Scan(&existed); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO admin (id, password_hash, updated_at) VALUES (1, ?, ?)
			ON CONFLICT (id) DO UPDATE SET password_hash = excluded.password_hash, updated_at = excluded.updated_at`,
			phc, s.clk.Now().Unix()); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE admin_security SET generation=generation+1 WHERE id=1"); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM admin_session"); err != nil {
			return err
		}
		if existed != 0 {
			return recordSecurityChange(tx, &AlertEvent{Transition: TransitionAuthChanged, At: s.clk.Now(), Summary: "管理员密码已由本机更新"})
		}
		return nil
	})
}

// AdminPasswordHash 的第二个返回值为 false 表示还没有管理员；调用方必须把它
// 当作"无人可登录"处理，而不是跳过校验。
func (s *Store) AdminPasswordHash(ctx context.Context) (string, bool, error) {
	var phc string
	err := s.r.QueryRowContext(ctx, "SELECT password_hash FROM admin WHERE id = 1").Scan(&phc)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return phc, true, nil
}

var ErrAdminChanged = errors.New("admin password changed before session creation")

// Sessions 读取持久化会话；有效性由 auth 与鉴权路径共用的判定裁决。
func (s *Store) Sessions(ctx context.Context) ([]Session, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT token_hash, created_at, last_used_at, expires_at, source FROM admin_session ORDER BY created_at DESC, token_hash ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func scanSession(scan func(...any) error) (Session, error) {
	var hash []byte
	var created, used, exp int64
	var source string
	if err := scan(&hash, &created, &used, &exp, &source); err != nil {
		return Session{}, err
	}
	if len(hash) != 32 {
		return Session{}, fmt.Errorf("stored session hash must have 32 bytes; got %d", len(hash))
	}
	return Session{TokenHash: [32]byte(hash), CreatedAt: time.Unix(created, 0).UTC(), LastUsedAt: time.Unix(used, 0).UTC(), ExpiresAt: time.Unix(exp, 0).UTC(), Source: source}, nil
}

// TouchSessionAsync 只刷新已有会话的最近使用时刻，避免撤销后被延迟写入重新创建。
// writeAsync 投递后即返回，刷新失败通过 done 报告，调用方不等待事务提交。
func (s *Store) TouchSessionAsync(hash [32]byte, now time.Time, done func(error)) {
	s.writeAsync(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE admin_session SET last_used_at = ? WHERE token_hash = ?", now.Unix(), hash[:])
		return err
	}, done)
}

func (s *Store) DeleteSession(ctx context.Context, hash [32]byte) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM admin_session WHERE token_hash = ?", hash[:])
		return err
	})
}

// DeleteExpiredSessions 只按绝对过期时刻清理；空闲过期由鉴权调用方判定，
// 因而本函数不会把仍未绝对过期的会话视作应删除的记录。
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec("DELETE FROM admin_session WHERE expires_at <= ?", now.Unix())
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}
