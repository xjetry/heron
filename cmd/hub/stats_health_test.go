package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// CLI 的 SQL 健康行与 GetStorageStats 的响应出自同一份 store.StorageStats：同一个库上，行数与顺序和响应一一对应、
// 数值相等。响应经 API token 取得，也钉住只读口径可以调用它。
func TestStatsHealthLinesMatchGetStorageStats(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hub.db")
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(db, clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	node, _, err := st.CreateNode(ctx, store.NewNode{Name: "n"}, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := st.SaveProbeTask(ctx, &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "192.0.2.1", IntervalS: 60, TimeoutMs: 1000}, store.NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := clk.Now().Add(-time.Hour).Unix()
	b := metric.NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(5)})
	batch := metric.Batch{
		Rows:   []metric.Row{{NodeID: node, TS: ts, CoverageStart: ts, Bucket: b}},
		Probes: []metric.ProbeRow{{NodeID: node, TS: ts, TaskID: task.Task.Id, Bucket: &metric.ProbeBucket{Sent: 1}}},
	}
	if _, err := st.WriteMinuteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	// 只上卷、不清理：CLI 与响应里 rollup 有值、prune 缺失，两种形态都要对上。
	if err := st.Rollup(ctx); err != nil {
		t.Fatal(err)
	}
	// 删一个节点、不跑清理：留下一个待清理作业，两边的 cleanup.pending 都要是 1 而不是零值。
	gone, _, err := st.CreateNode(ctx, store.NewNode{Name: "gone"}, append(make([]byte, 31), 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteNode(ctx, gone); err != nil {
		t.Fatal(err)
	}
	plain, hash := auth.NewAPIToken()
	if _, err := st.CreateAPIToken(ctx, "reader", hash, clk.Now(), 100, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := runStatsWith([]string{"--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	start := slices.IndexFunc(lines, func(l string) bool { k, _, _ := strings.Cut(l, ":"); return strings.Contains(k, ".") })
	if start < 0 {
		t.Fatalf("no health lines in %q", out.String())
	}
	// 逐行对照只比 SQL 读出的值。两入口各算一次：sql.observed_at 是各自那次计算的时刻（CLI 经 openOffline 用真实时钟，
	// 下面的 API 用假时钟），wal.* 与 schema_backup* 是各自的文件观测，两入口之间还关闭又打开过库，它们都不参与对照
	// （备份行的口径由 TestStatsPrintsSchemaBackups 钉住）。
	cli := slices.DeleteFunc(lines[start:], func(line string) bool {
		return strings.HasPrefix(line, "wal.") || strings.HasPrefix(line, "schema_backup") || strings.HasPrefix(line, "sql.observed_at: ")
	})

	st, err = store.Open(db, clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(newTestMuxOn(t, st, clk))
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/heron.v1.AdminService/GetStorageStats", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GetStorageStats with an API token: %d %s %v", resp.StatusCode, body, err)
	}
	var msg heronv1.GetStorageStatsResponse
	if err := protojson.Unmarshal(body, &msg); err != nil {
		t.Fatal(err)
	}
	orNoneOf := func(has bool, v int64) string {
		if !has {
			return "none"
		}
		return fmt.Sprint(v)
	}
	var want []string
	for _, h := range msg.GetSeries() {
		want = append(want, h.GetTable()+".oldest: "+orNoneOf(h.OldestTs != nil, h.GetOldestTs()))
		if h.WatermarkTs != nil {
			want = append(want, fmt.Sprintf("%s.watermark: %d", h.GetTable(), h.GetWatermarkTs()))
		}
	}
	want = append(want, "prune.finished_at: "+orNoneOf(msg.LastPruneAt != nil, msg.GetLastPruneAt()),
		"rollup.finished_at: "+orNoneOf(msg.LastRollupAt != nil, msg.GetLastRollupAt()),
		fmt.Sprintf("cleanup.pending: %d", msg.GetCleanupPending()))
	if !slices.Equal(cli, want) {
		t.Fatalf("CLI health lines:\n%s\nAPI response as lines:\n%s", strings.Join(cli, "\n"), strings.Join(want, "\n"))
	}
	// 两边一致之外，内容本身也要是这次造的数据：否则两边一起为空也会相等。
	for _, line := range []string{fmt.Sprintf("metric_1m.oldest: %d", ts-ts%60), fmt.Sprintf("probe_1m.oldest: %d", ts-ts%60),
		fmt.Sprintf("rollup.finished_at: %d", clk.Now().Unix()), "prune.finished_at: none", "metric_1h.oldest: none", "cleanup.pending: 1"} {
		if !slices.Contains(cli, line) {
			t.Errorf("CLI health lines lack %q:\n%s", line, strings.Join(cli, "\n"))
		}
	}
	// scripts/e2e.sh 用同一规则从 CLI 输出里认表行（键不带点号、值为整数、不是 db_bytes），再与响应的表清单逐行比较，
	// 但它只在 make e2e 里跑。这里在单元测试里按同一规则先查一遍：不是表的读数若用了不带点号的键，会被当成一张表。
	tableRow := regexp.MustCompile(`^([a-z0-9_]+): [0-9]+$`)
	var cliTables []string
	for _, line := range lines {
		if m := tableRow.FindStringSubmatch(line); m != nil && m[1] != "db_bytes" {
			cliTables = append(cliTables, m[1])
		}
	}
	var apiTables []string
	for _, tr := range msg.GetTables() {
		apiTables = append(apiTables, tr.GetName())
	}
	if !slices.Equal(cliTables, apiTables) {
		t.Fatalf("CLI lines read as table rows:\n%s\nAPI tables:\n%s", strings.Join(cliTables, "\n"), strings.Join(apiTables, "\n"))
	}
}
