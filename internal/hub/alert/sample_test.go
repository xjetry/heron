package alert

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
)

// templateChannel 存一个带自定义请求体模板的 Webhook 渠道，模板引用 Message 的全部字段。
func templateChannel(t *testing.T, f *fixture, url string) store.NotifyChannel {
	t.Helper()
	cfg, err := json.Marshal(WebhookConfig{URL: url, Method: "PUT", Headers: map[string]string{"X-Secret": "s3cr3t"},
		BodyTemplate: `{"r":{{json .Rule}},"n":{{json .Node}},"k":{{json .Kind}},"t":{{json .Transition}},"s":{{json .Summary}},"v":{{.Value}},"u":{{json .ValueUnit}},"at":{{.At.Unix}}}`})
	must(t, err)
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "tmpl", Kind: store.ChannelWebhook, Config: string(cfg)})
	must(t, err)
	return c
}

func newTestQueue(f *fixture) *Queue {
	return NewQueue(QueueConfig{}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
}

// 预览的请求体必须与真实投递发出的逐字节相同：离线规则在 TTL 处触发，样例取同一个时长，同一时刻渲染。
func TestSamplePreviewMatchesRealOfflineDelivery(t *testing.T) {
	f := newFixture(t)
	bodies := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- string(b)
	}))
	defer srv.Close()
	c := templateChannel(t, f, srv.URL)
	r := offline()
	r.AllNodes, r.NodeIDs, r.ChannelIDs = false, f.ids[:1], []int64{c.ID}
	r = f.rule(t, r)
	q := newTestQueue(f)
	f.e.SetSender(q)
	stop := startQueue(t, q)
	f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
	f.clk.Advance(30 * time.Second)
	f.sweep(t)
	evs := f.events(t)
	if len(evs) != 1 || evs[0].Transition != store.TransitionFiring {
		t.Fatalf("events=%+v", evs)
	}
	awaitDeliveries(t, f, evs[0].ID, allDone)
	stop()
	delivered := <-bodies

	node, err := f.st.GetNode(t.Context(), f.ids[0])
	must(t, err)
	s, err := f.e.Sample(t.Context(), r, node, store.TransitionFiring)
	must(t, err)
	if len(s.Channels) != 1 || s.Channels[0].ID != c.ID {
		t.Fatalf("channels=%+v", s.Channels)
	}
	p, err := q.Preview(s.Channels[0], s.Message)
	must(t, err)
	if p.Body != delivered {
		t.Fatalf("preview body\n%s\ndelivered\n%s", p.Body, delivered)
	}
	if p.Method != "PUT" || p.Target != srv.URL {
		t.Fatalf("preview=%+v", p)
	}
}

// 每种规则在两个阶段的样例文案与观测值。期望文本按各种类评估的文案格式手写：夹具时区东八区、此刻当地 2026-09-24，
// 样例观测取刚好触发或刚好回到正常的那一点，恢复文案带固定 30 分钟的告警持续时长。
func TestSampleEveryKindAndPhase(t *testing.T) {
	f := newFixture(t)
	c := queueChannel(t, f, "http://127.0.0.1:1")
	node, err := f.st.GetNode(t.Context(), f.ids[0])
	must(t, err)
	probe := store.AlertRule{Name: "P", Kind: store.KindProbe, TaskID: 7, Metric: store.MetricLossPct, Threshold: 50, ForMinutes: 3}
	fixedRtt := store.AlertRule{Name: "P", Kind: store.KindProbe, TaskID: 7, Metric: store.MetricRttMs, RttMode: store.RttRelative, ForMinutes: 3,
		BaselineMode: store.BaselineFixed, FixedBaselineMs: 80, UpperDeviationPct: 50, LowerDeviationPct: 25, CooldownS: 60}
	adaptiveRtt := fixedRtt
	adaptiveRtt.BaselineMode, adaptiveRtt.FixedBaselineMs, adaptiveRtt.BaselineWindowS, adaptiveRtt.BaselineMinSamples = store.BaselineAdaptive, 0, 3600, 10
	resource := store.AlertRule{Name: "R", Kind: store.KindResource, ResourceMetric: store.MetricCpuPct, Threshold: 90, RecoveryThreshold: 70, ForMinutes: 5}
	traffic := store.AlertRule{Name: "T", Kind: store.KindTraffic, Threshold: 80}
	expiry := store.AlertRule{Name: "E", Kind: store.KindExpiry, DaysBefore: 7}
	cert := store.AlertRule{Name: "C", Kind: store.KindCertExpiry, TaskID: 7, DaysBefore: 7}
	const tail = "，告警持续 30 分钟"
	for _, tc := range []struct {
		name    string
		rule    store.AlertRule
		phase   store.Transition
		summary string
		value   float64
		unit    string
	}{
		{"offline_firing", offline(), store.TransitionFiring, "节点 node1 离线 30s（规则 离线）", 30, metric.UnitSeconds},
		{"offline_recovered", offline(), store.TransitionRecovered, "节点 node1 已恢复上报（规则 离线）" + tail, 0, metric.UnitSeconds},
		{"probe_firing", probe, store.TransitionFiring, "节点 node1 规则 P：loss_pct 50.0", 50, metric.UnitPercent},
		{"probe_recovered", probe, store.TransitionRecovered, "节点 node1 规则 P：loss_pct 25.0" + tail, 25, metric.UnitPercent},
		{"rtt_fixed_firing", fixedRtt, store.TransitionFiring, "节点 node1 规则 P：rtt 均值 120.0 ms，基线 80.0 ms，偏差 +50.0%（正常带 60.0–120.0 ms）", 120, metric.UnitMilliseconds},
		{"rtt_fixed_recovered", fixedRtt, store.TransitionRecovered, "节点 node1 规则 P：rtt 均值 80.0 ms，基线 80.0 ms，偏差 +0.0%（正常带 60.0–120.0 ms）" + tail, 80, metric.UnitMilliseconds},
		{"rtt_adaptive_firing", adaptiveRtt, store.TransitionFiring, "节点 node1 规则 P：rtt 均值 150.0 ms，基线 100.0 ms，偏差 +50.0%（正常带 75.0–150.0 ms）", 150, metric.UnitMilliseconds},
		{"resource_firing", resource, store.TransitionFiring, "节点 node1 CPU持续 5 分钟达到 90.0%（规则 R）", 90, metric.UnitPercent},
		{"resource_recovered", resource, store.TransitionRecovered, "节点 node1 CPU持续 5 分钟不高于 70.0%，已恢复（规则 R）" + tail, 70, metric.UnitPercent},
		{"traffic_firing", traffic, store.TransitionFiring, "节点 node1 本周期流量已用 80.00%（819 GiB / 1 TiB，收+发，规则 T）", 80, metric.UnitPercent},
		{"traffic_recovered", traffic, store.TransitionRecovered, "节点 node1 本周期流量用量 40.00% 已低于阈值 80.00%（规则 T）" + tail, 40, metric.UnitPercent},
		{"expiry_firing", expiry, store.TransitionFiring, "节点 node1 将于 2026-10-01 到期（剩 7 天，规则 E）", 7, metric.UnitDays},
		{"expiry_recovered", expiry, store.TransitionRecovered, "节点 node1 已不在提醒窗口内（规则 E）" + tail, 7, metric.UnitDays},
		{"cert_firing", cert, store.TransitionFiring, "节点 node1 的证书将于 2026-10-01 到期（剩 7 天，规则 C）", 7, metric.UnitDays},
		{"cert_recovered", cert, store.TransitionRecovered, "节点 node1 的证书已不在提醒窗口内（规则 C）" + tail, 7, metric.UnitDays},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.rule
			r.ChannelIDs = []int64{c.ID}
			s, err := f.e.Sample(t.Context(), r, node, tc.phase)
			must(t, err)
			want := Message{Rule: r.Name, Node: "node1", Kind: string(r.Kind), Transition: string(tc.phase), Summary: tc.summary, Value: tc.value, ValueUnit: tc.unit, At: f.clk.Now()}
			if s.Message != want {
				t.Fatalf("message\n%+v\nwant\n%+v", s.Message, want)
			}
		})
	}
}

// 样例只发往已保存的渠道：不认识的 id 是 NotFound；重复的 id 合并；合并后须有 1 到 SampleChannelLimit 个。
// 规则字段与保存同一套校验。
func TestSampleRejectsUnsavedChannelsAndInvalidRules(t *testing.T) {
	f := newFixture(t)
	node, err := f.st.GetNode(t.Context(), f.ids[0])
	must(t, err)
	c := queueChannel(t, f, "http://127.0.0.1:1")
	r := offline()
	r.ChannelIDs = []int64{c.ID, c.ID}
	s, err := f.e.Sample(t.Context(), r, node, store.TransitionFiring)
	must(t, err)
	if len(s.Channels) != 1 || s.Channels[0].ID != c.ID {
		t.Fatalf("duplicate ids not merged: %+v", s.Channels)
	}
	many := make([]int64, SampleChannelLimit+1)
	for i := range many {
		many[i] = int64(i + 1)
	}
	for _, tc := range []struct {
		name string
		edit func(*store.AlertRule)
		want string
	}{
		{"empty", func(r *store.AlertRule) { r.ChannelIDs = nil }, "invalid: channel_ids must name between 1 and 16 saved notify channels"},
		{"too_many", func(r *store.AlertRule) { r.ChannelIDs = many }, "invalid: channel_ids must name between 1 and 16 saved notify channels"},
		{"unsaved", func(r *store.AlertRule) { r.ChannelIDs = []int64{c.ID, 99} }, "notify channel 99 does not exist"},
		{"rule_field", func(r *store.AlertRule) { r.Name = "" }, "invalid: name must contain between 1 and 64 characters"},
		{"kind_field", func(r *store.AlertRule) { r.Threshold = 3 }, "invalid: threshold must be 0 unless kind is probe, resource or traffic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := offline()
			r.ChannelIDs = []int64{c.ID}
			tc.edit(&r)
			_, err := f.e.Sample(t.Context(), r, node, store.TransitionFiring)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
		})
	}
}

// 模板执行到引用不存在字段的分支时，预览给出与投递相同类别的失败，文本点明模板里出错的位置与字段。
// 保存时的样例执行只走 firing 分支，恢复分支里的错字段要到恢复的预览才暴露——这正是预览要让管理员看到的。
func TestPreviewReportsTemplateErrorOnTheExecutedBranch(t *testing.T) {
	f := newFixture(t)
	cfg, err := json.Marshal(WebhookConfig{URL: "https://hook.example/x?k=secret", Method: "POST",
		BodyTemplate: `{{if eq .Transition "recovered"}}{{.Recovered}}{{else}}{"s":{{json .Summary}}}{{end}}`})
	must(t, err)
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "branchy", Kind: store.ChannelWebhook, Config: string(cfg)})
	must(t, err)
	node, err := f.st.GetNode(t.Context(), f.ids[0])
	must(t, err)
	r := offline()
	r.ChannelIDs = []int64{c.ID}
	q := newTestQueue(f)
	s, err := f.e.Sample(t.Context(), r, node, store.TransitionFiring)
	must(t, err)
	p, err := q.Preview(s.Channels[0], s.Message)
	must(t, err)
	if p.Body != `{"s":"节点 node1 离线 30s（规则 离线）"}` || p.Target != "https://hook.example" {
		t.Fatalf("firing preview=%+v", p)
	}
	s, err = f.e.Sample(t.Context(), r, node, store.TransitionRecovered)
	must(t, err)
	_, err = q.Preview(s.Channels[0], s.Message)
	res, retry := Classify(err)
	if res.Failure != store.FailureRequest || retry || !strings.Contains(err.Error(), `<.Recovered>: can't evaluate field Recovered`) {
		t.Fatalf("recovered preview err=%v result=%+v", err, res)
	}
}

// Telegram 的 bot token 在请求路径里：预览的目标只到 scheme://host，请求体是投递发出的那一份。
func TestPreviewTelegramHidesToken(t *testing.T) {
	f := newFixture(t)
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "tg", Kind: store.ChannelTelegram, Config: `{"bot_token":"123:secret","chat_id":"42"}`})
	must(t, err)
	q := NewQueue(QueueConfig{TelegramBase: "http://tg.example:8080/"}, QueueDeps{Store: f.st, Channels: f.e.Channels, Client: outbound.NewClient(NotifyTimeout), Clock: f.clk, Log: f.log})
	p, err := q.Preview(c, Message{Summary: "[测试] 你好"})
	must(t, err)
	if p != (Preview{Method: "POST", Target: "http://tg.example:8080", Body: `{"chat_id":"42","text":"[测试] 你好"}`}) {
		t.Fatalf("preview=%+v", p)
	}
}

// SendNow 不计入渠道节奏：节奏 1/分钟的渠道在 SendNow 之后，worker 的第一次投递仍立即发出；它也不落库。
func TestSendNowDoesNotConsumeRateOrRecord(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	cfg, err := json.Marshal(WebhookConfig{URL: srv.URL, Method: "POST"})
	must(t, err)
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "slow", Kind: store.ChannelWebhook, Config: string(cfg), RatePerMinute: 1})
	must(t, err)
	q := newTestQueue(f)
	must(t, q.SendNow(t.Context(), c, Message{Summary: "[测试] x", At: f.clk.Now()}))
	must(t, q.SendNow(t.Context(), c, Message{Summary: "[测试] y", At: f.clk.Now()}))
	if len(f.events(t)) != 0 {
		t.Fatal("SendNow persisted an event")
	}
	if _, full := q.rateSlot(c, f.clk.Mono()); full {
		t.Fatal("SendNow consumed the channel rate")
	}
	ev := queueEvent(t, f, c)
	q.Enqueue(ev)
	stop := startQueue(t, q)
	ds := awaitDeliveries(t, f, ev.ID, allDone)
	stop()
	if !ds[0].OK || calls.Load() != 3 {
		t.Fatalf("delivery=%+v calls=%d", ds[0], calls.Load())
	}
}

// 样例阶段只有 firing 与 recovered：CheckSamplePhase 是唯一判定，协议层与 Sample 都调用它，错误点名 transition 字段。
func TestSamplePhaseIsFiringOrRecovered(t *testing.T) {
	for _, phase := range []store.Transition{store.TransitionFiring, store.TransitionRecovered} {
		must(t, CheckSamplePhase(phase))
	}
	err := CheckSamplePhase("pending")
	var field FieldError
	if !errors.As(err, &field) || field.Path != "transition" || field.Detail() != `must be one of firing, recovered; got "pending"` {
		t.Fatalf("err = %v", err)
	}
	f := newFixture(t)
	node, err := f.st.GetNode(t.Context(), f.ids[0])
	must(t, err)
	r := offline()
	r.ChannelIDs = []int64{queueChannel(t, f, "http://127.0.0.1:1").ID}
	if _, err := f.e.Sample(t.Context(), r, node, "pending"); !errors.As(err, &field) || field.Path != "transition" {
		t.Fatalf("Sample with phase pending: err = %v", err)
	}
}
