package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
)

// hubAndOffline 在同一个库上建运行中 hub 的 Auth 与离线子命令的 Auth（库外写者，与 cmd/hub 的 openOffline 同形）。
func hubAndOffline(t *testing.T) (hub, offline *Auth) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	open := func(opts ...store.OpenOption) *Auth {
		st, err := store.Open(path, clk, slog.Default(), store.MigrateSchema, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		a := New(st, probe.New(st, slog.Default()), nil, clk, time.UTC, slog.Default())
		if err := a.Load(t.Context()); err != nil {
			t.Fatal(err)
		}
		return a
	}
	hub = open()
	return hub, open(store.ExternalWriter())
}

// Reload 返回的恰是库外删除的节点：库外建节点、换发不算；重建之后旧映射里的被删节点不再鉴权，库外新建的认得。
func TestReloadReturnsNodesDeletedOutsideTheProcess(t *testing.T) {
	hub, offline := hubAndOffline(t)
	ctx := t.Context()
	from := netip.MustParseAddr("127.0.0.1")
	deleted, deletedToken, err := runningNode(t, hub, "deleted")
	if err != nil {
		t.Fatal(err)
	}
	rotated, _, err := runningNode(t, hub, "rotated")
	if err != nil {
		t.Fatal(err)
	}
	// 每次离线命令都在打开库时加载映射（openOffline），这里同样先加载，离线一侧看到的是库的当前凭据。
	if err := offline.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := offline.DeleteNode(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	install, err := offline.RotateToken(ctx, rotated)
	if err != nil {
		t.Fatal(err)
	}
	created, createdInstall, err := offline.CreateNode(ctx, store.NewNode{Name: "created"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.Authenticate(deletedToken); !ok {
		t.Fatal("fixture: the hub's map must still hold the offline-deleted node before reload")
	}
	removed, err := hub.Reload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []int64{deleted}) {
		t.Fatalf("removed = %v, want only the offline-deleted node %d", removed, deleted)
	}
	if _, ok := hub.Authenticate(deletedToken); ok {
		t.Fatal("offline-deleted node still authenticates after reload")
	}
	for _, c := range []struct {
		id      int64
		install string
	}{{rotated, install}, {created, createdInstall}} {
		if got, _, err := hub.Register(ctx, c.install, "", from); err != nil || got != c.id {
			t.Fatalf("register with offline-issued credential: id %d, %v; want %d", got, err, c.id)
		}
	}
	if removed, err := hub.Reload(ctx); err != nil || len(removed) != 0 {
		t.Fatalf("second reload removed %v, %v; want nothing", removed, err)
	}
}

// 读库失败时映射不变，删除留给下一次重建算出。
func TestReloadFailureLeavesMapAndDeletionsForNextTime(t *testing.T) {
	hub, offline := hubAndOffline(t)
	deleted, token, err := runningNode(t, hub, "deleted")
	if err != nil {
		t.Fatal(err)
	}
	if err := offline.DeleteNode(t.Context(), deleted); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := hub.Reload(canceled); err == nil {
		t.Fatal("reload with a canceled context succeeded")
	}
	if _, ok := hub.Authenticate(token); !ok {
		t.Fatal("failed reload changed the map")
	}
	if removed, err := hub.Reload(t.Context()); err != nil || !slices.Equal(removed, []int64{deleted}) {
		t.Fatalf("reload after failure removed %v, %v; want [%d]", removed, err, deleted)
	}
}

// 映射落后于库时（库外已换发或删除、尚未重载），拿映射里仍有效的安装凭据认领：比较并换发失败，回答与查无此凭据
// 相同的 ErrDenied，库里留着库外换发的凭据；重载之后库外换发的凭据可以认领。
func TestRegisterWithCredentialSupersededOutsideIsDenied(t *testing.T) {
	hub, offline := hubAndOffline(t)
	ctx := t.Context()
	from := netip.MustParseAddr("127.0.0.1")
	rotated, staleInstall, err := hub.CreateNode(ctx, store.NewNode{Name: "rotated"})
	if err != nil {
		t.Fatal(err)
	}
	deleted, deletedInstall, err := hub.CreateNode(ctx, store.NewNode{Name: "deleted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := offline.Load(ctx); err != nil {
		t.Fatal(err)
	}
	install, err := offline.RotateToken(ctx, rotated)
	if err != nil {
		t.Fatal(err)
	}
	if err := offline.DeleteNode(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]string{"rotated": staleInstall, "deleted": deletedInstall} {
		if _, _, err := hub.Register(ctx, key, "", from); !errors.Is(err, ErrDenied) {
			t.Fatalf("register with the %s node's superseded credential: %v, want ErrDenied", name, err)
		}
	}
	stored, err := hub.store.TokenHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored[HashToken(install)] != rotated {
		t.Fatal("the superseded credential overwrote the one issued outside the process")
	}
	if _, err := hub.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _, err := hub.Register(ctx, install, "", from); err != nil || got != rotated {
		t.Fatalf("register with the reissued credential after reload: %d, %v", got, err)
	}
}

// 映射落后于库时换发 token：期望值取自映射，库里已不是它，得到 ErrCredentialChanged，库与映射都不变；重载后成功。
func TestRotateTokenOverStaleMapReportsCredentialChanged(t *testing.T) {
	hub, offline := hubAndOffline(t)
	ctx := t.Context()
	id, running, err := runningNode(t, hub, "n")
	if err != nil {
		t.Fatal(err)
	}
	if err := offline.Load(ctx); err != nil {
		t.Fatal(err)
	}
	install, err := offline.RotateToken(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.RotateToken(ctx, id); !errors.Is(err, store.ErrCredentialChanged) {
		t.Fatalf("rotate over a stale map: %v, want ErrCredentialChanged", err)
	}
	if _, ok := hub.Authenticate(running); !ok {
		t.Fatal("a refused rotation changed the map")
	}
	stored, err := hub.store.TokenHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored[HashToken(install)] != id {
		t.Fatal("a refused rotation changed the database")
	}
	if _, err := hub.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.RotateToken(ctx, id); err != nil {
		t.Fatalf("rotate after reload: %v", err)
	}
}
