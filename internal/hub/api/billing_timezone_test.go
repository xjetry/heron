package api

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

// zonedUpdate 是只改计费时区、重置时刻与计费的整体替换请求，其余取建节点时的缺省。
func zonedUpdate(id int64, name, zone string, minute uint32, b *heronv1.Billing) *heronv1.UpdateNodeRequest {
	return &heronv1.UpdateNodeRequest{Id: id, Name: name, Public: true, TrafficResetDay: 1, TrafficResetMinute: minute, OfflineGraceS: proto.Uint32(0), BillingTimezone: zone, Billing: b}
}

// hub 在上海，UTC 9 月 24 日 20:00 上海已是 25 日：没设计费时区的节点按上海的今天剩 7 天，计费时区为 UTC 的节点按 UTC
// 的今天剩 8 天。管理端的 Node、公开快照与 GetTraffic 逐节点下发的周期时区说的是同一个节点时区；ListNodes 的 hub_today
// 是 hub 时区的今天。
func TestDaysLeftUsesTheNodeBillingZone(t *testing.T) {
	t.Parallel()
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	h := newZonedHarness(t, "", shanghai, store.DefaultRetention)
	h.clk.SetWall(time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC))
	h.login(t)
	hubNode, _ := h.createNode(t, "hub")
	utcNode, _ := h.createNode(t, "utc")
	billing := &heronv1.Billing{ExpiresOn: "2026-10-02"}
	for id, zone := range map[int64]string{hubNode: "", utcNode: "UTC"} {
		name := map[int64]string{hubNode: "hub", utcNode: "utc"}[id]
		if _, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(zonedUpdate(id, name, zone, 0, billing))); err != nil {
			t.Fatal(err)
		}
	}
	want := map[int64]int32{hubNode: 7, utcNode: 8}
	list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := list.Msg.GetHubToday(); got != "2026-09-25" {
		t.Errorf("hub_today = %q, want 2026-09-25", got)
	}
	for _, n := range list.Msg.GetNodes() {
		if got := n.GetBilling().GetDaysLeft(); got != want[n.GetId()] {
			t.Errorf("ListNodes node %s days_left = %d, want %d", n.GetName(), got, want[n.GetId()])
		}
	}
	snap, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range snap.Msg.GetNodes() {
		if got := n.GetBilling().GetDaysLeft(); got != want[n.GetId()] {
			t.Errorf("public node %s days_left = %d, want %d", n.GetName(), got, want[n.GetId()])
		}
		if zone := map[int64]string{hubNode: "Asia/Shanghai", utcNode: "UTC"}[n.GetId()]; n.GetTraffic().GetTimezone() != zone {
			t.Errorf("public node %s traffic.timezone = %q, want %q", n.GetName(), n.GetTraffic().GetTimezone(), zone)
		}
	}
}

// 节点列表的版本覆盖每个在用计费时区的今天：hub 时区的日期没变、只有某个节点的计费时区换了日期时，days_left 变了，
// 版本也必须变，条件读取不能回答"未变"。
func TestNodeListRevisionFollowsEveryBillingZoneDate(t *testing.T) {
	t.Parallel()
	h := newZonedHarness(t, "", time.UTC, store.DefaultRetention)
	h.clk.SetWall(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	h.login(t)
	id, _ := h.createNode(t, "ny")
	if _, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(zonedUpdate(id, "ny", "America/New_York", 0, &heronv1.Billing{ExpiresOn: "2026-10-02"}))); err != nil {
		t.Fatal(err)
	}
	first, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	// UTC 23:00：UTC 仍是 24 日，纽约（EDT）19:00 也还是 24 日，什么都没变。
	h.clk.SetWall(time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC))
	same, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{KnownRevision: first.Msg.GetRevision()}))
	if err != nil || !same.Msg.GetUnchanged() {
		t.Fatalf("nothing changed but the list was resent: %v %v", same, err)
	}
	// UTC 25 日 02:00：hub（UTC）换了日期——这一步两边都变，只为把基准推进到 hub 的新一天。
	h.clk.SetWall(time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC))
	second, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{KnownRevision: first.Msg.GetRevision()}))
	if err != nil || second.Msg.GetUnchanged() {
		t.Fatalf("hub date changed but the list was reported unchanged: %v %v", second, err)
	}
	// UTC 25 日 04:30：hub 仍是 25 日，纽约在 04:00 UTC 跨进 25 日，days_left 从 8 变成 7。
	h.clk.SetWall(time.Date(2026, 9, 25, 4, 30, 0, 0, time.UTC))
	third, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{KnownRevision: second.Msg.GetRevision()}))
	if err != nil || third.Msg.GetUnchanged() {
		t.Fatalf("the New York date changed but the list was reported unchanged: %v %v", third, err)
	}
	if got := third.Msg.GetNodes()[0].GetBilling().GetDaysLeft(); got != 7 {
		t.Fatalf("days_left after the New York midnight = %d, want 7", got)
	}
}

// 写入口拒绝非法的计费时区与越界的重置时刻：InvalidArgument，文案点名字段与期望取值；合法值原样回显。
func TestScheduleFieldsAdmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	for _, c := range []struct {
		zone   string
		minute uint32
		want   string
	}{
		{"Mars/Olympus", 0, `invalid_argument: billing_timezone must be empty (use the hub's --timezone) or an IANA time zone name such as Asia/Shanghai or UTC; got "Mars/Olympus"`},
		{"Local", 0, `invalid_argument: billing_timezone must be empty (use the hub's --timezone) or an IANA time zone name such as Asia/Shanghai or UTC; got "Local"`},
		{"", 1440, "invalid_argument: traffic_reset_minute must be between 0 and 1439 (minutes after local midnight in billing_timezone, 00:00–23:59, e.g. 510 for 08:30); got 1440"},
	} {
		_, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(zonedUpdate(id, "n", c.zone, c.minute, nil)))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || err.Error() != c.want {
			t.Errorf("UpdateNode(%q, %d) err = %v, want %q", c.zone, c.minute, err, c.want)
		}
	}
	resp, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(zonedUpdate(id, "n", "Asia/Kolkata", 510, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if n := resp.Msg.GetNode(); n.GetBillingTimezone() != "Asia/Kolkata" || n.GetTrafficResetMinute() != 510 {
		t.Fatalf("echoed %q %d, want Asia/Kolkata 510", n.GetBillingTimezone(), n.GetTrafficResetMinute())
	}
	created, err := h.admin.CreateNode(t.Context(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: "c", BillingTimezone: "America/New_York"}))
	if err != nil || created.Msg.GetNode().GetBillingTimezone() != "America/New_York" {
		t.Fatalf("CreateNode with a billing zone = %v %v", created, err)
	}
	_, err = h.admin.CreateNode(t.Context(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: "bad", BillingTimezone: "posix/UTC"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), `billing_timezone must be empty`) {
		t.Fatalf("CreateNode with posix/UTC err = %v, want InvalidArgument naming billing_timezone", err)
	}
}

// 已续费的"今天"按节点计费时区：UTC 9 月 24 日 20:00 上海已是 25 日。8 月 24 日到期、按月：先推后一个周期得 9 月 24 日；
// 按节点计费时区 UTC 的今天（24 日）它已不早于今天，停在这里、剩 0 天；若按 hub（上海）的今天（25 日）它早于今天，会再
// 推到 10 月 24 日。
func TestRenewNodeBillingUsesTheNodeBillingZone(t *testing.T) {
	t.Parallel()
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	h := newZonedHarness(t, "", shanghai, store.DefaultRetention)
	h.clk.SetWall(time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC))
	h.login(t)
	id, _ := h.createNode(t, "utc")
	b := &heronv1.Billing{BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_MONTHLY, ExpiresOn: "2026-08-24"}
	if _, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(zonedUpdate(id, "utc", "UTC", 0, b))); err != nil {
		t.Fatal(err)
	}
	resp, err := h.admin.RenewNodeBilling(t.Context(), connect.NewRequest(&heronv1.RenewNodeBillingRequest{NodeId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Msg.GetNode().GetBilling(); got.GetExpiresOn() != "2026-09-24" || got.GetDaysLeft() != 0 {
		t.Fatalf("renewed billing = %v, want 2026-09-24 with 0 days left", got)
	}
}
