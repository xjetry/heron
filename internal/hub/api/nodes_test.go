package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

// 节点名与 token 名的协议面校验统一走 store.CleanName；拒绝是 InvalidArgument、文案逐字同 store.NameError。
// overlongNameMsg 与改动前 api 侧名字校验产出的文案逐字相同（改动前的写法见 git 历史）——规则收口进 store 后文案不变。
const overlongNameMsg = "invalid_argument: name must be 1–64 characters after trimming whitespace and control characters; got 65"

func overlongName() string { return strings.Repeat("名", store.MaxNameRunes+1) }

func TestCreateNodeRejectsOverlongName(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, tc := range []struct{ name, raw, want string }{
		{"overlong", overlongName(), overlongNameMsg},
		{"blank", "", "invalid_argument: name must be 1–64 characters after trimming whitespace and control characters; got 0"},
	} {
		_, err := h.admin.CreateNode(context.Background(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: tc.raw}))
		if codeOf(err) != connect.CodeInvalidArgument || err.Error() != tc.want {
			t.Fatalf("%s: err=%v, want %q", tc.name, err, tc.want)
		}
	}
}

// 首尾空白照常被清洗后入库（行为不变），清洗后的值即规范形。
func TestCreateNodeCleansName(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	resp, err := h.admin.CreateNode(context.Background(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: "  web-01  "}))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Msg.GetNode().GetName(); got != "web-01" {
		t.Fatalf("stored name = %q, want web-01", got)
	}
}

func TestUpdateNodeRejectsOverlongName(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	id, _ := h.createNode(t, "web")
	full := func(name string) *heronv1.UpdateNodeRequest {
		return &heronv1.UpdateNodeRequest{Id: id, Name: name, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(900), Tags: []string{}}
	}
	if _, err := h.admin.UpdateNode(ctx, connect.NewRequest(full(overlongName()))); codeOf(err) != connect.CodeInvalidArgument || err.Error() != overlongNameMsg {
		t.Fatalf("UpdateNode: err=%v, want %q", err, overlongNameMsg)
	}
	// ExecuteChange 的节点更新经同一个 handler 上岸（preview 路径即 invokeChange），同码同文案。
	_, err := h.admin.ExecuteChange(ctx, connect.NewRequest(&heronv1.ExecuteChangeRequest{
		RequestId:  "bad-name",
		Preview:    true,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}},
		Change:     &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: full(overlongName())},
	}))
	if codeOf(err) != connect.CodeInvalidArgument || err.Error() != overlongNameMsg {
		t.Fatalf("ExecuteChange: err=%v, want %q", err, overlongNameMsg)
	}
	// 两次被拒后名字未变。
	n, err := h.store.GetNode(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "web" {
		t.Fatalf("node name = %q, want web", n.Name)
	}
}

// token 名与节点名同一条共享规则：Create/Update 都以 InvalidArgument 拒绝超长，文案逐字同 store.NameError。
func TestApiTokenRejectsOverlongName(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	if _, err := h.admin.CreateApiToken(ctx, connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: overlongName()})); codeOf(err) != connect.CodeInvalidArgument || err.Error() != overlongNameMsg {
		t.Fatalf("CreateApiToken: err=%v, want %q", err, overlongNameMsg)
	}
	if _, err := h.admin.UpdateApiToken(ctx, connect.NewRequest(&heronv1.UpdateApiTokenRequest{Id: 1, Name: overlongName()})); codeOf(err) != connect.CodeInvalidArgument || err.Error() != overlongNameMsg {
		t.Fatalf("UpdateApiToken: err=%v, want %q", err, overlongNameMsg)
	}
}

// 注册入口的超长名字按 agent 口径（store.AgentNodeName）截断入库，入库值即规范形；
// 节点之后带着库内名字 UpdateNode 成功——不会出现"注册进来一个库内非法名，此后每次编辑都撞名字校验"。
func TestRegisteredNodeWithTruncatedNameStaysEditable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := context.Background()
	opened, err := h.admin.OpenRegisterWindow(ctx, connect.NewRequest(&heronv1.OpenRegisterWindowRequest{TtlS: 3600, MaxNodes: 1}))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := h.agent.Register(ctx, connect.NewRequest(&heronv1.RegisterRequest{Key: opened.Msg.GetKey(), Name: overlongName()}))
	if err != nil {
		t.Fatal(err)
	}
	id := reg.Msg.GetNodeId()
	n, err := h.store.GetNode(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	stored := strings.Repeat("名", store.MaxNameRunes)
	if n.Name != stored {
		t.Fatalf("stored name = %q, want the truncated form %q", n.Name, stored)
	}
	if _, err := h.admin.UpdateNode(ctx, connect.NewRequest(&heronv1.UpdateNodeRequest{
		Id: id, Name: n.Name, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(900), Tags: []string{},
	})); err != nil {
		t.Fatalf("registered node must stay editable with its stored name: %v", err)
	}
}
