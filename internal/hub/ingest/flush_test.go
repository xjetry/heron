package ingest

import (
	"testing"
	"time"
)

func TestNextFlushAtLandsHalfSecondPastTheNextMinute(t *testing.T) {
	const layout = "15:04:05.000"
	for _, tc := range []struct{ wall, next string }{
		{"12:00:00.000", "12:01:00.500"},
		{"12:00:00.499", "12:01:00.500"},
		{"12:00:00.500", "12:01:00.500"},
		{"12:00:59.999", "12:01:00.500"},
		{"12:01:00.500", "12:02:00.500"},
	} {
		t.Run(tc.wall, func(t *testing.T) {
			wall, err := time.Parse(layout, tc.wall)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(layout, tc.next)
			if err != nil {
				t.Fatal(err)
			}
			next := nextFlushAt(wall)
			if !next.Equal(want) {
				t.Errorf("next = %s, want %s", next.Format(layout), tc.next)
			}
			if !next.After(wall) {
				t.Errorf("next %v must be after wall %v", next, wall)
			}
		})
	}
}
