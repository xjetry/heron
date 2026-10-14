package prober

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xjetry/heron/internal/probelimit"
	"google.golang.org/protobuf/proto"
)

func TestQueueTakesBoundedOldestBatch(t *testing.T) {
	const limit = probelimit.MaxResultsPerReport
	q := NewQueue(limit + 3)
	q.Push(Result{TaskID: 9999, At: -2 * time.Minute})
	for id := 1; id <= limit+2; id++ {
		q.Push(Result{TaskID: uint64(id)})
	}
	for round, want := range []int{limit, 2} {
		got := q.Take(time.Second, time.Minute, limit)
		if len(got) != want {
			t.Fatalf("round %d: count=%d want=%d", round, len(got), want)
		}
		for i, r := range got {
			if r.TaskID != uint64(round*limit+i+1) {
				t.Errorf("round %d index %d id=%d", round, i, r.TaskID)
			}
		}
	}
}

func TestToProtoBoundsErrorBytes(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"ascii", strings.Repeat("x", 200), strings.Repeat("x", 128)},
		{"rune", strings.Repeat("x", 127) + "界", strings.Repeat("x", 127)},
		{"invalid", "bad\xff", "bad\uFFFD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ToProto([]Result{{Outcome: Outcome{Err: tc.input}}}, 0)[0]
			got := r.GetError().GetMessage()
			if got != tc.want || len(got) > probelimit.MaxErrorMessageLen || !utf8.ValidString(got) {
				t.Fatalf("error message=%q bytes=%d want=%q", got, len(got), tc.want)
			}
			if _, err := proto.Marshal(r); err != nil {
				t.Fatal(err)
			}
		})
	}
}
