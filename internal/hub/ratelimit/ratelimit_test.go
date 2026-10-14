package ratelimit

import (
	"strings"
	"testing"
	"time"
)

func TestAllowSpendsCapacityThenRefillsPerKey(t *testing.T) {
	b := New[string](60, time.Second/10)
	for i := range 60 {
		if !b.Allow("a", 0) {
			t.Fatalf("request %d within capacity denied", i+1)
		}
	}
	if b.Allow("a", 0) {
		t.Fatal("request 61 at the same instant allowed")
	}
	if !b.Allow("b", 0) {
		t.Fatal("another key shares the bucket")
	}
	// 半个补充周期只补回半个令牌；两个半周期的和在浮点里恰为 1，边界不靠舍入。
	if b.Allow("a", 50*time.Millisecond) {
		t.Fatal("token refilled before refillPer elapsed")
	}
	if !b.Allow("a", 100*time.Millisecond) || b.Allow("a", 100*time.Millisecond) {
		t.Fatal("refillPer should restore exactly one token")
	}
	b.Forget("a")
	if !b.Allow("a", 100*time.Millisecond) {
		t.Fatal("forgotten key should start from a full bucket")
	}
}

func TestBucketsSweepIdleKeys(t *testing.T) {
	per := time.Second
	b := New[string](3, per)
	b.Allow("a", 0)
	b.Allow("b", 0)
	b.Allow("c", 3*per)
	if len(b.m) != 1 || b.m["c"] == nil {
		t.Fatalf("idle keys not swept: %+v", b.m)
	}
	b.Allow("active", 3*per)
	active := b.m["active"]
	b.Allow("active", 6*per-time.Millisecond)
	if b.lastSweep != 3*per || len(b.m) != 2 || b.m["active"] != active {
		t.Fatal("swept before period or replaced active bucket")
	}
	b.Allow("d", 6*per)
	if len(b.m) != 2 || b.m["active"] != active || b.m["d"] == nil {
		t.Fatalf("sweep removed active key or retained idle key: %+v", b.m)
	}
}

func TestNewRejectsDegenerateParameters(t *testing.T) {
	for _, c := range []struct {
		capacity int
		per      time.Duration
	}{{0, time.Second}, {1, 0}, {1, -time.Second}} {
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(r.(string), "capacity must be at least 1 and refillPer positive") {
					t.Errorf("New(%d, %v): recover() = %v", c.capacity, c.per, r)
				}
			}()
			New[string](c.capacity, c.per)
		}()
	}
}
