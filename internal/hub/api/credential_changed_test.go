package api

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
)

// offlineRotate 像 heron-hub node rotate-token 一样经库外写者换发凭据，返回新的安装凭据。
func offlineRotate(t *testing.T, h *harness, id int64) string {
	t.Helper()
	st, err := store.Open(h.dbPath, clock.Real(), slog.Default(), store.RequireCurrentSchema, store.ExternalWriter())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := auth.New(st, probe.New(st, slog.Default()), nil, clock.Real(), time.UTC, slog.Default())
	if err := a.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	install, err := a.RotateToken(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return install
}

func TestChangeErrorMapsCredentialChangedToAborted(t *testing.T) {
	t.Parallel()
	if code := connect.CodeOf(changeError(store.ErrCredentialChanged)); code != connect.CodeAborted {
		t.Fatalf("changeError(ErrCredentialChanged) = %v, want Aborted", code)
	}
}

// 凭据已被另一个进程换过、hub 的映射尚未重载：直连 RotateNodeToken 与 ExecuteChange 的轮换都回答 Aborted（资源已变，
// 请重试），库里留着另一个进程换发的凭据；映射重载之后同一请求成功。
func TestRotateNodeTokenAbortsWhenAnotherProcessRotated(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	id, _ := h.createNode(t, "n")
	install := offlineRotate(t, h, id)
	stored := func() int64 {
		t.Helper()
		m, err := h.store.TokenHashes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return m[auth.HashToken(install)]
	}

	if _, err := h.admin.RotateNodeToken(ctx, connect.NewRequest(&heronv1.RotateNodeTokenRequest{Id: id})); codeOf(err) != connect.CodeAborted {
		t.Fatalf("direct rotate over a stale map: %v, want Aborted", err)
	}
	grant := &heronv1.TokenGrant{Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_ROTATE}, AllNodes: true}
	client, _, _ := grantedClient(t, h, grant)
	preview := &heronv1.ExecuteChangeRequest{Preview: true, Change: &heronv1.ExecuteChangeRequest_RotateNodeToken{RotateNodeToken: &heronv1.RotateNodeTokenRequest{Id: id}}}
	if _, err := client.ExecuteChange(ctx, connect.NewRequest(preview)); codeOf(err) != connect.CodeAborted {
		t.Fatalf("typed rotate over a stale map: %v, want Aborted", err)
	}
	if got := stored(); got != id {
		t.Fatalf("the other process's credential was overwritten (hash maps to node %d)", got)
	}

	if err := h.auth.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.RotateNodeToken(ctx, connect.NewRequest(&heronv1.RotateNodeTokenRequest{Id: id})); err != nil {
		t.Fatalf("rotate after reload: %v", err)
	}
	if got := stored(); got != 0 {
		t.Fatal("rotate after reload left the previous credential in the database")
	}
}

// 安装凭据被另一个进程换过：拿旧凭据注册与查无此凭据一样被拒（对外不区分），库里仍是新凭据；映射重载之后新凭据可用。
func TestRegisterWithCredentialRotatedByAnotherProcessIsDenied(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	created, err := h.admin.CreateNode(ctx, connect.NewRequest(&heronv1.CreateNodeRequest{Name: "n"}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.Node.GetId()
	install := offlineRotate(t, h, id)
	_, err = h.agent.Register(ctx, connect.NewRequest(&heronv1.RegisterRequest{Key: created.Msg.Token}))
	if codeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("register with the superseded credential: %v, want Unauthenticated", err)
	}
	if _, _, err := h.auth.Register(ctx, created.Msg.Token, "n", netip.MustParseAddr("127.0.0.1")); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("auth.Register with the superseded credential: %v, want ErrDenied", err)
	}
	m, err := h.store.TokenHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m[auth.HashToken(install)] != id {
		t.Fatal("the superseded credential overwrote the one the other process issued")
	}
	if err := h.auth.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.report(t, h.claimNode(t, install), &heronv1.Metrics{}); err != nil {
		t.Fatalf("report with the token claimed by the reissued credential: %v", err)
	}
}
