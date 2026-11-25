package ingest

import (
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func diskReport(tok string, total uint32, mounts ...*heronv1.DiskMountUsage) *connect.Request[heronv1.ReportRequest] {
	req := report(tok, &heronv1.Metrics{BootId: testBootID})
	req.Msg.DiskMounts, req.Msg.DiskMountsTotal = mounts, total
	return req
}

// 逐盘容量进内存实时状态，携带它的那次上报的到达时刻一起记下；下一次上报没带就清空，不回显旧读数。
func TestReportKeepsDiskMountsInLive(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	data := &heronv1.DiskMountUsage{Path: "/data", Total: 100, Used: 90}
	root := &heronv1.DiskMountUsage{Path: "/", Total: 100, Used: 50}
	if _, err := h.client.Report(t.Context(), diskReport(tok, 2, data, root)); err != nil {
		t.Fatal(err)
	}
	d, ok := h.live.Disks(id)
	if !ok || d.Total != 2 || len(d.Mounts) != 2 || !proto.Equal(d.Mounts[0], data) || !proto.Equal(d.Mounts[1], root) {
		t.Fatalf("live disks = %+v", d)
	}
	e, _ := h.live.Get(id)
	if !d.SampledAt.Equal(e.LastSeenWall) || !d.Online {
		t.Fatalf("sampled at %v online %v, want the report's arrival %v and online", d.SampledAt, d.Online, e.LastSeenWall)
	}
	if _, err := h.client.Report(t.Context(), diskReport(tok, 0)); err != nil {
		t.Fatal(err)
	}
	if d, _ := h.live.Disks(id); d.Total != 0 || d.Mounts != nil {
		t.Fatalf("a report without disk readings left the previous ones: %+v", d)
	}
}

// 形状不合法的逐盘容量整条拒收，live 不变。
func TestReportRejectsMalformedDiskMounts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		total  uint32
		mounts []*heronv1.DiskMountUsage
		err    string
	}{
		{"unsorted", 2, []*heronv1.DiskMountUsage{{Path: "/", Total: 100, Used: 10}, {Path: "/data", Total: 100, Used: 90}}, "sorted by usage"},
		{"relative path", 1, []*heronv1.DiskMountUsage{{Path: "data", Total: 100, Used: 10}}, "clean absolute"},
		{"count mismatch", 3, []*heronv1.DiskMountUsage{{Path: "/", Total: 100, Used: 10}}, "disk_mounts: must contain"},
		{"used over total", 1, []*heronv1.DiskMountUsage{{Path: "/", Total: 100, Used: 101}}, "used must not exceed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t)
			id, tok := h.node(t)
			_, err := h.client.Report(t.Context(), diskReport(tok, tc.total, tc.mounts...))
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("error = %v, want InvalidArgument naming %q", err, tc.err)
			}
			if _, ok := h.live.Get(id); ok {
				t.Error("rejected report changed live state")
			}
		})
	}
}

// 最满盘使用率由 hub 推导：带逐盘列表时取首项（列表是全集排序后的前缀，首项即全集最满），不带列表时取根盘，
// 根盘也读不出时缺失。agent 自己填的值一律被覆盖，不进实时读数。
func TestReportDerivesFullestDiskUsage(t *testing.T) {
	data := &heronv1.DiskMountUsage{Path: "/data", Total: 200, Used: 180}
	root := &heronv1.DiskMountUsage{Path: "/", Total: 100, Used: 30}
	for _, tc := range []struct {
		name   string
		total  uint32
		mounts []*heronv1.DiskMountUsage
		root   bool
		want   *float64
	}{
		// 根盘 30%、/data 90%：取列表首项而不是根盘，也不是字节合计 210/300 = 70%。
		{"list", 2, []*heronv1.DiskMountUsage{data, root}, true, proto.Float64(90)},
		// 列表被截断时 total 大于列表长度；首项仍是全集最满。
		{"truncated list", 40, truncatedMounts(), true, proto.Float64(99)},
		{"no list falls back to root", 0, nil, true, proto.Float64(30)},
		{"neither", 0, nil, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t)
			id, tok := h.node(t)
			req := diskReport(tok, tc.total, tc.mounts...)
			// agent 自报一个与事实不符的值，验证它不被采信。
			req.Msg.Metrics.DiskMaxUsedPct = proto.Float64(1)
			if tc.root {
				req.Msg.Metrics.DiskTotal, req.Msg.Metrics.DiskUsed = proto.Uint64(100), proto.Uint64(30)
			}
			if _, err := h.client.Report(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			e, _ := h.live.Get(id)
			got := e.Metrics.DiskMaxUsedPct
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("disk_max_used_pct = %v, want %v", fmtPtr(got), fmtPtr(tc.want))
			}
			if tc.root && *e.Metrics.DiskUsed != 30 {
				t.Fatalf("root disk_used rewritten to %d", *e.Metrics.DiskUsed)
			}
		})
	}
}

// truncatedMounts 是 40 块盘截断到 16 项的列表：首项 99%，其余依次更空。
func truncatedMounts() []*heronv1.DiskMountUsage {
	out := []*heronv1.DiskMountUsage{{Path: "/full", Total: 100, Used: 99}}
	for i := range 15 {
		out = append(out, &heronv1.DiskMountUsage{Path: "/d" + string(rune('a'+i)), Total: 100, Used: uint64(50 - i)})
	}
	return out
}

func fmtPtr(v *float64) string {
	if v == nil {
		return "<nil>"
	}
	return strconv.FormatFloat(*v, 'g', -1, 64)
}
