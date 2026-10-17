package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/store"
)

func apiTokenProto(t store.APIToken) *heronv1.ApiToken {
	out := &heronv1.ApiToken{Id: t.ID, Name: t.Name, CreatedAt: t.CreatedAt.Unix()}
	out.Grant = &heronv1.TokenGrant{AllNodes: t.AllNodes, NodeIds: t.NodeIDs}
	for _, p := range t.Permissions {
		out.Grant.Permissions = append(out.Grant.Permissions, enumFor(tokenPermissions, p))
	}
	if !t.LastUsedAt.IsZero() {
		out.LastUsedAt = proto.Int64(t.LastUsedAt.Unix())
	}
	if !t.ExpiresAt.IsZero() {
		out.ExpiresAt = proto.Int64(t.ExpiresAt.Unix())
	}
	out.Version = t.Version()
	return out
}

// tokenGrant 把请求里的 grant 换成 store 的形状。缺失返回 nil，由 store 按全站只读处理（grantOrDefault），建与改同一缺省。
// 这里只换权限枚举；形状与节点是否存在由 store 在写事务里校验（TokenGrant.validate），经 grantError 点名字段。
func tokenGrant(g *heronv1.TokenGrant) (*store.TokenGrant, error) {
	if g == nil {
		return nil, nil
	}
	grant := &store.TokenGrant{AllNodes: g.AllNodes, NodeIDs: g.NodeIds}
	for _, p := range g.Permissions {
		v, ok := tokenPermissions[p]
		if !ok {
			return nil, invalid("grant.permissions: unknown permission %d", p)
		}
		grant.Permissions = append(grant.Permissions, v)
	}
	return grant, nil
}

// grantError 把 store 的 grant 校验失败写成请求里的字段路径；不是 grant 的错误返回 nil。
func grantError(err error) error {
	var ge *store.GrantError
	if errors.As(err, &ge) {
		return invalid("grant.%s: %s", ge.Field, ge.Reason)
	}
	return nil
}

// tokenExpiry 校验 expires_at：缺失返回零值（不过期）；给出时必须在 [1, store.MaxAPITokenExpiresAt] 内。新建另要求晚于
// hub 当前时间（建一枚已经失效的 token 只会是填错）；编辑不要求，带回过去的时刻即保持过期。
func tokenExpiry(v *int64, now time.Time, requireFuture bool) (time.Time, error) {
	if v == nil {
		return time.Time{}, nil
	}
	if *v < 1 || *v > store.MaxAPITokenExpiresAt {
		return time.Time{}, invalid("expires_at: %d is outside 1..%d (Unix seconds, at most 9999-12-31T23:59:59Z)", *v, store.MaxAPITokenExpiresAt)
	}
	if requireFuture && *v <= now.Unix() {
		return time.Time{}, invalid("expires_at: %d is not after the hub's current time %d; omit it for a token that does not expire", *v, now.Unix())
	}
	return time.Unix(*v, 0).UTC(), nil
}

func (s *Service) ListApiTokens(ctx context.Context, _ *connect.Request[heronv1.ListApiTokensRequest]) (*connect.Response[heronv1.ListApiTokensResponse], error) {
	list, err := s.store.ListAPITokens(ctx)
	if err != nil {
		s.log.Error("listing API tokens failed", "err", err)
		return nil, internalError("listing API tokens failed")
	}
	out := &heronv1.ListApiTokensResponse{Now: s.clk.Now().Unix()}
	for _, t := range list {
		out.Tokens = append(out.Tokens, apiTokenProto(t))
	}
	return connect.NewResponse(out), nil
}

func (s *Service) CreateApiToken(ctx context.Context, req *connect.Request[heronv1.CreateApiTokenRequest]) (*connect.Response[heronv1.CreateApiTokenResponse], error) {
	name, err := store.CleanName(req.Msg.GetName())
	if err != nil {
		return nil, invalidName(err)
	}
	grant, err := tokenGrant(req.Msg.Grant)
	if err != nil {
		return nil, err
	}
	expires, err := tokenExpiry(req.Msg.ExpiresAt, s.clk.Now(), true)
	if err != nil {
		return nil, err
	}
	tok, plain, err := s.auth.CreateAPIToken(ctx, name, grant, expires)
	if mapped := invalidName(err); mapped != nil {
		return nil, mapped
	}
	if gerr := grantError(err); gerr != nil {
		return nil, gerr
	}
	if errors.Is(err, store.ErrAPITokenLimit) {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("at most %d API tokens may exist; delete an unused one first", auth.MaxAPITokens))
	}
	if err != nil {
		s.log.Error("creating API token failed", "err", err)
		return nil, internalError("creating API token failed")
	}
	return connect.NewResponse(&heronv1.CreateApiTokenResponse{ApiToken: apiTokenProto(tok), Token: plain}), nil
}

var tokenPermissions = map[heronv1.TokenPermission]store.Permission{
	heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE: store.PermissionConfigure,
	heronv1.TokenPermission_TOKEN_PERMISSION_CREATE:    store.PermissionCreate,
	heronv1.TokenPermission_TOKEN_PERMISSION_REGISTER:  store.PermissionRegister,
	heronv1.TokenPermission_TOKEN_PERMISSION_ROTATE:    store.PermissionRotate,
	heronv1.TokenPermission_TOKEN_PERMISSION_DELETE:    store.PermissionDelete,
	heronv1.TokenPermission_TOKEN_PERMISSION_UPDATE:    store.PermissionUpdate,
}

func (s *Service) DeleteApiToken(ctx context.Context, req *connect.Request[heronv1.DeleteApiTokenRequest]) (*connect.Response[heronv1.DeleteApiTokenResponse], error) {
	found, err := s.store.DeleteAPIToken(ctx, req.Msg.GetId())
	if err != nil {
		s.log.Error("deleting API token failed", "err", err)
		return nil, internalError("deleting API token failed")
	}
	if !found {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("API token %d does not exist", req.Msg.GetId()))
	}
	return connect.NewResponse(&heronv1.DeleteApiTokenResponse{}), nil
}

func (s *Service) UpdateApiToken(ctx context.Context, req *connect.Request[heronv1.UpdateApiTokenRequest]) (*connect.Response[heronv1.UpdateApiTokenResponse], error) {
	name, err := store.CleanName(req.Msg.GetName())
	if err != nil {
		return nil, invalidName(err)
	}
	grant, err := tokenGrant(req.Msg.Grant)
	if err != nil {
		return nil, err
	}
	expires, err := tokenExpiry(req.Msg.ExpiresAt, s.clk.Now(), false)
	if err != nil {
		return nil, err
	}
	// 空的期望版本不能当作"不比对"：那样整体替换会静默拿走读到之后自动纳入的节点。
	if req.Msg.GetExpectedVersion() == "" {
		return nil, invalid("expected_version: required; send the ApiToken.version read from ListApiTokens")
	}
	tok, err := s.store.UpdateAPIToken(ctx, req.Msg.GetId(), name, grant, expires, req.Msg.GetExpectedVersion())
	if mapped := invalidName(err); mapped != nil {
		return nil, mapped
	}
	if gerr := grantError(err); gerr != nil {
		return nil, gerr
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("API token %d does not exist", req.Msg.GetId()))
	case errors.Is(err, store.ErrAPITokenChanged):
		return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("API token %d changed since it was read (edited, or a node it created or registered joined its scope); read it again with ListApiTokens and resubmit", req.Msg.GetId()))
	case err != nil:
		s.log.Error("updating API token failed", "err", err)
		return nil, internalError("updating API token failed")
	}
	return connect.NewResponse(&heronv1.UpdateApiTokenResponse{ApiToken: apiTokenProto(tok)}), nil
}

// GetCaller 给 token 的是入口鉴权时读到的那一行：每个请求都重新读库，所以编辑后的下一个请求即看到新授权。
func (s *Service) GetCaller(ctx context.Context, _ *connect.Request[heronv1.GetCallerRequest]) (*connect.Response[heronv1.GetCallerResponse], error) {
	if p, ok := store.Principal(ctx); ok {
		return connect.NewResponse(&heronv1.GetCallerResponse{Caller: &heronv1.GetCallerResponse_ApiToken{ApiToken: apiTokenProto(p)}}), nil
	}
	return connect.NewResponse(&heronv1.GetCallerResponse{Caller: &heronv1.GetCallerResponse_Session{Session: &heronv1.SessionCaller{}}}), nil
}
