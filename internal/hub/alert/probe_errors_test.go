package alert

import (
	"testing"

	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

// 探测错误不是可达性读数：一分钟里全是错误（本机失去所需地址族、名字解析失败）时这一分钟没有丢包率，
// 不能算成 0% 把正在触发的丢包告警判成恢复；有一次有效测量的分钟照常按丢包率判定。
func TestAllErrorMinuteNeitherRecoversNorFiresLoss(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, f.ids[:1])
	ts := f.clk.Now().Unix() - 180
	r := f.rule(t, store.AlertRule{Name: "探测", Kind: store.KindProbe, Enabled: true, AllNodes: true, TaskID: task, Metric: store.MetricLossPct, Threshold: 20, ForMinutes: 1})
	f.minutes(t, task, f.ids[0], ts, metric.ProbeBucket{Sent: 2, Lost: 2}, metric.ProbeBucket{Sent: 3, Errors: 3}, metric.ProbeBucket{Sent: 3, Lost: 0, Errors: 2, RttN: 1, RttSumUs: 900, RttMinUs: 900, RttMaxUs: 900})
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	must(t, f.e.EvaluateProbes(t.Context(), ts+60))
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	must(t, f.e.EvaluateProbes(t.Context(), ts+120))
	wantState(t, f.e, r.ID, f.ids[0], store.StateOK)
}
