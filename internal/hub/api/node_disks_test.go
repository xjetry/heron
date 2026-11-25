package api

import (
	"bytes"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

// 逐盘容量经 GetNodeDisks 给管理端；挂载路径不进公开快照，也不进管理端的可压缩快照（它们是 agent 自报的任意文本）。
// 指定节点的 token 只读得到范围内的节点。
func TestNodeDisksAdminOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, token := h.createNode(t, "disks")
	h.setPublic(t, id, "disks", true)
	other, _ := h.createNode(t, "other")
	const secretPath = "/srv/private-volume-name"
	mounts := []*heronv1.DiskMountUsage{{Path: secretPath, Total: 1000, Used: 900}, {Path: "/", Total: 1000, Used: 500}}
	req := connect.NewRequest(&heronv1.ReportRequest{Metrics: &heronv1.Metrics{BootId: testBootID, DiskTotal: proto.Uint64(1000), DiskUsed: proto.Uint64(500)}, DiskMounts: mounts, DiskMountsTotal: 2})
	req.Header().Set("Authorization", "Bearer "+token)
	h.clk.Advance(time.Second)
	if _, err := h.agent.Report(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	arrived := h.clk.Now().Unix()

	got, err := h.admin.GetNodeDisks(t.Context(), connect.NewRequest(&heronv1.GetNodeDisksRequest{NodeId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetTotal() != 2 || len(got.Msg.GetMounts()) != 2 || !proto.Equal(got.Msg.GetMounts()[0], mounts[0]) || got.Msg.SampledAt == nil || got.Msg.GetSampledAt() != arrived || !got.Msg.GetOnline() {
		t.Fatalf("GetNodeDisks = %v, want the reported list sampled at %d", got.Msg, arrived)
	}
	// 从未上报的节点：没有读数，也没有采样时刻。
	empty, err := h.admin.GetNodeDisks(t.Context(), connect.NewRequest(&heronv1.GetNodeDisksRequest{NodeId: other}))
	if err != nil || len(empty.Msg.GetMounts()) != 0 || empty.Msg.SampledAt != nil || empty.Msg.GetOnline() {
		t.Fatalf("never reported node: %v %v", empty.Msg, err)
	}
	if _, err := h.admin.GetNodeDisks(t.Context(), connect.NewRequest(&heronv1.GetNodeDisksRequest{NodeId: 9999})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing node err = %v, want NotFound", err)
	}

	scoped, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{other}})
	if _, err := scoped.GetNodeDisks(t.Context(), connect.NewRequest(&heronv1.GetNodeDisksRequest{NodeId: other})); err != nil {
		t.Fatalf("scoped token reading its own node: %v", err)
	}
	if _, err := scoped.GetNodeDisks(t.Context(), connect.NewRequest(&heronv1.GetNodeDisksRequest{NodeId: id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("scoped token reading a node outside its scope: err = %v, want NotFound", err)
	}

	public := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	if !bytes.Contains(public.body, []byte(`"name":"disks"`)) {
		t.Fatalf("public positive control missing: %s", public.body)
	}
	if bytes.Contains(public.body, []byte("private-volume-name")) {
		t.Fatalf("mount path leaked into the public snapshot: %s", public.body)
	}
	snap, err := h.admin.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(snap.Msg)
	if err != nil || len(snap.Msg.GetNodes()) == 0 || bytes.Contains(raw, []byte("private-volume-name")) {
		t.Fatalf("mount path reached the compressible admin snapshot (err %v)", err)
	}
}
