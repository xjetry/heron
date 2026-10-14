package live

import (
	"context"
	"time"
)

const ObservationInterval = 2500 * time.Millisecond

type reading struct {
	wall time.Time
	mono time.Duration
}

type observation struct {
	enabled        bool
	opened, closed time.Time
	readings       []reading
	finalized      int64
	hasFinalized   bool
}

// LoadCoverage 只恢复覆盖起点，不建立在线条目；离线节点也需要后续的纯观测行。
func (l *Live) LoadCoverage(starts map[int64]int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.coverage = make(map[int64]int64, len(starts))
	for id, start := range starts {
		l.coverage[id] = start
	}
}

// SetReceiving 的调用方与 HTTP 准入边界同步；构造 Live 不代表已经开始接收。
func (l *Live) SetReceiving(enabled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.observation.enabled == enabled {
		return
	}
	r := l.readClock()
	o := &l.observation
	if enabled {
		o.opened, o.closed = r.wall, time.Time{}
		o.readings = []reading{r}
	} else {
		o.closed = r.wall
		o.readings = append(o.readings, r)
	}
	o.enabled = enabled
}

func (l *Live) readClock() reading { return reading{wall: l.clk.Now(), mono: l.clk.Mono()} }

func (l *Live) ObservationHeartbeat() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.observation.enabled {
		l.observation.readings = append(l.observation.readings, l.readClock())
	}
}

// time.Ticker 以单调钟调度；实际读数取注入时钟，不把调度目标当成真的执行时刻。
func (l *Live) RunObservation(ctx context.Context) {
	ticker := time.NewTicker(ObservationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.ObservationHeartbeat()
		}
	}
}

// sealObservations 只在 take 持锁时调用。封口读数参与全部尚未定案分钟的间隙与跳变检查；
// 判定后保留下轮左边界之前最后一个读数及之后的读数，finalized 单调前进，回拨不会重开已定案分钟。
func (l *Live) sealObservations(seal reading) map[int64]bool {
	out := map[int64]bool{}
	o := &l.observation
	if o.opened.IsZero() {
		return out
	}
	o.readings = append(o.readings, seal)
	end := minuteOf(seal.wall)
	begin := minuteOf(o.opened) + 60
	if o.hasFinalized {
		begin = max(begin, o.finalized+60)
	}
	for ts := begin; ts < end; ts += 60 {
		if o.covers(ts) {
			out[ts] = true
		}
	}
	if !o.hasFinalized || end-60 > o.finalized {
		o.finalized = end - 60
		o.hasFinalized = true
		keep := 0
		boundary := time.Unix(end, 0)
		for i, r := range o.readings {
			if r.wall.After(boundary) {
				break
			}
			keep = i
		}
		o.readings = append([]reading(nil), o.readings[keep:]...)
	}
	return out
}

func (o *observation) covers(ts int64) bool {
	start, end := time.Unix(ts, 0), time.Unix(ts+60, 0)
	if !o.opened.Before(start) || (!o.closed.IsZero() && !o.closed.After(end)) {
		return false
	}
	// 左边界取按采样次序第一次越过分钟起点之前的读数。其后的任何跳变都不能
	// 通过挑选回拨后的另一段读数绕开，直到 take 定案并丢掉这段证据为止。
	first := -1
	for i, r := range o.readings {
		if r.wall.After(start) {
			break
		}
		first = i
	}
	if first < 0 || o.readings[len(o.readings)-1].wall.Before(end) {
		return false
	}
	for i := first + 1; i < len(o.readings); i++ {
		a, b := o.readings[i-1], o.readings[i]
		mono := b.mono - a.mono
		wall := b.wall.Sub(a.wall)
		delta := wall - mono
		if mono < 0 || mono > 5*time.Second || delta > 2*time.Second || delta < -2*time.Second {
			return false
		}
	}
	return true
}
