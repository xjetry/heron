package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

const goodPassword = "correct horse battery"

func TestPasswordHashRoundTrip(t *testing.T) {
	phc, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("PHC = %q", phc)
	}
	if ok, err := VerifyPassword(phc, goodPassword); err != nil || !ok {
		t.Fatalf("verify = %v %v", ok, err)
	}
	if ok, err := VerifyPassword(phc, goodPassword+"x"); err != nil || ok {
		t.Fatalf("wrong password verified: %v %v", ok, err)
	}
	other, _ := HashPassword(goodPassword)
	if other == phc {
		t.Fatal("two hashes of the same password share a salt")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$YQ$YQ", "$argon2id$v=19$m=1,t=1,p=1$!!$YQ"} {
		t.Run(bad, func(t *testing.T) {
			if _, err := VerifyPassword(bad, goodPassword); err == nil {
				t.Fatalf("malformed hash %q accepted", bad)
			}
		})
	}
}

func TestSetPasswordRejectsShortOnes(t *testing.T) {
	a, _, _ := setup(t)
	if err := a.SetPassword(context.Background(), strings.Repeat("密", MinPasswordLen-1)); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("11 runes accepted: %v", err)
	}
	if err := a.SetPassword(context.Background(), strings.Repeat("密", MinPasswordLen)); err != nil {
		t.Fatalf("12 runes rejected: %v", err)
	}
}

func TestLoginWithoutAdminAlwaysFails(t *testing.T) {
	a, _, _ := setup(t)
	for _, password := range []string{"", goodPassword} {
		t.Run(password, func(t *testing.T) {
			if _, err := a.Login(context.Background(), password, netip.MustParseAddr("10.0.0.1")); !errors.Is(err, ErrNoAdmin) {
				t.Fatalf("empty admin table: %v, want ErrNoAdmin", err)
			}
		})
	}
}

func TestLoginWithoutAdminLocksAfterFailures(t *testing.T) {
	a, _, _ := setup(t)
	from := netip.MustParseAddr("10.0.0.1")
	for i := 0; i < failLimit; i++ {
		if token, err := a.Login(context.Background(), goodPassword, from); !errors.Is(err, ErrNoAdmin) || token != "" {
			t.Fatalf("no-admin attempt %d: token=%q err=%v", i+1, token, err)
		}
	}
	if _, err := a.Login(context.Background(), goodPassword, from); !errors.Is(err, ErrLocked) {
		t.Fatalf("no-admin failures did not lock: %v", err)
	}
}

func TestLoginIssuesSessionAndLocksOutAfterFailures(t *testing.T) {
	a, _, clk := setup(t)
	ctx := context.Background()
	from := netip.MustParseAddr("10.0.0.1")
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < failLimit; i++ {
		if _, err := a.Login(ctx, "wrong password here", from); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, err := a.Login(ctx, goodPassword, from); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked address logged in with the right password: %v", err)
	}
	other := netip.MustParseAddr("10.0.0.2")
	tok, err := a.Login(ctx, goodPassword, other)
	if err != nil {
		t.Fatalf("another address must not be affected: %v", err)
	}
	if _, ok, err := a.AuthenticateSession(ctx, []string{tok}); err != nil || !ok {
		t.Fatalf("fresh session rejected: %v %v", ok, err)
	}
	if _, ok, _ := a.AuthenticateSession(ctx, []string{"not a token"}); ok {
		t.Fatal("unknown token authenticated")
	}
	clk.Advance(failWindow)
	if _, err := a.Login(ctx, goodPassword, from); err != nil {
		t.Fatalf("lockout must lift after the window: %v", err)
	}
}

// 登录失败按来源键计：一台主机通常独占整个 IPv6 /64，每次换一个 /64 里的地址也在累加同一份计数，
// 锁定后同一 /64 的任何地址都进不来；另一个 /64 不受影响。IPv4 逐地址计由上一个用例钉住。
func TestLoginLockoutCountsAnIPv6Prefix64AsOneSource(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < failLimit; i++ {
		from := netip.MustParseAddr(fmt.Sprintf("2001:db8:1:2::%x", i+1))
		if _, err := a.Login(ctx, "wrong password here", from); !errors.Is(err, ErrBadPassword) {
			t.Fatalf("attempt %d from %s: %v", i+1, from, err)
		}
	}
	if _, err := a.Login(ctx, goodPassword, netip.MustParseAddr("2001:db8:1:2:ffff:ffff:ffff:ffff")); !errors.Is(err, ErrLocked) {
		t.Fatalf("another address in the locked /64 got past the lockout: %v", err)
	}
	if _, err := a.Login(ctx, goodPassword, netip.MustParseAddr("2001:db8:1:3::1")); err != nil {
		t.Fatalf("another /64 must not be affected: %v", err)
	}
}

func TestSessionExpiresIdleAndAbsolute(t *testing.T) {
	a, _, clk := setup(t)
	ctx := context.Background()
	from := netip.MustParseAddr("10.0.0.1")
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	idle, _ := a.Login(ctx, goodPassword, from)
	clk.Advance(SessionIdle)
	if _, ok, _ := a.AuthenticateSession(ctx, []string{idle}); ok {
		t.Fatal("idle session survived SessionIdle")
	}
	active, _ := a.Login(ctx, goodPassword, from)
	step := 12 * time.Hour
	for elapsed := time.Duration(0); elapsed+step < SessionAbsolute; elapsed += step {
		clk.Advance(step)
		if _, ok, err := a.AuthenticateSession(ctx, []string{active}); err != nil || !ok {
			t.Fatalf("session used every %v died after %v: %v %v", step, elapsed+step, ok, err)
		}
		waitTouch(t, a, active) // 等异步记录落库，下一次空闲判定才以它为基准
	}
	clk.Advance(step)
	if _, ok, _ := a.AuthenticateSession(ctx, []string{active}); ok {
		t.Fatal("session survived SessionAbsolute although it was kept active")
	}
}

// storedSession 从会话表里取出 hash 对应的那一行；没有时第二个返回值为 false。
func storedSession(t *testing.T, st *store.Store, h [32]byte) (store.Session, bool) {
	t.Helper()
	rows, err := st.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range rows {
		if sess.TokenHash == h {
			return sess, true
		}
	}
	return store.Session{}, false
}

// waitTouch 等待 last_used_at 追上当前时刻：TouchSessionAsync 投递即返回，
// 测试要观察它的结果就得等写协程。
func waitTouch(t *testing.T, a *Auth, token string) {
	t.Helper()
	testwait.Until(t, time.Millisecond, func() bool {
		sess, ok := storedSession(t, a.store, HashToken(token))
		return ok && sess.LastUsedAt.Equal(a.clk.Now())
	}, "last_used_at was not recorded")
}

func TestPasswordChangeRevokesSessions(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	from := netip.MustParseAddr("10.0.0.1")
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	tok, _ := a.Login(ctx, goodPassword, from)
	if err := a.SetPassword(ctx, "another long password"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.AuthenticateSession(ctx, []string{tok}); ok {
		t.Fatal("session survived a password change")
	}
	if _, err := a.Login(ctx, goodPassword, from); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("old password still works: %v", err)
	}
}

func TestLogoutEndsSession(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	tok, _ := a.Login(ctx, goodPassword, netip.MustParseAddr("10.0.0.1"))
	if err := a.Logout(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.AuthenticateSession(ctx, []string{tok}); ok {
		t.Fatal("session survived logout")
	}
}

func TestMalformedPasswordHashReturnsError(t *testing.T) {
	salt := "MDEyMzQ1Njc4OWFiY2RlZg"
	digest := strings.Repeat("A", 43)
	base := "$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$" + digest
	cases := map[string]string{
		"salt-encoding":        strings.Replace(base, salt, "!!", 1),
		"large-memory":         strings.Replace(base, "m=65536", "m=262145", 1),
		"small-memory":         strings.Replace(base, "m=65536", "m=0", 1),
		"large-time":           strings.Replace(base, "t=3", "t=11", 1),
		"large-threads":        strings.Replace(base, "p=4", "p=17", 1),
		"unknown-version":      strings.Replace(base, "v=19", "v=18", 1),
		"malformed-parameters": strings.Replace(base, "t=3", "t=x", 1),
		"zero-time":            strings.Replace(base, "t=3", "t=0", 1),
		"zero-threads":         strings.Replace(base, "p=4", "p=0", 1),
		"empty-digest":         strings.TrimSuffix(base, digest),
		"empty-salt":           strings.Replace(base, salt, "", 1),
		"version-suffix":       strings.Replace(base, "v=19", "v=19junk", 1),
		"parameter-suffix":     strings.Replace(base, "p=4", "p=4junk", 1),
		"short-digest":         strings.TrimSuffix(base, digest) + "YQ",
		"short-salt":           strings.Replace(base, salt, "YQ", 1),
		"digest-encoding":      strings.TrimSuffix(base, digest) + "!!",
	}
	for name, phc := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("malformed PHC panicked: %v", p)
				}
			}()
			if ok, err := VerifyPassword(phc, goodPassword); err == nil || ok {
				t.Fatalf("malformed PHC accepted: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestFailureWindowAndFullLockDuration(t *testing.T) {
	for _, kind := range []string{"login", "register"} {
		t.Run(kind, func(t *testing.T) {
			a, _, clk := setup(t)
			ctx := context.Background()
			if err := a.SetPassword(ctx, goodPassword); err != nil {
				t.Fatal(err)
			}
			key, _, err := a.OpenWindow(ctx, time.Hour, 10)
			if err != nil {
				t.Fatal(err)
			}
			from := netip.MustParseAddr("10.0.0.1")
			attempt := func(valid bool) error {
				if kind == "login" {
					password := "wrong"
					if valid {
						password = goodPassword
					}
					_, err := a.Login(ctx, password, from)
					return err
				}
				password := "wrong"
				if valid {
					password = key
				}
				_, _, err := a.Register(ctx, password, "n", from)
				return err
			}
			// 最早失败过期后，后面的四次仍位于滑动窗口内。
			_ = attempt(false)
			clk.Advance(10 * time.Minute)
			_ = attempt(false)
			clk.Advance(6 * time.Minute)
			for i := 0; i < 4; i++ {
				_ = attempt(false)
			}
			if err := attempt(true); err == nil {
				t.Fatal("rolling failures did not lock address")
			}
			clk.Advance(10 * time.Minute)
			if err := attempt(true); err == nil {
				t.Fatal("lock ended before fifteen minutes after threshold")
			}
			clk.SetWall(clk.Now().Add(failWindow + time.Second))
			if err := attempt(true); err == nil {
				t.Fatal("wall clock jump lifted lock")
			}
			clk.Advance(5 * time.Minute)
			if err := attempt(true); err != nil {
				t.Fatalf("full lock duration elapsed: %v", err)
			}
		})
	}
}

// selectLoginChannel 建一个 webhook 渠道并选为登录通知渠道。没选渠道时 RecordLoginEvent 不写事件，
// 用例就看不到通知；渠道的地址不会被访问，auth 的用例不跑投递队列。
func selectLoginChannel(t *testing.T, st *store.Store) {
	t.Helper()
	channel, err := st.SaveNotifyChannel(context.Background(), store.NotifyChannel{Name: "login", Kind: store.ChannelWebhook, Config: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	channels := []int64{channel.ID}
	if _, err := st.SaveSettings(context.Background(), store.SettingsUpdate{LoginChannels: &channels}); err != nil {
		t.Fatal(err)
	}
}

func lockEvents(t *testing.T, st *store.Store) []store.AlertEvent {
	t.Helper()
	events, err := st.ListAlertEvents(context.Background(), 0, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(events, func(ev store.AlertEvent) bool { return ev.Transition != store.TransitionLoginLocked })
}

// 每次锁定通知一条：锁满后继续猜错、锁定期间给出正确密码都不再通知；锁定到期后再猜满上限是新的一次锁定，
// 再通知一条。第二次锁定能通知，要求到期时 record 的回收清掉了上一次锁定的失败。
func TestEachLoginLockNotifiesOnce(t *testing.T) {
	a, st, clk := setup(t)
	ctx := context.Background()
	selectLoginChannel(t, st)
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	from := netip.MustParseAddr("192.0.2.9")
	for lock := 1; lock <= 2; lock++ {
		for i := 1; i <= failLimit+2; i++ {
			want := ErrBadPassword
			if i > failLimit {
				want = ErrLocked
			}
			if _, err := a.Login(ctx, "wrong password here", from); !errors.Is(err, want) {
				t.Fatalf("lock %d, attempt %d: %v, want %v", lock, i, err, want)
			}
		}
		if _, err := a.Login(ctx, goodPassword, from); !errors.Is(err, ErrLocked) {
			t.Fatalf("lock %d: correct password during the lock: %v, want ErrLocked", lock, err)
		}
		if evs := lockEvents(t, st); len(evs) != lock {
			t.Fatalf("after lock %d: %d lock notifications, want %d: %+v", lock, len(evs), lock, evs)
		}
		clk.Advance(failWindow)
	}
}

// lockBeforeRecordClock 在门内的第一次单调时钟读取之前调用 lock。门内读单调时钟的只有记账那一句
// （a.login.record(from, a.clk.Mono())），调用方此时持 mu 与门，lock 在同一协程里直接改计数器。
// 门外的读取（判锁定）靠门是否空闲区分：那时门还没取，TryLock 成功，立刻放回。
type lockBeforeRecordClock struct {
	*clock.Fake
	gate  *sync.Mutex
	lock  func(now time.Duration)
	fired bool
}

func (c *lockBeforeRecordClock) Mono() time.Duration {
	now := c.Fake.Mono()
	if c.fired {
		return now
	}
	if c.gate.TryLock() {
		c.gate.Unlock()
		return now
	}
	c.fired = true
	c.lock(now)
	return now
}

// 记账时来源已被锁满，也就是锁定期间的请求走到了 record：这次失败不再通知。判锁定与取门在同一个 mu
// 临界区里、门只准入一个，正常调度造不出这个交错，这里在记账前直接把来源锁满。钉住的是 Login 只按
// record 的报告通知：改成按返回的次数等于上限推断，或 record 在锁定期间报告 true，这条都会红。
func TestLockedSourceReachingRecordDoesNotNotifyAgain(t *testing.T) {
	a, st, clk := setup(t)
	ctx := context.Background()
	selectLoginChannel(t, st)
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	from := netip.MustParseAddr("192.0.2.7")
	hook := &lockBeforeRecordClock{Fake: clk, gate: &a.loginGate, lock: func(now time.Duration) {
		for range failLimit {
			a.login.record(from, now)
		}
	}}
	a.clk = hook
	if _, err := a.Login(ctx, "wrong password here", from); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("login into a lock set during its verification: %v, want ErrBadPassword", err)
	}
	if !hook.fired {
		t.Fatal("login did not read the monotonic clock inside the gate; the source was never locked before its record")
	}
	if _, err := a.Login(ctx, goodPassword, from); !errors.Is(err, ErrLocked) {
		t.Fatalf("source locked before the record is not locked: %v", err)
	}
	if evs := lockEvents(t, st); len(evs) != 0 {
		t.Fatalf("a failure recorded into an existing lock notified: %+v", evs)
	}
}

// 锁定通知在门外发：停在达到阈值那次失败的通知步骤上时，另一来源的一次校验立即得到 ErrBadPassword，
// 而不是 ErrLoginBusy。暂停点是 Auth 的墙钟读取：门内只读单调时钟，这条路径上第一次读墙钟就是通知
// 取事件时刻，它先于通知写库。
func TestLockNotificationIsOutsideGate(t *testing.T) {
	a, st, clk := setup(t)
	ctx := context.Background()
	selectLoginChannel(t, st)
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	from, other := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	for range failLimit - 1 {
		if _, err := a.Login(ctx, "wrong password here", from); !errors.Is(err, ErrBadPassword) {
			t.Fatal(err)
		}
	}
	gate := &observationClock{Fake: clk, entered: make(chan struct{}), release: make(chan struct{})}
	gate.block.Store(true)
	a.clk = gate
	release := sync.OnceFunc(func() { close(gate.release) })
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := a.Login(ctx, "wrong password here", from)
		done <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(testwait.Bound):
		t.Fatal("the threshold failure never reached the lock notification")
	}
	otherDone := make(chan error, 1)
	go func() {
		_, err := a.Login(ctx, "wrong password here", other)
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		if !errors.Is(err, ErrBadPassword) {
			t.Errorf("another source while the lock notification is in progress: %v, want ErrBadPassword", err)
		}
	case <-time.After(testwait.Bound):
		release()
		<-otherDone
		t.Error("another source waited for the lock notification")
	}
	release()
	if err := <-done; !errors.Is(err, ErrBadPassword) {
		t.Fatalf("threshold failure: %v, want ErrBadPassword", err)
	}
	if evs := lockEvents(t, st); len(evs) != 1 {
		t.Fatalf("lock notifications: %+v, want one", evs)
	}
}

// 没设管理员时登录一律失败，失败照常计数；猜满上限同样锁定并通知一条：有人在对一个还没设密码的 hub
// 反复尝试，运维者同样要知道。
func TestLockWithoutAdminNotifies(t *testing.T) {
	a, st, _ := setup(t)
	ctx := context.Background()
	selectLoginChannel(t, st)
	from := netip.MustParseAddr("192.0.2.3")
	for i := 1; i <= failLimit; i++ {
		if _, err := a.Login(ctx, goodPassword, from); !errors.Is(err, ErrNoAdmin) {
			t.Fatalf("attempt %d without an admin: %v, want ErrNoAdmin", i, err)
		}
	}
	if _, err := a.Login(ctx, goodPassword, from); !errors.Is(err, ErrLocked) {
		t.Fatalf("after %d failures without an admin: %v, want ErrLocked", failLimit, err)
	}
	if evs := lockEvents(t, st); len(evs) != 1 {
		t.Fatalf("lock notifications without an admin: %+v, want one", evs)
	}
}

func TestPasswordChangeDuringLoginDoesNotIssueSession(t *testing.T) {
	a, st, clk := setup(t)
	ctx := context.Background()
	selectLoginChannel(t, st)
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	gate := &observationClock{Fake: clk, entered: make(chan struct{}), release: make(chan struct{})}
	gate.block.Store(true)
	a.clk = gate
	type result struct {
		token string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		tok, err := a.Login(ctx, goodPassword, netip.MustParseAddr("10.0.0.1"))
		done <- result{tok, err}
	}()
	<-gate.entered
	// 另一个 Auth 不共享锁，代表 passwd 独立于服务进程的修改。
	other := New(st, probe.New(st, a.log), nil, clk, time.UTC, a.log)
	err := other.SetPassword(ctx, "another long password")
	close(gate.release)
	got := <-done
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(got.err, ErrBadPassword) || got.token != "" {
		t.Fatalf("stale password issued session: token=%q err=%v", got.token, got.err)
	}
	events, err := st.ListAlertEvents(ctx, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Transition != store.TransitionAuthChanged {
		t.Fatalf("password change must emit only its audit, not login success: %v", events)
	}
}

// loginPausedInGate 让 from 的一次错误密码登录停在 "login failed" 日志上：它已记完失败、
// 尚未放门，只持 loginGate，不持 mu，也不持数据库连接，别的请求读库与取 mu 都不受它影响。
// 暂停点靠日志语句的位置，所以先断言门确实被占。finish 放行并返回这次登录的结果，可重复调用。
func loginPausedInGate(t *testing.T, a *Auth, from netip.Addr) (finish func() error) {
	t.Helper()
	pause, entered, release := testwait.PauseAtLog(a.log.Handler(), "login failed")
	a.log = slog.New(pause)
	done := make(chan error, 1)
	go func() {
		_, err := a.Login(context.Background(), "wrong password here", from)
		done <- err
	}()
	finish = sync.OnceValue(func() error {
		release()
		return <-done
	})
	t.Cleanup(func() { _ = finish() })
	select {
	case <-entered:
	case <-time.After(testwait.Bound):
		t.Fatal("wrong-password login did not reach its failure log")
	}
	if a.loginGate.TryLock() {
		a.loginGate.Unlock()
		t.Fatal("login paused at its failure log without holding the gate")
	}
	return finish
}

// 门不分来源，失败按请求自己的来源记。攻击者的错误密码占住门时，管理员用正确密码重试
// failLimit+1 次，每次都立即得到 ErrLoginBusy 且不留计数；忙碌若计入失败，管理员来源在
// 第 failLimit 次后就被锁满 failWindow。
func TestConcurrentLoginsRejectWithoutWaitingForPasswordVerification(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	attacker, admin := netip.MustParseAddr("198.51.100.7"), netip.MustParseAddr("203.0.113.9")
	finish := loginPausedInGate(t, a, attacker)
	start := make(chan struct{})
	done := make(chan error, failLimit+1)
	for range cap(done) {
		go func() {
			<-start
			_, err := a.Login(ctx, goodPassword, admin)
			done <- err
		}()
	}
	close(start)
	for i := range cap(done) {
		select {
		case err := <-done:
			if !errors.Is(err, ErrLoginBusy) {
				t.Errorf("admin login while another source holds the gate = %v, want ErrLoginBusy", err)
			}
		case <-time.After(testwait.Bound):
			_ = finish()
			for range cap(done) - i {
				<-done
			}
			t.Fatal("admin login waited for another source's password verification")
		}
	}
	a.mu.Lock()
	f := a.login.m[SourceKey(admin)]
	locked := a.login.locked(admin, a.clk.Mono())
	a.mu.Unlock()
	if f != nil || locked {
		t.Fatalf("busy attempts counted against the admin source: entry=%+v locked=%v", f, locked)
	}
	if err := finish(); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("paused login = %v, want ErrBadPassword", err)
	}
	if _, err := a.Login(ctx, goodPassword, admin); err != nil {
		t.Fatalf("admin login after the gate freed: %v", err)
	}
}

// 门在读密码哈希之前：门被占时，已取消的 context 还没被读库用到就得到 ErrLoginBusy。
// 门空闲时同一个 context 必须在读库处失败，否则这条用例分辨不出门在读库之前还是之后；
// 用错误密码，是因为正确密码会走到会话写库，那里同样报取消，就证明不了读库用了 context。
func TestLoginTakesGateBeforeReadingPassword(t *testing.T) {
	a, _, _ := setup(t)
	if err := a.SetPassword(context.Background(), goodPassword); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	from := netip.MustParseAddr("10.0.0.1")
	if _, err := a.Login(ctx, "wrong password here", from); !errors.Is(err, context.Canceled) {
		t.Fatalf("login with cancelled context and free gate = %v, want context.Canceled from the password read", err)
	}
	a.loginGate.Lock()
	defer a.loginGate.Unlock()
	if _, err := a.Login(ctx, "wrong password here", from); !errors.Is(err, ErrLoginBusy) {
		t.Fatalf("login with busy gate = %v, want ErrLoginBusy before reading the password", err)
	}
}

func TestLoginChecksLockoutBeforeBusyGate(t *testing.T) {
	a, _, _ := setup(t)
	from := netip.MustParseAddr("10.0.0.1")
	a.mu.Lock()
	for range failLimit {
		a.login.record(from, a.clk.Mono())
	}
	a.mu.Unlock()
	a.loginGate.Lock()
	defer a.loginGate.Unlock()
	if _, err := a.Login(context.Background(), goodPassword, from); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked source while gate busy = %v, want ErrLocked", err)
	}
}

func TestSessionAuthenticationDoesNotUseLoginGate(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	token, err := a.Login(ctx, goodPassword, netip.MustParseAddr("10.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	a.loginGate.Lock()
	release := sync.OnceFunc(a.loginGate.Unlock)
	defer release()
	done := make(chan error, 1)
	go func() {
		_, ok, err := a.AuthenticateSession(ctx, []string{token})
		if err == nil && !ok {
			err = errors.New("live session rejected")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("session authentication used busy login gate: %v", err)
		}
	case <-time.After(testwait.Bound):
		release()
		<-done
		t.Fatal("session authentication waited for busy login gate")
	}
}

func TestLoginReleasesGateBeforeSessionIssuance(t *testing.T) {
	a, _, clk := setup(t)
	ctx := context.Background()
	from := netip.MustParseAddr("10.0.0.1")
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	gate := &observationClock{Fake: clk, entered: make(chan struct{}), release: make(chan struct{})}
	gate.block.Store(true)
	a.clk = gate
	release := sync.OnceFunc(func() { close(gate.release) })
	done := make(chan error, 1)
	go func() {
		_, err := a.Login(ctx, goodPassword, from)
		done <- err
	}()
	defer release()
	select {
	case <-gate.entered:
	case <-time.After(testwait.Bound):
		t.Fatal("login did not reach session issuance")
	}
	for range failLimit {
		if _, err := a.Login(ctx, "wrong", from); !errors.Is(err, ErrBadPassword) {
			t.Errorf("password verification blocked by session issuance: %v", err)
		}
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := a.Login(ctx, goodPassword, from); !errors.Is(err, ErrLocked) {
		t.Fatalf("session issuance cleared newer failures: %v", err)
	}
}

// 登录不占 mutMu，节点变更与 Load 不等它。两个暂停点：门内，停在读密码哈希、argon2 校验与
// 记账之后、放门之前的失败日志上；放门之后，停在签发会话前的墙钟读取上。门内这一点照得到
// 从取门起持有到放门的锁，照不到只包住校验那一句、在失败日志之前就释放的锁。
func TestNodeMutationsDoNotWaitForLogin(t *testing.T) {
	pauses := []struct {
		name string
		// start 让一次登录停在暂停点；finish 放行并核对这次登录的结果，可重复调用。
		start func(t *testing.T, a *Auth, clk *clock.Fake) (finish func() error)
	}{
		{"in gate", func(t *testing.T, a *Auth, _ *clock.Fake) func() error {
			finish := loginPausedInGate(t, a, netip.MustParseAddr("10.0.0.1"))
			return func() error {
				if err := finish(); !errors.Is(err, ErrBadPassword) {
					return fmt.Errorf("paused login = %v, want ErrBadPassword", err)
				}
				return nil
			}
		}},
		{"before session issuance", func(t *testing.T, a *Auth, clk *clock.Fake) func() error {
			// 只暂停 Login 在签发前的墙钟读取，存储仍用原时钟，排除写队列阻塞。
			gate := &observationClock{Fake: clk, entered: make(chan struct{}), release: make(chan struct{})}
			gate.block.Store(true)
			a.clk = gate
			done := make(chan error, 1)
			go func() {
				_, err := a.Login(context.Background(), goodPassword, netip.MustParseAddr("10.0.0.1"))
				done <- err
			}()
			finish := sync.OnceValue(func() error {
				close(gate.release)
				return <-done
			})
			t.Cleanup(func() { _ = finish() })
			select {
			case <-gate.entered:
			case <-time.After(testwait.Bound):
				t.Fatal("Login did not reach session issuance")
			}
			return finish
		}},
	}
	for _, pause := range pauses {
		for _, operation := range []string{"Register", "CreateNode", "RotateToken", "DeleteNode", "Load"} {
			t.Run(pause.name+"/"+operation, func(t *testing.T) {
				a, _, clk := setup(t)
				ctx := context.Background()
				if err := a.SetPassword(ctx, goodPassword); err != nil {
					t.Fatal(err)
				}
				id, _, err := a.CreateNode(ctx, store.NewNode{Name: "existing"})
				if err != nil {
					t.Fatal(err)
				}
				key, _, err := a.OpenWindow(ctx, time.Hour, 1)
				if err != nil {
					t.Fatal(err)
				}
				finish := pause.start(t, a, clk)
				done := make(chan error, 1)
				go func() {
					var err error
					switch operation {
					case "Register":
						_, _, err = a.Register(ctx, key, "registered", netip.MustParseAddr("10.0.0.2"))
					case "CreateNode":
						_, _, err = a.CreateNode(ctx, store.NewNode{Name: "new"})
					case "RotateToken":
						_, err = a.RotateToken(ctx, id)
					case "DeleteNode":
						err = a.DeleteNode(ctx, id)
					case "Load":
						err = a.Load(ctx)
					}
					done <- err
				}()
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("%s failed while Login was paused %s: %v", operation, pause.name, err)
					}
				case <-time.After(testwait.Bound):
					// 先放行登录并等待节点操作退出，避免失败路径留下访问存储的协程。
					_ = finish()
					<-done
					t.Fatalf("%s waited for Login paused %s", operation, pause.name)
				}
				if err := finish(); err != nil {
					t.Errorf("login after release: %v", err)
				}
			})
		}
	}
}

// cheapPHC 按 m=32,t=1,p=1 生成 password 的 PHC，远低于新哈希的默认成本。
func cheapPHC(password string) string {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(password), salt, 1, 32, 1, 32)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=32,t=1,p=1$%s$%s", enc.EncodeToString(salt), enc.EncodeToString(key))
}

func TestPasswordVerificationUsesStoredParameters(t *testing.T) {
	if ok, err := VerifyPassword(cheapPHC(goodPassword), goodPassword); err != nil || !ok {
		t.Fatalf("stored parameters ignored: %v %v", ok, err)
	}
}

// 同一来源并发猜错密码，真正走到校验的次数恰为 failLimit。钉住 verifyLoginPassword 的两处
// 语句顺序：判锁定与 TryLock 在同一个 mu 临界区里，拆开后，判定时尚未锁定的请求能在第
// failLimit 次失败落账、放门之后进门；失败在放门之前记账，先放门则另一请求能在落账前进门。
// 两处都没有测试能在不持 mu 时挂住的调用（时钟在 mu 内读），这是统计性用例：越界要调度
// 恰好落在窗口里，所以每轮 32 个协程循环争用，跑 250 轮、每轮换一个来源。正确实现下，进门的
// 请求判定时已看到此前全部落账，调度怎样都不越界。存储的哈希按 TestPasswordVerificationUsesStoredParameters 钉住的
// 自带参数校验，低成本让一次尝试以微秒计。
func TestConcurrentWrongPasswordsVerifyExactlyFailLimit(t *testing.T) {
	const rounds, workers = 250, 32
	// 每轮的来源必须互不相同：假时钟不走，用过的来源在整个用例里一直锁着，再用它的那一轮
	// 开头就全是 ErrLocked，正确实现也会报校验 0 次。来源按轮次编进 198.18.0.0/15 的低两字节。
	if rounds > 1<<16 {
		t.Fatalf("%d rounds do not fit in the two-byte source number", rounds)
	}
	a, _, _ := setup(t)
	ctx := context.Background()
	if err := a.store.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	for round := range rounds {
		from := netip.AddrFrom4([4]byte{198, 18, byte(round >> 8), byte(round)})
		var verified atomic.Int64
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for range cap(errs) {
			wg.Go(func() {
				for {
					_, err := a.Login(ctx, "wrong password here", from)
					switch {
					case errors.Is(err, ErrLoginBusy):
					case errors.Is(err, ErrBadPassword):
						verified.Add(1)
					case errors.Is(err, ErrLocked):
						return
					default:
						errs <- err
						return
					}
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: unexpected login error: %v", round, err)
		}
		if got := verified.Load(); got != failLimit {
			t.Fatalf("round %d: verified wrong passwords = %d, want %d", round, got, failLimit)
		}
	}
}

func TestLoginAndRegisterFailuresAreIndependent(t *testing.T) {
	for _, lockedKind := range []string{"login", "register"} {
		t.Run(lockedKind, func(t *testing.T) {
			a, _, _ := setup(t)
			ctx := context.Background()
			if err := a.SetPassword(ctx, goodPassword); err != nil {
				t.Fatal(err)
			}
			key, _, err := a.OpenWindow(ctx, time.Hour, 10)
			if err != nil {
				t.Fatal(err)
			}
			from := netip.MustParseAddr("10.0.0.1")
			attempt := func(kind string, valid bool) error {
				if kind == "login" {
					p := "wrong"
					if valid {
						p = goodPassword
					}
					_, err := a.Login(ctx, p, from)
					return err
				}
				p := "wrong"
				if valid {
					p = key
				}
				_, _, err := a.Register(ctx, p, "n", from)
				return err
			}
			for i := 0; i < failLimit; i++ {
				_ = attempt(lockedKind, false)
			}
			other := "login"
			if lockedKind == "login" {
				other = "register"
			}
			if err := attempt(other, true); err != nil {
				t.Fatalf("failure counter leaked into %s: %v", other, err)
			}
			if err := attempt(lockedKind, true); err == nil {
				t.Fatal("success in other flow cleared lock")
			}
		})
	}
}

func TestSuccessfulLoginClearsFailures(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	from := netip.MustParseAddr("10.0.0.1")
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		for i := 0; i < failLimit-1; i++ {
			_, _ = a.Login(ctx, "wrong", from)
		}
		if _, err := a.Login(ctx, goodPassword, from); err != nil {
			t.Fatalf("success did not clear consecutive failures: %v", err)
		}
	}
}

// 登录成功清掉的是整个来源键的计数：隐私地址轮换后从同一 /64 的另一个地址登录成功，旧地址的失败也一并清零。
func TestSuccessfulLoginClearsFailuresOfTheWholeSource(t *testing.T) {
	a, _, _ := setup(t)
	ctx := context.Background()
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	first, second := netip.MustParseAddr("2001:db8:1:2::a"), netip.MustParseAddr("2001:db8:1:2::b")
	for i := 0; i < failLimit-1; i++ {
		_, _ = a.Login(ctx, "wrong", first)
	}
	if _, err := a.Login(ctx, goodPassword, second); err != nil {
		t.Fatalf("login from the same /64: %v", err)
	}
	for i := 0; i < failLimit-1; i++ {
		if _, err := a.Login(ctx, "wrong", first); errors.Is(err, ErrLocked) {
			t.Fatalf("failure %d after a successful login from the same /64 hit the lock: failures were not cleared", i+1)
		}
	}
	if _, err := a.Login(ctx, goodPassword, first); err != nil {
		t.Fatalf("correct password: %v", err)
	}
}

func TestSessionTouchThreshold(t *testing.T) {
	a, st, clk := setup(t)
	ctx := context.Background()
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	tok, err := a.Login(ctx, goodPassword, netip.MustParseAddr("10.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	initial := clk.Now()
	for _, elapsed := range []time.Duration{touchEvery - time.Second, time.Second} {
		clk.Advance(elapsed)
		if _, ok, err := a.AuthenticateSession(ctx, []string{tok}); err != nil || !ok {
			t.Fatalf("live session rejected: %v %v", ok, err)
		}
		// 同步写排在异步刷新之后，完成时才读回，以免用空队列竞态证明未刷新。
		if _, err := st.DeleteExpiredSessions(ctx, initial); err != nil {
			t.Fatal(err)
		}
		sess, ok := storedSession(t, st, HashToken(tok))
		want := initial
		if clk.Now().Sub(initial) >= touchEvery {
			want = clk.Now()
		}
		if !ok || !sess.LastUsedAt.Equal(want) {
			t.Fatalf("touch threshold: last=%v want=%v ok=%v", sess.LastUsedAt, want, ok)
		}
	}
}

// 过期的候选不挡住后面活着的那个；查到的过期会话不论排在选中的那个之前还是之后都删掉，删掉后墙钟回拨也不会复活它。
func TestAuthenticateSessionPicksFirstLiveCandidateAndDeletesExpiredOnes(t *testing.T) {
	for _, expiredFirst := range []bool{true, false} {
		t.Run(fmt.Sprint("expiredFirst=", expiredFirst), func(t *testing.T) {
			a, st, clk := setup(t)
			ctx := context.Background()
			from := netip.MustParseAddr("10.0.0.1")
			if err := a.SetPassword(ctx, goodPassword); err != nil {
				t.Fatal(err)
			}
			expired, err := a.Login(ctx, goodPassword, from)
			if err != nil {
				t.Fatal(err)
			}
			clk.Advance(SessionIdle)
			live, err := a.Login(ctx, goodPassword, from)
			if err != nil {
				t.Fatal(err)
			}
			unknown, _ := NewToken()
			candidates := []string{"not a token", unknown, expired, live}
			if !expiredFirst {
				candidates = []string{"not a token", unknown, live, expired}
			}
			got, ok, err := a.AuthenticateSession(ctx, candidates)
			if err != nil || !ok || got != live {
				t.Fatalf("AuthenticateSession(%v) = %q %v %v, want the live session", candidates, got, ok, err)
			}
			if sess, ok := storedSession(t, st, HashToken(expired)); ok {
				t.Errorf("expired candidate was not deleted: %+v", sess)
			}
		})
	}
}

// 多个候选都有效时只刷新选中那个的最近使用时刻。空闲过期量的是会话被使用的间隔，而这次请求记在选中的会话上
// （ctx 里放的是它，Logout 与"当前"都读它），没选中的会话这次没有被用到。
func TestAuthenticateSessionTouchesOnlyTheChosenSession(t *testing.T) {
	a, st, clk := setup(t)
	ctx := context.Background()
	from := netip.MustParseAddr("10.0.0.1")
	if err := a.SetPassword(ctx, goodPassword); err != nil {
		t.Fatal(err)
	}
	first, err := a.Login(ctx, goodPassword, from)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Login(ctx, goodPassword, from)
	if err != nil {
		t.Fatal(err)
	}
	initial := clk.Now()
	clk.Advance(touchEvery)
	if got, ok, err := a.AuthenticateSession(ctx, []string{first, second}); err != nil || !ok || got != first {
		t.Fatalf("AuthenticateSession = %q %v %v, want the first candidate", got, ok, err)
	}
	// 同步写排在异步刷新之后，它返回时刷新已经落库；传入 initial 时它不删任何行。
	if _, err := st.DeleteExpiredSessions(ctx, initial); err != nil {
		t.Fatal(err)
	}
	if sess, ok := storedSession(t, st, HashToken(first)); !ok || !sess.LastUsedAt.Equal(clk.Now()) {
		t.Errorf("chosen session last used %v (found %v), want %v", sess.LastUsedAt, ok, clk.Now())
	}
	if sess, ok := storedSession(t, st, HashToken(second)); !ok || !sess.LastUsedAt.Equal(initial) {
		t.Errorf("session that was not chosen last used %v (found %v), want it untouched at %v", sess.LastUsedAt, ok, initial)
	}
}
