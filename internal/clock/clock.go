// Package clock 把墙钟与单调钟分开注入。
//
// hub 内凡是"时长"（在线判定、限速、退避）一律用 Mono：墙钟被 NTP 向后拨
// 时差值为负，拿它做除数或比较都会得到荒谬的结果。墙钟只用于给样本打点
// 与展示。
package clock

import (
	"sync"
	"time"
)

type Clock interface {
	// Now 是墙钟。
	Now() time.Time
	// Mono 是单调钟，自任意固定起点起算，只能用于相减。
	Mono() time.Duration
}

type real struct{ start time.Time }

// Real 返回进程时钟。Mono 基于 time.Since，它读取 time.Time 内嵌的
// 单调读数，不受墙钟调整影响。
func Real() Clock { return &real{start: time.Now()} }

func (r *real) Now() time.Time      { return time.Now() }
func (r *real) Mono() time.Duration { return time.Since(r.start) }

// Fake 供测试确定性推进，两只钟可分别拨动。
type Fake struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func NewFake(wall time.Time) *Fake { return &Fake{wall: wall, mono: time.Hour} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wall
}

func (f *Fake) Mono() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mono
}

// Advance 同时推进两只钟，模拟正常流逝。
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wall = f.wall.Add(d)
	f.mono += d
}

// SetWall 只拨墙钟，模拟 NTP 调整；单调钟不动。
func (f *Fake) SetWall(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wall = t
}
