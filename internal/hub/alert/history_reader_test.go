package alert

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

// recordingHistory 把评估读转给真实的评估读者，并记下每次调用的种类与节点。
type recordingHistory struct {
	inner HistoryReader
	mu    sync.Mutex
	calls []string
}

func (h *recordingHistory) record(kind string, node int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, fmt.Sprintf("%s node=%d", kind, node))
}

func (h *recordingHistory) QueryMetrics(ctx context.Context, nodeID int64, from, to int64, lv store.Level, step int64) ([]metric.Row, error) {
	h.record("metrics", nodeID)
	return h.inner.QueryMetrics(ctx, nodeID, from, to, lv, step)
}

func (h *recordingHistory) QueryProbes(ctx context.Context, nodeID int64, from, to int64, lv store.Level, step int64) ([]metric.ProbeRow, error) {
	h.record("probes", nodeID)
	return h.inner.QueryProbes(ctx, nodeID, from, to, lv, step)
}

func (h *recordingHistory) ProbeBucketMeans(ctx context.Context, nodeID int64, taskID uint64, from, to int64) ([]float64, error) {
	h.record("bucket means", nodeID)
	return h.inner.ProbeBucketMeans(ctx, nodeID, taskID, from, to)
}

// 引擎只经 HistoryReader 读历史：Storage 上没有历史读方法（引擎代码因此在编译期就拿不到 r / hr 上的历史读），
// 资源与探测两类评估、以及基线重算的历史读都落在给 New 的读者上，且判定用的正是读者返回的行。
func TestEngineReadsHistoryOnlyThroughReader(t *testing.T) {
	storage := reflect.TypeFor[Storage]()
	for _, name := range []string{"QueryMetrics", "QueryProbes", "ProbeBucketMeans"} {
		if _, ok := storage.MethodByName(name); ok {
			t.Errorf("alert.Storage has %s; history reads must only be reachable through HistoryReader", name)
		}
	}

	f := newFixture(t)
	history := &recordingHistory{inner: f.st.Evaluation()}
	f.e = New(Config{TTL: 30 * time.Second, Location: f.loc}, f.st, history, f.l, f.clk, f.log)
	must(t, f.e.Load(t.Context()))
	node := f.ids[0]
	task := f.task(t, f.ids[:1])
	ts := f.clk.Now().Unix() - 60
	probeRule := f.rule(t, store.AlertRule{Name: "探测", Kind: store.KindProbe, Enabled: true, NodeIDs: []int64{node}, TaskID: task, Metric: store.MetricLossPct, Threshold: 20, ForMinutes: 1})
	resourceRule := f.rule(t, store.AlertRule{Name: "内存", Kind: store.KindResource, Enabled: true, NodeIDs: []int64{node}, ResourceMetric: store.MetricMemoryUsedPct, Threshold: 90, RecoveryThreshold: 80, ForMinutes: 1})
	f.minutes(t, task, node, ts, metric.ProbeBucket{Sent: 1, Lost: 1})
	b := metric.NewBucket()
	i := metric.Index(string(store.MetricMemoryUsedPct))
	b.Sum[i], b.N[i] = 95, 1
	_, err := f.st.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: node, TS: ts, CoverageStart: ts, Bucket: b}}})
	must(t, err)

	must(t, f.e.EvaluateResources(t.Context(), ts))
	must(t, f.e.EvaluateProbes(t.Context(), ts))
	baselineRule := f.rule(t, adaptiveRule(task, 1, 1))
	must(t, f.e.RecomputeBaselines(t.Context(), f.clk.Now()))

	history.mu.Lock()
	calls := slices.Clone(history.calls)
	history.mu.Unlock()
	for _, want := range []string{fmt.Sprintf("metrics node=%d", node), fmt.Sprintf("probes node=%d", node), fmt.Sprintf("bucket means node=%d", node)} {
		if !slices.Contains(calls, want) {
			t.Errorf("history reader calls = %v, missing %q", calls, want)
		}
	}
	wantState(t, f.e, resourceRule.ID, node, store.StateFiring)
	wantState(t, f.e, probeRule.ID, node, store.StateFiring)
	if _, ok := f.baseline(t, baselineRule.ID, node); !ok {
		t.Error("recompute through the reader wrote no baseline row")
	}
}
