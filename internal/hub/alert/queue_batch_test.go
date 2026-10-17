package alert

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

type sentMessage struct {
	text string
	at   time.Time
}

// telegramServer 是假的 Telegram：记下每条 sendMessage 的正文与到达时的夹具墙钟。reply 按到达序号（从 0 起）决定应答，
// 为 nil 或不写状态码时是 200。
type telegramServer struct {
	mu    sync.Mutex
	got   []sentMessage
	reply func(n int, w http.ResponseWriter)
}

func newTelegramServer(t *testing.T, f *fixture, reply func(n int, w http.ResponseWriter)) (string, *telegramServer) {
	t.Helper()
	s := &telegramServer{reply: reply}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		s.mu.Lock()
		n := len(s.got)
		s.got = append(s.got, sentMessage{body.Text, f.clk.Now()})
		s.mu.Unlock()
		if s.reply != nil {
			s.reply(n, w)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, s
}

func (s *telegramServer) messages() []sentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func telegramChannel(t *testing.T, f *fixture, rate int) store.NotifyChannel {
	t.Helper()
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "tg", Kind: store.ChannelTelegram, Config: `{"bot_token":"tok","chat_id":"chat"}`, RatePerMinute: rate})
	must(t, err)
	return c
}

// nodes 把夹具补足到 n 个节点，新节点名沿夹具的 node1、node2 往下排（与 grace 改写的名字相同），返回全部节点 id。
func (f *fixture) nodes(t *testing.T, n int) []int64 {
	t.Helper()
	for i := len(f.ids); i < n; i++ {
		id, _, err := f.st.CreateNode(t.Context(), store.NewNode{Name: fmt.Sprintf("node%d", i+1)}, []byte(fmt.Sprintf("hash%d", i)))
		must(t, err)
		f.ids = append(f.ids, id)
	}
	return f.ids
}

func offlineRuleTo(t *testing.T, f *fixture, name string, channels ...store.NotifyChannel) store.AlertRule {
	t.Helper()
	r := offline()
	r.Name = name
	for _, c := range channels {
		r.ChannelIDs = append(r.ChannelIDs, c.ID)
	}
	return f.rule(t, r)
}

// wire 让引擎的转换经真实队列投递；返回的 sleeps 在 stop 之后读。
func wire(t *testing.T, f *fixture, base string) (*Queue, *[]time.Duration, func()) {
	t.Helper()
	sleeps := new([]time.Duration)
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	f.e.SetSender(q)
	return q, sleeps, startQueue(t, q)
}

// awaitSettled 等到库里没有待投递的批次：一次巡检的全部投递都已终态。失败时列出仍未终态的行。
func awaitSettled(t *testing.T, f *fixture) {
	t.Helper()
	testwait.Until(t, time.Millisecond, func() bool {
		pending, err := f.st.PendingBatches(t.Context())
		must(t, err)
		return len(pending) == 0
	}, "deliveries did not settle: %s", testwait.When(func() string {
		var open []store.Delivery
		for _, ev := range f.events(t) {
			for _, d := range ev.Deliveries {
				if !d.Done {
					open = append(open, d)
				}
			}
		}
		return fmt.Sprintf("unfinished rows %+v", open)
	}))
}

// deliveriesOf 是事件在给定渠道上的投递行，按事件 id 升序。
func deliveriesOf(t *testing.T, f *fixture, channel int64) []store.Delivery {
	t.Helper()
	var out []store.Delivery
	events := f.events(t)
	slices.Reverse(events)
	for _, ev := range events {
		for _, d := range ev.Deliveries {
			if d.ChannelID == channel {
				out = append(out, d)
			}
		}
	}
	return out
}

// fireAll 让从未上报的节点在同一次巡检里一起进入 firing：宽限为 0 时以 TTL（30 s）为准。
func fireAll(t *testing.T, f *fixture) {
	t.Helper()
	f.sweep(t)
	f.clk.Advance(31 * time.Second)
	f.sweep(t)
}

// 同一次巡检里同一规则的三个节点同时 firing：Telegram 只收到一条消息，文案列出三个节点；三行投递共享批次，
// 尝试次数与结果一起写。
func TestTelegramMergesOneSweepIntoOneMessage(t *testing.T) {
	f := newFixture(t)
	f.nodes(t, 3)
	base, tg := newTelegramServer(t, f, nil)
	c := telegramChannel(t, f, 20)
	offlineRuleTo(t, f, "离线", c)
	_, _, stop := wire(t, f, base)
	fireAll(t, f)
	awaitSettled(t, f)
	stop()
	got := tg.messages()
	if len(got) != 1 {
		t.Fatalf("telegram messages=%d want 1: %+v", len(got), got)
	}
	for _, want := range []string{"规则 离线：3 台节点触发告警", "节点 node1 离线", "节点 node2 离线", "节点 node3 离线"} {
		if !strings.Contains(got[0].text, want) {
			t.Errorf("merged message lacks %q:\n%s", want, got[0].text)
		}
	}
	ds := deliveriesOf(t, f, c.ID)
	if len(ds) != 3 {
		t.Fatalf("deliveries=%+v want 3", ds)
	}
	for _, d := range ds {
		if d.BatchID != ds[0].ID || d.Attempts != 1 || !d.OK || !d.Done || !d.DeliveredAt.Equal(ds[0].DeliveredAt) {
			t.Errorf("row %+v does not share batch %d and its result", d, ds[0].ID)
		}
	}
}

// firing 与 recovered 不混：一次巡检里三个节点触发、一个节点恢复，发两条——合并的触发一条，恢复单独一条。
func TestTelegramKeepsFiringAndRecoveredApart(t *testing.T) {
	f := newFixture(t)
	ids := f.nodes(t, 4)
	for _, id := range ids[:3] {
		f.grace(t, id, 120)
	}
	base, tg := newTelegramServer(t, f, nil)
	c := telegramChannel(t, f, 20)
	offlineRuleTo(t, f, "离线", c)
	_, _, stop := wire(t, f, base)
	fireAll(t, f) // 只有宽限为 0 的 node4 触发。
	awaitSettled(t, f)
	f.clk.Advance(90 * time.Second)
	f.l.Observe(ids[3], "", &heronv1.Metrics{})
	f.sweep(t)
	awaitSettled(t, f)
	stop()
	got := tg.messages()
	if len(got) != 3 {
		t.Fatalf("telegram messages=%d want 3 (node4 firing, then merged firing and recovered): %+v", len(got), got)
	}
	var merged, recovered int
	for _, m := range got[1:] {
		switch {
		case strings.Contains(m.text, "规则 离线：3 台节点触发告警"):
			merged++
			for _, name := range []string{"node1", "node2", "node3"} {
				if !strings.Contains(m.text, "节点 "+name+" 离线") {
					t.Errorf("merged firing lacks %s:\n%s", name, m.text)
				}
			}
			if strings.Contains(m.text, "node4") {
				t.Errorf("merged firing mentions the recovered node:\n%s", m.text)
			}
		case m.text == "节点 node4 已恢复上报（规则 离线），告警持续 1 分钟":
			recovered++
		}
	}
	if merged != 1 || recovered != 1 {
		t.Fatalf("second sweep sent %q and %q, want one merged firing and the lone recovery", got[1].text, got[2].text)
	}
}

// 合并的键含规则：同一次巡检里两条规则对同一批节点触发，各发一条。
func TestTelegramDoesNotMergeAcrossRules(t *testing.T) {
	f := newFixture(t)
	base, tg := newTelegramServer(t, f, nil)
	c := telegramChannel(t, f, 20)
	offlineRuleTo(t, f, "甲", c)
	offlineRuleTo(t, f, "乙", c)
	_, _, stop := wire(t, f, base)
	fireAll(t, f)
	awaitSettled(t, f)
	stop()
	got := tg.messages()
	if len(got) != 2 || !strings.Contains(got[0].text, "规则 甲：2 台节点") || !strings.Contains(got[1].text, "规则 乙：2 台节点") {
		t.Fatalf("messages=%+v, want one merged message per rule", got)
	}
}

// 一条消息至多逐行列出 20 个节点，其余写"另外 N 台"。
func TestTelegramMergedMessageListsAtMostTwentyNodes(t *testing.T) {
	f := newFixture(t)
	f.nodes(t, 25)
	base, tg := newTelegramServer(t, f, nil)
	c := telegramChannel(t, f, 20)
	offlineRuleTo(t, f, "离线", c)
	_, _, stop := wire(t, f, base)
	fireAll(t, f)
	awaitSettled(t, f)
	stop()
	got := tg.messages()
	if len(got) != 1 {
		t.Fatalf("telegram messages=%d want 1", len(got))
	}
	lines := strings.Split(got[0].text, "\n")
	listed := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "节点 ") && strings.Contains(l, " 离线") {
			listed++
		}
	}
	if lines[0] != "规则 离线：25 台节点触发告警" || listed != 20 || lines[len(lines)-1] != "另外 5 台" || len(lines) != 22 {
		t.Fatalf("merged message lists %d nodes in %d lines:\n%s", listed, len(lines), got[0].text)
	}
}

// Webhook 不合并：同一次巡检的三个事件是三次请求，各自一批，请求体仍是逐事件的模板。
func TestWebhookSendsOneRequestPerEvent(t *testing.T) {
	f := newFixture(t)
	f.nodes(t, 3)
	var mu sync.Mutex
	var bodies []receivedMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m receivedMessage
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Error(err)
		}
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
	}))
	defer srv.Close()
	c := queueChannel(t, f, srv.URL)
	offlineRuleTo(t, f, "离线", c)
	_, _, stop := wire(t, f, "")
	fireAll(t, f)
	awaitSettled(t, f)
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("webhook requests=%d want 3", len(bodies))
	}
	nodes := map[string]bool{}
	for _, b := range bodies {
		nodes[b.Node] = true
		if b.Transition != "firing" || !strings.HasPrefix(b.Summary, "节点 "+b.Node+" 离线") {
			t.Errorf("webhook body=%+v, want one event per request", b)
		}
	}
	if len(nodes) != 3 {
		t.Errorf("webhook nodes=%v want three distinct", nodes)
	}
	for _, d := range deliveriesOf(t, f, c.ID) {
		if d.BatchID != d.ID {
			t.Errorf("webhook row %+v shares a batch", d)
		}
	}
}

// 重启后未完成的批次按原批次重投，不重新合并：两次巡检各开一批（两台一起触发、一台随后触发），重启后仍是两条消息，
// 各自就是崩溃前定下的那一批；已开始过一次尝试的批次接着计数。
func TestRestartResendsPersistedBatchesWithoutRemerging(t *testing.T) {
	f := newFixture(t)
	ids := f.nodes(t, 3)
	f.grace(t, ids[2], 60)
	c := telegramChannel(t, f, 20)
	offlineRuleTo(t, f, "离线", c)
	// 不装配 Sender：转换照常落库，投递行留待重启后的 Requeue。
	fireAll(t, f)
	f.clk.Advance(30 * time.Second)
	f.sweep(t)
	ds := deliveriesOf(t, f, c.ID)
	if len(ds) != 3 || ds[0].BatchID != ds[1].BatchID || ds[2].BatchID == ds[0].BatchID {
		t.Fatalf("rows before restart=%+v, want node1 and node2 in one batch and node3 in another", ds)
	}
	// 崩溃前第一批已开始一次尝试，结果没落盘。
	if _, err := f.st.BeginBatchAttempt(t.Context(), ds[0].BatchID, []int64{ds[0].ID, ds[1].ID}); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	base, tg := newTelegramServer(t, f, nil)
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	must(t, q.Requeue(t.Context()))
	stop := startQueue(t, q)
	awaitSettled(t, f)
	stop()
	got := tg.messages()
	if len(got) != 2 {
		t.Fatalf("messages after restart=%d want 2 (the two persisted batches): %+v", len(got), got)
	}
	if !strings.Contains(got[0].text, "规则 离线：2 台节点触发告警") || !strings.Contains(got[0].text, "节点 node1 离线") || !strings.Contains(got[0].text, "节点 node2 离线") || strings.Contains(got[0].text, "node3") {
		t.Errorf("first batch resent as:\n%s", got[0].text)
	}
	if !strings.HasPrefix(got[1].text, "节点 node3 离线") {
		t.Errorf("second batch resent as:\n%s", got[1].text)
	}
	for i, d := range deliveriesOf(t, f, c.ID) {
		want := 1
		if i < 2 {
			want = 2
		}
		if !d.OK || d.Attempts != want {
			t.Errorf("row %d after restart=%+v, want delivered with %d attempts", i, d, want)
		}
	}
}

// 重试按批次：合并的消息第一次 500，第二次仍是同一条合并消息，三行一起记两次尝试与成功。
func TestRetryResendsWholeBatch(t *testing.T) {
	f := newFixture(t)
	f.nodes(t, 3)
	base, tg := newTelegramServer(t, f, func(n int, w http.ResponseWriter) {
		if n == 0 {
			w.WriteHeader(500)
		}
	})
	c := telegramChannel(t, f, 20)
	offlineRuleTo(t, f, "离线", c)
	_, sleeps, stop := wire(t, f, base)
	fireAll(t, f)
	awaitSettled(t, f)
	stop()
	got := tg.messages()
	if len(got) != 2 || got[0].text != got[1].text || !strings.Contains(got[0].text, "3 台节点触发告警") {
		t.Fatalf("messages=%+v, want the merged message sent twice", got)
	}
	for _, d := range deliveriesOf(t, f, c.ID) {
		if d.Attempts != 2 || !d.OK || !d.Done || d.Failure != store.FailureNone {
			t.Errorf("row after batch retry=%+v, want 2 attempts and success", d)
		}
	}
	if !slices.Equal(*sleeps, []time.Duration{time.Second}) {
		t.Errorf("sleeps=%v want [1s]", *sleeps)
	}
}

// recordBatches 直接落 n 个各自成批的事件（每个一条规则×节点转换），返回它们供 Enqueue。
func recordBatches(t *testing.T, f *fixture, c store.NotifyChannel, n int) []store.AlertEvent {
	t.Helper()
	r := f.rule(t, offline())
	out := make([]store.AlertEvent, n)
	for i := range out {
		ev, err := f.st.RecordTransition(t.Context(), r.ID, f.ids[0], store.StateFiring, "", time.Time{},
			store.AlertEvent{At: f.clk.Now(), Transition: store.TransitionFiring, Summary: fmt.Sprintf("event %02d", i)}, []store.DeliveryTarget{{ChannelID: c.ID}})
		must(t, err)
		out[i] = ev
	}
	return out
}

// 节奏 20/分钟：同一分钟里就绪的 25 个批次只发 20 条，其余排到下一分钟发出，不丢弃。
func TestChannelRateDefersExcessBatchesToNextMinute(t *testing.T) {
	f := newFixture(t)
	base, tg := newTelegramServer(t, f, nil)
	c := telegramChannel(t, f, 20)
	events := recordBatches(t, f, c, 25)
	var sleeps []time.Duration
	q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	start := f.clk.Now()
	for _, ev := range events {
		q.Enqueue(ev)
	}
	stop := startQueue(t, q)
	awaitSettled(t, f)
	stop()
	got := tg.messages()
	var first, next int
	for _, m := range got {
		switch {
		case m.at.Equal(start):
			first++
		case m.at.Equal(start.Add(time.Minute)):
			next++
		}
	}
	if len(got) != 25 || first != 20 || next != 5 {
		t.Fatalf("sent %d messages, %d in the first minute and %d a minute later; want 25 = 20 + 5", len(got), first, next)
	}
	if !slices.Equal(sleeps, []time.Duration{time.Minute}) {
		t.Errorf("sleeps=%v want [1m]", sleeps)
	}
}

// 429 的 Retry-After（秒或 HTTP 日期）纳入退避，上限 5 分钟；缺失或读不懂时沿用固定退避。
func TestRetryAfterExtendsBackoff(t *testing.T) {
	// 与 newFixture 的起始墙钟相同：HTTP 日期写法相对夹具的当前时刻。
	start := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, header string
		want         time.Duration
	}{
		{"seconds", "30", 30 * time.Second},
		{"capped", "900", MaxRetryAfter},
		{"date", start.Add(45 * time.Second).Format(http.TimeFormat), 45 * time.Second},
		{"missing", "", time.Second},
		{"zero", "0", time.Second},
		{"garbage", "soon", time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			base, tg := newTelegramServer(t, f, func(n int, w http.ResponseWriter) {
				if n == 0 {
					if tc.header != "" {
						w.Header().Set("Retry-After", tc.header)
					}
					w.WriteHeader(http.StatusTooManyRequests)
				}
			})
			c := telegramChannel(t, f, 20)
			ev := recordBatches(t, f, c, 1)[0]
			if !f.clk.Now().Equal(start) {
				t.Fatalf("fixture starts at %v, the date case assumes %v", f.clk.Now(), start)
			}
			var sleeps []time.Duration
			q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
			q.Enqueue(ev)
			stop := startQueue(t, q)
			d := awaitDeliveries(t, f, ev.ID, allDone)[0]
			stop()
			got := tg.messages()
			if len(got) != 2 || !got[1].at.Equal(start.Add(tc.want)) || !d.OK || d.Attempts != 2 {
				t.Fatalf("requests=%+v row=%+v, want the retry %v after the 429", got, d, tc.want)
			}
			if !slices.Equal(sleeps, []time.Duration{tc.want}) {
				t.Errorf("sleeps=%v want [%v]", sleeps, tc.want)
			}
		})
	}
}

// 等待不占用 worker：一个渠道的节奏已满或正按 Retry-After 等待时，发往另一个渠道的批次照常立即发出。
func TestWaitingBatchesDoNotBlockOtherChannels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rate  int
		reply func(n int, w http.ResponseWriter)
	}{
		{"rate", 1, nil},
		{"retry_after", 20, func(n int, w http.ResponseWriter) {
			if n == 0 {
				w.Header().Set("Retry-After", "300")
				w.WriteHeader(http.StatusTooManyRequests)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			base, tg := newTelegramServer(t, f, tc.reply)
			var hooked []time.Time
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				hooked = append(hooked, f.clk.Now())
				mu.Unlock()
			}))
			defer srv.Close()
			c := telegramChannel(t, f, tc.rate)
			tgEvents := recordBatches(t, f, c, 2)
			hookEvent := recordBatches(t, f, queueChannel(t, f, srv.URL), 1)[0]
			var sleeps []time.Duration
			q := NewQueue(QueueConfig{TelegramBase: base, Sleep: advancing(f, &sleeps)}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
			start := f.clk.Now()
			q.Enqueue(tgEvents[0])
			q.Enqueue(tgEvents[1])
			q.Enqueue(hookEvent)
			stop := startQueue(t, q)
			awaitSettled(t, f)
			stop()
			mu.Lock()
			defer mu.Unlock()
			if len(hooked) != 1 || !hooked[0].Equal(start) {
				t.Fatalf("webhook delivered at %v, want immediately at %v while telegram waits (telegram: %+v)", hooked, start, tg.messages())
			}
			if n := len(tg.messages()); n < 2 {
				t.Fatalf("telegram messages=%d, want both batches eventually sent", n)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"30", 30 * time.Second, true},
		{" 7 ", 7 * time.Second, true},
		{"301", MaxRetryAfter, true},
		{"99999999999999999999999", MaxRetryAfter, true},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), -time.Minute, true},
		{"-5", 0, false},
		{"1.5", 0, false},
		{"", 0, false},
	} {
		got, ok := parseRetryAfter(tc.in, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseRetryAfter(%q)=%v,%v want %v,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// 节奏上限为负在保存与投递构造共用的检查里被拒绝，错误点名字段；0 是"不限"，合法。
func TestCheckChannelRejectsNegativeRate(t *testing.T) {
	c := store.NotifyChannel{Name: "tg", Kind: store.ChannelTelegram, Config: `{"bot_token":"tok","chat_id":"chat"}`}
	must(t, CheckChannel(c))
	c.RatePerMinute = -1
	if err := CheckChannel(c); err == nil || !strings.Contains(err.Error(), "rate_per_minute") {
		t.Fatalf("negative rate err=%v, want a rate_per_minute field error", err)
	}
}

// countingSender 在每次 Enqueue 时回读库里已落的事件数：引擎在评估调用结束时才入队，第一次入队时这次巡检的转换都已落库。
type countingSender struct {
	t    *testing.T
	st   *store.Store
	seen []int
}

func (s *countingSender) Enqueue(store.AlertEvent) {
	events, err := s.st.ListAlertEvents(s.t.Context(), 0, 0, 100)
	if err != nil {
		s.t.Error(err)
	}
	s.seen = append(s.seen, len(events))
}

// 事件在评估调用结束时才交给队列：此前入队，worker 可能在后续同键的行加入之前就开始发送，批次一旦开始尝试就不再接纳新行。
func TestEngineEnqueuesAfterEvaluationCall(t *testing.T) {
	f := newFixture(t)
	f.nodes(t, 3)
	c := telegramChannel(t, f, 20)
	offlineRuleTo(t, f, "离线", c)
	sender := &countingSender{t: t, st: f.st}
	f.e.SetSender(sender)
	fireAll(t, f)
	if !slices.Equal(sender.seen, []int{3, 3, 3}) {
		t.Fatalf("events persisted at each Enqueue=%v, want all three before the first", sender.seen)
	}
}
