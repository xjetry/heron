package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// locked 不回收别的来源：被登录门拒绝的请求在 mu 写锁下调它，扫表会让持锁时长随表大小增长。
// 过期来源由下一次 record 删除，表才不会无界增长。
func TestFailureLockCheckLeavesReclamationToRecord(t *testing.T) {
	tr := newFailureTracker(failLimit, failWindow)
	stale, fresh := netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("203.0.113.2")
	tr.record(stale, 0)
	if tr.locked(fresh, failWindow) {
		t.Fatal("source without failures is locked")
	}
	if len(tr.m) != 1 || tr.m[stale] == nil {
		t.Fatalf("lock check touched other sources: %v", tr.m)
	}
	tr.record(fresh, failWindow)
	if len(tr.m) != 1 || tr.m[fresh] == nil {
		t.Fatalf("record kept an expired source: %v", tr.m)
	}
}

// 只有设下锁定的那次 record 报告 true。锁定期间的调用返回的次数仍等于上限，报告必须是 false；
// 锁定到期后重新累计满上限，下一次锁定再报告一次。登录的锁定通知按这个报告发送。
func TestFailureRecordReportsOnlyTheCallThatSetsTheLock(t *testing.T) {
	tr := newFailureTracker(failLimit, failWindow)
	from := netip.MustParseAddr("203.0.113.1")
	var now time.Duration
	for lock := range 2 {
		for i := 1; i <= failLimit; i++ {
			count, newlyLocked := tr.record(from, now)
			if count != i || newlyLocked != (i == failLimit) {
				t.Fatalf("lock %d, failure %d: record = (%d, %v), want (%d, %v)", lock, i, count, newlyLocked, i, i == failLimit)
			}
		}
		for _, during := range []time.Duration{0, time.Second, failWindow - time.Nanosecond} {
			if count, newlyLocked := tr.record(from, now+during); count != failLimit || newlyLocked {
				t.Fatalf("lock %d: record %v into the lock = (%d, %v), want (%d, false)", lock, during, count, newlyLocked, failLimit)
			}
		}
		now += failWindow
	}
}

// 被门拒绝的登录只查本来源的计数：表里别的来源有多少，不该改变它的耗时，也不该改变洪水期间
// 上报鉴权 Authenticate 等 mu 的时长。entries=0 是对照组；洪水协程数取 8。
func BenchmarkBusyLoginRejection(b *testing.B) {
	for _, entries := range []int{0, 20000} {
		b.Run(fmt.Sprintf("entries=%d", entries), func(b *testing.B) {
			a := busyLoginAuth(b, entries)
			ctx := context.Background()
			from := netip.MustParseAddr("192.0.2.1")
			for b.Loop() {
				if _, err := a.Login(ctx, "wrong password here", from); !errors.Is(err, ErrLoginBusy) {
					b.Fatalf("login with busy gate = %v, want ErrLoginBusy", err)
				}
			}
		})
	}
}

func BenchmarkAuthenticateDuringBusyLoginFlood(b *testing.B) {
	for _, entries := range []int{0, 20000} {
		for _, flood := range []int{0, 8} {
			b.Run(fmt.Sprintf("entries=%d/flood=%d", entries, flood), func(b *testing.B) {
				a := busyLoginAuth(b, entries)
				ctx := context.Background()
				_, tok, err := runningNode(b, a, "reporting")
				if err != nil {
					b.Fatal(err)
				}
				stop := make(chan struct{})
				var wg sync.WaitGroup
				for range flood {
					wg.Go(func() {
						from := netip.MustParseAddr("192.0.2.1")
						for {
							select {
							case <-stop:
								return
							default:
							}
							_, _ = a.Login(ctx, "wrong password here", from)
						}
					})
				}
				for b.Loop() {
					if _, ok := a.Authenticate(tok); !ok {
						b.Fatal("node token rejected")
					}
				}
				close(stop)
				wg.Wait()
			})
		}
	}
}

// busyLoginAuth 按 record 的路径预填 entries 个不同 IPv6 /64 的未过期失败，并替登录占住门，
// 此后的 Login 都走忙碌拒绝分支。
func busyLoginAuth(b *testing.B, entries int) *Auth {
	a, _, _ := setup(b)
	a.mu.Lock()
	for i := range entries {
		from := netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)})
		a.login.record(from, a.clk.Mono())
	}
	n := len(a.login.m)
	a.mu.Unlock()
	if n != entries {
		b.Fatalf("prefilled %d sources, want %d", n, entries)
	}
	a.loginGate.Lock()
	b.Cleanup(a.loginGate.Unlock)
	return a
}
