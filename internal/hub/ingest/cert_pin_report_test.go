package ingest

import (
	"bytes"
	"context"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

func TestPinnedTaskDeliveredWithoutDeclaration(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	pin := bytes.Repeat([]byte{7}, 32)
	d, _, err := h.reg.Save(context.Background(), &heronv1.ProbeTask{
		Kind: heronv1.ProbeKind_PROBE_KIND_HTTP, Target: "https://pinned.example/", IntervalS: 30, TimeoutMs: 1000,
	}, store.NodeSelector{NodeIDs: []int64{id}}, store.WithCertPin(pin))
	if err != nil {
		t.Fatal(err)
	}
	req := report(tok, &heronv1.Metrics{})

	resp, err := h.client.Report(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if tasks := resp.Msg.GetTasks().GetTasks(); len(tasks) != 1 || tasks[0].Id != d.Task.Id {
		t.Fatalf("pinned task not delivered: %v", tasks)
	}
	h.clk.Advance(time.Minute)
	result := rtt(d.Task.Id, 0, 1000)
	result.CertNotAfterS = proto.Int64(1_900_000_000)
	result.TaskConfigId = d.Task.ConfigId
	req = report(tok, &heronv1.Metrics{})
	req.Msg.ProbeResults = []*heronv1.ProbeResult{result}
	if _, err := h.client.Report(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	h.svc.Flush(t.Context(), true)
	rows := readProbeMinutes(t, h, id)
	if len(rows) != 1 || rows[0].TaskID != d.Task.Id || rows[0].Bucket.RttN != 1 {
		t.Fatalf("pinned result not persisted: %+v", rows)
	}
}

func TestDigestMismatchResendsWhenVersionMatches(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	task := h.task(t, id)
	current := h.reg.TasksFor(id)
	if current.GetVersion() == 0 || len(current.Tasks) != 1 || current.Tasks[0].GetId() != task {
		t.Fatalf("tasks = %v", current)
	}
	same, err := agentwire.TasksDigest(current.Tasks)
	if err != nil {
		t.Fatal(err)
	}
	req := report(tok, &heronv1.Metrics{})

	req.Msg.TasksDigest = same

	resp, err := h.client.Report(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Tasks != nil {
		t.Fatalf("matching digest resent tasks: %v", resp.Msg.Tasks)
	}
	h.clk.Advance(time.Minute)
	stale := proto.Clone(current).(*heronv1.ProbeTasks)
	stale.Tasks[0].ConfigId = bytes.Repeat([]byte{9}, 16)
	wrong, err := agentwire.TasksDigest(stale.Tasks)
	if err != nil {
		t.Fatal(err)
	}
	req = report(tok, &heronv1.Metrics{})

	req.Msg.TasksDigest = wrong

	resp, err = h.client.Report(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(resp.Msg.Tasks, current) {
		t.Fatalf("same version but different config digest: got %v want %v", resp.Msg.Tasks, current)
	}
}

// countingTasks 记下 TasksFor 被调用的次数：清单只在需要下发或缓存失效时才取。
type countingTasks struct {
	TaskSource
	tasksFor int
}

func (c *countingTasks) TasksFor(nodeID int64) *heronv1.ProbeTasks {
	c.tasksFor++
	return c.TaskSource.TasksFor(nodeID)
}

// 稳态上报的摘要与缓存相等：不取清单、不克隆任务。注册表版本一变，缓存失效，重新取清单并按摘要决定下发。
func TestMatchingDigestSkipsTheTaskList(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	h.task(t, id)
	counting := &countingTasks{TaskSource: h.svc.tasks}
	h.svc.tasks = counting
	current := h.reg.TasksFor(id)
	digest, err := agentwire.TasksDigest(current.Tasks)
	if err != nil {
		t.Fatal(err)
	}
	send := func() *heronv1.ReportResponse {
		t.Helper()
		h.clk.Advance(time.Minute)
		req := report(tok, &heronv1.Metrics{})

		req.Msg.TasksDigest = digest

		resp, err := h.client.Report(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg
	}
	if resp := send(); resp.Tasks != nil || counting.tasksFor != 1 {
		t.Fatalf("first report: tasks=%v, TasksFor calls=%d, want no tasks and one call to fill the cache", resp.Tasks, counting.tasksFor)
	}
	if resp := send(); resp.Tasks != nil || counting.tasksFor != 1 {
		t.Fatalf("steady report: tasks=%v, TasksFor calls=%d, want no tasks and no new call", resp.Tasks, counting.tasksFor)
	}
	added := h.task(t, id)
	resp := send()
	if counting.tasksFor != 2 || len(resp.Tasks.GetTasks()) != 2 || resp.Tasks.GetTasks()[1].GetId() != added {
		t.Fatalf("after a version bump: tasks=%v, TasksFor calls=%d, want the new list and one more call", resp.Tasks, counting.tasksFor)
	}
}
