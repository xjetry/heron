package api

import (
	"context"
	"encoding/hex"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/auth"
)

func (s *Service) ListSessions(ctx context.Context, _ *connect.Request[heronv1.ListSessionsRequest]) (*connect.Response[heronv1.ListSessionsResponse], error) {
	rows, err := s.auth.ListSessions(ctx)
	if err != nil {
		s.log.Error("listing sessions failed", "err", err)
		return nil, internalError("listing sessions failed")
	}
	current := auth.HashToken(ctx.Value(sessionKey{}).(string))
	// request_source 是本次请求经 --trusted-proxies 解析出的来源（与登录锁定、登录通知同一口径）；
	// Session.source 是签发时记下的来源，读取不改写它。两个事实分列。
	out := &heronv1.ListSessionsResponse{RequestSource: auth.SourceText(ctx.Value(peerKey{}).(peerInfo).from)}
	for _, sess := range rows {
		// 库与响应只持有 hash；鉴权仍须对 cookie 明文哈希，列表 id 不能成为登录凭据。
		out.Sessions = append(out.Sessions, &heronv1.Session{
			Id: hex.EncodeToString(sess.TokenHash[:]), CreatedAt: sess.CreatedAt.Unix(),
			LastUsedAt: sess.LastUsedAt.Unix(), Current: sess.TokenHash == current,
			Source: sess.Source,
		})
	}
	return connect.NewResponse(out), nil
}

func (s *Service) RevokeSession(ctx context.Context, req *connect.Request[heronv1.RevokeSessionRequest]) (*connect.Response[heronv1.RevokeSessionResponse], error) {
	raw, err := hex.DecodeString(req.Msg.Id)
	if err != nil || len(raw) != 32 {
		return nil, invalid("id must be 64 hexadecimal characters from ListSessions")
	}
	hash := [32]byte(raw)
	// DELETE 不要求命中行：超时重试或另一设备已经撤销时，目标均已达成。
	if err := s.store.DeleteSession(ctx, hash); err != nil {
		s.log.Error("revoking session failed", "err", err)
		return nil, internalError("revoking session failed")
	}
	resp := connect.NewResponse(&heronv1.RevokeSessionResponse{})
	if hash == auth.HashToken(ctx.Value(sessionKey{}).(string)) {
		clearSessionCookie(ctx, resp.Header())
	}
	return resp, nil
}
