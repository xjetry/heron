package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// 公开备注的准入：单行、至多 100 个码点、不含控制字符；超长拒绝而不截断，被拒的更新什么都不写。
func TestPublicRemarkAdmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	id, _ := h.createNode(t, "remarked")
	update := func(remark string) error {
		_, err := h.admin.UpdateNode(ctx, connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "remarked", PublicRemark: remark, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0)}))
		return err
	}
	if err := update(strings.Repeat("联", 100)); err != nil {
		t.Fatalf("100 characters rejected: %v", err)
	}
	if n, err := h.store.GetNode(ctx, id); err != nil || n.PublicRemark != strings.Repeat("联", 100) {
		t.Fatalf("remark not stored: %+v %v", n, err)
	}
	for name, remark := range map[string]string{
		"too long":       strings.Repeat("联", 101),
		"newline":        "联通\n4837",
		"control":        "联通\x004837",
		"bidi control":   "联通\u202e4837",
		"over both ends": strings.Repeat("a", 99) + "\n",
	} {
		if err := update(remark); codeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	// 超长被拒而不是被截断：库里仍是上一次接受的值。
	if n, err := h.store.GetNode(ctx, id); err != nil || n.PublicRemark != strings.Repeat("联", 100) {
		t.Fatalf("rejected update wrote: %+v %v", n, err)
	}
	// 空串合法，表示清除；下一次整体替换不带它也同样清掉。
	if err := update(""); err != nil {
		t.Fatalf("empty rejected: %v", err)
	}
	if n, err := h.store.GetNode(ctx, id); err != nil || n.PublicRemark != "" {
		t.Fatalf("empty remark did not clear: %+v %v", n, err)
	}
}

// 公开备注只随公开节点下发：管理端总是可见，公开快照只含公开节点，私有节点的备注不经公开端出现。
func TestPublicRemarkProjection(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	a, _ := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	for _, n := range []struct {
		id   int64
		name string
	}{{a, "a"}, {b, "b"}} {
		req := &heronv1.UpdateNodeRequest{Id: n.id, Name: n.name, Public: n.id == a, PublicRemark: "联通 4837", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0)}
		if _, err := h.admin.UpdateNode(ctx, connect.NewRequest(req)); err != nil {
			t.Fatal(err)
		}
	}
	// 管理端两个节点都回显公开备注。
	nodes, err := h.admin.ListNodes(ctx, connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes.Msg.Nodes {
		if node.GetPublicRemark() != "联通 4837" {
			t.Fatalf("admin node %d public_remark = %q", node.Id, node.GetPublicRemark())
		}
	}
	snap, err := h.publicClient().GetSnapshot(ctx, connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Msg.Nodes) != 1 || snap.Msg.Nodes[0].GetId() != a {
		t.Fatalf("snapshot nodes = %v, want only the public one", snap.Msg.Nodes)
	}
	if snap.Msg.Nodes[0].GetPublicRemark() != "联通 4837" {
		t.Fatalf("public node remark = %q", snap.Msg.Nodes[0].GetPublicRemark())
	}
}

// 只改公开备注的变更也要改变预期版本并留下审计差异：快照白名单带着 public_remark，before/after 因此不同。
func TestPublicRemarkExecuteChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "changed")
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{id}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	m := &heronv1.ExecuteChangeRequest{RequestId: "remark", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"public_remark"}},
		Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: id, PublicRemark: "移动 CMI"}}}
	p := previewChange(t, client, m)
	if !strings.Contains(p.Operation.AfterJson, `"public_remark":"移动 CMI"`) || !strings.Contains(p.Operation.BeforeJson, `"public_remark":""`) {
		t.Fatalf("preview did not describe the remark: before=%s after=%s", p.Operation.BeforeJson, p.Operation.AfterJson)
	}
	if n, err := h.store.GetNode(t.Context(), id); err != nil || n.PublicRemark != "" {
		t.Fatalf("preview changed database: %+v %v", n, err)
	}
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); err != nil {
		t.Fatal(err)
	}
	if n, err := h.store.GetNode(t.Context(), id); err != nil || n.PublicRemark != "移动 CMI" || n.Name != "changed" || n.TrafficResetDay != 1 {
		t.Fatalf("field mask lost fields: %+v %v", n, err)
	}
	// 再改一次备注，预期版本随之变化：快照摘要计入了 public_remark。
	next := &heronv1.ExecuteChangeRequest{RequestId: "remark-2", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"public_remark"}},
		Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: id, PublicRemark: "电信 163"}}}
	if p2 := previewChange(t, client, next); p2.ExpectedVersion == p.ExpectedVersion {
		t.Fatalf("remark-only change kept the expected version: %s", p2.ExpectedVersion)
	}
	// 旧版本执行被拒，库里的备注不变。
	old := &heronv1.ExecuteChangeRequest{RequestId: "remark-3", UpdateMask: m.UpdateMask, ExpectedVersion: p.ExpectedVersion,
		Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: id, PublicRemark: "stale"}}}
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(old)); codeOf(err) != connect.CodeAborted {
		t.Fatalf("stale version accepted: %v", err)
	}
	if n, err := h.store.GetNode(t.Context(), id); err != nil || n.PublicRemark != "移动 CMI" {
		t.Fatalf("stale write changed the remark: %+v %v", n, err)
	}
	// mask 缺席 = 保持：改别的字段的变更不得顺带清掉公开备注（merge 基底登记了它）。
	other := &heronv1.ExecuteChangeRequest{RequestId: "remark-4", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"note"}},
		Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: id, Note: "内部备注"}}}
	p3 := previewChange(t, client, other)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(&heronv1.ExecuteChangeRequest{RequestId: other.RequestId, UpdateMask: other.UpdateMask, ExpectedVersion: p3.ExpectedVersion, Change: other.Change})); err != nil {
		t.Fatal(err)
	}
	if n, err := h.store.GetNode(t.Context(), id); err != nil || n.PublicRemark != "移动 CMI" || n.Note != "内部备注" {
		t.Fatalf("note-only change lost the remark: %+v %v", n, err)
	}
}
