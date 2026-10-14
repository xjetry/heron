package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AdminSecurity 的版本由所有凭据修改和认证消费共同推进，防止验证期间的撤销被旧结果覆盖。
type AdminSecurity struct {
	PasswordHash string
	Generation   int64
	Data         string
	SessionHash  *[32]byte
	Event        *AlertEvent
}

func (s *Store) AdminSecurity(ctx context.Context) (AdminSecurity, error) {
	var out AdminSecurity
	err := s.r.QueryRowContext(ctx, `SELECT a.password_hash, s.generation, s.data FROM admin a JOIN admin_security s ON s.id=a.id WHERE a.id=1`).Scan(&out.PasswordHash, &out.Generation, &out.Data)
	if err == sql.ErrNoRows {
		return out, ErrAdminChanged
	}
	return out, err
}

// CommitAdminSecurity 将校验依据、一次性因子消费和会话签发放在同一个事务中裁决。
// source 只在 session != nil 时有意义：签发该会话的登录请求的来源地址（规范文本，空串 = 未知），
// 与会话行一起写入，事后不可由别的请求改。
func (s *Store) CommitAdminSecurity(ctx context.Context, before AdminSecurity, data string, revoke bool, session *[32]byte, now, expires time.Time, source string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if before.SessionHash != nil {
			var count int
			if err := tx.QueryRow(`SELECT count(*) FROM admin_session WHERE token_hash=? AND expires_at>? AND last_used_at>?`, before.SessionHash[:], now.Unix(), now.Add(-24*time.Hour).Unix()).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				return ErrAdminChanged
			}
		}
		res, err := tx.Exec(`UPDATE admin_security SET data=?, generation=generation+1 WHERE id=1 AND generation=? AND EXISTS(SELECT 1 FROM admin WHERE id=1 AND password_hash=?)`, data, before.Generation, before.PasswordHash)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrAdminChanged
		}
		if revoke {
			if _, err = tx.Exec(`DELETE FROM admin_session`); err != nil {
				return err
			}
			if before.Event == nil {
				return errors.New("authentication change requires audit event")
			}
			if err = recordSecurityChange(tx, before.Event); err != nil {
				return err
			}
		}
		if session != nil {
			_, err = tx.Exec(`INSERT INTO admin_session(token_hash,created_at,last_used_at,expires_at,source) VALUES(?,?,?,?,?)`, session[:], now.Unix(), now.Unix(), expires.Unix(), source)
		}
		return err
	})
}

func (s *Store) ResetAdminSecurity(ctx context.Context) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE admin_security SET data='{}', generation=generation+1 WHERE id=1`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM admin_session`); err != nil {
			return err
		}
		return recordSecurityChange(tx, &AlertEvent{Transition: TransitionAuthChanged, At: s.clk.Now(), Summary: "管理员认证方式已由本机重置"})
	})
}

func recordSecurityChange(tx *sql.Tx, event *AlertEvent) error {
	ids, err := storedChannelIDs(tx, LoginNotifyList)
	if err != nil {
		return err
	}
	return recordAlertEvent(tx, event, systemTargets(ids))
}
