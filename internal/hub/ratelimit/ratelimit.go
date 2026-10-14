// Package ratelimit 是 hub 的按键令牌桶（按单调钟补充）与匿名入口按来源的限流中间件（BySource；IPv4 按地址、IPv6 按 /64）。
// AgentService 的 Report（按节点）、Register 与 PublicService（按来源，经 BySource）都用它：限速只有这一份实现。
package ratelimit

import (
	"fmt"
	"sync"
	"time"
)

// Buckets 的容量与补充周期在构造时固定。空闲满 capacity × refillPer 的桶必已补满，与新桶不可区分，
// 所以可以回收；这个推论要求同一实例的补充周期不变，构造时固定就是它的保证。
// 按补满周期最多扫描一次，避免每次请求遍历全部键。mu 保护桶状态与回收进度。
type Buckets[K comparable] struct {
	mu        sync.Mutex
	m         map[K]*bucket
	capacity  float64
	refillPer time.Duration
	lastSweep time.Duration
}

type bucket struct {
	tokens float64
	last   time.Duration
}

// New 要求 capacity 至少为 1、refillPer 为正，否则 panic：零容量拒绝一切、零周期除零，都只可能是装配错误。
func New[K comparable](capacity int, refillPer time.Duration) *Buckets[K] {
	if capacity < 1 || refillPer <= 0 {
		panic(fmt.Sprintf("ratelimit.New(%d, %v): capacity must be at least 1 and refillPer positive", capacity, refillPer))
	}
	return &Buckets[K]{m: map[K]*bucket{}, capacity: float64(capacity), refillPer: refillPer}
}

// Allow 从 key 的桶里取一个令牌，取不到返回 false。now 是单调钟读数（clock.Clock.Mono）。
func (b *Buckets[K]) Allow(key K, now time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	fullAfter := time.Duration(b.capacity * float64(b.refillPer))
	if now-b.lastSweep >= fullAfter {
		for k, bk := range b.m {
			if now-bk.last >= fullAfter {
				delete(b.m, k)
			}
		}
		b.lastSweep = now
	}
	bk := b.m[key]
	if bk == nil {
		bk = &bucket{tokens: b.capacity, last: now}
		b.m[key] = bk
	}
	bk.tokens = min(b.capacity, bk.tokens+float64(now-bk.last)/float64(b.refillPer))
	bk.last = now
	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}

// Forget 丢掉 key 的桶，下次请求从满桶开始。
func (b *Buckets[K]) Forget(key K) {
	b.mu.Lock()
	delete(b.m, key)
	b.mu.Unlock()
}
