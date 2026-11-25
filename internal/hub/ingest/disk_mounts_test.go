package ingest

import (
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
