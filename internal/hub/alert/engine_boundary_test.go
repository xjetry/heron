package alert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

func TestUnreportedOfflineCannotRecover(t *testing.T) {
	for _, cur := range []store.AlertState{store.StateOK, store.StatePending, store.StateFiring} {
		for _, seconds := range []int{1, 30, 60} {
			t.Run(fmt.Sprintf("%s/%d", cur, seconds), func(t *testing.T) {
				want := cur
				var tr *store.Transition
				if cur != store.StateFiring {
					if seconds >= 60 {
						want = store.StateFiring
						tr = ptr(store.TransitionFiring)
					} else if seconds >= 30 {
						want = store.StatePending
					}
				}
				got, event := NextOffline(cur, Observation{Reported: false, Unseen: time.Duration(seconds) * time.Second, TTL: 30 * time.Second, Grace: 60 * time.Second})
				if got != want || !reflect.DeepEqual(event, tr) {
					t.Fatalf("unreported state=%s event=%v want %s %v", got, event, want, tr)
				}
			})
		}
	}
}

func TestPendingSurvivesRestartUntilGrace(t *testing.T) {
	f := newFixture(t)
	f.grace(t, f.ids[0], 60)
	r := f.rule(t, offline())
	f.clk.Advance(31 * time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StatePending)
	f.restart(t)
	f.clk.Advance(time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StatePending)
	f.clk.Advance(59 * time.Second)
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateOK)
}

func probeRule(task uint64) store.AlertRule {
	return store.AlertRule{Name: "probe", Kind: store.KindProbe, Enabled: true, AllNodes: true, TaskID: task, Metric: store.MetricLossPct, Threshold: 20, ForMinutes: 1}
}

func TestProbeCandidateRemovalPrunesPersistedState(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, f.ids)
	r := f.rule(t, probeRule(task))
	ts := f.clk.Now().Unix() - 60
	f.minutes(t, task, f.ids[0], ts, metric.ProbeBucket{Sent: 1, Lost: 1})
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	_, _, err := f.st.SaveProbeTask(t.Context(), &heronv1.ProbeTask{Id: task, Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "127.0.0.1", IntervalS: 5, TimeoutMs: 100}, store.NodeSelector{AllNodes: false, NodeIDs: f.ids[1:]})
	must(t, err)
	var logs bytes.Buffer
	f.e.log = slog.New(slog.NewJSONHandler(&logs, nil))
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	wantState(t, f.e, r.ID, f.ids[0], "")
	rows, err := f.st.ListAlertStates(t.Context())
	must(t, err)
	if len(rows) != 0 {
		t.Fatalf("pruned states remain persisted: %v", rows)
	}
	if !strings.Contains(logs.String(), `"level":"INFO"`) || !strings.Contains(logs.String(), `"rule_id":1`) || !strings.Contains(logs.String(), `"node_id":1`) {
		t.Fatalf("missing prune log: %s", logs.String())
	}
	if len(f.events(t)) != 1 {
		t.Fatal("candidate removal emitted a recovery event")
	}
	f.restart(t)
	wantState(t, f.e, r.ID, f.ids[0], "")
}

func TestRuleIdentityChangeClearsState(t *testing.T) {
	for _, change := range []string{"kind", "task", "metric", "threshold"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			task := f.task(t, f.ids)
			nextTask := f.task(t, nil)
			ts := f.clk.Now().Unix() - 60
			r := offline()
			if change != "kind" {
				r = probeRule(task)
			}
			r = f.rule(t, r)
			if change == "kind" {
				f.clk.Advance(time.Minute)
				f.sweep(t)
			} else {
				f.minutes(t, task, f.ids[0], ts, metric.ProbeBucket{Sent: 1, Lost: 1})
				must(t, f.e.EvaluateProbes(t.Context(), ts))
			}
			wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
			switch change {
			case "kind":
				r = probeRule(task)
				r.ID = f.e.Rules()[0].ID
			case "task":
				r.TaskID = nextTask
			case "metric":
				r.Metric, r.RttMode = store.MetricRttMs, store.RttThreshold
			case "threshold":
				r.Threshold = 30
			}
			f.rule(t, r)
			f.sweep(t)
			want := 0
			if change == "threshold" {
				want = 1
			}
			if len(f.e.States()) != want {
				t.Fatalf("old identity states cached: %v", f.e.States())
			}
			rows, err := f.st.ListAlertStates(t.Context())
			must(t, err)
			if len(rows) != want {
				t.Fatalf("old identity states persisted: %v", rows)
			}
			f.restart(t)
			if len(f.e.States()) != want {
				t.Fatal("identity state changed on restart")
			}
		})
	}
}

func TestEvaluatorsKeepOtherKindStates(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, f.ids)
	p := f.rule(t, probeRule(task))
	o := f.rule(t, offline())
	ts := f.clk.Now().Unix() - 60
	f.minutes(t, task, f.ids[0], ts, metric.ProbeBucket{Sent: 1, Lost: 1})
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	f.clk.Advance(time.Minute)
	f.sweep(t)
	wantState(t, f.e, p.ID, f.ids[0], store.StateFiring)
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	wantState(t, f.e, o.ID, f.ids[0], store.StateFiring)
}

func TestProbeThresholdEqualityFiresAndDoesNotRecover(t *testing.T) {
	for _, c := range []struct {
		metric    store.ProbeMetric
		threshold float64
		bucket    metric.ProbeBucket
	}{
		{store.MetricLossPct, 100, metric.ProbeBucket{Sent: 2, Lost: 2}},
		{store.MetricRttMs, 50, metric.ProbeBucket{Sent: 2, RttN: 2, RttSumUs: 100_000, RttMinUs: 50_000, RttMaxUs: 50_000}},
	} {
		t.Run(string(c.metric), func(t *testing.T) {
			f := newFixture(t)
			task := f.task(t, f.ids)
			r := probeRule(task)
			r.Metric = c.metric
			if c.metric == store.MetricRttMs {
				r.RttMode = store.RttThreshold
			}
			r.Threshold = c.threshold
			r = f.rule(t, r)
			ts := f.clk.Now().Unix() - 120
			f.minutes(t, task, f.ids[0], ts, c.bucket, c.bucket)
			must(t, f.e.EvaluateProbes(t.Context(), ts))
			wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
			must(t, f.e.EvaluateProbes(t.Context(), ts+60))
			wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
			if ev := f.events(t); len(ev) != 1 || ev[0].Value != c.threshold {
				t.Fatalf("equal threshold events=%+v", ev)
			}
		})
		t.Run(string(c.metric)+"/already_firing", func(t *testing.T) {
			f := newFixture(t)
			task := f.task(t, f.ids)
			r := probeRule(task)
			r.Metric, r.Threshold = c.metric, c.threshold
			if c.metric == store.MetricRttMs {
				r.RttMode = store.RttThreshold
			}
			r = f.rule(t, r)
			must(t, f.st.SetAlertState(t.Context(), r.ID, f.ids[0], store.StateFiring, f.clk.Now(), time.Time{}))
			must(t, f.e.Load(t.Context()))
			ts := f.clk.Now().Unix() - 60
			f.minutes(t, task, f.ids[0], ts, c.bucket)
			must(t, f.e.EvaluateProbes(t.Context(), ts))
			wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
			if ev := f.events(t); len(ev) != 0 {
				t.Fatalf("equal threshold recovered: %+v", ev)
			}
		})
	}
}

func TestEvaluateRejectsUnalignedMinute(t *testing.T) {
	f := newFixture(t)
	if err := f.e.EvaluateProbes(t.Context(), f.clk.Now().Unix()+30); err == nil || !strings.Contains(err.Error(), "minuteTS") || !strings.Contains(err.Error(), "60") {
		t.Fatalf("unaligned minute error=%v", err)
	}
}

func TestLoadSkipsInvalidRulesAndTheirStates(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, f.ids)
	bad := probeRule(task)
	bad.ForMinutes = 0
	bad, err := f.st.SaveAlertRule(t.Context(), bad)
	must(t, err)
	must(t, f.st.SetAlertState(t.Context(), bad.ID, f.ids[0], store.StateFiring, f.clk.Now(), time.Time{}))
	good := f.rule(t, offline())
	var logs bytes.Buffer
	f.e.log = slog.New(slog.NewJSONHandler(&logs, nil))
	must(t, f.e.Load(t.Context()))
	if rules := f.e.Rules(); len(rules) != 1 || rules[0].ID != good.ID {
		t.Fatalf("invalid rule loaded: %v", rules)
	}
	if len(f.e.States()) != 0 {
		t.Fatal("invalid rule state loaded")
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), "for_minutes") {
		t.Fatalf("missing invalid rule warning: %s", logs.String())
	}
	must(t, f.e.EvaluateProbes(t.Context(), f.clk.Now().Unix()-60))
}

func TestChannelRejectsInvalidTemplateFieldsAndHeaderValues(t *testing.T) {
	for _, body := range []string{`{{.Rulee}}`, `{{json .Value}}`} {
		t.Run(body, func(t *testing.T) {
			b, err := json.Marshal(WebhookConfig{URL: "https://example.invalid", BodyTemplate: body})
			must(t, err)
			err = CheckChannel(store.NotifyChannel{Name: "webhook", Kind: store.ChannelWebhook, Config: string(b)})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "body_template") {
				t.Fatalf("template accepted: %v", err)
			}
		})
	}
	for _, value := range []string{"a\r\nb", "a\x00b", "a\x7fb", "a\tb"} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			b, err := json.Marshal(WebhookConfig{URL: "https://example.invalid", Headers: map[string]string{"X-Test": value}})
			must(t, err)
			err = CheckChannel(store.NotifyChannel{Name: "webhook", Kind: store.ChannelWebhook, Config: string(b)})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "headers") {
				t.Fatalf("header value accepted: %v", err)
			}
		})
	}
}

func TestSaveWebhookNormalizesMethod(t *testing.T) {
	for _, method := range []string{"", "PUT"} {
		t.Run(method, func(t *testing.T) {
			f := newFixture(t)
			b, err := json.Marshal(WebhookConfig{URL: "https://example.invalid", Method: method})
			must(t, err)
			saved, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "webhook", Kind: store.ChannelWebhook, Config: string(b)})
			must(t, err)
			rows, err := f.st.ListNotifyChannels(t.Context())
			must(t, err)
			want := method
			if want == "" {
				want = "POST"
			}
			for _, c := range []store.NotifyChannel{saved, rows[0], f.e.Channels()[0]} {
				var cfg WebhookConfig
				must(t, json.Unmarshal([]byte(c.Config), &cfg))
				if cfg.Method != want {
					t.Fatalf("method=%q want %q", cfg.Method, want)
				}
			}
		})
	}
}

func TestOfflineSummaryUsesPersistedLastSeen(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(fmt.Sprint(reported), func(t *testing.T) {
			f := newFixture(t)
			last := f.clk.Now().Add(-72 * time.Hour)
			if reported {
				_, err := f.st.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: f.ids[0], TS: last.Unix(), CoverageStart: last.Unix(), LastSeen: last, Bucket: metric.NewBucket()}}})
				must(t, err)
			}
			r := offline()
			r.AllNodes = false
			r.NodeIDs = f.ids[:1]
			f.rule(t, r)
			f.clk.Advance(time.Minute)
			f.sweep(t)
			ev := f.events(t)
			if len(ev) != 1 {
				t.Fatalf("events=%v", ev)
			}
			want := "从未上报"
			if reported {
				want = "最后在线于 " + last.Format(time.RFC3339)
			}
			if !strings.Contains(ev[0].Summary, want) || strings.Contains(ev[0].Summary, "离线 1m0s") {
				t.Fatalf("summary=%q want %q", ev[0].Summary, want)
			}
		})
	}
}

func TestProbeMinuteAtIsPreviousClosedMinute(t *testing.T) {
	for _, seconds := range []int64{180, 183, 239} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			if got := probeMinuteAt(time.Unix(seconds, 0)); got != 120 {
				t.Fatalf("minute=%d want previous closed minute 120", got)
			}
		})
	}
}

func TestPruneFailureKeepsMemoryState(t *testing.T) {
	f := newFixture(t)
	f.rule(t, offline())
	f.clk.Advance(time.Minute)
	f.sweep(t)
	before := f.e.States()
	must(t, f.st.Close())
	f.e.writeMu.Lock()
	err := f.e.pruneCandidates(t.Context(), before[0].RuleID, map[int64]bool{})
	f.e.writeMu.Unlock()
	if !errors.Is(err, store.ErrClosed) {
		t.Fatalf("prune error=%v", err)
	}
	if !reflect.DeepEqual(f.e.States(), before) {
		t.Fatalf("failed prune changed states: %v", f.e.States())
	}
}

func TestSweepPrunesDeletedNodeWithoutTouchingCandidates(t *testing.T) {
	f := newFixture(t)
	r := f.rule(t, offline())
	f.clk.Advance(time.Minute)
	f.sweep(t)
	must(t, f.st.DeleteNode(t.Context(), f.ids[0]))
	f.sweep(t)
	wantState(t, f.e, r.ID, f.ids[0], "")
	wantState(t, f.e, r.ID, f.ids[1], store.StateFiring)
}
