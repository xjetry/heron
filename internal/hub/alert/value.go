package alert

import (
	"fmt"

	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

// reading 是一次规则评估给出的观测值。absent 为真表示这次转换没有可报告的观测：到期规则因清除到期日而恢复没有剩余天数，
// 流量规则因清除配额而恢复没有配额占比。其余转换都带观测值，单位由 valueUnit 按规则身份给出。
type reading struct {
	value  float64
	absent bool
}

func observedValue(v float64) reading { return reading{value: v} }

// noReading 是没有观测值的转换。
var noReading = reading{absent: true}

// valueUnit 是规则事件 value 的单位，取自单位词表（metric.Units）。单位只由规则身份决定——种类、探测指标、资源
// 指标——这些字段一变，规则的状态就整体清掉（SaveAlertRule 同事务裁剪、publishRule 同步内存），所以同一个状态项上的
// 触发、补发与恢复用同一个单位，补发（renotifyUnsilenced）按当前规则取到的就是原触发的单位。
// 资源规则的单位就是它读取的那一列的单位（resourceIndex 与评估同一处取列），不另记一份。
// ok 为假表示种类或指标不在表内：新增种类而没有在这里给出单位时，生成事件以错误失败，而不是落下一个说不清的值。
func valueUnit(r store.AlertRule) (unit string, ok bool) {
	switch r.Kind {
	case store.KindOffline:
		// 未上报的时长，秒。
		return metric.UnitSeconds, true
	case store.KindProbe:
		switch r.Metric {
		case store.MetricLossPct:
			return metric.UnitPercent, true
		case store.MetricRttMs:
			return metric.UnitMilliseconds, true
		}
	case store.KindResource:
		if i := resourceIndex(r.ResourceMetric); i >= 0 {
			return metric.Columns[i].Unit, true
		}
	case store.KindTraffic:
		// 已用量占配额的百分比。
		return metric.UnitPercent, true
	case store.KindExpiry, store.KindCertExpiry:
		// 距到期的日历日数，负数是已过期的天数。
		return metric.UnitDays, true
	}
	return "", false
}

// eventValue 把一次观测写成事件的 value 与 value_unit：没有观测值的转换写 0 与 n/a。
func eventValue(r store.AlertRule, rd reading) (float64, string, error) {
	if rd.absent {
		return 0, store.ValueUnitNotApplicable, nil
	}
	unit, ok := valueUnit(r)
	if !ok {
		return 0, "", fmt.Errorf("rule %d: no value unit for kind %q (probe metric %q, resource metric %q)", r.ID, r.Kind, r.Metric, r.ResourceMetric)
	}
	return rd.value, unit, nil
}
