package auth

import (
	"context"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

// sessionAlive 同时供鉴权与会话列表使用，避免列表展示的有效会话与 cookie 准入分叉。
func sessionAlive(sess store.Session, now time.Time) bool {
	return now.Before(sess.ExpiresAt) && now.Sub(sess.LastUsedAt) < SessionIdle
}

func (a *Auth) ListSessions(ctx context.Context) ([]store.Session, error) {
	rows, err := a.store.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	now := a.clk.Now()
	out := rows[:0]
	for _, sess := range rows {
		if sessionAlive(sess, now) {
			out = append(out, sess)
		}
	}
	return out, nil
}
