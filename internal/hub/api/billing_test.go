package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/store"
)

// billed 是一次 UpdateNode 请求：名称 n<id>（几个节点在失败输出里分得开）、重置日 1、宽限期取默认，计费取 b
// （nil 即不带 billing）。
func billed(id int64, b *heronv1.Billing) *heronv1.UpdateNodeRequest {
	return &heronv1.UpdateNodeRequest{Id: id, Name: fmt.Sprintf("n%d", id), TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), Billing: b}
}

func (h *harness) update(t *testing.T, req *heronv1.UpdateNodeRequest) *heronv1.Node {
	t.Helper()
	resp, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetNode()
}

func TestBillingCyclesMapEveryValue(t *testing.T) {
	t.Parallel()
	values := heronv1.BillingCycle(0).Descriptor().Values()
	var stored []store.BillingCycle
	for i := 0; i < values.Len(); i++ {
		v := heronv1.BillingCycle(values.Get(i).Number())
		c, ok := billingCycles[v]
		if !ok {
			t.Fatalf("%s has no stored form", v)
		}
		if enumFor(billingCycles, c) != v {
			t.Errorf("%s does not round-trip through %q", v, c)
		}
		stored = append(stored, c)
	}
	want := append([]store.BillingCycle{store.CycleNone}, store.BillingCycles()...)
	if len(billingCycles) != values.Len() || !slices.Equal(stored, want) {
		t.Fatalf("stored forms %q, want %q", stored, want)
	}
}

func TestFiveYearBillingRoundTripsAndRenews(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "five-year")
	request := billed(id, &heronv1.Billing{Price: "150", Currency: "TWD", BillingCycle: 7, ExpiresOn: "2024-02-29", AutoRenew: true})
	request.Public = true
	n := h.update(t, request)
	if n.GetBilling().GetBillingCycle() != 7 || n.GetBilling().GetExpiresOn() != "2029-02-28" {
		t.Fatalf("five-year billing = %v", n.GetBilling())
	}
	public, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(public.Msg.Nodes) != 1 || public.Msg.Nodes[0].GetBilling().GetBillingCycle() != 7 || public.Msg.Nodes[0].GetBilling().GetExpiresOn() != "2029-02-28" {
		t.Fatalf("public five-year billing = %v", public.Msg)
	}
}

// 每种不合格的取值都被拒绝，错误以请求路径写明字段、约束与收到的值，库里的计费不变。
func TestUpdateNodeRejectsMalformedBilling(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	kept := h.update(t, billed(id, &heronv1.Billing{Price: "5", Currency: "EUR", ExpiresOn: "2026-06-01"}))
	const price = `billing.price: must match ^\d{1,9}(\.\d{1,2})?$, e.g. 12.50; got `
	const date = `billing.expires_on: must be an existing date in YYYY-MM-DD form; got `
	for _, c := range []struct {
		b    *heronv1.Billing
		want string
	}{
		{&heronv1.Billing{Price: "12.345", Currency: "USD"}, price + `"12.345"`},
		{&heronv1.Billing{Price: "1234567890", Currency: "USD"}, price + `"1234567890"`},
		{&heronv1.Billing{Price: "-1", Currency: "USD"}, price + `"-1"`},
		{&heronv1.Billing{Price: "12.", Currency: "USD"}, price + `"12."`},
		{&heronv1.Billing{Price: ".5", Currency: "USD"}, price + `".5"`},
		{&heronv1.Billing{Price: "1e3", Currency: "USD"}, price + `"1e3"`},
		{&heronv1.Billing{Price: " 12", Currency: "USD"}, price + `" 12"`},
		{&heronv1.Billing{Price: "１２", Currency: "USD"}, price + `"１２"`},
		{&heronv1.Billing{Price: "12"}, "billing.currency: required when billing.price is set"},
		{&heronv1.Billing{Currency: "usd"}, `billing.currency: must be three uppercase letters (ISO 4217), e.g. USD; got "usd"`},
		{&heronv1.Billing{Currency: "USDT"}, `billing.currency: must be three uppercase letters (ISO 4217), e.g. USD; got "USDT"`},
		{&heronv1.Billing{BillingCycle: 99}, "billing.billing_cycle: must be one of BILLING_CYCLE_UNSPECIFIED, BILLING_CYCLE_MONTHLY, BILLING_CYCLE_QUARTERLY, BILLING_CYCLE_SEMIANNUAL, BILLING_CYCLE_YEARLY, BILLING_CYCLE_BIENNIAL, BILLING_CYCLE_TRIENNIAL, BILLING_CYCLE_QUINQUENNIAL; got 99"},
		{&heronv1.Billing{ExpiresOn: "2026-02-29"}, date + `"2026-02-29"`},
		{&heronv1.Billing{ExpiresOn: "2026-1-05"}, date + `"2026-1-05"`},
		{&heronv1.Billing{ExpiresOn: "2026-10-01T00:00:00Z"}, date + `"2026-10-01T00:00:00Z"`},
		{&heronv1.Billing{AutoRenew: true, ExpiresOn: "2026-10-01"}, "billing.auto_renew: requires billing.billing_cycle and billing.expires_on to be set"},
		{&heronv1.Billing{AutoRenew: true, BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_MONTHLY}, "billing.auto_renew: requires billing.billing_cycle and billing.expires_on to be set"},
	} {
		_, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(billed(id, c.b)))
		if codeOf(err) != connect.CodeInvalidArgument || err.Error() != "invalid_argument: "+c.want {
			t.Errorf("%v: err = %v, want %s", c.b, err, c.want)
		}
	}
	list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || !proto.Equal(list.Msg.GetNodes()[0], kept) {
		t.Fatalf("rejected updates changed the node: %v %v, want %v", list, err, kept)
	}
}

// 边界上的合法取值照常保存；币种可以单独填。days_left 由 hub 算出，请求里的值不读；空的 billing 与不带 billing
// 都是五项全清，回显里 billing 缺失。
func TestUpdateNodeAcceptsBoundaryBillingAndIgnoresDaysLeft(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	for _, b := range []*heronv1.Billing{
		{Price: "0", Currency: "USD"},
		{Price: "999999999.99", Currency: "JPY", BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_TRIENNIAL},
		{Price: "12.5", Currency: "CNY", BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_MONTHLY, ExpiresOn: "2028-02-29", AutoRenew: true},
		{Currency: "EUR"},
		{ExpiresOn: "9999-12-31"},
	} {
		got := h.update(t, billed(id, b)).GetBilling()
		if got.GetPrice() != b.GetPrice() || got.GetCurrency() != b.GetCurrency() || got.GetBillingCycle() != b.GetBillingCycle() ||
			got.GetExpiresOn() != b.GetExpiresOn() || got.GetAutoRenew() != b.GetAutoRenew() || (got.DaysLeft != nil) != (b.GetExpiresOn() != "") {
			t.Errorf("saved %v, echoed %v", b, got)
		}
	}
	// 时钟是 2026-01-01（UTC），2026-01-10 剩 9 天；请求里的 999 不起作用。
	if got := h.update(t, billed(id, &heronv1.Billing{ExpiresOn: "2026-01-10", DaysLeft: proto.Int32(999)})).GetBilling(); got.GetDaysLeft() != 9 {
		t.Fatalf("days_left = %d, want 9 computed by the hub", got.GetDaysLeft())
	}
	for _, b := range []*heronv1.Billing{{}, nil} {
		h.update(t, billed(id, &heronv1.Billing{Price: "5", Currency: "EUR", ExpiresOn: "2026-06-01"}))
		if got := h.update(t, billed(id, b)); got.Billing != nil {
			t.Fatalf("billing %v did not clear: %v", b, got)
		}
		list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
		if err != nil || list.Msg.GetNodes()[0].Billing != nil {
			t.Fatalf("billing %v: ListNodes = %v %v", b, list, err)
		}
	}
}

// days_left 取 hub 时区的今天：UTC 16:30 在东八区已是次日，比按 UTC 算少一天。管理端、公开端，以及公开快照的
// now 与 days_left 都是同一个口径。
func TestDaysLeftUsesTheHubZone(t *testing.T) {
	t.Parallel()
	h := newZonedHarness(t, "", time.FixedZone("UTC+8", 8*3600), store.DefaultRetention)
	h.login(t)
	id, _ := h.createNode(t, "n")
	h.clk.SetWall(time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC))
	req := billed(id, &heronv1.Billing{ExpiresOn: "2026-01-10"})
	req.Public = true
	if got := h.update(t, req).GetBilling().GetDaysLeft(); got != 8 {
		t.Fatalf("UpdateNode days_left = %d, want 8", got)
	}
	list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || list.Msg.GetNodes()[0].GetBilling().GetDaysLeft() != 8 {
		t.Fatalf("ListNodes = %v %v", list, err)
	}
	snap, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil || snap.Msg.GetNow() != h.clk.Now().Unix() || snap.Msg.GetNodes()[0].GetBilling().GetDaysLeft() != 8 {
		t.Fatalf("public snapshot = %v %v", snap, err)
	}
}

// 公开快照带价格、币种、周期、到期日与 days_left，不带自动续期；没有到期日的节点 days_left 缺失，什么都没填的
// 节点 billing 缺失。
func TestPublicSnapshotCarriesBillingWithoutAutoRenew(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, _ := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	c, _ := h.createNode(t, "c")
	for _, req := range []*heronv1.UpdateNodeRequest{
		billed(a, &heronv1.Billing{Price: "12.50", Currency: "USD", BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_YEARLY, ExpiresOn: "2025-12-29"}),
		billed(b, &heronv1.Billing{Price: "3", Currency: "EUR", BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_MONTHLY, ExpiresOn: "2026-03-01", AutoRenew: true}),
		billed(c, nil),
	} {
		req.Public = true
		h.update(t, req)
	}
	snap, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	want := &heronv1.PublicBilling{Price: "12.50", Currency: "USD", BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_YEARLY, ExpiresOn: "2025-12-29", DaysLeft: proto.Int32(-3)}
	if got := snap.Msg.GetNodes()[0].GetBilling(); !proto.Equal(got, want) {
		t.Fatalf("public billing of a = %v, want %v", got, want)
	}
	if got := snap.Msg.GetNodes()[2]; got.Billing != nil {
		t.Fatalf("node without billing = %v", got)
	}
	upd := billed(b, &heronv1.Billing{Price: "3", Currency: "EUR"})
	upd.Public = true
	h.update(t, upd)
	h.clk.Advance(2 * time.Second) // 越过快照缓存的 1 秒窗口
	raw := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	if raw.status != 200 || bytes.Contains(raw.body, []byte("autoRenew")) || bytes.Contains(raw.body, []byte("auto_renew")) {
		t.Fatalf("snapshot JSON = %d %s", raw.status, raw.body)
	}
	if !bytes.Contains(raw.body, []byte(`"daysLeft":-3`)) || strings.Count(string(raw.body), "daysLeft") != 1 || strings.Count(string(raw.body), `"billing"`) != 2 {
		t.Fatalf("days_left must appear only for the node with an expiry date, billing only for the nodes that have one: %s", raw.body)
	}
}

// 库里读不懂的到期日只可能来自绕过 UpdateNode 的写库。它照原样下发，days_left 缺失而不是 0：0 会显示成"今天到期"。
func TestUnreadableExpiresOnHasNoDaysLeft(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	if _, err := h.store.UpdateNode(t.Context(), id, store.NodeEdit{Name: "n", TrafficResetDay: 1, Billing: store.Billing{ExpiresOn: "2026-02-30"}}); err != nil {
		t.Fatal(err)
	}
	list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if b := list.Msg.GetNodes()[0].GetBilling(); b.GetExpiresOn() != "2026-02-30" || b.DaysLeft != nil {
		t.Fatalf("billing = %v, want expires_on kept and days_left missing", b)
	}
}

// steppingClock 每读一次墙钟前进一天。GetSnapshot 若分两次读钟，now 与 days_left 就落在不同的日历日上。
type steppingClock struct {
	mu   sync.Mutex
	next time.Time
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.next
	c.next = t.Add(24 * time.Hour)
	return t
}
func (c *steppingClock) Mono() time.Duration { return 0 }

// 公开快照的 now 与 days_left 出自同一次读钟：第三方主题拿 now 核对 days_left，不会差一天。快照经真实的 Connect
// 处理器取得，公开服务用每读一次就前进一天的钟构造。
func TestPublicSnapshotReadsTheClockOnce(t *testing.T) {
	t.Parallel()
	h := newZonedHarness(t, "", time.FixedZone("UTC+8", 8*3600), store.DefaultRetention)
	h.login(t)
	id, _ := h.createNode(t, "n")
	req := billed(id, &heronv1.Billing{ExpiresOn: "2026-03-01"})
	req.Public = true
	h.update(t, req)
	loc := time.FixedZone("UTC+8", 8*3600)
	clk := &steppingClock{next: time.Date(2026, 1, 1, 23, 59, 59, 0, loc)}
	deps := h.publicDeps()
	deps.Clock = clk
	pub := NewPublic(PublicConfig{ReportInterval: 10 * time.Second, Location: loc}, deps)
	path, handler := heronv1connect.NewPublicServiceHandler(pub)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := heronv1connect.NewPublicServiceClient(srv.Client(), srv.URL)
	for range 3 {
		snap, err := client.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := alert.DaysLeft("2026-03-01", alert.Today(time.Unix(snap.Msg.GetNow(), 0), loc))
		if got := snap.Msg.GetNodes()[0].GetBilling().GetDaysLeft(); got != int32(want) {
			t.Fatalf("now %d is %s in the hub zone, so days_left should be %d; got %d", snap.Msg.GetNow(), time.Unix(snap.Msg.GetNow(), 0).In(loc).Format(time.DateOnly), want, got)
		}
	}
}

// 计费字段变了才扫描：只改名称时已过期的自动续期不推后；计费一变，响应里就是推后之后的日期。表单带着推后之前的
// 旧到期日提交也算变了（与库里推后的日期比），扫描随即再推后一次。
func TestUpdateNodeSweepsExpiryWhenBillingChanges(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	renewing := func() *heronv1.Billing {
		return &heronv1.Billing{BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_MONTHLY, ExpiresOn: "2026-01-15", AutoRenew: true}
	}
	if got := h.update(t, billed(id, renewing())).GetBilling().GetExpiresOn(); got != "2026-01-15" {
		t.Fatalf("expires_on = %s", got)
	}
	h.clk.SetWall(time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC))
	h.login(t) // 墙钟跳过了会话的有效期
	renamed := billed(id, renewing())
	renamed.Name = "renamed"
	if got := h.update(t, renamed); got.GetBilling().GetExpiresOn() != "2026-01-15" || got.GetBilling().GetDaysLeft() != -5 {
		t.Fatalf("a rename swept expiry: %v", got)
	}
	priced := renewing()
	priced.Price, priced.Currency = "9", "USD"
	if got := h.update(t, billed(id, priced)); got.GetBilling().GetExpiresOn() != "2026-02-15" || got.GetBilling().GetDaysLeft() != 26 {
		t.Fatalf("a billing change did not renew: %v", got)
	}
	if got := h.update(t, billed(id, priced)); got.GetBilling().GetExpiresOn() != "2026-02-15" {
		t.Fatalf("stale form: %v", got)
	}
}

// 到期告警跟着 UpdateNode 同步转换：响应返回时事件已经落库，续费之后不等到零点。
func TestUpdateNodeFiresAndRecoversExpiryAlerts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	saveRule(t, h, expiryRuleProto())
	events := func() []*heronv1.AlertEvent {
		t.Helper()
		resp, err := h.admin.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{NodeId: id}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetEvents()
	}
	h.update(t, billed(id, &heronv1.Billing{ExpiresOn: "2026-01-04"}))
	if ev := events(); len(ev) != 1 || ev[0].GetTransition() != "firing" || ev[0].GetSummary() != "节点 n1 将于 2026-01-04 到期（剩 3 天，规则 到期）" || ev[0].GetValue() != 3 {
		t.Fatalf("events after setting a close date: %v", ev)
	}
	h.update(t, billed(id, &heronv1.Billing{ExpiresOn: "2027-01-04"}))
	if ev := events(); len(ev) != 2 || ev[0].GetTransition() != "recovered" || ev[0].GetSummary() != "节点 n1 到期日已更新为 2027-01-04（规则 到期）" {
		t.Fatalf("events after renewing: %v", ev)
	}
}

func expiryRuleProto() *heronv1.AlertRule {
	return &heronv1.AlertRule{Name: "到期", Kind: heronv1.AlertKind_ALERT_KIND_EXPIRY, Enabled: true, AllNodes: true, DaysBefore: 7}
}

// 建节点可带计费：与 UpdateNode 同一个校验、同一种落库；响应节点带回显与 hub 算好的 days_left。
func TestCreateNodeWithBilling(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	create := func(req *heronv1.CreateNodeRequest) *heronv1.Node {
		t.Helper()
		resp, err := h.admin.CreateNode(t.Context(), connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetNode()
	}
	full := &heronv1.Billing{Price: "12.50", Currency: "USD", BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_MONTHLY, ExpiresOn: "2026-01-10"}
	n := create(&heronv1.CreateNodeRequest{Name: "billed", Billing: full})
	if got := n.GetBilling(); got.GetPrice() != "12.50" || got.GetCurrency() != "USD" || got.GetBillingCycle() != heronv1.BillingCycle_BILLING_CYCLE_MONTHLY ||
		got.GetExpiresOn() != "2026-01-10" || got.GetAutoRenew() || got.GetDaysLeft() != 9 {
		t.Fatalf("created billing = %v", got)
	}
	if n := create(&heronv1.CreateNodeRequest{Name: "plain"}); n.Billing != nil {
		t.Fatalf("node without billing = %v", n.Billing)
	}
	list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || list.Msg.GetNodes()[0].GetBilling().GetDaysLeft() != 9 || list.Msg.GetNodes()[1].Billing != nil {
		t.Fatalf("ListNodes = %v %v", list, err)
	}
}

// 建节点的计费与 UpdateNode 同一处裁决：不合格的取值报同样的错误，节点不建。
func TestCreateNodeRejectsMalformedBilling(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, c := range []struct {
		b    *heronv1.Billing
		want string
	}{
		{&heronv1.Billing{Price: "12.345", Currency: "USD"}, `billing.price: must match ^\d{1,9}(\.\d{1,2})?$, e.g. 12.50; got "12.345"`},
		{&heronv1.Billing{Price: "12"}, "billing.currency: required when billing.price is set"},
		{&heronv1.Billing{ExpiresOn: "2026-02-29"}, `billing.expires_on: must be an existing date in YYYY-MM-DD form; got "2026-02-29"`},
		{&heronv1.Billing{AutoRenew: true, ExpiresOn: "2026-10-01"}, "billing.auto_renew: requires billing.billing_cycle and billing.expires_on to be set"},
	} {
		_, err := h.admin.CreateNode(t.Context(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: "n", Billing: c.b}))
		if codeOf(err) != connect.CodeInvalidArgument || err.Error() != "invalid_argument: "+c.want {
			t.Errorf("%v: err = %v, want %s", c.b, err, c.want)
		}
	}
	list, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || len(list.Msg.GetNodes()) != 0 {
		t.Fatalf("rejected creates left nodes: %v %v", list, err)
	}
}

// 带着计费建节点立刻扫描一次到期：提醒窗口内的新节点不等到零点才触发；开着自动续期且已过期的新节点在响应里就是
// 推后之后的日期，与 UpdateNode 改计费后的行为一致。
func TestCreateNodeSweepsExpiry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	saveRule(t, h, expiryRuleProto())
	resp, err := h.admin.CreateNode(t.Context(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: "expiring",
		Billing: &heronv1.Billing{ExpiresOn: "2026-01-04"}}))
	if err != nil {
		t.Fatal(err)
	}
	events, err := h.admin.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{NodeId: resp.Msg.GetNode().GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if ev := events.Msg.GetEvents(); len(ev) != 1 || ev[0].GetTransition() != "firing" || ev[0].GetSummary() != "节点 expiring 将于 2026-01-04 到期（剩 3 天，规则 到期）" {
		t.Fatalf("events after creating a node inside the window: %v", ev)
	}
	renewed, err := h.admin.CreateNode(t.Context(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: "renewing",
		Billing: &heronv1.Billing{BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_MONTHLY, ExpiresOn: "2025-12-15", AutoRenew: true}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := renewed.Msg.GetNode().GetBilling(); got.GetExpiresOn() != "2026-01-15" || got.GetDaysLeft() != 14 {
		t.Fatalf("auto-renewing node created with a past date = %v", got)
	}
}

// 到期规则经协议保存并回显提前天数；提前天数越界或带着探测字段时，错误以请求路径写明字段与约束。
func TestSaveAlertRuleExpiryKind(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	saved := saveRule(t, h, expiryRuleProto())
	if saved.GetKind() != heronv1.AlertKind_ALERT_KIND_EXPIRY || saved.GetDaysBefore() != 7 {
		t.Fatalf("saved %v", saved)
	}
	list, err := h.admin.ListAlertRules(t.Context(), connect.NewRequest(&heronv1.ListAlertRulesRequest{}))
	if err != nil || len(list.Msg.GetRules()) != 1 || list.Msg.GetRules()[0].GetDaysBefore() != 7 {
		t.Fatalf("listed %v %v", list, err)
	}
	for _, c := range []struct {
		change func(*heronv1.AlertRule)
		want   string
	}{
		{func(r *heronv1.AlertRule) { r.DaysBefore = 0 }, "rule.days_before must be between 1 and 365"},
		{func(r *heronv1.AlertRule) { r.DaysBefore = 366 }, "rule.days_before must be between 1 and 365"},
		{func(r *heronv1.AlertRule) { r.TaskId = 1 }, "rule.task_id must be 0 unless kind is probe or cert_expiry"},
		{func(r *heronv1.AlertRule) { r.Metric = heronv1.ProbeMetric_PROBE_METRIC_LOSS_PCT }, "rule.metric must be unspecified unless kind is probe"},
		{func(r *heronv1.AlertRule) { r.Threshold = 1 }, "rule.threshold must be 0 unless kind is probe, resource or traffic"},
		{func(r *heronv1.AlertRule) { r.ForMinutes = 1 }, "rule.for_minutes must be 0 unless kind is probe or resource"},
	} {
		r := expiryRuleProto()
		c.change(r)
		_, err := h.admin.SaveAlertRule(t.Context(), connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: r}))
		if codeOf(err) != connect.CodeInvalidArgument || err.Error() != "invalid_argument: "+c.want {
			t.Errorf("%v: err = %v, want %s", r, err, c.want)
		}
	}
}

// 离线规则带任一探测字段或提前天数、探测规则带提前天数，都被拒绝，什么也不保存。离线规则带探测字段原来会被存储层
// 静默清零，调用方发了什么、存下的却是零值，无从察觉。
func TestSaveAlertRuleRejectsFieldsOfOtherKinds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	offline := func(change func(*heronv1.AlertRule)) *heronv1.AlertRule { r := offlineRule(); change(r); return r }
	probeWithDays := &heronv1.AlertRule{Name: "丢包", Kind: heronv1.AlertKind_ALERT_KIND_PROBE, Enabled: true, AllNodes: true,
		TaskId: 1, Metric: heronv1.ProbeMetric_PROBE_METRIC_LOSS_PCT, Threshold: 20, ForMinutes: 3, DaysBefore: 7}
	for _, c := range []struct {
		rule *heronv1.AlertRule
		want string
	}{
		{offline(func(r *heronv1.AlertRule) { r.TaskId = 1 }), "rule.task_id must be 0 unless kind is probe or cert_expiry"},
		{offline(func(r *heronv1.AlertRule) { r.Metric = heronv1.ProbeMetric_PROBE_METRIC_RTT_MS }), "rule.metric must be unspecified unless kind is probe"},
		{offline(func(r *heronv1.AlertRule) { r.Threshold = 5 }), "rule.threshold must be 0 unless kind is probe, resource or traffic"},
		{offline(func(r *heronv1.AlertRule) { r.ForMinutes = 3 }), "rule.for_minutes must be 0 unless kind is probe or resource"},
		{offline(func(r *heronv1.AlertRule) { r.DaysBefore = 7 }), "rule.days_before must be 0 unless kind is expiry or cert_expiry"},
		{probeWithDays, "rule.days_before must be 0 unless kind is expiry or cert_expiry"},
	} {
		_, err := h.admin.SaveAlertRule(t.Context(), connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: c.rule}))
		if codeOf(err) != connect.CodeInvalidArgument || err.Error() != "invalid_argument: "+c.want {
			t.Errorf("%v: err = %v, want %s", c.rule, err, c.want)
		}
	}
	list, err := h.admin.ListAlertRules(t.Context(), connect.NewRequest(&heronv1.ListAlertRulesRequest{}))
	if err != nil || len(list.Msg.GetRules()) != 0 {
		t.Fatalf("rejected rules were saved: %v %v", list, err)
	}
}

func TestConstructorsRequireLocation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	cfg := h.svc.cfg
	cfg.Location = nil
	pubCfg := h.pub.cfg
	pubCfg.Location = nil
	for _, c := range []struct {
		want string
		call func()
	}{
		{"api.Config.Location must be set", func() { New(cfg, h.deps()) }},
		{"api.PublicConfig.Location must be set", func() { NewPublic(pubCfg, h.publicDeps()) }},
	} {
		func() {
			defer func() {
				if r := recover(); r != c.want {
					t.Errorf("panic = %v, want %s", r, c.want)
				}
			}()
			c.call()
		}()
	}
}
