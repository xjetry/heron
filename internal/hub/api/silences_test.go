package api

import (
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"google.golang.org/protobuf/proto"
)

func saveSilence(t *testing.T, h *harness, s *heronv1.Silence) *heronv1.Silence {
	t.Helper()
	resp, err := h.admin.SaveSilence(t.Context(), connect.NewRequest(&heronv1.SaveSilenceRequest{Silence: s}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.Silence
}

func listSilences(t *testing.T, h *harness) []*heronv1.SilenceEntry {
	t.Helper()
	resp, err := h.admin.ListSilences(t.Context(), connect.NewRequest(&heronv1.ListSilencesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.Silences
}

// 静默的作用域与告警规则同一形状：selector_tags 读出当前交集的展开，标签变化经 UpdateNode 在同一事务里重算。
func TestSilenceAPIScopeFollowsTagChanges(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a, _ := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	edit := func(id int64, name string, tags ...string) {
		t.Helper()
		_, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: name, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(30), Tags: tags}))
		if err != nil {
			t.Fatal(err)
		}
	}
	edit(a, "a", "db")
	// 夹具时区是 UTC、墙钟 2026-01-01 00:00：00:00–23:59 的每日窗口此刻生效。
	s := saveSilence(t, h, &heronv1.Silence{AllRules: true, Name: "夜间维护", Enabled: true, Kind: heronv1.SilenceKind_SILENCE_KIND_DAILY, StartHhmm: "00:00", EndHhmm: "23:59", SelectorTags: []string{"db"}, Reason: "机房巡检"})
	if s.Id == 0 || !slices.Equal(s.NodeIds, []int64{a}) || !slices.Equal(s.SelectorTags, []string{"db"}) || s.CreatedAt == 0 {
		t.Fatalf("saved=%v", s)
	}
	entries := listSilences(t, h)
	if len(entries) != 1 || !entries[0].Active {
		t.Fatalf("list=%v, want one active entry", entries)
	}
	edit(b, "b", "db")
	if got := listSilences(t, h)[0].Silence.NodeIds; !slices.Equal(got, []int64{a, b}) {
		t.Fatalf("after tagging b: node_ids=%v", got)
	}
	edit(a, "a")
	if got := listSilences(t, h)[0].Silence.NodeIds; !slices.Equal(got, []int64{b}) {
		t.Fatalf("after untagging a: node_ids=%v", got)
	}
	// 被静默引用的标签不能删除；错误带出引用它的静默。
	_, err := h.admin.DeleteTag(t.Context(), connect.NewRequest(&heronv1.DeleteTagRequest{Name: "db"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "夜间维护") {
		t.Fatalf("referenced tag deletion=%v", err)
	}
	s.NodeIds, s.SelectorTags = []int64{b}, nil
	saveSilence(t, h, s)
	if _, err := h.admin.DeleteTag(t.Context(), connect.NewRequest(&heronv1.DeleteTagRequest{Name: "db"})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.DeleteSilence(t.Context(), connect.NewRequest(&heronv1.DeleteSilenceRequest{Id: s.Id})); err != nil {
		t.Fatal(err)
	}
	if got := listSilences(t, h); len(got) != 0 {
		t.Fatalf("after delete: %v", got)
	}
	if _, err := h.admin.DeleteSilence(t.Context(), connect.NewRequest(&heronv1.DeleteSilenceRequest{Id: s.Id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("deleting a missing silence=%v", err)
	}
}

// 端到端：生效的静默让离线触发的事件落库为 silenced 且没有投递；一次性窗口过期后不再抑制。
func TestSilenceSuppressesAlertDeliveriesEndToEnd(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "a")
	c := saveChannel(t, h, webhook("https://example.invalid/hook"))
	saveRule(t, h, &heronv1.AlertRule{Name: "离线", Kind: heronv1.AlertKind_ALERT_KIND_OFFLINE, Enabled: true, AllNodes: true, ChannelIds: []int64{c.Id}})
	s := saveSilence(t, h, &heronv1.Silence{AllRules: true, Name: "维护", Enabled: true, Kind: heronv1.SilenceKind_SILENCE_KIND_ONCE, AllNodes: true, FromAt: h.clk.Now().Add(-time.Hour).Unix(), UntilAt: h.clk.Now().Add(time.Hour).Unix()})
	h.clk.Advance(time.Minute)
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, err := h.admin.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{NodeId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Msg.Events) != 1 || !events.Msg.Events[0].Silenced || len(events.Msg.Events[0].Deliveries) != 0 {
		t.Fatalf("silenced firing=%v", events.Msg.Events)
	}
	// 窗口过期后再次触发照常投递。
	if _, err := h.admin.DeleteSilence(t.Context(), connect.NewRequest(&heronv1.DeleteSilenceRequest{Id: s.Id})); err != nil {
		t.Fatal(err)
	}
	h.live.Observe(id, "", &heronv1.Metrics{})
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(2 * time.Hour)
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, err = h.admin.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{NodeId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Msg.Events) != 3 || events.Msg.Events[0].Silenced || len(events.Msg.Events[0].Deliveries) != 1 {
		t.Fatalf("firing after the silence ended=%v", events.Msg.Events)
	}
}

// 维护状态随 UpdateNode 整体替换，回显在 Node 与公开页的 PublicNode 上。
func TestUpdateNodeMaintenanceRoundTrip(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "a")
	resp, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "a", Public: true, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(30), Maintenance: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Msg.Node.Maintenance {
		t.Fatalf("updated node=%v", resp.Msg.Node)
	}
	nodes, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes.Msg.Nodes) != 1 || !nodes.Msg.Nodes[0].Maintenance {
		t.Fatalf("list=%v", nodes.Msg.Nodes)
	}
	snap, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Msg.Nodes) != 1 || !snap.Msg.Nodes[0].Maintenance {
		t.Fatalf("public snapshot=%v", snap.Msg.Nodes)
	}
	resp, err = h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "a", Public: true, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(30)}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Node.Maintenance {
		t.Fatalf("maintenance not cleared: %v", resp.Msg.Node)
	}
}

// ListSilences 读库：维护任务删掉到期的一次性静默后（不经引擎），列表不再显示它，而不是等 hub 重启。
func TestListSilencesReadsTheStoreAfterPruning(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "a")
	s := saveSilence(t, h, &heronv1.Silence{AllRules: true, Name: "过期窗口", Enabled: true, Kind: heronv1.SilenceKind_SILENCE_KIND_ONCE, NodeIds: []int64{id}, FromAt: h.clk.Now().Add(-200 * 24 * time.Hour).Unix(), UntilAt: h.clk.Now().Add(-100 * 24 * time.Hour).Unix()})
	keep := saveSilence(t, h, &heronv1.Silence{AllRules: true, Name: "夜间维护", Enabled: true, Kind: heronv1.SilenceKind_SILENCE_KIND_DAILY, StartHhmm: "22:00", EndHhmm: "06:00", AllNodes: true})
	if got := listSilences(t, h); len(got) != 2 {
		t.Fatalf("before pruning: %v", got)
	}
	if _, err := h.store.PruneAlertEvents(t.Context(), h.clk.Now().Add(-90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := listSilences(t, h)
	if len(got) != 1 || got[0].Silence.Id != keep.Id {
		t.Fatalf("after pruning the expired once silence %d is still listed: %v", s.Id, got)
	}
}

func TestSaveSilenceValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "a")
	valid := func() *heronv1.Silence {
		return &heronv1.Silence{AllRules: true, Name: "维护", Enabled: true, Kind: heronv1.SilenceKind_SILENCE_KIND_DAILY, StartHhmm: "22:00", EndHhmm: "06:00", NodeIds: []int64{id}}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*heronv1.Silence)
	}{
		{"kind unspecified", func(s *heronv1.Silence) { s.Kind = heronv1.SilenceKind_SILENCE_KIND_UNSPECIFIED }},
		{"daily with once fields", func(s *heronv1.Silence) { s.FromAt = 1 }},
		{"bad hhmm", func(s *heronv1.Silence) { s.StartHhmm = "9:00" }},
		{"same start and end", func(s *heronv1.Silence) { s.EndHhmm = "22:00" }},
		{"empty scope", func(s *heronv1.Silence) { s.NodeIds = nil }},
		{"empty name", func(s *heronv1.Silence) { s.Name = "" }},
		{"all nodes with explicit ids", func(s *heronv1.Silence) { s.AllNodes = true }},
		{"missing node", func(s *heronv1.Silence) { s.NodeIds = []int64{999} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := valid()
			tc.mutate(s)
			_, err := h.admin.SaveSilence(t.Context(), connect.NewRequest(&heronv1.SaveSilenceRequest{Silence: s}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument && connect.CodeOf(err) != connect.CodeNotFound {
				t.Fatalf("err=%v, want invalid_argument or not_found", err)
			}
		})
	}
	if got := saveSilence(t, h, valid()); got.Id == 0 || got.Kind != heronv1.SilenceKind_SILENCE_KIND_DAILY {
		t.Fatalf("valid silence rejected: %v", got)
	}
}

func TestSilenceAPIRuleScopeAndClock(t *testing.T) {
	h := newHarness(t, "")
	h.login(t)
	r := saveRule(t, h, &heronv1.AlertRule{Name: "离线", Kind: heronv1.AlertKind_ALERT_KIND_OFFLINE, Enabled: true, AllNodes: true})
	now := h.clk.Now().Unix()
	s := saveSilence(t, h, &heronv1.Silence{Name: "维护", Enabled: true, AllNodes: true, RuleIds: []int64{r.Id}, Kind: heronv1.SilenceKind_SILENCE_KIND_ONCE, FromAt: now, UntilAt: now + 1})
	if s.AllRules || !slices.Equal(s.RuleIds, []int64{r.Id}) {
		t.Fatalf("saved rules=%v", s)
	}
	check := func(wantNow int64, active bool) {
		t.Helper()
		resp, err := h.admin.ListSilences(t.Context(), connect.NewRequest(&heronv1.ListSilencesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Msg.Now != wantNow || len(resp.Msg.Silences) != 1 || resp.Msg.Silences[0].Active != active {
			t.Fatalf("now/active=%v, want now=%d active=%v", resp.Msg, wantNow, active)
		}
	}
	check(now, true)
	h.clk.Advance(time.Second)
	check(now+1, false)
	s.UntilAt = now + 100
	saveSilence(t, h, s)
	check(now+1, true)
	if _, err := h.admin.DeleteAlertRule(t.Context(), connect.NewRequest(&heronv1.DeleteAlertRuleRequest{Id: r.Id})); err != nil {
		t.Fatal(err)
	}
	check(now+1, false)
	if got := listSilences(t, h)[0].Silence; got.AllRules || len(got.RuleIds) != 0 {
		t.Fatalf("deleted rules=%v", got)
	}
}

func TestSilenceAPIRejectsMissingOrInvalidRules(t *testing.T) {
	h := newHarness(t, "")
	h.login(t)
	for _, tc := range []struct {
		name string
		all  bool
		ids  []int64
		want string
	}{
		{"old client", false, nil, "all_rules"},
		{"conflicting fields", true, []int64{999}, "exactly one of all_rules or non-empty rule_ids is required"},
		{"unknown rule", false, []int64{999}, "rule 999 does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.admin.SaveSilence(t.Context(), connect.NewRequest(&heronv1.SaveSilenceRequest{Silence: &heronv1.Silence{Name: "维护", AllNodes: true, AllRules: tc.all, RuleIds: tc.ids, Kind: heronv1.SilenceKind_SILENCE_KIND_ONCE, FromAt: 1, UntilAt: 2}}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want InvalidArgument containing %q", err, tc.want)
			}
		})
	}
}

type silenceStepClock struct {
	clock.Clock
	now time.Time
}

func (c *silenceStepClock) Now() time.Time {
	now := c.now
	c.now = c.now.Add(time.Second)
	return now
}

func TestSilenceListUsesOneClockSample(t *testing.T) {
	h := newHarness(t, "")
	h.login(t)
	now := h.clk.Now()
	saveSilence(t, h, &heronv1.Silence{Name: "维护", Enabled: true, AllNodes: true, AllRules: true, Kind: heronv1.SilenceKind_SILENCE_KIND_ONCE, FromAt: now.Unix(), UntilAt: now.Unix() + 1})
	// 每次读钟跨过一个窗口边界；响应时间和 active 若各读一次，就不可能同时匹配起点。
	svc := &Service{store: h.store, cfg: h.svc.cfg, clk: &silenceStepClock{Clock: h.clk, now: now}}
	resp, err := svc.ListSilences(t.Context(), connect.NewRequest(&heronv1.ListSilencesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Now != now.Unix() || len(resp.Msg.Silences) != 1 || !resp.Msg.Silences[0].Active {
		t.Fatalf("inconsistent clock sample: %v", resp.Msg)
	}
}
