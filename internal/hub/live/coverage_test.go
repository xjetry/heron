package live

import (
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
)

func coverageLive(t *testing.T) (*Live, *clock.Fake, int64) {
	t.Helper()
	base := int64(1200)
	clk := clock.NewFake(time.Unix(base-1, 0))
	l := New(clk, time.Minute)
	l.SetReceiving(true)
	l.Observe(1, "source", &heronv1.Metrics{})
	return l, clk, base
}

func beatUntil(l *Live, clk *clock.Fake, end time.Time) {
	for clk.Now().Before(end) {
		d := min(ObservationInterval, end.Sub(clk.Now()))
		clk.Advance(d)
		l.ObservationHeartbeat()
	}
}

func atMinute(b metric.Batch, ts int64) (metric.Row, bool) {
	for _, r := range b.Rows {
		if r.NodeID == 1 && r.TS == ts {
			return r, true
		}
	}
	return metric.Row{}, false
}

func TestObservationTenMinutesAndOfflineRows(t *testing.T) {
	for _, report := range []bool{false, true} {
		t.Run(map[bool]string{false: "offline", true: "reported"}[report], func(t *testing.T) {
			l, clk, base := coverageLive(t)
			l.Flush()
			for n := int64(0); n < 10; n++ {
				if report {
					beatUntil(l, clk, time.Unix(base+n*60+1, 0))
					l.Observe(1, "source", &heronv1.Metrics{})
				}
				beatUntil(l, clk, time.Unix(base+(n+1)*60, 500000000))
				b := l.Flush()
				r, ok := atMinute(b, base+n*60)
				if !ok || !r.Observed || r.ObservationOnly == report || r.CoverageStart != base-60 {
					t.Fatalf("minute %d row=%+v present=%v", n, r, ok)
				}
				if !report && (!r.LastSeen.IsZero() || r.Source != "") {
					t.Fatalf("pure observation carried last_seen/source: %+v", r)
				}
			}
		})
	}
}

func TestObservationGapPhaseAndClockJumps(t *testing.T) {
	t.Run("repeated rollback into first 25 seconds", func(t *testing.T) {
		l, clk, base := coverageLive(t)
		for i := 0; i < 6; i++ {
			beatUntil(l, clk, time.Unix(base+25, 0))
			clk.SetWall(time.Unix(base, 0))
			l.ObservationHeartbeat()
		}
		beatUntil(l, clk, time.Unix(base+60, 0))
		if r, ok := atMinute(l.Flush(), base); ok && r.Observed {
			t.Fatal("repeated rollback observed", r)
		}
	})
	for _, gap := range []time.Duration{5 * time.Second, 5001 * time.Millisecond} {
		for phase := time.Duration(0); phase < 5*time.Second; phase += 500 * time.Millisecond {
			t.Run(gap.String()+"/"+phase.String(), func(t *testing.T) {
				l, clk, base := coverageLive(t)
				beatUntil(l, clk, time.Unix(base+20, 0).Add(phase))
				clk.Advance(gap)
				l.ObservationHeartbeat()
				beatUntil(l, clk, time.Unix(base+60, 0))
				_, ok := atMinute(l.Flush(), base)
				if ok != (gap <= 5*time.Second) {
					t.Fatalf("gap=%v phase=%v observed=%v", gap, phase, ok)
				}
			})
		}
	}
	for _, jump := range []time.Duration{-25 * time.Second, 25 * time.Second} {
		t.Run(jump.String(), func(t *testing.T) {
			l, clk, base := coverageLive(t)
			beatUntil(l, clk, time.Unix(base+40, 0))
			clk.SetWall(clk.Now().Add(jump))
			l.ObservationHeartbeat()
			beatUntil(l, clk, time.Unix(base+120, 0))
			if r, ok := atMinute(l.Flush(), base); ok && r.Observed {
				t.Fatal("clock jump observed", r)
			}
		})
	}
}

func TestObservationMidMinuteStartAndNeverReported(t *testing.T) {
	clk := clock.NewFake(time.Unix(1230, 0))
	l := New(clk, time.Minute)
	l.SetReceiving(true)
	beatUntil(l, clk, time.Unix(1320, 0))
	if b := l.Flush(); len(b.Rows) != 0 {
		t.Fatal("never-reported nodes generated rows", b)
	}
	l.Observe(1, "source", &heronv1.Metrics{})
	beatUntil(l, clk, time.Unix(1380, 0))
	r, ok := atMinute(l.Flush(), 1320)
	if !ok || !r.Observed || r.CoverageStart != 1320 {
		t.Fatal("first report boundary", r, ok)
	}
	clk = clock.NewFake(time.Unix(1230, 0))
	l = New(clk, time.Minute)
	l.SetReceiving(true)
	l.Observe(1, "source", &heronv1.Metrics{})
	beatUntil(l, clk, time.Unix(1260, 0))
	r, ok = atMinute(l.Flush(), 1200)
	if !ok || r.Observed || r.CoverageStart != 1200 {
		t.Fatal("partial startup minute observed", r, ok)
	}
}

func TestObservationSealAndIrrevocableDecision(t *testing.T) {
	t.Run("seal closes before heartbeat", func(t *testing.T) {
		l, clk, base := coverageLive(t)
		beatUntil(l, clk, time.Unix(base+58, 0))
		clk.Advance(2500 * time.Millisecond)
		if r, ok := atMinute(l.Flush(), base); !ok || !r.Observed {
			t.Fatal("seal did not close", r, ok)
		}
	})
	t.Run("jump after final heartbeat", func(t *testing.T) {
		l, clk, base := coverageLive(t)
		beatUntil(l, clk, time.Unix(base+60, 0))
		clk.SetWall(clk.Now().Add(3 * time.Second))
		if r, ok := atMinute(l.Flush(), base); ok && r.Observed {
			t.Fatal("seal missed jump", r)
		}
	})
	t.Run("rollback cannot reopen", func(t *testing.T) {
		l, clk, base := coverageLive(t)
		beatUntil(l, clk, time.Unix(base+60, 0))
		l.Flush()
		clk.SetWall(time.Unix(base+10, 0))
		l.Observe(1, "source", &heronv1.Metrics{})
		beatUntil(l, clk, time.Unix(base+120, 0))
		r, ok := atMinute(l.Flush(), base)
		if !ok || r.Observed || r.ObservationOnly {
			t.Fatal("reopened decision", r, ok)
		}
		l.SetReceiving(false)
		clk.SetWall(time.Unix(base-1, 0))
		l.SetReceiving(true)
		beatUntil(l, clk, time.Unix(base+60, 0))
		if r, ok := atMinute(l.Flush(), base); ok {
			t.Fatal("reception restart reopened finalized minute", r)
		}
	})
	t.Run("shutdown minute and drain", func(t *testing.T) {
		l, clk, base := coverageLive(t)
		beatUntil(l, clk, time.Unix(base+60, 0))
		clk.Advance(time.Second)
		l.SetReceiving(false)
		b := l.Drain()
		if r, ok := atMinute(b, base); !ok || !r.Observed {
			t.Fatal("closed minute missing from drain", r, ok)
		}
		if r, ok := atMinute(b, base+60); ok && r.Observed {
			t.Fatal("shutdown minute observed", r)
		}
	})
	t.Run("restart loads coverage not online state", func(t *testing.T) {
		l, clk, base := coverageLive(t)
		l.Forget(1)
		l.LoadCoverage(map[int64]int64{1: base - 600})
		beatUntil(l, clk, time.Unix(base+60, 0))
		b := l.Flush()
		if r, ok := atMinute(b, base); !ok || !r.ObservationOnly || r.CoverageStart != base-600 {
			t.Fatal(r, ok)
		}
		if l.Online(1) {
			t.Fatal("loading coverage marked node online")
		}
		for _, r := range b.Rows {
			if r.NodeID != 1 {
				t.Fatal("never-reported node observed")
			}
		}
	})
}
