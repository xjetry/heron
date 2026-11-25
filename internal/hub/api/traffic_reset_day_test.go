package api

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// 重置日 29–31 经管理接口与预授权变更两个入口都被接受，落盘与回显保持原值；周期边界在短月钳到月末，之后的大月
// 回到原日号。harness 的 hub 时区是 UTC，时钟从 2026-01-01 00:00 起。
func TestResetDayAcceptsMonthEndDaysAndKeepsTheConfiguredValue(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	utc := func(m time.Month, d int) int64 { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC).Unix() }
	traffic := func() *heronv1.Traffic {
		t.Helper()
		all, err := h.admin.GetTraffic(t.Context(), connect.NewRequest(&heronv1.GetTrafficRequest{}))
		if err != nil || len(all.Msg.Nodes) != 1 {
			t.Fatalf("GetTraffic: %v %v", all, err)
		}
		return all.Msg.Nodes[0].GetTraffic()
	}
	echoed := func(want uint32) {
		t.Helper()
		list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
		if err != nil || list.Msg.Nodes[0].GetTrafficResetDay() != want {
			t.Fatalf("ListNodes reset day = %v %v, want %d", list, err, want)
		}
		if n, err := h.store.GetNode(t.Context(), id); err != nil || n.TrafficResetDay != int(want) {
			t.Fatalf("stored reset day = %+v %v, want %d", n, err, want)
		}
		if tr := traffic(); tr.GetResetDay() != want {
			t.Fatalf("traffic reset day = %d, want %d", tr.GetResetDay(), want)
		}
	}

	if n := h.update(t, &heronv1.UpdateNodeRequest{Id: id, Name: "n", TrafficResetDay: 31, OfflineGraceS: proto.Uint32(0)}); n.GetTrafficResetDay() != 31 {
		t.Fatalf("UpdateNode echoed reset day %d, want 31", n.GetTrafficResetDay())
	}
	if tr := traffic(); tr.GetPeriodStart() != time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC).Unix() || tr.GetNextResetAt() != utc(1, 31) {
		t.Fatalf("January period with reset day 31: %v", tr)
	}
	// 墙钟跨过一个半月，会话随之过期，每次拨钟后重新登录。
	h.clk.SetWall(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC))
	h.login(t)
	if tr := traffic(); tr.GetPeriodStart() != utc(1, 31) || tr.GetNextResetAt() != utc(2, 28) {
		t.Fatalf("February period with reset day 31: %v", tr)
	}
	h.clk.SetWall(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	h.login(t)
	if tr := traffic(); tr.GetPeriodStart() != utc(2, 28) || tr.GetNextResetAt() != utc(3, 31) {
		t.Fatalf("March period after the clamped February boundary: %v", tr)
	}
	echoed(31)

	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{id}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	change := func(requestID string, day uint32) *heronv1.ExecuteChangeRequest {
		return &heronv1.ExecuteChangeRequest{RequestId: requestID, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"traffic_reset_day"}},
			Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: id, TrafficResetDay: day}}}
	}
	m := change("reset-day-30", 30)
	previewChange(t, client, m)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); err != nil {
		t.Fatalf("ExecuteChange reset day 30: %v", err)
	}
	if tr := traffic(); tr.GetPeriodStart() != utc(2, 28) || tr.GetNextResetAt() != utc(3, 30) {
		t.Fatalf("period after changing the reset day to 30 on March 1: %v", tr)
	}
	echoed(30)

	const wantRange = "traffic_reset_day must be between 1 and 31"
	for _, day := range []uint32{0, 32} {
		_, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "n", TrafficResetDay: day, OfflineGraceS: proto.Uint32(0)}))
		if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), wantRange) {
			t.Fatalf("UpdateNode reset day %d: %v, want InvalidArgument %q", day, err, wantRange)
		}
		p := change("reset-day-out-of-range", day)
		p.Preview = true
		if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(p)); codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), wantRange) {
			t.Fatalf("ExecuteChange reset day %d: %v, want InvalidArgument %q", day, err, wantRange)
		}
	}
	echoed(30)
}
