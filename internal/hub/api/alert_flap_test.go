package api

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// ListAlertRules 的状态带 flapping：从未恢复过的普通 pending（离线未满节点宽限）为 false；恢复后 10 分钟内再次离线、
// 已满节点宽限却因抖动抑制停在 pending 时为 true；进入 firing 后回到 false。
func TestListAlertRulesMarksFlappingPending(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	h.update(t, &heronv1.UpdateNodeRequest{Id: id, Name: "n", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(60)})
	if _, err := h.admin.SaveAlertRule(t.Context(), connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: &heronv1.AlertRule{
		Name: "离线", Kind: heronv1.AlertKind_ALERT_KIND_OFFLINE, Enabled: true, NodeIds: []int64{id}}})); err != nil {
		t.Fatal(err)
	}
	sweep := func() {
		t.Helper()
		if err := h.alerts.SweepOffline(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	state := func(label, want string, flapping bool) {
		t.Helper()
		resp, err := h.admin.ListAlertRules(t.Context(), connect.NewRequest(&heronv1.ListAlertRulesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		states := resp.Msg.GetStates()
		if len(states) != 1 || states[0].GetState() != want || states[0].GetFlapping() != flapping {
			t.Fatalf("%s: states = %v, want %s flapping=%v", label, states, want, flapping)
		}
	}
	h.live.Observe(id, "", &heronv1.Metrics{})
	h.clk.Advance(31 * time.Second)
	sweep()
	state("offline within the node grace", "pending", false)
	h.clk.Advance(30 * time.Second)
	sweep()
	state("offline past the node grace", "firing", false)
	h.live.Observe(id, "", &heronv1.Metrics{})
	sweep()
	h.clk.Advance(10 * time.Minute)
	h.live.Observe(id, "", &heronv1.Metrics{})
	sweep()
	h.clk.Advance(61 * time.Second)
	sweep()
	state("offline again 10 minutes after recovery", "pending", true)
	h.clk.Advance(30*time.Minute - 61*time.Second)
	sweep()
	state("flap grace elapsed", "firing", false)
}
