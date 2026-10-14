package store_test

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

func TestDelayedSessionTouchCannotResurrectLogout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(path, clk, log, store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	other, err := store.Open(path, clk, log, store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	a := auth.New(st, probe.New(st, log), nil, clk, time.UTC, log)
	if err := a.SetPassword(ctx, "a sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	token, err := a.Login(ctx, "a sufficiently long password", netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	release, pending, drain := st.HoldWriterForTest()
	defer release()
	clk.Advance(time.Minute)
	type authentication struct {
		ok  bool
		err error
	}
	authenticated := make(chan authentication, 1)
	go func() {
		_, ok, err := a.AuthenticateSession(ctx, []string{token})
		authenticated <- authentication{ok, err}
	}()
	var result authentication
	blocked := false
	select {
	case result = <-authenticated:
	case <-time.After(testwait.Bound):
		// 缺陷使刷新变为同步写时也要释放夹具，避免测试自身无限等待。
		blocked = true
		release()
		result = <-authenticated
	}
	if result.err != nil || !result.ok {
		t.Fatalf("live session rejected: %v %v", result.ok, result.err)
	}
	if blocked {
		t.Fatal("authentication waited for asynchronous refresh")
	}
	if n := pending(); n != 1 {
		t.Fatalf("refresh not queued: %d writes", n)
	}
	// 单实例队列是 FIFO；另一连接先撤销才会产生延迟刷新晚于登出的真实交错。
	if err := auth.New(other, probe.New(other, log), nil, clk, time.UTC, log).Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if n := countSessions(t, st, auth.HashToken(token)); n != 0 {
		t.Fatalf("logout did not remove session before refresh: %d rows", n)
	}
	release()
	if err := drain(); err != nil {
		t.Fatal(err)
	}
	if n := countSessions(t, st, auth.HashToken(token)); n != 0 {
		t.Fatalf("delayed refresh resurrected logged-out session: %d rows", n)
	}
}

// countSessions 数会话表里 hash 等于 h 的行。
func countSessions(t *testing.T, st *store.Store, h [32]byte) int {
	t.Helper()
	rows, err := st.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, sess := range rows {
		if sess.TokenHash == h {
			n++
		}
	}
	return n
}
