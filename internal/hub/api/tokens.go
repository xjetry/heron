package api

import (
	"context"
	"errors"
	"fmt"

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
	return out
}

func (s *Service) ListApiTokens(ctx context.Context, _ *connect.Request[heronv1.ListApiTokensRequest]) (*connect.Response[heronv1.ListApiTokensResponse], error) {
	list, err := s.store.ListAPITokens(ctx)
	if err != nil {
		s.log.Error("listing API tokens failed", "err", err)
		return nil, internalError("listing API tokens failed")
	}
	out := &heronv1.ListApiTokensResponse{}
	for _, t := range list {
		out.Tokens = append(out.Tokens, apiTokenProto(t))
	}
	return connect.NewResponse(out), nil
}

func (s *Service) CreateApiToken(ctx context.Context, req *connect.Request[heronv1.CreateApiTokenRequest]) (*connect.Response[heronv1.CreateApiTokenResponse], error) {
	name, err := cleanName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	var grant *store.TokenGrant
	if g := req.Msg.Grant; g != nil {
		grant = &store.TokenGrant{AllNodes: g.AllNodes, NodeIDs: g.NodeIds}
		for _, p := range g.Permissions {
			v, ok := tokenPermissions[p]
			if !ok {
				return nil, invalid("grant.permissions: unknown permission")
			}
			grant.Permissions = append(grant.Permissions, v)
		}
		if g.AllNodes && len(g.NodeIds) != 0 {
			return nil, invalid("grant: all_nodes and node_ids are mutually exclusive")
		}
	}
	tok, plain, err := s.auth.CreateAPIToken(ctx, name, grant)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidGrant) {
		return nil, invalid("grant: invalid scope or node does not exist")
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
