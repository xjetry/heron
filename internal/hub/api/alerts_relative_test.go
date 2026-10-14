package api

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

// 相对判定的两种基线来源经真实 Connect 处理器保存并原样回显；协议层的拒绝（未指定基线来源、非 rtt 规则带模式、
// 表外枚举值、区间越界）都是 InvalidArgument，并点名协议字段。
func TestSaveAlertRuleRelativeBaseline(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	task, _, err := h.reg.Save(t.Context(), validProbeTask(), store.NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	relative := func() *heronv1.AlertRule {
		return &heronv1.AlertRule{Name: "基线", Kind: heronv1.AlertKind_ALERT_KIND_PROBE, AllNodes: true, Enabled: true, TaskId: task.Task.Id,
			Metric: heronv1.ProbeMetric_PROBE_METRIC_RTT_MS, ForMinutes: 5, RttMode: heronv1.RttMode_RTT_MODE_RELATIVE,
			BaselineMode: heronv1.BaselineMode_BASELINE_MODE_ADAPTIVE, BaselineWindowS: 86400, BaselineMinSamples: 12,
			UpperDeviationPct: 100, LowerDeviationPct: 50, CooldownS: 1800}
	}
	fixed := relative()
	fixed.BaselineMode, fixed.BaselineWindowS, fixed.BaselineMinSamples, fixed.FixedBaselineMs = heronv1.BaselineMode_BASELINE_MODE_FIXED, 0, 0, 42.5
	for _, want := range []*heronv1.AlertRule{relative(), fixed} {
		got := saveRule(t, h, want)
		want.Id, want.CreatedAt = got.Id, h.clk.Now().Unix()
		if !proto.Equal(got, want) {
			t.Fatalf("rule=%v want=%v", got, want)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*heronv1.AlertRule)
		want   string
	}{
		{"baseline_mode_unspecified", func(r *heronv1.AlertRule) { r.BaselineMode = heronv1.BaselineMode_BASELINE_MODE_UNSPECIFIED }, "rule.baseline_mode"},
		{"baseline_mode_out_of_table", func(r *heronv1.AlertRule) { r.BaselineMode = 9 }, "BASELINE_MODE_ADAPTIVE"},
		{"rtt_mode_out_of_table", func(r *heronv1.AlertRule) { r.RttMode = 9 }, "RTT_MODE_RELATIVE"},
		{"loss_with_mode", func(r *heronv1.AlertRule) {
			*r = heronv1.AlertRule{Name: "丢包", Kind: heronv1.AlertKind_ALERT_KIND_PROBE, AllNodes: true, TaskId: r.TaskId, Metric: heronv1.ProbeMetric_PROBE_METRIC_LOSS_PCT,
				Threshold: 20, ForMinutes: 3, RttMode: heronv1.RttMode_RTT_MODE_THRESHOLD}
		}, "rule.rtt_mode"},
		{"relative_threshold", func(r *heronv1.AlertRule) { r.Threshold = 80 }, "rule.threshold"},
		{"upper_out_of_range", func(r *heronv1.AlertRule) { r.UpperDeviationPct = 1001 }, "rule.upper_deviation_pct"},
		{"window_shorter_than_judgment", func(r *heronv1.AlertRule) { r.BaselineWindowS = 299 }, "rule.baseline_window_s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := relative()
			tc.change(r)
			_, err := h.admin.SaveAlertRule(t.Context(), connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: r}))
			if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v; want InvalidArgument naming %s", err, tc.want)
			}
		})
	}
}
