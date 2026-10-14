package alert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

func TestCheckRule(t *testing.T) {
	base := store.AlertRule{Name: "规则", Kind: store.KindProbe, Enabled: true, AllNodes: true, TaskID: 1, Metric: store.MetricLossPct, Threshold: 20, ForMinutes: 3}
	cases := []struct {
		name   string
		change func(*store.AlertRule)
		field  string
	}{
		{"empty_name", func(r *store.AlertRule) { r.Name = "" }, "name"},
		{"long_name", func(r *store.AlertRule) { r.Name = strings.Repeat("字", 65) }, "name"},
		{"kind", func(r *store.AlertRule) { r.Kind = "other" }, "kind"},
		{"task", func(r *store.AlertRule) { r.TaskID = 0 }, "task_id"},
		{"metric", func(r *store.AlertRule) { r.Metric = "other" }, "metric"},
		{"loss_low", func(r *store.AlertRule) { r.Threshold = -1 }, "threshold"},
		{"loss_high", func(r *store.AlertRule) { r.Threshold = 101 }, "threshold"},
		{"rtt_zero", func(r *store.AlertRule) { r.Metric, r.RttMode = store.MetricRttMs, store.RttThreshold; r.Threshold = 0 }, "threshold"},
		{"nan", func(r *store.AlertRule) { r.Threshold = math.NaN() }, "threshold"},
		{"infinite", func(r *store.AlertRule) {
			r.Metric, r.RttMode = store.MetricRttMs, store.RttThreshold
			r.Threshold = math.Inf(1)
		}, "threshold"},
		{"minutes_low", func(r *store.AlertRule) { r.ForMinutes = 0 }, "for_minutes"},
		{"minutes_high", func(r *store.AlertRule) { r.ForMinutes = 61 }, "for_minutes"},
		{"probe_days_before", func(r *store.AlertRule) { r.DaysBefore = 7 }, "days_before"},
		{"offline_task", func(r *store.AlertRule) { *r = offline(); r.TaskID = 1 }, "task_id"},
		{"offline_metric", func(r *store.AlertRule) { *r = offline(); r.Metric = store.MetricLossPct }, "metric"},
		{"offline_threshold", func(r *store.AlertRule) { *r = offline(); r.Threshold = 20 }, "threshold"},
		{"offline_nan", func(r *store.AlertRule) { *r = offline(); r.Threshold = math.NaN() }, "threshold"},
		{"offline_minutes", func(r *store.AlertRule) { *r = offline(); r.ForMinutes = 3 }, "for_minutes"},
		{"offline_days_before", func(r *store.AlertRule) { *r = offline(); r.DaysBefore = 7 }, "days_before"},
		{"expiry_days_low", func(r *store.AlertRule) { *r = expiryRule(); r.DaysBefore = 0 }, "days_before"},
		{"expiry_days_high", func(r *store.AlertRule) { *r = expiryRule(); r.DaysBefore = 366 }, "days_before"},
		{"expiry_task", func(r *store.AlertRule) { *r = expiryRule(); r.TaskID = 1 }, "task_id"},
		{"expiry_metric", func(r *store.AlertRule) { *r = expiryRule(); r.Metric = store.MetricRttMs }, "metric"},
		{"expiry_threshold", func(r *store.AlertRule) { *r = expiryRule(); r.Threshold = 1 }, "threshold"},
		{"expiry_minutes", func(r *store.AlertRule) { *r = expiryRule(); r.ForMinutes = 1 }, "for_minutes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base
			c.change(&r)
			err := CheckRule(r)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.field) {
				t.Fatalf("error=%v want ErrInvalid and %s", err, c.field)
			}
		})
	}
	for _, change := range []func(*store.AlertRule){
		func(r *store.AlertRule) { r.Name = strings.Repeat("字", 64) },
		func(r *store.AlertRule) { r.Threshold = 0; r.ForMinutes = 1 },
		func(r *store.AlertRule) { r.Threshold = 100; r.ForMinutes = 60 },
		func(r *store.AlertRule) {
			r.Metric, r.RttMode = store.MetricRttMs, store.RttThreshold
			r.Threshold = 0.1
		},
		func(r *store.AlertRule) { r.AllNodes = false; r.NodeIDs = []int64{1} },
		func(r *store.AlertRule) { r.AllNodes = false },
		func(r *store.AlertRule) { *r = offline() },
		func(r *store.AlertRule) { *r = expiryRule(); r.DaysBefore = 1 },
		func(r *store.AlertRule) { *r = expiryRule(); r.DaysBefore = 365 },
	} {
		r := base
		change(&r)
		if err := CheckRule(r); err != nil {
			t.Errorf("valid rule %+v: %v", r, err)
		}
	}
}

func TestCheckRuleResourceThresholdRange(t *testing.T) {
	base := store.AlertRule{Name: "资源", Kind: store.KindResource, Enabled: true, AllNodes: true, ResourceMetric: store.MetricMemoryUsedPct, Threshold: 90, RecoveryThreshold: 80, ForMinutes: 3}
	bounds := []struct {
		metric  store.ResourceMetric
		max     float64
		maxText string
	}{
		{store.MetricMemoryUsedPct, 100, "100"},
		{store.MetricDiskUsedPct, 100, "100"},
		{store.MetricCpuPct, 100, "100"},
		{store.MetricLoad1PerCore, 64, "64"},
		{store.MetricNetRxBps, 1 << 40, "1099511627776"},
		{store.MetricNetTxBps, 1 << 40, "1099511627776"},
	}
	for _, b := range bounds {
		// 边界值合法，越界与零各拒绝一次；错误文案点名字段与该指标的取值范围。
		for _, threshold := range []float64{0, b.max + 1} {
			r := base
			r.ResourceMetric, r.Threshold, r.RecoveryThreshold = b.metric, threshold, 0
			err := CheckRule(r)
			want := "threshold must be greater than 0 and at most " + b.maxText
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s threshold=%v: error=%v want %q", b.metric, threshold, err, want)
			}
		}
		r := base
		r.ResourceMetric, r.Threshold, r.RecoveryThreshold = b.metric, b.max, 0
		if err := CheckRule(r); err != nil {
			t.Errorf("%s boundary %v: %v", b.metric, b.max, err)
		}
	}
	r := base
	r.ResourceMetric = "load1"
	if err := CheckRule(r); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "resource_metric") {
		t.Fatalf("unknown resource metric error=%v", err)
	}
}

func TestCheckChannel(t *testing.T) {
	cases := []struct {
		name   string
		kind   store.ChannelKind
		config string
		field  string
	}{
		{"telegram", store.ChannelTelegram, `{"bot_token":"secret","chat_id":"chat"}`, ""},
		{"missing_token", store.ChannelTelegram, `{"chat_id":"chat"}`, "bot_token"},
		{"missing_chat", store.ChannelTelegram, `{"bot_token":"secret"}`, "chat_id"},
		{"telegram_json", store.ChannelTelegram, `{"chat_id":1}`, "config"},
		{"webhook_default", store.ChannelWebhook, `{"url":"https://example.invalid"}`, ""},
		{"webhook_post", store.ChannelWebhook, `{"url":"http://example.invalid","method":"POST"}`, ""},
		{"webhook_put", store.ChannelWebhook, `{"url":"https://example.invalid","method":"PUT"}`, ""},
		{"webhook_patch", store.ChannelWebhook, `{"url":"https://example.invalid","method":"PATCH","body_template":"{{json .Summary}}"}`, ""},
		{"webhook_json", store.ChannelWebhook, `{"headers":1}`, "config"},
		{"webhook_url", store.ChannelWebhook, `{"url":"file:///tmp/data"}`, "url"},
		{"webhook_host", store.ChannelWebhook, `{"url":"http:///relative"}`, "url"},
		{"webhook_method", store.ChannelWebhook, `{"url":"https://example.invalid","method":"GET"}`, "method"},
		{"webhook_template", store.ChannelWebhook, `{"url":"https://example.invalid","body_template":"{{"}`, "body_template"},
		{"webhook_function", store.ChannelWebhook, `{"url":"https://example.invalid","body_template":"{{missing .Rule}}"}`, "body_template"},
		{"kind", "other", `{}`, "kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckChannel(store.NotifyChannel{Name: "通知", Kind: c.kind, Config: c.config})
			if c.field == "" {
				must(t, err)
			} else if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.field) {
				t.Fatalf("error=%v want ErrInvalid and %s", err, c.field)
			}
		})
	}
	for _, name := range []string{"", strings.Repeat("字", 65)} {
		err := CheckChannel(store.NotifyChannel{Name: name, Kind: store.ChannelTelegram, Config: `{"bot_token":"a","chat_id":"b"}`})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "name") {
			t.Fatalf("name error=%v", err)
		}
	}
	for _, count := range []int{16, 17} {
		t.Run(fmt.Sprintf("headers_%d", count), func(t *testing.T) {
			headers := map[string]string{}
			for i := 0; i < count; i++ {
				headers[fmt.Sprintf("X-%d", i)] = "v"
			}
			b, err := json.Marshal(WebhookConfig{URL: "https://example.invalid", Headers: headers})
			must(t, err)
			err = CheckChannel(store.NotifyChannel{Name: "n", Kind: store.ChannelWebhook, Config: string(b)})
			if count == 16 {
				must(t, err)
			} else if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "16") {
				t.Fatalf("header count error=%v", err)
			}
		})
	}
	for _, key := range []string{"", "has space", "bad:colon", "nonascii字", "bad\r\n"} {
		t.Run("header_"+key, func(t *testing.T) {
			b, err := json.Marshal(WebhookConfig{URL: "https://example.invalid", Headers: map[string]string{key: "v"}})
			must(t, err)
			err = CheckChannel(store.NotifyChannel{Name: "n", Kind: store.ChannelWebhook, Config: string(b)})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "HTTP token") {
				t.Fatalf("header key error=%v", err)
			}
		})
	}
}

func TestBodyTemplateContract(t *testing.T) {
	data := Message{Rule: "rule\"", Node: "node\n", Kind: "probe", Transition: "firing", Value: 50.5, At: time.Unix(123, 0), Summary: "line\n\"quote"}
	for _, body := range []string{"", DefaultWebhookTemplate} {
		tmpl, err := parseBodyTemplate(body)
		must(t, err)
		var b bytes.Buffer
		must(t, tmpl.Execute(&b, data))
		var got map[string]any
		must(t, json.Unmarshal(b.Bytes(), &got))
		want := map[string]any{"rule": data.Rule, "node": data.Node, "kind": data.Kind, "transition": data.Transition, "value": data.Value, "at": float64(123), "summary": data.Summary}
		for key, value := range want {
			if got[key] != value {
				t.Errorf("template %s=%v want %v; output=%s", key, got[key], value, b.String())
			}
		}
	}
}

func TestNextProbeAt(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 3, 45, 0, time.UTC)
	want := time.Date(2026, 9, 24, 12, 4, 3, 0, time.UTC)
	if got := nextProbeAt(now); !got.Equal(want) {
		t.Fatalf("next=%s want %s", got, want)
	}
}
