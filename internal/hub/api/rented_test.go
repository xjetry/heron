package api

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// 已出租随新建落库，回显在 Node 与公开快照上；UpdateNode 是整体替换，不带 rented 即清除。维护与已出租同真
// 被保存入口拒绝、一行不改，错误点名两个字段（两个开关各自有"为真"的含义，叠加起来没有）。
func TestRentedRoundTripAndExclusion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	created, err := h.admin.CreateNode(t.Context(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: "leased", Rented: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !created.Msg.GetNode().GetRented() {
		t.Fatalf("created node rented = false")
	}
	id := created.Msg.GetNode().GetId()
	if n := h.update(t, &heronv1.UpdateNodeRequest{Id: id, Name: "leased", Public: true, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), Rented: true}); !n.GetRented() {
		t.Fatalf("updated node rented = false: %v", n)
	}
	snap, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Msg.GetNodes()) != 1 || !snap.Msg.GetNodes()[0].GetRented() {
		t.Fatalf("public snapshot = %v", snap.Msg.GetNodes())
	}
	_, err = h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "leased", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), Rented: true, Maintenance: true}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "maintenance") || !strings.Contains(err.Error(), "rented") {
		t.Fatalf("both flags: err = %v, want InvalidArgument naming maintenance and rented", err)
	}
	list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || len(list.Msg.GetNodes()) != 1 || !list.Msg.GetNodes()[0].GetRented() {
		t.Fatalf("rejected update changed the node: %v %v", list, err)
	}
	if n := h.update(t, &heronv1.UpdateNodeRequest{Id: id, Name: "leased", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0)}); n.GetRented() {
		t.Fatalf("UpdateNode without rented should clear it: %v", n)
	}
}

// ExecuteChange 只带 rented 掩码时，合并基底带着库里的 maintenance：翻转后与它冲突，预览与执行撞同一处拒绝，
// 错误同样点名两个字段。基底也带库里的 rented：只改别的字段的执行不会把它清掉。
func TestExecuteChangeRentedConflictsWithStoredMaintenance(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "m")
	h.update(t, &heronv1.UpdateNodeRequest{Id: id, Name: "m", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), Maintenance: true})
	rentedOnly := &heronv1.ExecuteChangeRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rented"}},
		Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: id, Rented: true}}}
	preview := proto.Clone(rentedOnly).(*heronv1.ExecuteChangeRequest)
	preview.Preview = true
	_, err := h.admin.ExecuteChange(t.Context(), connect.NewRequest(preview))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "maintenance") || !strings.Contains(err.Error(), "rented") {
		t.Fatalf("preview: err = %v, want InvalidArgument naming maintenance and rented", err)
	}
	// 执行的预期版本取自同一节点一次合法预览；版本其间没有变化，执行走到主写的写事务才撞同一拒绝。
	probe := &heronv1.ExecuteChangeRequest{Preview: true, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"note"}},
		Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: id, Note: "probe"}}}
	v, err := h.admin.ExecuteChange(t.Context(), connect.NewRequest(probe))
	if err != nil {
		t.Fatal(err)
	}
	rentedOnly.RequestId = "rented-only"
	rentedOnly.ExpectedVersion = v.Msg.GetExpectedVersion()
	if _, err := h.admin.ExecuteChange(t.Context(), connect.NewRequest(rentedOnly)); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "maintenance") || !strings.Contains(err.Error(), "rented") {
		t.Fatalf("execute: err = %v, want InvalidArgument naming maintenance and rented", err)
	}
	// 基底若不带库里的 rented，这次只改 note 的执行会把它整体替换成 false。
	h.update(t, &heronv1.UpdateNodeRequest{Id: id, Name: "m", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), Rented: true})
	m := &heronv1.ExecuteChangeRequest{RequestId: "note-only", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"note"}},
		Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: id, Note: "edited"}}}
	previewChange(t, h.admin, m)
	if _, err := h.admin.ExecuteChange(t.Context(), connect.NewRequest(m)); err != nil {
		t.Fatal(err)
	}
	list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || len(list.Msg.GetNodes()) != 1 || !list.Msg.GetNodes()[0].GetRented() {
		t.Fatalf("masked note edit cleared rented: %v %v", list, err)
	}
}
