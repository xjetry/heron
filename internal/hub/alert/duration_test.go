package alert

import (
	"fmt"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

func TestFormatAlertDuration(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    string
	}{
		{-1, "不到 1 分钟"}, {0, "不到 1 分钟"}, {59, "不到 1 分钟"},
		{60, "1 分钟"}, {3599, "59 分钟"}, {3600, "1 小时"},
		{86399, "23 小时 59 分钟"}, {86400, "1 天"}, {90061, "1 天 1 小时"},
	} {
		t.Run(fmt.Sprint(tc.seconds), func(t *testing.T) {
			if got := formatAlertDuration(time.Duration(tc.seconds) * time.Second); got != tc.want {
				t.Fatalf("duration=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestRecoveryDurationThroughEvaluators(t *testing.T) {
	for _, kind := range []store.AlertKind{store.KindOffline, store.KindResource, store.KindExpiry} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart=%v", kind, restart), func(t *testing.T) {
				f := newFixture(t)
				var recoverNow func()
				switch kind {
				case store.KindOffline:
					r := f.nodeRule(t)
					f.fireOffline(t, r, f.ids[0])
					recoverNow = func() { f.recoverOffline(t, r, f.ids[0]) }
				case store.KindResource:
					f.rule(t, store.AlertRule{Name: "资源", Kind: kind, Enabled: true, NodeIDs: f.ids[:1], ResourceMetric: store.MetricCpuPct, Threshold: 90, RecoveryThreshold: 80, ForMinutes: 1})
					base := f.clk.Now().Unix()
					write := func(ts int64, value float64) {
						bucket := metric.NewBucket()
						i := metric.Index("cpu")
						bucket.Sum[i], bucket.N[i] = value, 1
						_, err := f.st.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: f.ids[0], TS: ts, CoverageStart: base, Bucket: bucket}}})
						must(t, err)
						must(t, f.e.EvaluateResources(t.Context(), ts))
					}
					write(base, 95)
					recoverNow = func() { write(base+60, 70) }
				case store.KindExpiry:
					f.rule(t, expiryRule())
					f.billing(t, f.ids[0], store.Billing{ExpiresOn: "2026-09-28"})
					f.sweepExpiry(t)
					recoverNow = func() { f.billing(t, f.ids[0], store.Billing{ExpiresOn: "2026-11-01"}); f.sweepExpiry(t) }
				}
				fired := f.events(t)
				if len(fired) != 1 || fired[0].Transition != store.TransitionFiring || strings.Contains(fired[0].Summary, "告警持续") {
					t.Fatalf("firing summary=%+v", fired)
				}
				f.clk.Advance(61 * time.Minute)
				if restart {
					f.restart(t)
				}
				recoverNow()
				events := f.events(t)
				if len(events) != 2 || events[0].Transition != store.TransitionRecovered || !strings.HasSuffix(events[0].Summary, "，告警持续 1 小时 1 分钟") {
					t.Fatalf("recovery summary=%+v", events)
				}
			})
		}
	}
}

func TestRecoveryDurationRequiresFiringState(t *testing.T) {
	f := newFixture(t)
	r := f.nodeRule(t)
	f.fireOffline(t, r, f.ids[0])
	f.recoverOffline(t, r, f.ids[0])
	f.clk.Advance(time.Minute)
	// firedAt 在恢复后保留；非 firing 状态不能把上次触发时刻当作本次恢复的起点。
	k := stateKey{r.ID, f.ids[0]}
	f.e.writeMu.Lock()
	f.e.mu.Lock()
	entry := f.e.states[k]
	entry.state = store.StatePending
	f.e.states[k] = entry
	f.e.mu.Unlock()
	tr := store.TransitionRecovered
	cy := &cycle{batches: map[batchKey]int64{}}
	err := f.e.apply(t.Context(), cy, r, f.ids[0], store.StateOK, false, false, "", &tr, "恢复", 0)
	f.e.writeMu.Unlock()
	must(t, err)
	if got := f.events(t)[0].Summary; got != "恢复" {
		t.Fatalf("non-firing recovery summary=%q", got)
	}
}

func TestMergedRecoveryMessagePreservesDurations(t *testing.T) {
	f := newFixture(t)
	c, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "通知", Kind: store.ChannelTelegram, Config: `{"bot_token":"test","chat_id":"test"}`})
	must(t, err)
	r := offline()
	r.ChannelIDs = []int64{c.ID}
	f.rule(t, r)
	f.clk.Advance(time.Minute)
	f.sweep(t)
	f.clk.Advance(2 * time.Minute)
	for _, id := range f.ids {
		f.l.Observe(id, "", &heronv1.Metrics{})
	}
	f.sweep(t)
	events := f.events(t)[:2]
	if events[0].Deliveries[0].BatchID != events[1].Deliveries[0].BatchID {
		t.Fatal("recovery events were not merged")
	}
	q := &Queue{st: f.st}
	msg, err := q.message(t.Context(), store.DeliveryBatch{Events: events})
	must(t, err)
	if strings.Count(msg.Summary, "，告警持续 2 分钟") != 2 {
		t.Fatalf("merged summary=%q", msg.Summary)
	}
}
