package api

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	heronv1connect "github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/hub/store"
)

func TestListRestoreRecordsAccessAndLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	client := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL)
	if _, err := client.ListRestoreRecords(t.Context(), connect.NewRequest(&heronv1.ListRestoreRecordsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous restore records=%v, want unauthenticated", err)
	}
	h.login(t)
	_, token := createToken(t, h, "reader")
	read := func(limit uint32) (*connect.Response[heronv1.ListRestoreRecordsResponse], error) {
		req := connect.NewRequest(&heronv1.ListRestoreRecordsRequest{Limit: limit})
		req.Header().Set("Authorization", "Bearer "+token)
		return client.ListRestoreRecords(t.Context(), req)
	}
	// 只读 token 可调用：恢复记录是 hub 知道的事，与 GetBackupStatus 同一口径。
	resp, err := read(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.GetRecords()) != 0 {
		t.Fatalf("fresh hub restore records=%v", resp.Msg.GetRecords())
	}
	// 上限拒绝而不是截断，错误点名 limit。
	_, err = read(101)
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("limit 101: %v, want invalid_argument naming limit", err)
	}
	if _, err := read(100); err != nil {
		t.Fatalf("limit 100: %v", err)
	}
}

func TestRestoreRecordProtoMapping(t *testing.T) {
	t.Parallel()
	metrics := int64(460)
	got := restoreRecordProto(store.RestoreRecord{ID: "hex", RestoreResult: store.RestoreResult{
		RestoredAt: 500, ConfigTakenAt: 400, MetricsTakenAt: &metrics,
		Orphans: map[string]int64{"probe_1h": 2, "metric_1m": 10},
		Themes:  store.ThemeRestoreSummary{Restored: []string{"dark"}, Missing: []string{"old"}, Ignored: []string{"stray.zip"}},
	}})
	if got.Id != "hex" || got.RestoredAt != 500 || got.ConfigTakenAt != 400 || got.MetricsTakenAt == nil || *got.MetricsTakenAt != metrics {
		t.Fatalf("scalars=%+v", got)
	}
	if len(got.Orphans) != 2 || got.Orphans[0].Table != "metric_1m" || got.Orphans[0].Rows != 10 || got.Orphans[1].Table != "probe_1h" || got.Orphans[1].Rows != 2 {
		t.Fatalf("orphans must be sorted by table name: %+v", got.Orphans)
	}
	if got.Themes.Restored[0] != "dark" || got.Themes.Missing[0] != "old" || got.Themes.Ignored[0] != "stray.zip" {
		t.Fatalf("themes=%+v", got.Themes)
	}
	// 没带指标层的恢复必须保持缺席：nil 指针不落成 0。
	absent := restoreRecordProto(store.RestoreRecord{ID: "config-only", RestoreResult: store.RestoreResult{RestoredAt: 1, ConfigTakenAt: 1}})
	if absent.MetricsTakenAt != nil {
		t.Fatalf("config-only metrics_taken_at=%d, want absent", *absent.MetricsTakenAt)
	}
}
