package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/metric"
)

// readsWithPoolHeld 占住 held 的全部连接后执行 read：read 必须在 5 秒内无错完成、哪个池都不排队。read 若需要 held，
// 就会排在占住的连接后面直到超时。
func readsWithPoolHeld(t *testing.T, s *Store, held *sql.DB, name string, read func(context.Context) (int, error)) {
	t.Helper()
	release := holdReadConns(t, held, held.Stats().MaxOpenConnections)
	defer release()
	before := poolWaits(s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	n, err := read(ctx)
	cancel()
	if waits := poolWaitsSince(s, before); err != nil || waits != "" || n == 0 {
		t.Errorf("%s: %d rows, err=%v, queued %q; want rows, no error and no queueing", name, n, err, waits)
	}
}

// 单节点探测查询的首次选池按调用方给的序列数估：2 个任务、6 小时 1m 级是 360 桶 × 2 = 720 行，在分界之下，历史池被
// 占满时照常在轻池完成；按序列上限 64 估（23040 行）它就会进历史池、排在占住的连接后面。
func TestProbeSeriesEstimatePicksFirstPool(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	seedProbeTasks(t, s, 1, 2)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	const base = int64(86400 * 10)
	var rows []metric.ProbeRow
	for m := int64(0); m < 360; m += 30 {
		rows = append(rows, probeRow(id, base+m*60, 1, []uint32{500}, 0, 0), probeRow(id, base+m*60, 2, []uint32{700}, 0, 0))
	}
	seedMinute(t, s, rows...)
	lv, _ := LevelByName("1m")
	if est := scanEstimate(base, base+6*3600-1, lv, 2); est > lightScanRows || scanEstimate(base, base+6*3600-1, lv, 64) <= lightScanRows {
		t.Fatalf("window must sit between the two estimates: 2 series %d, 64 series %d, cutoff %d", est, scanEstimate(base, base+6*3600-1, lv, 64), lightScanRows)
	}
	readsWithPoolHeld(t, s, s.hr, "2-task probe query, 6h at 1m", func(ctx context.Context) (int, error) {
		got, err := s.QueryProbes(ctx, id, base, base+6*3600, lv, 60, 2)
		return len(got), err
	})
}

// 对比查询的序列数就是节点数（每节点每桶至多一行）：6 小时 1m 级，2 个节点 720 行在分界之下、历史池占满时照常在轻池
// 完成；MaxComparisonNodes 个节点 5760 行进历史池，轻池占满时照常完成。
func TestComparisonEstimateUsesNodeCount(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	seedProbeTasks(t, s, 7)
	const base = int64(86400 * 10)
	var nodes []int64
	for i := range MaxComparisonNodes {
		id, _, err := s.CreateNode(t.Context(), NewNode{Name: fmt.Sprintf("n%d", i)}, hash(byte(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, id)
		seedMinute(t, s, probeRow(id, base+600, 7, []uint32{500}, 0, 0))
	}
	lv, _ := LevelByName("1m")
	readsWithPoolHeld(t, s, s.hr, "comparison over 2 nodes, 6h at 1m", func(ctx context.Context) (int, error) {
		got, err := s.QueryProbeComparison(ctx, 7, nodes[:2], base, base+6*3600, lv, 60)
		return len(got), err
	})
	readsWithPoolHeld(t, s, s.r, "comparison over MaxComparisonNodes nodes, 6h at 1m", func(ctx context.Context) (int, error) {
		got, err := s.QueryProbeComparison(ctx, 7, nodes, base, base+6*3600, lv, 60)
		return len(got), err
	})
}
