package alert

import (
	"strings"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

// 磁盘规则读的是最满盘那一列，不是根盘的 disk_used_pct：写进别的列的读数不会让它触发。
func TestResourceContinuousWindowsAndMissingReadings(t *testing.T) {
	for _, tc := range []struct {
		resource store.ResourceMetric
		column   string
	}{
		{store.MetricMemoryUsedPct, "memory_used_pct"},
		{store.MetricDiskUsedPct, "disk_max_used_pct"},
	} {
		resource := tc.resource
		t.Run(string(resource), func(t *testing.T) {
			f := newFixture(t)
			r := f.rule(t, store.AlertRule{Name: "容量", Kind: store.KindResource, Enabled: true, NodeIDs: f.ids[:1], ResourceMetric: resource, Threshold: 90, RecoveryThreshold: 80, ForMinutes: 2})
			base := f.clk.Now().Unix()
			write := func(offset int64, value float64) {
				b := metric.NewBucket()
				i := metric.Index(tc.column)
				b.Sum[i], b.N[i] = value, 1
				_, err := f.st.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: f.ids[0], TS: base + offset*60, CoverageStart: base, Bucket: b}}})
				must(t, err)
			}
			eval := func(offset int64, want store.AlertState) {
				must(t, f.e.EvaluateResources(t.Context(), base+offset*60))
				wantState(t, f.e, r.ID, f.ids[0], want)
			}
			write(0, 90)
			eval(0, store.StatePending)
			write(1, 95)
			eval(1, store.StateFiring)
			write(2, 85)
			eval(2, store.StateFiring)
			write(3, 80)
			eval(3, store.StateFiring)
			_, err := f.st.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: f.ids[0], TS: base + 4*60, CoverageStart: base, Bucket: metric.NewBucket(), ObservationOnly: true, Observed: true}}})
			must(t, err)
			eval(4, store.StateFiring)
			f.restart(t)
			write(5, 75)
			eval(5, store.StateFiring)
			write(6, 80)
			eval(6, store.StateOK)
			events := f.events(t)
			if len(events) != 2 || events[0].Transition != store.TransitionRecovered || events[1].Transition != store.TransitionFiring {
				t.Fatalf("events=%+v", events)
			}
		})
	}
}

// 四个新指标的窗口语义与既有百分比指标同形：写入值就是列的读数（按核负载写 agent 算好的商，不再由 hub 去除），
// high 达到阈值、higher 更高、mid 落在滞回区间、low 低于恢复阈值；无写入的分钟验证缺读数不恢复。
func TestResourceNewMetricWindows(t *testing.T) {
	for _, tc := range []struct {
		name      string
		metric    store.ResourceMetric
		column    string
		threshold float64
		recovery  float64
		high      float64
		higher    float64
		mid       float64
		low       float64
	}{
		{"cpu_pct", store.MetricCpuPct, "cpu", 90, 80, 90, 95, 85, 75},
		{"net_rx_bps", store.MetricNetRxBps, "net_rx_bps", 1.25e8, 1e8, 1.25e8, 1.3e8, 1.1e8, 9e7},
		{"net_tx_bps", store.MetricNetTxBps, "net_tx_bps", 1.25e8, 1e8, 1.25e8, 1.3e8, 1.1e8, 9e7},
		{"load1_per_core", store.MetricLoad1PerCore, "load1_per_core", 2, 1, 2, 2.5, 1.5, 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r := f.rule(t, store.AlertRule{Name: "资源", Kind: store.KindResource, Enabled: true, NodeIDs: f.ids[:1], ResourceMetric: tc.metric, Threshold: tc.threshold, RecoveryThreshold: tc.recovery, ForMinutes: 2})
			base := f.clk.Now().Unix()
			write := func(offset int64, value float64) {
				b := metric.NewBucket()
				i := metric.Index(tc.column)
				b.Sum[i], b.N[i] = value, 1
				_, err := f.st.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: f.ids[0], TS: base + offset*60, CoverageStart: base, Bucket: b}}})
				must(t, err)
			}
			// 该分钟有行但目标列没有样本（N=0）：与完全没有行的分钟一样是缺读数，不能令 firing 恢复。
			writeOther := func(offset int64) {
				b := metric.NewBucket()
				i := metric.Index("mem_used")
				b.Sum[i], b.N[i] = 1024, 1
				_, err := f.st.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: f.ids[0], TS: base + offset*60, CoverageStart: base, Bucket: b}}})
				must(t, err)
			}
			eval := func(offset int64, want store.AlertState) {
				must(t, f.e.EvaluateResources(t.Context(), base+offset*60))
				wantState(t, f.e, r.ID, f.ids[0], want)
			}
			write(0, tc.high)
			eval(0, store.StatePending)
			write(1, tc.higher)
			eval(1, store.StateFiring)
			write(2, tc.mid)
			eval(2, store.StateFiring)
			writeOther(3)
			eval(3, store.StateFiring)
			eval(4, store.StateFiring)
			write(5, tc.low)
			eval(5, store.StateFiring)
			write(6, tc.recovery)
			eval(6, store.StateOK)
			events := f.events(t)
			if len(events) != 2 || events[0].Transition != store.TransitionRecovered || events[1].Transition != store.TransitionFiring {
				t.Fatalf("events=%+v", events)
			}
		})
	}
}

// 按核负载只看 load1_per_core 列。agent 不上报 load_cores 时该分钟无读数，既不触发也不恢复。
// 列里的商是采样当时算好的：之后 Facts 的核数变了，已经入库的分钟仍按原值评估，不再除一次。
func TestResourceLoad1PerCoreReadsTheColumn(t *testing.T) {
	f := newFixture(t)
	// 核数故意与列里的商不一致：3 是按 2 核算的，Facts 里是 4。读回 Facts 再除、或对列再除，都到不了阈值 2。
	must(t, f.st.UpsertFacts(t.Context(), f.ids[0], 1, &heronv1.Facts{CpuCores: 4}))
	r := f.rule(t, store.AlertRule{Name: "按核负载", Kind: store.KindResource, Enabled: true, NodeIDs: f.ids[:1], ResourceMetric: store.MetricLoad1PerCore, Threshold: 2, RecoveryThreshold: 1, ForMinutes: 2})
	base := f.clk.Now().Unix()
	write := func(offset int64, perCore float64, withColumn bool) {
		b := metric.NewBucket()
		load := metric.Index("load1")
		b.Sum[load], b.N[load] = perCore*2, 1
		if withColumn {
			i := metric.Index("load1_per_core")
			b.Sum[i], b.N[i] = perCore, 1
		}
		_, err := f.st.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: f.ids[0], TS: base + offset*60, CoverageStart: base, Bucket: b}}})
		must(t, err)
	}
	eval := func(offset int64, want store.AlertState) {
		must(t, f.e.EvaluateResources(t.Context(), base+offset*60))
		wantState(t, f.e, r.ID, f.ids[0], want)
	}
	evalOK := func(offset int64) {
		must(t, f.e.EvaluateResources(t.Context(), base+offset*60))
		if got := stateOf(f.e, r.ID, f.ids[0]); got != "" && got != store.StateOK {
			t.Fatalf("node %d state=%q want ok or no record", f.ids[0], got)
		}
	}
	write(0, 3, false)
	write(1, 3, false)
	evalOK(1)
	write(2, 3, true)
	eval(2, store.StatePending)
	write(3, 3, true)
	eval(3, store.StateFiring)
	must(t, f.st.UpsertFacts(t.Context(), f.ids[0], 2, &heronv1.Facts{CpuCores: 100}))
	write(4, 3, true)
	eval(4, store.StateFiring)
	write(5, 3, false)
	eval(5, store.StateFiring)
	write(6, 0.5, true)
	write(7, 0.5, true)
	eval(7, store.StateOK)
}

// 根盘空、数据盘满：磁盘规则按最满盘触发，根盘那一列再高也不参与。触发摘要点名 hub 实时状态里当前最满的挂载路径；
// 实时状态没有逐盘读数（旧版 agent、hub 刚重启）时回退为不带路径的原文案。恢复文案不带路径。
func TestResourceDiskFiresOnFullestMountAndNamesIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mounts []*heronv1.DiskMountUsage
		firing string
	}{
		{"live mounts", []*heronv1.DiskMountUsage{{Path: "/data", Total: 100, Used: 95}, {Path: "/", Total: 100, Used: 30}}, "节点 node1 磁盘（/data）持续 2 分钟达到 90.0%（规则 磁盘）"},
		{"no live mounts", nil, "节点 node1 磁盘持续 2 分钟达到 90.0%（规则 磁盘）"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r := f.rule(t, store.AlertRule{Name: "磁盘", Kind: store.KindResource, Enabled: true, NodeIDs: f.ids[:1], ResourceMetric: store.MetricDiskUsedPct, Threshold: 90, RecoveryThreshold: 80, ForMinutes: 2})
			if tc.mounts != nil {
				f.l.Observe(f.ids[0], "", &heronv1.Metrics{})
				f.l.ObserveDisks(f.ids[0], tc.mounts, uint32(len(tc.mounts)))
			}
			base := f.clk.Now().Unix()
			write := func(offset int64, root, fullest float64) {
				b := metric.NewBucket()
				ri, fi := metric.Index("disk_used_pct"), metric.Index("disk_max_used_pct")
				b.Sum[ri], b.N[ri] = root, 1
				b.Sum[fi], b.N[fi] = fullest, 1
				_, err := f.st.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: f.ids[0], TS: base + offset*60, CoverageStart: base, Bucket: b}}})
				must(t, err)
			}
			eval := func(offset int64, want store.AlertState) {
				t.Helper()
				must(t, f.e.EvaluateResources(t.Context(), base+offset*60))
				wantState(t, f.e, r.ID, f.ids[0], want)
			}
			// 根盘写满而最满盘列低于阈值：不触发（没有状态记录或 ok）。
			write(0, 99, 50)
			write(1, 99, 50)
			must(t, f.e.EvaluateResources(t.Context(), base+60))
			if got := stateOf(f.e, r.ID, f.ids[0]); got != "" && got != store.StateOK {
				t.Fatalf("root-only full disk moved the rule to %q", got)
			}
			write(2, 30, 95)
			write(3, 30, 95)
			eval(3, store.StateFiring)
			write(4, 30, 70)
			write(5, 30, 70)
			eval(5, store.StateOK)
			events := f.events(t)
			if len(events) != 2 || events[1].Transition != store.TransitionFiring || events[0].Transition != store.TransitionRecovered {
				t.Fatalf("events=%+v", events)
			}
			if events[1].Summary != tc.firing {
				t.Fatalf("firing summary = %q, want %q", events[1].Summary, tc.firing)
			}
			// 恢复文案之后引擎会追加告警持续时长，这里只钉前段。
			if want := "节点 node1 磁盘持续 2 分钟不高于 80.0%，已恢复（规则 磁盘）"; !strings.HasPrefix(events[0].Summary, want) {
				t.Fatalf("recovered summary = %q, want prefix %q", events[0].Summary, want)
			}
		})
	}
}
