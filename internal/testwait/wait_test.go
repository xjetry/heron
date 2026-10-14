package testwait

import (
	"testing"
	"time"
)

func TestUntilReturnsWhenConditionHolds(t *testing.T) {
	n := 0
	Until(t, time.Millisecond, func() bool {
		n++
		return n == 3
	}, "Until returned false for a condition that became true")
	if n != 3 {
		t.Fatalf("polls = %d, want 3", n)
	}
}
