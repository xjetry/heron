package agentwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func digestTask(id uint64, target string) *heronv1.ProbeTask {
	return &heronv1.ProbeTask{Id: id, Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: target, IntervalS: 5, TimeoutMs: 1000}
}

// 对照实现：严格按契约（升序、确定性编码、8 字节大端长度前缀、SHA-256）逐步手写。
func wantDigest(t *testing.T, tasks ...*heronv1.ProbeTask) []byte {
	t.Helper()
	h := sha256.New()
	var size [8]byte
	for _, task := range tasks {
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		binary.BigEndian.PutUint64(size[:], uint64(len(b)))
		h.Write(size[:])
		h.Write(b)
	}
	return h.Sum(nil)
}

func mustDigest(t *testing.T, tasks []*heronv1.ProbeTask) []byte {
	t.Helper()
	d, err := TasksDigest(tasks)
	if err != nil {
		t.Fatalf("TasksDigest: %v", err)
	}
	return d
}

func TestTasksDigestEmptyIsSHA256OfEmptyString(t *testing.T) {
	empty := sha256.Sum256(nil)
	if got := mustDigest(t, nil); !bytes.Equal(got, empty[:]) {
		t.Fatalf("TasksDigest(nil) = %x, want SHA-256 of empty string %x", got, empty)
	}
	if got := mustDigest(t, []*heronv1.ProbeTask{}); !bytes.Equal(got, empty[:]) {
		t.Fatalf("TasksDigest(empty) = %x, want %x", got, empty)
	}
}

func TestTasksDigestContract(t *testing.T) {
	tasks := []*heronv1.ProbeTask{digestTask(1, "a.example"), digestTask(3, "c.example"), digestTask(2, "b.example")}
	want := wantDigest(t, digestTask(1, "a.example"), digestTask(2, "b.example"), digestTask(3, "c.example"))
	if got := mustDigest(t, tasks); !bytes.Equal(got, want) {
		t.Fatalf("TasksDigest = %x, want contract encoding %x", got, want)
	}
	// 收到顺序不影响摘要。
	reversed := []*heronv1.ProbeTask{digestTask(3, "c.example"), digestTask(2, "b.example"), digestTask(1, "a.example")}
	if got := mustDigest(t, reversed); !bytes.Equal(got, want) {
		t.Fatalf("reordered digest = %x, want %x", got, want)
	}
	// 内容变化（含 config_id 与 pin）改变摘要。
	changed := digestTask(2, "b.example")
	changed.ConfigId = make([]byte, 16)
	if got := mustDigest(t, []*heronv1.ProbeTask{digestTask(1, "a.example"), changed, digestTask(3, "c.example")}); bytes.Equal(got, want) {
		t.Fatal("content change did not change digest")
	}
}

// 摘要是内容摘要：清单版本计数不参与，全局版本变了而清单不变时摘要相等。
func TestTasksDigestExcludesVersion(t *testing.T) {
	tasks := []*heronv1.ProbeTask{digestTask(1, "a.example"), digestTask(2, "b.example")}
	v1 := &heronv1.ProbeTasks{Version: 100, Tasks: tasks}
	v2 := &heronv1.ProbeTasks{Version: 200, Tasks: tasks}
	if got, want := mustDigest(t, v1.GetTasks()), mustDigest(t, v2.GetTasks()); !bytes.Equal(got, want) {
		t.Fatalf("version-only change altered digest: %x vs %x", got, want)
	}
}

// 摘要必须覆盖会被 agent 拒收的任务：被拒任务仍在持有的清单里，两侧摘要才可比。
func TestTasksDigestCoversRejectedTasks(t *testing.T) {
	bad := digestTask(9, "not a valid target at all:::")
	withBad := mustDigest(t, []*heronv1.ProbeTask{digestTask(1, "a.example"), bad})
	withoutBad := mustDigest(t, []*heronv1.ProbeTask{digestTask(1, "a.example")})
	if bytes.Equal(withBad, withoutBad) {
		t.Fatal("rejected task not covered by digest")
	}
}

// 编码失败（proto3 string 含非法 UTF-8）必须报错，不得吞掉：hub 侧的清单由存储行组装，
// 没有解码那一关。agent 侧收到这种清单时解码同样过不了，这里直接构造验证错误路径。
func TestTasksDigestReportsMarshalError(t *testing.T) {
	bad := digestTask(1, string([]byte{0xff, 0xfe}))
	if _, err := (proto.MarshalOptions{Deterministic: true}).Marshal(bad); err == nil {
		t.Fatal("premise broken: deterministic marshal accepted invalid UTF-8; the error path is dead code")
	}
	if _, err := TasksDigest([]*heronv1.ProbeTask{bad}); err == nil {
		t.Fatal("TasksDigest swallowed the marshal error")
	}
	// 好清单不受影响。
	if _, err := TasksDigest([]*heronv1.ProbeTask{digestTask(1, "a.example")}); err != nil {
		t.Fatalf("TasksDigest on valid list: %v", err)
	}
}
