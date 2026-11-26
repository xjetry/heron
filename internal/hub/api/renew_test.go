package api

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
)

func (h *harness) renew(t *testing.T, id int64) *heronv1.Node {
	t.Helper()
	resp, err := h.admin.RenewNodeBilling(t.Context(), connect.NewRequest(&heronv1.RenewNodeBillingRequest{NodeId: id}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetNode()
}

func monthlyUntil(expiresOn string) *heronv1.Billing {
	return &heronv1.Billing{BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_MONTHLY, ExpiresOn: expiresOn}
}

// "已续费"不要求开着自动续期：未过期的节点恰好推后一个周期（月末钳制），已过期的按自动续期的步进保留账单日；
// 响应与之后的 ListNodes 是同一个日期与 days_left（harness 的今天是 2026-01-01）。
func TestRenewNodeBillingPushesExpiryByTheAutoRenewalRule(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, c := range []struct {
		name, from, want string
		daysLeft         int32
	}{
		{"in the future, clamped to month end", "2026-01-31", "2026-02-28", 58},
		{"due today", "2026-01-01", "2026-02-01", 31},
		{"expired: keeps the billing day", "2025-12-15", "2026-01-15", 14},
	} {
		id, _ := h.createNode(t, c.name)
		h.update(t, billed(id, monthlyUntil(c.from)))
		got := h.renew(t, id).GetBilling()
		if got.GetExpiresOn() != c.want || got.GetDaysLeft() != c.daysLeft || got.GetAutoRenew() {
			t.Fatalf("%s: renewed billing = %v, want expires_on %s, days_left %d, auto_renew off", c.name, got, c.want, c.daysLeft)
		}
		list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range list.Msg.GetNodes() {
			if n.GetId() == id && n.GetBilling().GetExpiresOn() != c.want {
				t.Fatalf("%s: ListNodes after renew = %v", c.name, n.GetBilling())
			}
		}
	}
}

// 今天取 hub 时区：UTC 2026-01-01 16:30 在东八区已是 1 月 2 日。从 2025-12-01 按月步进，1 月 1 日在 hub 的今天之前，
// 要再推一个周期到 2 月 1 日；按 UTC 算，1 月 1 日不早于今天，会停在 1 月 1 日。
func TestRenewNodeBillingUsesTheHubZone(t *testing.T) {
	t.Parallel()
	h := newZonedHarness(t, "", time.FixedZone("UTC+8", 8*3600), store.DefaultRetention)
	h.login(t)
	id, _ := h.createNode(t, "n")
	h.update(t, billed(id, monthlyUntil("2025-12-01")))
	h.clk.SetWall(time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC))
	if got := h.renew(t, id).GetBilling(); got.GetExpiresOn() != "2026-02-01" || got.GetDaysLeft() != 30 {
		t.Fatalf("renewed billing = %v, want 2026-02-01 with 30 days left in the hub zone", got)
	}
}

// 没有周期或没有到期日的节点没有可推后的对象：InvalidArgument 点名缺的字段，库里不变；节点不存在是 NotFound。
func TestRenewNodeBillingRejectsNodesWithoutCycleOrExpiry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, c := range []struct {
		name    string
		billing *heronv1.Billing
		field   string
	}{
		{"no cycle", &heronv1.Billing{ExpiresOn: "2026-03-01"}, "billing.billing_cycle"},
		{"no expiry", &heronv1.Billing{BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_YEARLY}, "billing.expires_on"},
		{"no billing", nil, "billing.billing_cycle"},
	} {
		id, _ := h.createNode(t, c.name)
		before := h.update(t, billed(id, c.billing)).GetBilling()
		_, err := h.admin.RenewNodeBilling(t.Context(), connect.NewRequest(&heronv1.RenewNodeBillingRequest{NodeId: id}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), c.field) {
			t.Fatalf("%s: %v, want InvalidArgument naming %s", c.name, err, c.field)
		}
		n, err := h.store.GetNode(t.Context(), id)
		if err != nil || n.Billing.ExpiresOn != before.GetExpiresOn() {
			t.Fatalf("%s: billing after a refused renewal = %+v %v", c.name, n.Billing, err)
		}
	}
	if _, err := h.admin.RenewNodeBilling(t.Context(), connect.NewRequest(&heronv1.RenewNodeBillingRequest{NodeId: 999})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown node: %v, want NotFound", err)
	}
}

// hookClock 在第一次读墙钟时先跑一次 before。RenewNodeBilling 在读到节点之后、写入之前恰好读一次钟（今天），
// 用例借它在这个窗口里插进一次并发的计费修改。
type hookClock struct {
	clock.Clock
	before atomic.Pointer[func()]
}

func (c *hookClock) Now() time.Time {
	if f := c.before.Swap(nil); f != nil {
		(*f)()
	}
	return c.Clock.Now()
}

// 读到节点之后计费被并发修改：条件写不成立，回答 FailedPrecondition，管理员刚保存的值原样保留。handler 直接调用
// （不经 HTTP），换钟时没有别的协程在读 svc.clk。
func TestRenewNodeBillingRefusesWhenBillingChangedConcurrently(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	id, _, err := h.store.CreateNode(t.Context(), store.NewNode{Name: "n", Billing: store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-03-01"}}, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	edited := store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-06-30"}
	hook := &hookClock{Clock: h.svc.clk}
	edit := func() {
		if _, err := h.store.UpdateNode(t.Context(), id, store.NodeEdit{Name: "n", TrafficResetDay: 1, Billing: edited}); err != nil {
			t.Error(err)
		}
	}
	hook.before.Store(&edit)
	h.svc.clk = hook
	_, err = h.svc.RenewNodeBilling(t.Context(), connect.NewRequest(&heronv1.RenewNodeBillingRequest{NodeId: id}))
	if hook.before.Load() != nil {
		t.Fatal("the concurrent edit never ran: RenewNodeBilling did not read the clock")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "refresh") {
		t.Fatalf("renew over a concurrent edit: %v, want FailedPrecondition asking to refresh", err)
	}
	wantEdited := edited
	wantEdited.AnchorDay = 30
	if n, err := h.store.GetNode(t.Context(), id); err != nil || n.Billing != wantEdited {
		t.Fatalf("billing after the refused renewal = %+v %v, want the concurrent edit %+v", n.Billing, err, wantEdited)
	}
}

// 续费后与 UpdateNode 改计费一样立刻扫描：进入提醒窗口的到期告警在响应返回前已恢复。
func TestRenewNodeBillingRecoversExpiryAlerts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	saveRule(t, h, expiryRuleProto())
	h.update(t, billed(id, monthlyUntil("2026-01-04")))
	h.renew(t, id)
	resp, err := h.admin.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{NodeId: id}))
	if err != nil {
		t.Fatal(err)
	}
	ev := resp.Msg.GetEvents()
	if len(ev) != 2 || ev[0].GetTransition() != "recovered" || ev[0].GetSummary() != "节点 n1 到期日已更新为 2026-02-04（规则 到期），告警持续 不到 1 分钟" {
		t.Fatalf("events after renewing: %v", ev)
	}
}

// API token 只能经 ExecuteChange 续期：预授权 CONFIGURE 且节点在范围内才放行，回执带写前写后的到期日；直接调
// RenewNodeBilling 与 UpdateNode 一样只接受会话；范围外的节点被拒绝。
func TestRenewNodeBillingThroughExecuteChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, _ := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	h.update(t, billed(a, monthlyUntil("2026-01-20")))
	h.update(t, billed(b, monthlyUntil("2026-01-20")))
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{a}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
	if _, err := client.RenewNodeBilling(t.Context(), connect.NewRequest(&heronv1.RenewNodeBillingRequest{NodeId: a})); connect.CodeOf(err) == 0 {
		t.Fatal("an API token called RenewNodeBilling directly")
	}
	m := &heronv1.ExecuteChangeRequest{RequestId: "renew-a", Change: &heronv1.ExecuteChangeRequest_RenewNodeBilling{RenewNodeBilling: &heronv1.RenewNodeBillingRequest{NodeId: a}}}
	p := previewChange(t, client, m)
	if p.Operation.Action != "renew_node_billing" || !strings.Contains(p.Operation.BeforeJson, "2026-01-20") || !strings.Contains(p.Operation.AfterJson, "2026-02-20") {
		t.Fatalf("preview receipt = %v", p.Operation)
	}
	if n, _ := h.store.GetNode(t.Context(), a); n.Billing.ExpiresOn != "2026-01-20" {
		t.Fatalf("preview wrote %s", n.Billing.ExpiresOn)
	}
	r, err := client.ExecuteChange(t.Context(), connect.NewRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	var res heronv1.RenewNodeBillingResponse
	if err := r.Msg.Result.UnmarshalTo(&res); err != nil || res.GetNode().GetBilling().GetExpiresOn() != "2026-02-20" || r.Msg.Operation.CommittedAt == 0 {
		t.Fatalf("execute = %v (result %v, %v)", r.Msg, &res, err)
	}
	other := &heronv1.ExecuteChangeRequest{RequestId: "renew-b", Preview: true, Change: &heronv1.ExecuteChangeRequest_RenewNodeBilling{RenewNodeBilling: &heronv1.RenewNodeBillingRequest{NodeId: b}}}
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(other)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("renewing a node outside the grant: %v, want PermissionDenied", err)
	}
	if n, _ := h.store.GetNode(t.Context(), b); n.Billing.ExpiresOn != "2026-01-20" {
		t.Fatalf("node outside the grant renewed to %s", n.Billing.ExpiresOn)
	}
}

// 写到期日的全部接口入口（CreateNode、UpdateNode、ExecuteChange 的 create_node / update_node）写入并维护锚定日号：
//   - 1-31 建档或保存后手动连续续期依次落到 2-28、3-31，不漂到 3-28；
//   - 2-28 期间通过 UpdateNode 或 ExecuteChange(update_mask=["note"]) 改非到期字段，锚定日号保持 31；
//   - 改写到期日后锚定日号同步更新，清空到期日后归零；
//   - ExecuteChange 的 renew_node_billing 快照带出不变的 billing_anchor_day。
func TestRenewalAnchorDayAcrossAllAPIWriteEntries(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)

	// 1. 直接 CreateNode 带 1-31 到期日 → 锚定 31，连续手动续期落到 2-28、3-31。
	created, err := h.admin.CreateNode(t.Context(), connect.NewRequest(&heronv1.CreateNodeRequest{
		Name:    "eom-direct",
		Billing: monthlyUntil("2026-01-31"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetNode().GetId()
	if n, err := h.store.GetNode(t.Context(), id); err != nil || n.Billing.AnchorDay != 31 {
		t.Fatalf("after CreateNode: billing = %+v %v, want anchor 31", n.Billing, err)
	}
	if got := h.renew(t, id).GetBilling().GetExpiresOn(); got != "2026-02-28" {
		t.Fatalf("first manual renew = %s, want 2026-02-28", got)
	}
	// 2-28 期间经 UpdateNode 只改备注，不改到期日：锚定日号保持 31。
	upd := billed(id, monthlyUntil("2026-02-28"))
	upd.Note = "memo"
	h.update(t, upd)
	if n, err := h.store.GetNode(t.Context(), id); err != nil || n.Billing.AnchorDay != 31 {
		t.Fatalf("after UpdateNode(note only): billing = %+v %v, want anchor 31", n.Billing, err)
	}
	if got := h.renew(t, id).GetBilling().GetExpiresOn(); got != "2026-03-31" {
		t.Fatalf("second manual renew = %s, want 2026-03-31", got)
	}

	// 2. 经 ExecuteChange(create_node / update_node / renew_node_billing) 走同一链路。
	client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{
		AllNodes: true,
		Permissions: []heronv1.TokenPermission{
			heronv1.TokenPermission_TOKEN_PERMISSION_CREATE,
			heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE,
		},
	})
	createReq := &heronv1.ExecuteChangeRequest{
		RequestId: "create-eom",
		Change: &heronv1.ExecuteChangeRequest_CreateNode{
			CreateNode: &heronv1.CreateNodeRequest{Name: "eom-exec", Billing: monthlyUntil("2026-01-31")},
		},
	}
	previewChange(t, client, createReq)
	createResp, err := client.ExecuteChange(t.Context(), connect.NewRequest(createReq))
	if err != nil {
		t.Fatal(err)
	}
	var cRes heronv1.CreateNodeResponse
	if err := createResp.Msg.Result.UnmarshalTo(&cRes); err != nil {
		t.Fatal(err)
	}
	execID := cRes.GetNode().GetId()
	if n, err := h.store.GetNode(t.Context(), execID); err != nil || n.Billing.AnchorDay != 31 {
		t.Fatalf("after ExecuteChange(create_node): billing = %+v %v, want anchor 31", n.Billing, err)
	}

	// 经 ExecuteChange 首次续期到 2026-02-28，再只改 note 掩码，确认锚定仍在 31。
	renew1 := &heronv1.ExecuteChangeRequest{
		RequestId: "renew-eom-1",
		Change:    &heronv1.ExecuteChangeRequest_RenewNodeBilling{RenewNodeBilling: &heronv1.RenewNodeBillingRequest{NodeId: execID}},
	}
	previewChange(t, client, renew1)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(renew1)); err != nil {
		t.Fatal(err)
	}
	noteReq := &heronv1.ExecuteChangeRequest{
		RequestId:  "note-eom",
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"note"}},
		Change: &heronv1.ExecuteChangeRequest_UpdateNode{
			UpdateNode: &heronv1.UpdateNodeRequest{Id: execID, Note: "via-token"},
		},
	}
	previewChange(t, client, noteReq)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(noteReq)); err != nil {
		t.Fatal(err)
	}
	if n, err := h.store.GetNode(t.Context(), execID); err != nil || n.Billing.ExpiresOn != "2026-02-28" || n.Billing.AnchorDay != 31 {
		t.Fatalf("after ExecuteChange(update_node note): billing = %+v %v, want 2026-02-28 anchor 31", n.Billing, err)
	}

	// 再次经 ExecuteChange 续期：从 2026-02-28 回到 2026-03-31，快照中 billing_anchor_day 为 31 且不变。
	renew2 := &heronv1.ExecuteChangeRequest{
		RequestId: "renew-eom-2",
		Change:    &heronv1.ExecuteChangeRequest_RenewNodeBilling{RenewNodeBilling: &heronv1.RenewNodeBillingRequest{NodeId: execID}},
	}
	p2 := previewChange(t, client, renew2)
	if !strings.Contains(p2.Operation.BeforeJson, `"billing_anchor_day":31`) ||
		!strings.Contains(p2.Operation.AfterJson, `"billing_anchor_day":31`) ||
		!strings.Contains(p2.Operation.AfterJson, `"expires_on":"2026-03-31"`) {
		t.Fatalf("renew2 preview receipt = %+v", p2.Operation)
	}
	r2, err := client.ExecuteChange(t.Context(), connect.NewRequest(renew2))
	if err != nil {
		t.Fatal(err)
	}
	var renewRes heronv1.RenewNodeBillingResponse
	if err := r2.Msg.Result.UnmarshalTo(&renewRes); err != nil || renewRes.GetNode().GetBilling().GetExpiresOn() != "2026-03-31" {
		t.Fatalf("renew2 result = %+v %v, want 2026-03-31", &renewRes, err)
	}

	// 经 ExecuteChange 显式改写到期日为 2026-04-15 → 锚定日号更新为 15；再清空 → 锚定日号归零。
	reanchor := &heronv1.ExecuteChangeRequest{
		RequestId:  "reanchor-15",
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"billing.expires_on"}},
		Change: &heronv1.ExecuteChangeRequest_UpdateNode{
			UpdateNode: &heronv1.UpdateNodeRequest{Id: execID, Billing: &heronv1.Billing{ExpiresOn: "2026-04-15"}},
		},
	}
	previewChange(t, client, reanchor)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(reanchor)); err != nil {
		t.Fatal(err)
	}
	if n, err := h.store.GetNode(t.Context(), execID); err != nil || n.Billing.AnchorDay != 15 {
		t.Fatalf("after ExecuteChange(expires_on=2026-04-15): billing = %+v %v, want anchor 15", n.Billing, err)
	}
	clearExpiry := &heronv1.ExecuteChangeRequest{
		RequestId:  "clear-expiry",
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"billing.expires_on"}},
		Change: &heronv1.ExecuteChangeRequest_UpdateNode{
			UpdateNode: &heronv1.UpdateNodeRequest{Id: execID, Billing: &heronv1.Billing{ExpiresOn: ""}},
		},
	}
	previewChange(t, client, clearExpiry)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(clearExpiry)); err != nil {
		t.Fatal(err)
	}
	if n, err := h.store.GetNode(t.Context(), execID); err != nil || n.Billing.ExpiresOn != "" || n.Billing.AnchorDay != 0 {
		t.Fatalf("after ExecuteChange(clear expires_on): billing = %+v %v, want empty expiry & anchor 0", n.Billing, err)
	}
}
