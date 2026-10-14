package auth

import (
	"net/netip"
	"time"
)

// failureTracker 保存滑动窗口中的失败时刻，达到上限后从该次失败起锁满 window。
// 调用方持 Auth.mu，临界区无 I/O；每个键最多保存 limit 次失败。
// 键是 SourceKey 归一化后的来源（IPv4 一个地址，IPv6 一个 /64）：在自己的 /64 里换地址不重置计数。归一化在这里的
// 三个入口做，调用方传具体地址即可；传入已归一化的键（Register 从 ratelimit.SourceOf 拿到的就是）结果相同，掩码两次与一次无异。
//
// locked 只查本键，耗时与表里有多少别的来源无关：被登录门拒绝的请求也要在 mu 写锁下
// 调它，而 mu 同时是上报鉴权 Authenticate 的读锁，扫全表会让洪水按表大小拖住上报。
// 可回收的条目 until 已过，不影响 locked 的答案，所以回收放在 record：条目只在 record
// 里创建，每次先回收再记，表的上界仍是一个窗口内记过失败的来源数。密码和 Passkey 校验
// 共用登录门；第二因素失败与其他主动认证失败也在 Auth.mu 下进入同一来源计数。
type failureTracker struct {
	limit    int
	window   time.Duration
	m        map[netip.Addr]*failure
	sequence uint64
}

type failure struct {
	attempts []time.Duration
	until    time.Duration
	sequence uint64
}

func newFailureTracker(limit int, window time.Duration) *failureTracker {
	return &failureTracker{limit: limit, window: window, m: map[netip.Addr]*failure{}}
}

func (t *failureTracker) sweep(now time.Duration) {
	for ip, f := range t.m {
		if now < f.until {
			continue
		}
		for len(f.attempts) > 0 && now-f.attempts[0] >= t.window {
			f.attempts = f.attempts[1:]
		}
		if len(f.attempts) == 0 {
			delete(t.m, ip)
		}
	}
}

func (t *failureTracker) locked(from netip.Addr, now time.Duration) bool {
	from = SourceKey(from)
	f := t.m[from]
	return f != nil && now < f.until
}

// record 记下一次失败，返回窗口内的失败次数与这次调用是否设下了锁定。
//
// 设下锁定的调用恰好一次报告 true：锁定期间的调用不追加、报告 false，返回的次数此时仍等于 limit，所以
// "是否新锁定"不能由次数等于 limit 推出。until 是设下锁定那次失败的时刻加 window，锁定到期时窗口里已没有
// 失败，sweep 删掉整条记录，下一次锁定要重新累计满 limit 次，再报告一次 true。登录的锁定通知按这个报告
// 发送：锁定期间的请求即使走到了 record，也不会再发一条。
func (t *failureTracker) record(from netip.Addr, now time.Duration) (count int, newlyLocked bool) {
	t.sweep(now)
	from = SourceKey(from)
	f := t.m[from]
	if f == nil {
		f = &failure{}
		t.m[from] = f
	}
	if now < f.until {
		return len(f.attempts), false
	}
	f.attempts = append(f.attempts, now)
	t.sequence++
	f.sequence = t.sequence
	if len(f.attempts) >= t.limit {
		f.until = now + t.window
		return len(f.attempts), true
	}
	return len(f.attempts), false
}

func (t *failureTracker) clear(from netip.Addr) { delete(t.m, SourceKey(from)) }

// 认证成功只清除开始验证之前的失败；签发等待期间的新失败仍属于后来的尝试。
func (t *failureTracker) clearThrough(from netip.Addr, sequence uint64) {
	if f := t.m[SourceKey(from)]; f != nil && f.sequence <= sequence {
		t.clear(from)
	}
}
