package clock

import (
	"testing"
	"time"
)

func TestDriftDetectsClockJumps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		adjust   func(f *Fake)
		wantJump bool
	}{
		{"wall forward 25s", func(f *Fake) { f.SetWall(f.Now().Add(25 * time.Second)) }, true},
		{"both advance 25s", func(f *Fake) { f.Advance(25 * time.Second) }, false},
		{"wall backward 25s", func(f *Fake) { f.SetWall(f.Now().Add(-25 * time.Second)) }, true},
		{"wall forward 19s", func(f *Fake) { f.SetWall(f.Now().Add(19 * time.Second)) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			var d Drift
			if _, _, jumped := d.Observe(f.Now(), f.Mono()); jumped {
				t.Fatal("first observation establishes the baseline and must not report a jump")
			}
			tc.adjust(f)
			if _, _, jumped := d.Observe(f.Now(), f.Mono()); jumped != tc.wantJump {
				t.Fatalf("jumped = %v, want %v", jumped, tc.wantJump)
			}
		})
	}
}

func TestDriftReportsBothDeltas(t *testing.T) {
	f := NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var d Drift
	d.Observe(f.Now(), f.Mono())
	f.Advance(10 * time.Second)
	f.SetWall(f.Now().Add(40 * time.Second))
	wallDelta, monoDelta, jumped := d.Observe(f.Now(), f.Mono())
	if wallDelta != 50*time.Second || monoDelta != 10*time.Second || !jumped {
		t.Fatalf("deltas = %v/%v jumped %v, want 50s/10s true", wallDelta, monoDelta, jumped)
	}
	// 跳变后基线已更新：两钟同步流逝不再报跳变。
	f.Advance(10 * time.Second)
	if _, _, jumped := d.Observe(f.Now(), f.Mono()); jumped {
		t.Fatal("baseline must move to the post-jump pair; steady flow is not a jump")
	}
}
