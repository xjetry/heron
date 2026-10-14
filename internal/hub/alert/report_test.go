package alert

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
)

// 投递时刻是当天本地时间不小于 hour:00 的第一个存在时刻。time.Date 对间隙往回（New York、跨午夜的 Santiago）与往前
// （Lord Howe、Beirut）两种归一、对重复时段给第一次（New York）与第二次（London）两种选择都要落到同一个答案上。
func TestReportSendMomentAcrossDST(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		zone string
		day  time.Time
		hour int
		want time.Time
	}{
		{"Asia/Tokyo", date("2026-10-12"), 9, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)},
		{"America/New_York", date("2026-03-08"), 2, time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)},
		{"Australia/Lord_Howe", date("2026-10-04"), 2, time.Date(2026, 10, 3, 15, 30, 0, 0, time.UTC)},
		{"America/Santiago", date("2026-09-06"), 0, time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)},
		{"Asia/Beirut", date("2026-03-29"), 0, time.Date(2026, 3, 28, 22, 0, 0, 0, time.UTC)},
		{"Europe/London", date("2026-10-25"), 1, time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)},
		{"America/New_York", date("2026-11-01"), 1, time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)},
	} {
		if got := ReportSendMoment(c.day, c.hour, zone(t, c.zone)); !got.Equal(c.want) {
			t.Errorf("%s %s %02d:00: got %s, want %s", c.zone, c.day.Format(time.DateOnly), c.hour, got.UTC(), c.want)
		}
	}
}

// 周期边界按 hub 时区：Asia/Tokyo 的周一 08:30 在 UTC 里还是周日 23:30，按 UTC 取日会把日报记成周日、周报记成上周一。
func TestLatestReportPeriodUsesHubTimezone(t *testing.T) {
	t.Parallel()
	tokyo := zone(t, "Asia/Tokyo")
	for _, c := range []struct {
		name    string
		now     time.Time
		cadence store.ReportCadence
		want    time.Time
	}{
		{"daily after the hour", time.Date(2026, 10, 11, 23, 30, 0, 0, time.UTC), store.ReportDaily, date("2026-10-12")},
		{"weekly on Monday after the hour", time.Date(2026, 10, 11, 23, 30, 0, 0, time.UTC), store.ReportWeekly, date("2026-10-12")},
		{"monthly mid-month", time.Date(2026, 10, 11, 23, 30, 0, 0, time.UTC), store.ReportMonthly, date("2026-10-01")},
		{"daily before the hour", time.Date(2026, 10, 11, 22, 59, 0, 0, time.UTC), store.ReportDaily, date("2026-10-11")},
		{"weekly on Monday before the hour", time.Date(2026, 10, 11, 22, 59, 0, 0, time.UTC), store.ReportWeekly, date("2026-10-05")},
		{"monthly on the 1st before the hour", time.Date(2026, 10, 31, 22, 59, 0, 0, time.UTC), store.ReportMonthly, date("2026-10-01")},
		{"monthly on the 1st after the hour", time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC), store.ReportMonthly, date("2026-11-01")},
		{"weekly across a year", time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC), store.ReportWeekly, date("2026-12-28")},
	} {
		got := LatestReportPeriod(c.cadence, c.now, 8, tokyo)
		if got.Cadence != c.cadence || !got.Key.Equal(c.want) {
			t.Errorf("%s: got %s %s, want %s", c.name, got.Cadence, got.Key.Format(time.DateOnly), c.want.Format(time.DateOnly))
		}
	}
}

func reportRow(name string, used, quota uint64) ReportRow {
	r := ReportRow{Name: name, UsedBytes: used, QuotaBytes: quota, Mode: "sum", DaysLeft: 3}
	if quota > 0 {
		r.Percent = float64(used) / float64(quota) * 100
	}
	return r
}

// 节点按配额占比降序、再按用量降序；未设配额的排在设了配额的之后。21 台只列 20 台，余下写"另外 1 台"。
func TestTrafficReportTextOrdersAndCaps(t *testing.T) {
	t.Parallel()
	rows := []ReportRow{reportRow("no-quota-big", 900, 0), reportRow("half", 50, 100), reportRow("full", 100, 100), reportRow("half-bigger", 500, 1000)}
	for i := range 17 {
		rows = append(rows, reportRow(fmt.Sprintf("low%02d", i), uint64(i), 1000))
	}
	now := time.Date(2026, 10, 12, 0, 30, 0, 0, time.UTC)
	text := TrafficReportText([]store.ReportPeriod{{Cadence: store.ReportDaily}, {Cadence: store.ReportWeekly}}, rows, now, zone(t, "Asia/Tokyo"))
	lines := strings.Split(text, "\n")
	if len(lines) != 1+maxListedNodes+1 {
		t.Fatalf("%d lines, want header + %d nodes + rest:\n%s", len(lines), maxListedNodes, text)
	}
	if lines[0] != "流量报告（日报、周报）2026-10-12 Asia/Tokyo，共 21 台节点" {
		t.Errorf("header = %q", lines[0])
	}
	for i, prefix := range []string{"full：", "half-bigger：", "half："} {
		if !strings.HasPrefix(lines[1+i], prefix) {
			t.Errorf("line %d = %q, want it to start with %q", 1+i, lines[1+i], prefix)
		}
	}
	if lines[1] != "full：100 B / 100 B（100.00%，收+发），3 天后重置" {
		t.Errorf("quota line = %q", lines[1])
	}
	if strings.Contains(text, "no-quota-big") {
		t.Errorf("the node without a quota must sort after every node with one and fall past the cap:\n%s", text)
	}
	if last := lines[len(lines)-1]; last != "另外 1 台" {
		t.Errorf("last line = %q, want 另外 1 台", last)
	}
	short := TrafficReportText([]store.ReportPeriod{{Cadence: store.ReportMonthly}}, rows[:1], now, time.UTC)
	if want := "流量报告（月报）2026-10-12 UTC，共 1 台节点\nno-quota-big：900 B，未设配额，3 天后重置"; short != want {
		t.Errorf("short report = %q, want %q", short, want)
	}
}

// 每行的用量取已提交观测、不重算；周期已经结束的观测与没有观测的节点都是新周期的零用量。距重置的天数按 hub 时区的日历。
func TestReportRowsReadCommittedPeriod(t *testing.T) {
	t.Parallel()
	tokyo := zone(t, "Asia/Tokyo")
	now := time.Date(2026, 10, 11, 23, 30, 0, 0, time.UTC) // 东京 10-12 08:30
	nodes := []store.Node{
		{ID: 1, Name: "current", TrafficResetDay: 15, TrafficQuotaBytes: 1000, TrafficQuotaMode: "rx"},
		{ID: 2, Name: "stale", TrafficResetDay: 1, TrafficQuotaBytes: 1000, TrafficQuotaMode: "sum"},
		{ID: 3, Name: "missing", TrafficResetDay: 12, TrafficQuotaMode: "sum"},
	}
	committed := map[int64]traffic.State{
		1: {PeriodRx: 300, PeriodTx: 999, PeriodStart: time.Date(2026, 9, 15, 0, 0, 0, 0, tokyo)},
		2: {PeriodRx: 700, PeriodTx: 200, PeriodStart: time.Date(2026, 9, 1, 0, 0, 0, 0, tokyo)},
	}
	rows := ReportRows(nodes, committed, now, tokyo)
	want := []ReportRow{
		{Name: "current", UsedBytes: 300, QuotaBytes: 1000, Mode: "rx", Percent: 30, DaysLeft: 3},
		{Name: "stale", UsedBytes: 0, QuotaBytes: 1000, Mode: "sum", Percent: 0, DaysLeft: 20},
		{Name: "missing", UsedBytes: 0, Mode: "sum", DaysLeft: 31},
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

type recordingSender struct {
	mu  sync.Mutex
	got []store.AlertEvent
}

func (s *recordingSender) Enqueue(ev store.AlertEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, ev)
}

func (s *recordingSender) events() []store.AlertEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.AlertEvent(nil), s.got...)
}

func newReporter(t *testing.T, f *fixture, loc *time.Location, sender Sender) *Reporter {
	t.Helper()
	book := traffic.New(f.st, f.clk, loc, f.log)
	must(t, book.Load(t.Context()))
	return NewReporter(ReportDeps{Store: f.st, Traffic: book, Sender: sender, Location: loc, Clock: f.clk, Log: f.log})
}

func reportEvents(t *testing.T, f *fixture) []store.AlertEvent {
	t.Helper()
	var out []store.AlertEvent
	for _, ev := range f.events(t) {
		if ev.Transition == store.TransitionTrafficReport {
			out = append(out, ev)
		}
	}
	return out
}

// 调度：到期的周期合成一条报告；同一期不再发，hub 重启（新的 Reporter 读回库里的标记）也不再发；停机跨过几期只补发
// 最近一期；墙钟回拨不补发已过去的一期；关闭即不发。
func TestReporterSchedule(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tokyo := zone(t, "Asia/Tokyo")
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "hook", Kind: store.ChannelWebhook, Config: `{"url":"https://hooks.example/r","method":"POST"}`})
	must(t, err)
	sender := &recordingSender{}
	r := newReporter(t, f, tokyo, sender)
	check := func(at time.Time) {
		t.Helper()
		f.clk.SetWall(at)
		must(t, r.Check(t.Context()))
	}
	// 关闭时什么都不发。
	check(time.Date(2026, 10, 12, 0, 30, 0, 0, time.UTC))
	if evs := reportEvents(t, f); len(evs) != 0 {
		t.Fatalf("disabled report produced %d events", len(evs))
	}
	_, err = f.st.SaveSettings(t.Context(), store.SettingsUpdate{TrafficReport: &store.TrafficReportUpdate{Enabled: true, Daily: true, Weekly: true, Hour: 9, Channels: []int64{c.ID}}})
	must(t, err)
	// 东京周一 09:30：日报与周报都到期，合成一条。
	check(time.Date(2026, 10, 12, 0, 30, 0, 0, time.UTC))
	evs := reportEvents(t, f)
	if len(evs) != 1 || !strings.HasPrefix(evs[0].Summary, "流量报告（日报、周报）2026-10-12 Asia/Tokyo，共 2 台节点") || len(evs[0].Deliveries) != 1 {
		t.Fatalf("first report: %+v", evs)
	}
	if got := sender.events(); len(got) != 1 || got[0].ID != evs[0].ID {
		t.Fatalf("enqueued %+v, want the report event", got)
	}
	// 同一期不再发；重启后读回标记同样不发。
	check(time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC))
	r = newReporter(t, f, tokyo, sender)
	check(time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC))
	if n := len(reportEvents(t, f)); n != 1 {
		t.Fatalf("after recheck and restart: %d report events, want 1", n)
	}
	// 次日 08:59 未到，09:00 到期，只有日报。
	check(time.Date(2026, 10, 12, 23, 59, 0, 0, time.UTC))
	if n := len(reportEvents(t, f)); n != 1 {
		t.Fatalf("before the hour: %d report events, want 1", n)
	}
	check(time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC))
	if evs := reportEvents(t, f); len(evs) != 2 || !strings.HasPrefix(evs[0].Summary, "流量报告（日报）2026-10-13") {
		t.Fatalf("next day: %+v", evs)
	}
	// 停机跨过三天与一个周一：只补发最近一期，一条报告覆盖日报与周报。
	check(time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC))
	evs = reportEvents(t, f)
	if len(evs) != 3 || !strings.HasPrefix(evs[0].Summary, "流量报告（日报、周报）2026-10-20") {
		t.Fatalf("after downtime: %+v", evs)
	}
	sent, err := f.st.TrafficReportSent(t.Context())
	must(t, err)
	if !sent[store.ReportDaily].Equal(date("2026-10-20")) || !sent[store.ReportWeekly].Equal(date("2026-10-19")) {
		t.Fatalf("markers after downtime = %v", sent)
	}
	// 墙钟回拨到前一天的投递时刻之后：那一期早于已记的一期，不补发。
	check(time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC))
	if n := len(reportEvents(t, f)); n != 3 {
		t.Fatalf("after the wall clock went back: %d report events, want 3", n)
	}
}

// 报告与登录通知是两种系统事件：发往同一个渠道也各自成批、各发一条，消息的种类、标签与正文各是各的；Telegram 发
// 报告全文，Webhook 默认模板里 kind 与 transition 都是 traffic_report、summary 是报告全文。
func TestTrafficReportAndLoginAreDeliveredSeparately(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	base, tg := newTelegramServer(t, f, nil)
	var mu sync.Mutex
	var hooks []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Errorf("webhook body %q: %v", body, err)
		}
		mu.Lock()
		hooks = append(hooks, m)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	tgc := telegramChannel(t, f, 0)
	hook, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "hook", Kind: store.ChannelWebhook, Config: fmt.Sprintf(`{"url":%q,"method":"POST"}`, srv.URL)})
	must(t, err)
	both := []int64{tgc.ID, hook.ID}
	_, err = f.st.SaveSettings(t.Context(), store.SettingsUpdate{LoginChannels: &both, TrafficReport: &store.TrafficReportUpdate{Enabled: true, Daily: true, Channels: both}})
	must(t, err)
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, new([]time.Duration))}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	stop := startQueue(t, q)
	defer stop()
	login, err := f.st.RecordLoginEvent(t.Context(), store.AlertEvent{At: f.clk.Now(), Transition: store.TransitionLoginSuccess, Summary: "管理员登录成功（密码）"})
	must(t, err)
	q.Enqueue(login)
	must(t, newReporter(t, f, zone(t, "Asia/Tokyo"), q).Check(t.Context()))
	awaitSettled(t, f)
	var texts []string
	for _, m := range tg.messages() {
		texts = append(texts, m.text)
	}
	if len(texts) != 2 || texts[0] != "管理员登录成功（密码）" || !strings.HasPrefix(texts[1], "流量报告（日报）") || !strings.Contains(texts[1], "node1：") {
		t.Fatalf("telegram messages = %q, want the login line and the full report, one each", texts)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hooks) != 2 {
		t.Fatalf("webhook got %d requests, want 2: %v", len(hooks), hooks)
	}
	for i, want := range []struct{ kind, transition, summary string }{
		{store.SystemKindLogin, string(store.TransitionLoginSuccess), "管理员登录成功（密码）"},
		{store.SystemKindTrafficReport, string(store.TransitionTrafficReport), texts[1]},
	} {
		if h := hooks[i]; h["kind"] != want.kind || h["transition"] != want.transition || h["summary"] != want.summary || h["node"] != "Hub" {
			t.Errorf("webhook %d = %v, want kind %q transition %q", i, h, want.kind, want.transition)
		}
	}
}
