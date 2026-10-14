package prober

import (
	"bytes"
	"context"
	"crypto/sha256"
	"math"
	"reflect"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func TestQueueDropsOldestAndExpired(t *testing.T) {
	q := NewQueue(3)
	for i := uint64(1); i <= 4; i++ {
		q.Push(Result{TaskID: i, At: time.Duration(i) * time.Second})
	}
	want := []Result{{TaskID: 2, At: 2 * time.Second}, {TaskID: 3, At: 3 * time.Second}, {TaskID: 4, At: 4 * time.Second}}
	if got := q.Take(4*time.Second, 120*time.Second, 1024); !reflect.DeepEqual(got, want) || q.Dropped() != 1 {
		t.Fatalf("results=%v dropped=%d want=%v/1", got, q.Dropped(), want)
	}
	if got := q.Take(4*time.Second, 120*time.Second, 1024); len(got) != 0 {
		t.Fatalf("taken results remained: %v", got)
	}
	q.Push(Result{TaskID: 1, At: 0})
	q.Push(Result{TaskID: 2, At: time.Second})
	if got := q.Take(121*time.Second, 120*time.Second, 1024); !reflect.DeepEqual(got, []Result{{TaskID: 2, At: time.Second}}) || q.Dropped() != 2 {
		t.Fatalf("age boundary results=%v dropped=%d", got, q.Dropped())
	}
}

func TestQueueOrdersByCompletionTime(t *testing.T) {
	for _, capacity := range []int{3, 2} {
		q := NewQueue(capacity)
		for _, at := range []time.Duration{3, 1, 2} {
			q.Push(Result{TaskID: uint64(at), At: at * time.Second})
		}
		want := []Result{{TaskID: 1, At: time.Second}, {TaskID: 2, At: 2 * time.Second}, {TaskID: 3, At: 3 * time.Second}}
		want = want[3-capacity:]
		if got := q.Take(3*time.Second, time.Minute, 1024); !reflect.DeepEqual(got, want) || q.Dropped() != uint64(3-capacity) {
			t.Errorf("capacity=%d ordered results=%v dropped=%d want=%v", capacity, got, q.Dropped(), want)
		}
	}
}

func TestQueueClearDiscardsEverythingAndCounts(t *testing.T) {
	q := NewQueue(3)
	for i := uint64(1); i <= 3; i++ {
		q.Push(Result{TaskID: i, At: time.Duration(i) * time.Second})
	}
	q.Clear()
	if got := q.Take(3*time.Second, 120*time.Second, 1024); len(got) != 0 || q.Dropped() != 3 {
		t.Fatalf("after Clear results=%v dropped=%d, want empty/3", got, q.Dropped())
	}
	q.Push(Result{TaskID: 9, At: 4 * time.Second})
	if got := q.Take(4*time.Second, 120*time.Second, 1024); !reflect.DeepEqual(got, []Result{{TaskID: 9, At: 4 * time.Second}}) || q.Dropped() != 3 {
		t.Fatalf("post-Clear push results=%v dropped=%d", got, q.Dropped())
	}
	q.Push(Result{TaskID: 10, At: 5 * time.Second})
	q.Clear()
	if got := q.Take(5*time.Second, 120*time.Second, 1024); len(got) != 0 || q.Dropped() != 4 {
		t.Fatalf("second Clear results=%v dropped=%d, want empty/4", got, q.Dropped())
	}
}

func TestRequeueMergesDelayedPushByCompletionTime(t *testing.T) {
	for _, capacity := range []int{3, 2} {
		q := NewQueue(capacity)
		q.Push(Result{TaskID: 2, At: 2 * time.Second})
		old := q.Take(2*time.Second, time.Minute, 1024)
		q.Push(Result{TaskID: 1, At: time.Second})
		q.Push(Result{TaskID: 3, At: 3 * time.Second})
		q.Requeue(old)
		want := []Result{{TaskID: 1, At: time.Second}, {TaskID: 2, At: 2 * time.Second}, {TaskID: 3, At: 3 * time.Second}}
		want = want[3-capacity:]
		if got := q.Take(3*time.Second, time.Minute, 1024); !reflect.DeepEqual(got, want) || q.Dropped() != uint64(3-capacity) {
			t.Errorf("capacity=%d merged results=%v dropped=%d want=%v", capacity, got, q.Dropped(), want)
		}
	}
}

func TestQueueRequeuesBeforeNewResultsWithinCapacity(t *testing.T) {
	q := NewQueue(3)
	q.Push(Result{TaskID: 1})
	q.Push(Result{TaskID: 2})
	old := q.Take(0, time.Minute, 1024)
	q.Push(Result{TaskID: 3})
	q.Requeue(old)
	want := []Result{{TaskID: 1}, {TaskID: 2}, {TaskID: 3}}
	if got := q.Take(0, time.Minute, 1024); !reflect.DeepEqual(got, want) {
		t.Fatalf("requeue order=%v want=%v", got, want)
	}
	q.Push(Result{TaskID: 3})
	q.Push(Result{TaskID: 4})
	q.Requeue(old)
	want = []Result{{TaskID: 2}, {TaskID: 3}, {TaskID: 4}}
	if got := q.Take(0, time.Minute, 1024); !reflect.DeepEqual(got, want) || q.Dropped() != 1 {
		t.Fatalf("requeue overflow=%v dropped=%d", got, q.Dropped())
	}
}

func TestQueueCapacityBoundaries(t *testing.T) {
	q := NewQueue(0)
	q.Push(Result{TaskID: 1})
	if got := q.Take(0, time.Minute, 1024); len(got) != 0 || q.Dropped() != 1 {
		t.Fatalf("zero capacity results=%v dropped=%d", got, q.Dropped())
	}
	defer func() {
		if recover() == nil {
			t.Error("negative capacity accepted")
		}
	}()
	NewQueue(-1)
}

func TestToProtoMapsAgeAndOutcomes(t *testing.T) {
	rs := []Result{
		{TaskID: 1, At: time.Second, Outcome: Outcome{RttUs: 0}},
		{TaskID: 2, At: 1500 * time.Millisecond, Outcome: Outcome{Timeout: true}},
		{TaskID: 3, At: 1999 * time.Millisecond, Outcome: Outcome{Err: "socket denied"}},
	}
	got := ToProto(rs, 2*time.Second)
	want := []*heronv1.ProbeResult{
		{TaskId: 1, AgeMs: 1000, Outcome: &heronv1.ProbeResult_RttUs{RttUs: 0}},
		{TaskId: 2, AgeMs: 500, Outcome: &heronv1.ProbeResult_Timeout{Timeout: &heronv1.Timeout{}}},
		{TaskId: 3, AgeMs: 1, Outcome: &heronv1.ProbeResult_Error{Error: &heronv1.ProbeError{Message: "socket denied"}}},
	}
	if len(got) != len(want) {
		t.Fatalf("results=%v", got)
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("result=%v want=%v", got[i], want[i])
		}
	}
}

type engineFunc func(context.Context, *heronv1.ProbeTask) Outcome

func TestToProtoClampsAge(t *testing.T) {
	got := ToProto([]Result{{At: time.Second}, {At: -(time.Duration(math.MaxUint32) + 1) * time.Millisecond}}, 0)
	if got[0].AgeMs != 0 || got[1].AgeMs != math.MaxUint32 {
		t.Fatalf("clamped ages=%d/%d", got[0].AgeMs, got[1].AgeMs)
	}
}

func (f engineFunc) Probe(ctx context.Context, task *heronv1.ProbeTask) Outcome { return f(ctx, task) }

func TestMultiDispatchesByKind(t *testing.T) {
	m := Multi{
		ICMP: engineFunc(func(context.Context, *heronv1.ProbeTask) Outcome { return Outcome{RttUs: 11} }),
		TCP:  engineFunc(func(context.Context, *heronv1.ProbeTask) Outcome { return Outcome{RttUs: 22} }),
	}
	for _, tc := range []struct {
		kind heronv1.ProbeKind
		want Outcome
	}{
		{heronv1.ProbeKind_PROBE_KIND_ICMP, Outcome{RttUs: 11}},
		{heronv1.ProbeKind_PROBE_KIND_TCP, Outcome{RttUs: 22}},
		{heronv1.ProbeKind_PROBE_KIND_UNSPECIFIED, Outcome{Err: "unsupported probe kind PROBE_KIND_UNSPECIFIED"}},
	} {
		if got := m.Probe(t.Context(), &heronv1.ProbeTask{Kind: tc.kind}); got != tc.want {
			t.Errorf("kind=%v got=%v want=%v", tc.kind, got, tc.want)
		}
	}
}

// 每条结果原样回显产生它时的任务身份（空身份 = 字段缺席）；候选只随 timeout 结果编码。
func TestToProtoEchoesConfigIDAndPresented(t *testing.T) {
	cfg := bytes.Repeat([]byte{7}, 16)
	candidate := &PresentedCert{SPKI: sha256.Sum256([]byte("leaf")), NotAfterS: 1893456000, Reason: heronv1.PresentedReason_PRESENTED_REASON_PIN_MISMATCH}
	rs := ToProto([]Result{
		{TaskID: 1, Outcome: Outcome{RttUs: 100}, ConfigID: cfg},
		{TaskID: 2, Outcome: Outcome{Timeout: true}},
		{TaskID: 3, Outcome: Outcome{Timeout: true, Presented: candidate}, ConfigID: cfg},
		{TaskID: 4, Outcome: Outcome{Err: "boom"}, ConfigID: cfg},
		{TaskID: 5, Outcome: Outcome{RttUs: 100, Presented: candidate}}, // 成功结果不带候选，即使引擎误填
	}, time.Second)
	if !bytes.Equal(rs[0].GetTaskConfigId(), cfg) {
		t.Fatalf("rtt result TaskConfigId = %x, want %x", rs[0].GetTaskConfigId(), cfg)
	}
	if rs[1].TaskConfigId != nil {
		t.Fatalf("empty identity encoded as %x, want absent", rs[1].TaskConfigId)
	}
	presented := rs[2].GetPresented()
	if presented == nil || !bytes.Equal(presented.GetSpkiSha256(), candidate.SPKI[:]) ||
		presented.GetNotAfterS() != candidate.NotAfterS || presented.GetReason() != candidate.Reason {
		t.Fatalf("timeout result Presented = %v, want %v", presented, candidate)
	}
	if !bytes.Equal(rs[2].GetTaskConfigId(), cfg) {
		t.Fatalf("timeout result TaskConfigId = %x, want %x", rs[2].GetTaskConfigId(), cfg)
	}
	if !bytes.Equal(rs[3].GetTaskConfigId(), cfg) {
		t.Fatalf("error result TaskConfigId = %x, want %x", rs[3].GetTaskConfigId(), cfg)
	}
	for _, i := range []int{0, 1, 3, 4} {
		if rs[i].Presented != nil {
			t.Fatalf("result %d (%T) carries presented, want nil", i+1, rs[i].Outcome)
		}
	}
}
