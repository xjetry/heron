package alert

import (
	"slices"
	"strings"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

// shape 是决定事件单位的规则身份：种类，探测规则另带探测指标，资源规则另带资源指标。
type shape struct {
	kind     store.AlertKind
	probe    store.ProbeMetric
	resource store.ResourceMetric
}

// ruleShapes 从协议枚举列出全部规则形态：协议层每个种类、每个探测指标、每个资源指标都在内，所以协议新增一个种类或指标，
// 这里自动多出一项。枚举名去掉前缀转小写就是 store 里的取值——现有各值都如此（api 的 alertKinds、probeMetrics、
// resourceMetrics 逐项可查）；将来哪个值不守这个写法，推出的形态不在 valueUnit 的表内，测试在那一项上红，不会静默漏掉。
func ruleShapes() []shape {
	name := func(full, prefix string) string { return strings.ToLower(strings.TrimPrefix(full, prefix)) }
	var out []shape
	for v, kindName := range heronv1.AlertKind_name {
		if v == int32(heronv1.AlertKind_ALERT_KIND_UNSPECIFIED) {
			continue
		}
		kind := store.AlertKind(name(kindName, "ALERT_KIND_"))
		switch kind {
		case store.KindProbe:
			for m, metricName := range heronv1.ProbeMetric_name {
				if m != int32(heronv1.ProbeMetric_PROBE_METRIC_UNSPECIFIED) {
					out = append(out, shape{kind: kind, probe: store.ProbeMetric(name(metricName, "PROBE_METRIC_"))})
				}
			}
		case store.KindResource:
			for m, metricName := range heronv1.ResourceMetric_name {
				if m != int32(heronv1.ResourceMetric_RESOURCE_METRIC_UNSPECIFIED) {
					out = append(out, shape{kind: kind, resource: store.ResourceMetric(name(metricName, "RESOURCE_METRIC_"))})
				}
			}
		default:
			out = append(out, shape{kind: kind})
		}
	}
	return out
}

// 每一种规则形态都有明确的单位，且取自单位词表、过得了 store 的写入准入。期望表是 AlertEvent.value_unit 注释的逐项
// 落实；协议新增种类或指标而 valueUnit 没给单位时，eventValue 报错，这条测试红在那一项上。
func TestEveryRuleShapeHasValueUnit(t *testing.T) {
	want := map[shape]string{
		{kind: store.KindOffline}:                                       metric.UnitSeconds,
		{kind: store.KindProbe, probe: store.MetricLossPct}:             metric.UnitPercent,
		{kind: store.KindProbe, probe: store.MetricRttMs}:               metric.UnitMilliseconds,
		{kind: store.KindResource, resource: store.MetricMemoryUsedPct}: metric.UnitPercent,
		{kind: store.KindResource, resource: store.MetricDiskUsedPct}:   metric.UnitPercent,
		{kind: store.KindResource, resource: store.MetricCpuPct}:        metric.UnitPercent,
		{kind: store.KindResource, resource: store.MetricLoad1PerCore}:  metric.UnitDimensionless,
		{kind: store.KindResource, resource: store.MetricNetRxBps}:      metric.UnitBytesPerSecond,
		{kind: store.KindResource, resource: store.MetricNetTxBps}:      metric.UnitBytesPerSecond,
		{kind: store.KindTraffic}:                                       metric.UnitPercent,
		{kind: store.KindExpiry}:                                        metric.UnitDays,
		{kind: store.KindCertExpiry}:                                    metric.UnitDays,
	}
	shapes := ruleShapes()
	if len(shapes) != len(want) {
		t.Errorf("protocol enumerates %d rule shapes, the expectation table has %d", len(shapes), len(want))
	}
	for _, s := range shapes {
		r := store.AlertRule{ID: 1, Kind: s.kind, Metric: s.probe, ResourceMetric: s.resource}
		value, unit, err := eventValue(r, observedValue(42))
		if err != nil {
			t.Errorf("%+v: %v", s, err)
			continue
		}
		expected, ok := want[s]
		if !ok {
			t.Errorf("%+v: unit %q has no expectation; add the shape to the table and to the AlertEvent.value_unit comment", s, unit)
		} else if unit != expected {
			t.Errorf("%+v: unit %q, want %q", s, unit, expected)
		}
		if value != 42 || !slices.Contains(metric.Units(), unit) {
			t.Errorf("%+v: value %v unit %q, want 42 with a unit from the vocabulary", s, value, unit)
		}
		if err := store.CheckEventValueUnit(store.TransitionFiring, value, unit); err != nil {
			t.Errorf("%+v: store rejects the unit: %v", s, err)
		}
		// 没有观测值的转换与种类无关，一律 0 与 n/a。
		if v, u, err := eventValue(r, noReading); err != nil || v != 0 || u != store.ValueUnitNotApplicable {
			t.Errorf("%+v: no reading gives %v %q err=%v, want 0 n/a", s, v, u, err)
		}
	}
	// 表外的种类与指标不落事件：生成事件以错误失败，而不是写一个说不清的单位。
	for _, r := range []store.AlertRule{{Kind: "unknown"}, {Kind: store.KindProbe, Metric: "jitter_ms"}, {Kind: store.KindResource, ResourceMetric: "swap_pct"}} {
		if _, _, err := eventValue(r, observedValue(1)); err == nil {
			t.Errorf("%+v: eventValue accepted a rule shape without a unit", r)
		}
	}
}
