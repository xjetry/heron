package clock

import (
	"testing"
	"time"
)

func TestFakeAdvanceMovesBothClocks(t *testing.T) {
	f := NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m0, w0 := f.Mono(), f.Now()
	f.Advance(3 * time.Second)
	if f.Mono()-m0 != 3*time.Second {
		t.Fatalf("mono advanced %v, want 3s", f.Mono()-m0)
	}
	if f.Now().Sub(w0) != 3*time.Second {
		t.Fatalf("wall advanced %v, want 3s", f.Now().Sub(w0))
	}
}

func TestFakeSetWallLeavesMonoAlone(t *testing.T) {
	f := NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	m0 := f.Mono()
	f.SetWall(f.Now().Add(-time.Hour))
	if f.Mono() != m0 {
		t.Fatalf("mono moved to %v after wall was set back", f.Mono())
	}
}

func TestRealMonoNeverDecreases(t *testing.T) {
	c := Real()
	a := c.Mono()
	b := c.Mono()
	if b < a {
		t.Fatalf("mono went backwards: %v then %v", a, b)
	}
}
