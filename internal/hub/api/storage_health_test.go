package api

import (
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

func (h *harness) storageStatsWithToken(t *testing.T, tok string) *heronv1.GetStorageStatsResponse {
	t.Helper()
	client := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL)
	req := connect.NewRequest(&heronv1.GetStorageStatsRequest{})
	req.Header().Set("Authorization", "Bearer "+tok)
	resp, err := client.GetStorageStats(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg
}

// GetStorageStats 按表给最老桶、水位、保留期与两项判定，经 API token 可读。1m 指标表的最老桶比阈值早 60 秒，
// 1m 探测表恰在阈值上：前者标红、后者不标。上卷之前水位是种子值 0，四张有水位的表都标红，也没有上卷完成时刻；
// 上卷之后水位跟上、不再标红，完成时刻就是上卷时的时钟。
func TestGetStorageStatsReportsHealthWithStaleness(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	_, tok := createToken(t, h, "reader")
	node, _ := h.createNode(t, "n")
	now := h.clk.Now().Unix()
	day := int64(24 * 3600)
	// 阈值 = 保留期 7 天 + 桶长 60 秒 + 维护间隔 60 秒。
	edge := now - 7*day - 60 - 60
	b := metric.NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(5)})
	// 探测历史只为存在的任务落库（WriteMinuteBatch 拒收不存在的任务的行）。
	saved, _, err := h.store.SaveProbeTask(t.Context(), validProbeTask(), store.NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	batch := metric.Batch{
		Rows:   []metric.Row{{NodeID: node, TS: edge - 60, CoverageStart: edge - 60, Bucket: b}},
		Probes: []metric.ProbeRow{{NodeID: node, TS: edge, TaskID: saved.Task.Id, Bucket: &metric.ProbeBucket{Sent: 1}}},
	}
	if _, err := h.store.WriteMinuteBatch(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	type row struct {
		table                   string
		bucket                  uint32
		retention               uint64
		oldest, watermark       *int64
		oldestStale, waterStale bool
	}
	got := func(msg *heronv1.GetStorageStatsResponse) []row {
		var out []row
		for _, s := range msg.GetSeries() {
			out = append(out, row{s.GetTable(), s.GetBucketS(), s.GetRetentionS(), s.OldestTs, s.WatermarkTs, s.GetOldestStale(), s.GetWatermarkStale()})
		}
		return out
	}
	check := func(label string, msg *heronv1.GetStorageStatsResponse, want []row) {
		t.Helper()
		rows := got(msg)
		if len(rows) != len(want) {
			t.Fatalf("%s: %d series, want %d: %+v", label, len(rows), len(want), rows)
		}
		for i := range want {
			g, w := rows[i], want[i]
			if g.table != w.table || g.bucket != w.bucket || g.retention != w.retention || !sameInt(g.oldest, w.oldest) ||
				!sameInt(g.watermark, w.watermark) || g.oldestStale != w.oldestStale || g.waterStale != w.waterStale {
				t.Errorf("%s: series[%d] = %s, want %s", label, i, describeRow(g.table, g.oldest, g.watermark, g.oldestStale, g.waterStale), describeRow(w.table, w.oldest, w.watermark, w.oldestStale, w.waterStale))
			}
		}
	}
	r := store.DefaultRetention
	zero := int64(0)
	before := h.storageStatsWithToken(t, tok)
	check("before rollup", before, []row{
		{"metric_1m", 60, secs(r.M1), ptrTo(edge - 60), nil, true, false},
		{"metric_5m", 300, secs(r.M5), nil, &zero, false, true},
		{"metric_1h", 3600, secs(r.H1), nil, &zero, false, true},
		{"probe_1m", 60, secs(r.M1), ptrTo(edge), nil, false, false},
		{"probe_5m", 300, secs(r.M5), nil, &zero, false, true},
		{"probe_1h", 3600, secs(r.H1), nil, &zero, false, true},
	})
	if before.LastRollupAt != nil || before.LastPruneAt != nil {
		t.Fatalf("maintenance before any run: prune=%v rollup=%v", before.LastPruneAt, before.LastRollupAt)
	}

	if err := h.store.Rollup(t.Context()); err != nil {
		t.Fatal(err)
	}
	// 明确越过 SQL 统计的复用窗口，才用新快照检查上卷结果。
	h.clk.Advance(61 * time.Second)
	after := h.storageStatsWithToken(t, tok)
	for _, s := range after.GetSeries() {
		if s.GetWatermarkStale() || (s.WatermarkTs != nil && now-s.GetWatermarkTs() > 3*int64(s.GetBucketS())) {
			t.Errorf("%s watermark %d still behind after rollup (stale=%v)", s.GetTable(), s.GetWatermarkTs(), s.GetWatermarkStale())
		}
	}
	if after.LastRollupAt == nil || after.GetLastRollupAt() != now || after.LastPruneAt != nil {
		t.Fatalf("maintenance after rollup: prune=%v rollup=%v, want rollup %d", after.LastPruneAt, after.LastRollupAt, now)
	}
}

// GetStorageStats 的 retention_s 与两项标红都要用 api.Config.Retention（serve 传给它的那份配置），不是
// store.DefaultRetention：这个夹具不跑 RunMaintenance，全部数据只由这条用例写入、只由这次调用读出，没有
// 后台协程会把控制行删掉，判定不依赖任何真实时间窗。用 6 小时的 1m 保留期（远短于默认的 7 天）构造，一行
// 7 小时前的 probe_1m 数据在配置下已超期、在默认值下不会超期，两个结论只要有一个取到默认值就会分开。
func TestGetStorageStatsUsesTheConfiguredRetention(t *testing.T) {
	t.Parallel()
	retention := store.Retention{M1: 6 * time.Hour, M5: 168 * time.Hour, H1: 168 * time.Hour, AlertEvents: store.DefaultRetention.AlertEvents}
	h := newZonedHarness(t, "", time.UTC, retention)
	h.login(t)
	_, tok := createToken(t, h, "reader")
	node, _ := h.createNode(t, "n")
	now := h.clk.Now().Unix()
	staleTS := now - 7*3600
	// 探测历史只为存在的任务落库（WriteMinuteBatch 拒收不存在的任务的行）。
	saved, _, err := h.store.SaveProbeTask(t.Context(), validProbeTask(), store.NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	batch := metric.Batch{Probes: []metric.ProbeRow{{NodeID: node, TS: staleTS, TaskID: saved.Task.Id, Bucket: &metric.ProbeBucket{Sent: 1}}}}
	if _, err := h.store.WriteMinuteBatch(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	stats := h.storageStatsWithToken(t, tok)
	wantRetentionS := map[string]uint64{"metric_1m": secs(retention.M1), "metric_5m": secs(retention.M5), "metric_1h": secs(retention.H1),
		"probe_1m": secs(retention.M1), "probe_5m": secs(retention.M5), "probe_1h": secs(retention.H1)}
	var sawProbe1m bool
	for _, s := range stats.GetSeries() {
		if want := wantRetentionS[s.GetTable()]; s.GetRetentionS() != want {
			t.Errorf("%s retention_s = %d, want the configured %d", s.GetTable(), s.GetRetentionS(), want)
		}
		if s.GetTable() == "probe_1m" {
			sawProbe1m = true
			if !s.GetOldestStale() {
				t.Error("probe_1m oldest_stale = false, want true under the configured 6h retention (it is not yet 7 days old, so a fallback to the default retention would read it as healthy)")
			}
		}
	}
	if !sawProbe1m {
		t.Fatal("GetStorageStats did not report probe_1m")
	}
}

func secs(d time.Duration) uint64 { return uint64(d / time.Second) }

func ptrTo(v int64) *int64 { return &v }

func sameInt(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func describeRow(table string, oldest, watermark *int64, oldestStale, waterStale bool) string {
	show := func(v *int64) any {
		if v == nil {
			return "absent"
		}
		return *v
	}
	return fmt.Sprintf("%s oldest=%v watermark=%v oldest_stale=%v watermark_stale=%v", table, show(oldest), show(watermark), oldestStale, waterStale)
}
