package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

func setup(t testing.TB) (*Auth, *store.Store, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := New(st, probe.New(st, slog.Default()), nil, clk, time.UTC, slog.Default())
	if err := a.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a, st, clk
}

func TestNewRequiresLocation(t *testing.T) {
	defer func() {
		if r := recover(); r != "auth.New: loc must be set" {
			t.Fatalf("panic = %v", r)
		}
	}()
	New(nil, nil, nil, nil, nil, nil)
}

func TestTokenIs32RandomBytesHex(t *testing.T) {
	p1, h1 := NewToken()
	p2, _ := NewToken()
	if len(p1) != 64 || p1 == p2 {
		t.Fatalf("token %q / %q", p1, p2)
	}
	if HashToken(p1) != h1 {
		t.Fatal("hash must be derived from the plain token")
	}
}

// isTokenShaped 与 NewToken 的输出一一对应：签发的 token 全部通过；长度差一、大写、十六进制之外的字节全部拒绝。
// 收紧会把合法会话挡在外面，放宽只多算一些不可能命中的哈希；两个方向都在这里钉住。
func TestIsTokenShapedMatchesNewToken(t *testing.T) {
	for range 1000 {
		if plain, _ := NewToken(); !isTokenShaped(plain) {
			t.Fatalf("NewToken output %q rejected", plain)
		}
	}
	for _, s := range []string{
		strings.Repeat("0", 64), strings.Repeat("9", 64), strings.Repeat("a", 64), strings.Repeat("f", 64),
	} {
		if !isTokenShaped(s) {
			t.Errorf("%q rejected", s)
		}
	}
	a63 := strings.Repeat("a", 63)
	for _, s := range []string{
		"", a63, a63 + "aa",
		a63 + "A", a63 + "F", a63 + "g",
		a63 + "/", a63 + ":", a63 + "`", // 紧挨 '0'、'9'、'a' 两侧的字节
		strings.Repeat("a", 62) + "\u00e9", // 64 字节，含非 ASCII
		`"` + strings.Repeat("a", 62) + `"`,
	} {
		if isTokenShaped(s) {
			t.Errorf("%q (len %d) accepted", s, len(s))
		}
	}
}

func runningNode(t testing.TB, a *Auth, name string) (int64, string, error) {
	t.Helper()
	id, installation, err := a.CreateNode(t.Context(), store.NewNode{Name: name})
	if err != nil {
		return id, "", err
	}
	return a.Register(t.Context(), installation, name, netip.MustParseAddr("127.0.0.1"))
}

func TestCreateNodeIssuesInstallationCredential(t *testing.T) {
	a, _, _ := setup(t)
	id, plain, err := a.CreateNode(context.Background(), store.NewNode{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Authenticate(plain); ok {
		t.Fatal("安装凭据不能用于上报")
	}
	got, runtime, err := a.Register(t.Context(), plain, "ignored", netip.MustParseAddr("127.0.0.1"))
	if err != nil || got != id {
		t.Fatalf("认领失败：id=%d err=%v", got, err)
	}
	if got, ok := a.Authenticate(runtime); !ok || got != id {
		t.Fatalf("运行 token 鉴权 = %d,%v", got, ok)
	}
	if _, ok := a.Authenticate("nope"); ok {
		t.Fatal("unknown token authenticated")
	}
}

func TestRotateRemovesOldTokenImmediately(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	id, old, _ := runningNode(t, a, "a")
	fresh, err := a.RotateToken(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Authenticate(old); ok {
		t.Fatal("old token still authenticates after rotation")
	}
	_, runtime, err := a.Register(ctx, fresh, "a", netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := a.Authenticate(runtime); !ok || got != id {
		t.Fatal("new token does not authenticate")
	}
}

func TestFailedStoreWriteLeavesMapUntouched(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	_, plain, _ := runningNode(t, a, "a")
	if _, err := a.RotateToken(ctx, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := a.Authenticate(plain); !ok {
		t.Fatal("a failed rotation must not disturb existing tokens")
	}
	a.mu.RLock()
	n := len(a.byHash)
	a.mu.RUnlock()
	if n != 1 {
		t.Fatalf("map has %d entries, want 1", n)
	}
}

func TestLoadRebuildsMapFromStore(t *testing.T) {
	a, st, clk := setup(t)
	ctx := context.Background()
	id, plain, _ := runningNode(t, a, "a")
	b := New(st, probe.New(st, slog.Default()), nil, clk, time.UTC, slog.Default())
	if _, ok := b.Authenticate(plain); ok {
		t.Fatal("fresh Auth must not know tokens before Load")
	}
	if err := b.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := b.Authenticate(plain); !ok || got != id {
		t.Fatal("Load did not rebuild the map")
	}
}

func TestOpenWindowReturnsPersistedDeadline(t *testing.T) {
	a, st, clk := setup(t)
	ctx := context.Background()
	want := clk.Now().Add(time.Hour)
	key, until, err := a.OpenWindow(ctx, time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	w, ok, err := st.RegisterWindow(ctx)
	if err != nil || !ok {
		t.Fatalf("window = %+v %v %v", w, ok, err)
	}
	if key == "" || !until.Equal(want) || !until.Equal(w.ExpiresAt) {
		t.Fatalf("returned deadline %v, stored %v, want %v", until, w.ExpiresAt, want)
	}
}

func TestRegisterDeniedWithoutWindowAndDoesNotCount(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	from := netip.MustParseAddr("203.0.113.5")
	for i := 0; i < 10; i++ {
		if _, _, err := a.Register(ctx, "anything", "n", from); !errors.Is(err, ErrDenied) {
			t.Fatalf("err = %v", err)
		}
	}
	key, _, _ := a.OpenWindow(ctx, time.Hour, 1)
	if _, _, err := a.Register(ctx, key, "n", from); err != nil {
		t.Fatalf("closed-window attempts must not have locked the IP: %v", err)
	}
}

func TestRegisterWrongKeyOnOpenWindowLocksIP(t *testing.T) {
	a, _, clk := setup(t)
	ctx := context.Background()
	from := netip.MustParseAddr("203.0.113.5")
	key, _, _ := a.OpenWindow(ctx, time.Hour, 5)
	for i := 0; i < failLimit; i++ {
		if _, _, err := a.Register(ctx, "wrong", "n", from); !errors.Is(err, ErrDenied) {
			t.Fatalf("err = %v", err)
		}
	}
	if _, _, err := a.Register(ctx, key, "n", from); !errors.Is(err, ErrDenied) {
		t.Fatal("locked IP must be denied even with the right key")
	}
	other := netip.MustParseAddr("203.0.113.6")
	if _, _, err := a.Register(ctx, key, "n", other); err != nil {
		t.Fatalf("lockout is per IP: %v", err)
	}
	clk.Advance(failWindow)
	if _, _, err := a.Register(ctx, key, "n", from); err != nil {
		t.Fatalf("lockout must expire: %v", err)
	}
}

// 日志里的来源写成 DescribeSource 的形式：IPv6 带 /64。传具体地址与传已归一化的键结果相同。
func TestRegisterMismatchLogsTheSourceKey(t *testing.T) {
	a, _, _ := setup(t)
	var logs bytes.Buffer
	a.log = slog.New(slog.NewJSONHandler(&logs, nil))
	if _, _, err := a.OpenWindow(t.Context(), time.Hour, 5); err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"2001:db8:1:2::abcd", "2001:db8:1:2::"} {
		logs.Reset()
		if _, _, err := a.Register(t.Context(), "wrong", "n", netip.MustParseAddr(from)); !errors.Is(err, ErrDenied) {
			t.Fatalf("err = %v", err)
		}
		var record struct{ Msg, Source string }
		if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
			t.Fatalf("%v: %s", err, logs.Bytes())
		}
		if record.Msg != "register key mismatch" || record.Source != "2001:db8:1:2::/64" {
			t.Fatalf("from %s logged %s", from, logs.Bytes())
		}
	}
}

func TestRegisterExpiresFailuresFromOtherAddresses(t *testing.T) {
	a, _, clk := setup(t)
	ctx := context.Background()
	if _, _, err := a.OpenWindow(ctx, time.Hour, 10); err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"203.0.113.1", "203.0.113.2"} {
		if _, _, err := a.Register(ctx, "wrong", "n", netip.MustParseAddr(ip)); !errors.Is(err, ErrDenied) {
			t.Fatal(err)
		}
	}
	clk.Advance(failWindow + time.Second)
	third := netip.MustParseAddr("203.0.113.3")
	if _, _, err := a.Register(ctx, "wrong", "n", third); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.register.m) != 1 || a.register.m[third] == nil {
		t.Fatalf("expired IP failures retained: %+v", a.register.m)
	}
}

func TestRegisterIssuesWorkingToken(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	key, _, _ := a.OpenWindow(ctx, time.Hour, 1)
	id, plain, err := a.Register(ctx, key, "n", netip.MustParseAddr("10.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := a.Authenticate(plain); !ok || got != id {
		t.Fatal("registered token does not authenticate")
	}
	if _, _, err := a.Register(ctx, key, "m", netip.MustParseAddr("10.0.0.2")); !errors.Is(err, ErrDenied) {
		t.Fatal("window with max 1 must be exhausted")
	}
}

func TestRegisterAdoptsPrecreatedNodeToken(t *testing.T) {
	a, st, _ := setup(t)
	ctx := context.Background()
	from := netip.MustParseAddr("203.0.113.9")
	id, plain, err := a.CreateNode(ctx, store.NewNode{Name: "precreated"})
	if err != nil {
		t.Fatal(err)
	}
	// 窗口开着时节点 token 也走认领，不会另建一个节点、也不消耗名额。
	if _, _, err := a.OpenWindow(ctx, time.Hour, 3); err != nil {
		t.Fatal(err)
	}
	got, fresh, err := a.Register(ctx, plain, "ignored-hostname", from)
	if err != nil {
		t.Fatalf("register with a node token: %v", err)
	}
	if got != id {
		t.Fatalf("register id = %d, want %d", got, id)
	}
	if _, ok := a.Authenticate(plain); ok {
		t.Fatal("the install token must stop authenticating after adoption")
	}
	if n, ok := a.Authenticate(fresh); !ok || n != id {
		t.Fatal("the returned token must authenticate the adopted node")
	}
	// 名称沿用面板里设的，agent 自报的 name 不生效。
	node, err := st.GetNode(ctx, id)
	if err != nil || node.Name != "precreated" {
		t.Fatalf("node after adoption: %+v %v", node, err)
	}
	if nodes, err := st.ListNodes(ctx); err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %v %v, want only the precreated one", nodes, err)
	}
	if w, ok, err := st.RegisterWindow(ctx); err != nil || !ok || w.Remaining != 3 {
		t.Fatalf("adoption must not consume a window slot: %+v %v %v", w, ok, err)
	}
}

func TestRuntimeCredentialsCannotEnroll(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			a, st, _ := setup(t)
			ctx := t.Context()
			from := netip.MustParseAddr("203.0.113.9")
			var id int64
			var token string
			if legacy {
				var hash [32]byte
				token, hash = NewToken()
				var err error
				id, _, err = st.CreateNode(ctx, store.NewNode{Name: "legacy"}, hash[:])
				if err != nil {
					t.Fatal(err)
				}
			} else {
				key, _, err := a.OpenWindow(ctx, time.Hour, 1)
				if err != nil {
					t.Fatal(err)
				}
				id, token, err = a.Register(ctx, key, "window", from)
				if err != nil {
					t.Fatal(err)
				}
				n, err := st.GetNode(ctx, id)
				if err != nil || n.Public {
					t.Fatalf("窗口注册必须默认私有：%+v %v", n, err)
				}
			}
			if err := a.Load(ctx); err != nil {
				t.Fatal(err)
			}
			for _, candidate := range []string{token, installTokenPrefix + token} {
				if _, _, err := a.Register(ctx, candidate, "ignored", from); !errors.Is(err, ErrDenied) {
					t.Errorf("运行凭据不能认领或自报安装用途：%v", err)
				}
			}
			if got, ok := a.Authenticate(token); !ok || got != id {
				t.Error("运行凭据须继续上报")
			}
		})
	}
}

func TestInstallationCredentialConsumedOnceConcurrently(t *testing.T) {
	a, _, _ := setup(t)
	id, key, err := a.CreateNode(t.Context(), store.NewNode{Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		id    int64
		token string
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			got, token, err := a.Register(t.Context(), key, "ignored", netip.MustParseAddr("203.0.113.9"))
			results <- result{got, token, err}
		}()
	}
	close(start)
	accepted := 0
	for range 2 {
		r := <-results
		if r.err == nil {
			accepted++
			if got, ok := a.Authenticate(r.token); !ok || got != id || r.id != id {
				t.Error("唯一成功的认领必须产生有效运行凭据")
			}
		} else if !errors.Is(r.err, ErrDenied) {
			t.Fatal(r.err)
		}
	}
	if accepted != 1 {
		t.Fatalf("并发认领成功数=%d，期望 1", accepted)
	}
}

func TestDeleteNodeRevokesToken(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	id, plain, err := runningNode(t, a, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteNode(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Authenticate(plain); ok {
		t.Fatal("deleted node's token still authenticates")
	}
}

type observationClock struct {
	*clock.Fake
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

// Done 的求值发生在 store.write 投递请求时，用它确认注册已走到写队列。
type queuedContext struct {
	context.Context
	once   sync.Once
	queued chan struct{}
}

func (c *queuedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.queued) })
	return c.Context.Done()
}

func TestAuthenticateDoesNotWaitForRegisterTransaction(t *testing.T) {
	clk := &observationClock{Fake: clock.NewFake(time.Now()), entered: make(chan struct{}), release: make(chan struct{})}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := New(st, probe.New(st, slog.Default()), nil, clk, time.UTC, slog.Default())
	ctx := context.Background()
	id, tok, err := runningNode(t, a, "existing")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.OpenWindow(ctx, time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	clk.block.Store(true)
	gateDone := make(chan error, 1)
	go func() {
		_, _, err := st.CreateNode(ctx, store.NewNode{Name: "gate"}, make([]byte, 32))
		gateDone <- err
	}()
	<-clk.entered
	var release sync.Once
	defer release.Do(func() { close(clk.release) })
	queued := &queuedContext{Context: ctx, queued: make(chan struct{})}
	registered := make(chan error, 1)
	go func() {
		_, _, err := a.Register(queued, "wrong", "n", netip.MustParseAddr("203.0.113.7"))
		registered <- err
	}()
	select {
	case <-queued.queued:
	case <-time.After(testwait.Bound):
		t.Fatal("Register did not reach the write queue")
	}
	authenticated := make(chan bool, 1)
	go func() { got, ok := a.Authenticate(tok); authenticated <- ok && got == id }()
	select {
	case ok := <-authenticated:
		if !ok {
			t.Fatal("existing token denied")
		}
	case <-time.After(testwait.Bound):
		t.Fatal("Authenticate did not return while Register waited for its transaction")
	}
	release.Do(func() { close(clk.release) })
	if err := <-gateDone; err != nil {
		t.Fatal(err)
	}
	if err := <-registered; !errors.Is(err, ErrDenied) {
		t.Fatalf("Register = %v, want ErrDenied", err)
	}
}

func (c *observationClock) Now() time.Time {
	if c.block.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.release
	}
	return c.Fake.Now()
}

func TestCancelledCreateNodeKeepsMapConsistentWithStore(t *testing.T) {
	clk := &observationClock{
		Fake:    clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := New(st, probe.New(st, slog.Default()), nil, clk, time.UTC, slog.Default())
	if err := a.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		id    int64
		token string
		err   error
	}
	res := make(chan result, 1)
	clk.block.Store(true)
	go func() {
		id, token, err := a.CreateNode(ctx, store.NewNode{Name: "cancelled"})
		res <- result{id, token, err}
	}()
	<-clk.entered
	cancel()
	close(clk.release)
	got := <-res
	if err := st.SetRegisterWindow(context.Background(), make([]byte, 32), clk.Now().Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	if got.err != nil {
		t.Errorf("CreateNode returned %v, want nil: the transaction committed", got.err)
	}
	hashes, err := st.TokenHashes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	entries := len(a.byHash)
	a.mu.RUnlock()
	if len(hashes) != 1 || entries != 1 {
		t.Errorf("store hashes=%d auth hashes=%d, want 1 each", len(hashes), entries)
	}
	if id, _, err := a.Register(t.Context(), got.token, "ignored", netip.MustParseAddr("127.0.0.1")); err != nil || id != got.id {
		t.Errorf("认领 = %d,%v want %d,nil", id, err, got.id)
	}
}
