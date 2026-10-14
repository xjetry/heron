package api

import (
	"database/sql"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestUpdateNodeTrafficQuotaValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "quota")
	for _, tc := range []struct {
		name  string
		quota uint64
		mode  heronv1.TrafficQuotaMode
		valid bool
	}{
		{"disabled ignores mode", 0, 999, true},
		{"smallest", 1, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, true},
		{"largest", 1<<62 - 1, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_RX, true},
		{"upper excluded", 1 << 62, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_TX, false},
		{"unspecified", 1, 0, false},
		{"unknown", 1, 999, false},
		{"max", 1, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_MAX, true},
		{"tx", 1, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_TX, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "quota", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), TrafficQuotaBytes: tc.quota, TrafficQuotaMode: tc.mode}))
			if !tc.valid {
				if connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Fatalf("got %v, want InvalidArgument", err)
				}
				return
			}
			mode := tc.mode
			if tc.quota == 0 {
				mode = heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM
			}
			if err != nil || r.Msg.Node.TrafficQuotaBytes != tc.quota || r.Msg.Node.TrafficQuotaMode != mode {
				t.Fatalf("quota round trip: %v %v", r, err)
			}
		})
	}
	n := h.update(t, &heronv1.UpdateNodeRequest{Id: id, Name: "quota", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0)})
	if n.TrafficQuotaBytes != 0 || n.TrafficQuotaMode != heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM {
		t.Fatalf("omission did not clear quota: %v", n)
	}
}

func TestTrafficQuotaImmediateRecoveryInputs(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"adjust", "quota", "mode", "clear", "threshold", "change"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, "")
			h.login(t)
			id, _ := h.createNode(t, "quota")
			quotaNode(t, h, id, 100, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 1)
			_, err := h.admin.AdjustTraffic(t.Context(), connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: id, PeriodRx: 60, PeriodTx: 30}))
			if err != nil {
				t.Fatal(err)
			}
			r, err := h.alerts.SaveRule(t.Context(), store.AlertRule{Name: "quota", Kind: store.KindTraffic, Enabled: true, AllNodes: true, Threshold: 80})
			if err != nil {
				t.Fatal(err)
			}
			if len(quotaEvents(t, h)) != 1 {
				t.Fatal("save did not fire")
			}
			switch action {
			case "adjust":
				_, err = h.admin.AdjustTraffic(t.Context(), connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: id, PeriodRx: 10}))
				if err != nil {
					t.Fatal(err)
				}
			case "quota":
				quotaNode(t, h, id, 1000, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 1)
			case "mode":
				quotaNode(t, h, id, 100, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_TX, 1)
			case "clear":
				quotaNode(t, h, id, 0, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 1)
			case "threshold":
				r.Threshold = 100
				if _, err := h.alerts.SaveRule(t.Context(), r); err != nil {
					t.Fatal(err)
				}
			case "change":
				client, _, _ := grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{id}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
				m := &heronv1.ExecuteChangeRequest{RequestId: "quota", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"traffic_quota_bytes"}}, Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: id, TrafficQuotaBytes: 1000}}}
				previewChange(t, client, m)
				if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(m)); err != nil {
					t.Fatal(err)
				}
			}
			events := quotaEvents(t, h)
			if len(events) != 2 || events[0].Transition != store.TransitionRecovered {
				t.Fatalf("immediate recovery=%+v", events)
			}
		})
	}
}

func TestTrafficQuotaCrashBeforeFlushKeepsCommittedFiring(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, tok := h.createNode(t, "quota")
	quotaNode(t, h, id, 100, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 1)
	if err := h.report(t, tok, netCounters(testBootID, 100, 100)); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(10 * time.Second)
	if err := h.report(t, tok, netCounters(testBootID, 190, 100)); err != nil {
		t.Fatal(err)
	}
	if err := h.book.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err := h.alerts.SaveRule(t.Context(), store.AlertRule{Name: "quota", Kind: store.KindTraffic, Enabled: true, AllNodes: true, Threshold: 80})
	if err != nil {
		t.Fatal(err)
	}
	// 跨周期后的未提交清零只存在实时账本；崩溃重建时不能先恢复再触发。
	h.clk.SetWall(time.Date(2026, 2, 1, 0, 0, 1, 0, time.UTC))
	h.book.View(id)
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(quotaEvents(t, h)) != 1 {
		t.Fatal("uncommitted rollover converted firing before crash")
	}
	book := traffic.New(h.store, h.clk, time.UTC, slog.Default())
	if err := book.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	engine := alert.New(alert.Config{TTL: 30 * time.Second, Location: time.UTC}, h.store, h.store.Evaluation(), h.live, h.clk, slog.Default())
	engine.SetTraffic(book)
	if err := engine.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := engine.SweepTraffic(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(quotaEvents(t, h)) != 1 {
		t.Fatal("crash restart repeated transitions")
	}
	if err := book.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := engine.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	if events := quotaEvents(t, h); len(events) != 2 || events[0].Transition != store.TransitionRecovered {
		t.Fatalf("durable rollover did not recover: %+v", events)
	}
}

func TestTrafficQuotaSuccessfulResetEditSequence(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.clk.SetWall(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	h.login(t)
	id, _ := h.createNode(t, "quota")
	quotaNode(t, h, id, 100, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 1)
	if _, err := h.admin.AdjustTraffic(t.Context(), connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: id, PeriodRx: 90})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.alerts.SaveRule(t.Context(), store.AlertRule{Name: "quota", Kind: store.KindTraffic, Enabled: true, AllNodes: true, Threshold: 80}); err != nil {
		t.Fatal(err)
	}
	quotaNode(t, h, id, 100, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 5)
	if _, err := h.admin.GetTraffic(t.Context(), connect.NewRequest(&heronv1.GetTrafficRequest{})); err != nil {
		t.Fatal(err)
	}
	quotaNode(t, h, id, 100, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 1)
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	if events := quotaEvents(t, h); len(events) != 2 || events[0].Transition != store.TransitionRecovered {
		t.Fatalf("reset edit notifications=%+v", events)
	}
}

func TestTrafficQuotaCommitFailureSkipsImmediateEvaluation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "quota")
	quotaNode(t, h, id, 100, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 1)
	if _, err := h.admin.AdjustTraffic(t.Context(), connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: id, PeriodRx: 90})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.alerts.SaveRule(t.Context(), store.AlertRule{Name: "quota", Kind: store.KindTraffic, Enabled: true, AllNodes: true, Threshold: 80}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_quota_commit BEFORE UPDATE ON traffic BEGIN SELECT RAISE(ABORT,'commit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	// 周期仍有效，只有 Commit 失败门能阻止这次按新配额即时恢复。
	quotaNode(t, h, id, 1000, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 1)
	if events := quotaEvents(t, h); len(events) != 1 {
		t.Fatalf("failed Commit evaluated valid old observation: %+v", events)
	}
	if _, err := h.book.Commit(t.Context(), id); err == nil {
		t.Fatal("write failure injection did not take effect")
	}
	if _, err := db.Exec("DROP TRIGGER reject_quota_commit"); err != nil {
		t.Fatal(err)
	}
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	if events := quotaEvents(t, h); len(events) != 2 || events[0].Transition != store.TransitionRecovered {
		t.Fatalf("next sweep did not converge: %+v", events)
	}
}

func quotaNode(t *testing.T, h *harness, id int64, quota uint64, mode heronv1.TrafficQuotaMode, day uint32) {
	t.Helper()
	h.update(t, &heronv1.UpdateNodeRequest{Id: id, Name: "quota", Public: true, TrafficResetDay: day, OfflineGraceS: proto.Uint32(0), TrafficQuotaBytes: quota, TrafficQuotaMode: mode})
}

func quotaEvents(t *testing.T, h *harness) []store.AlertEvent {
	t.Helper()
	events, err := h.store.ListAlertEvents(t.Context(), 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var rules []store.AlertEvent
	for _, event := range events {
		if event.RuleID != 0 {
			rules = append(rules, event)
		}
	}
	return rules
}

func TestTrafficQuotaReportCommitAndAllResponses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode heronv1.TrafficQuotaMode
		used uint64
	}{
		{heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 90}, {heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_RX, 60},
		{heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_TX, 30}, {heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_MAX, 60},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			h := newHarness(t, "")
			h.login(t)
			id, tok := h.createNode(t, "quota")
			quotaNode(t, h, id, 100, tc.mode, 1)
			_, err := h.alerts.SaveRule(t.Context(), store.AlertRule{Name: "quota", Kind: store.KindTraffic, Enabled: true, AllNodes: true, Threshold: float64(tc.used)})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.report(t, tok, netCounters(testBootID, 100, 100)); err != nil {
				t.Fatal(err)
			}
			h.clk.Advance(10 * time.Second)
			if err := h.report(t, tok, netCounters(testBootID, 160, 130)); err != nil {
				t.Fatal(err)
			}
			if err := h.alerts.SweepOffline(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(quotaEvents(t, h)) != 0 {
				t.Fatal("uncommitted report fired")
			}
			if err := h.book.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := h.alerts.SweepOffline(t.Context()); err != nil {
				t.Fatal(err)
			}
			events := quotaEvents(t, h)
			if len(events) != 1 || events[0].Value != float64(tc.used) || events[0].Transition != store.TransitionFiring {
				t.Fatalf("events=%+v", events)
			}
			all, err := h.admin.GetTraffic(t.Context(), connect.NewRequest(&heronv1.GetTrafficRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			snap, err := h.admin.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.GetSnapshotRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			pub, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			adj, err := h.admin.AdjustTraffic(t.Context(), connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: id, PeriodRx: 60, PeriodTx: 30}))
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range []*heronv1.Traffic{all.Msg.Nodes[0].Traffic, snap.Msg.Nodes[0].Traffic, pub.Msg.Nodes[0].Traffic, adj.Msg.Traffic} {
				if v.QuotaBytes != 100 || v.QuotaMode != tc.mode || v.QuotaUsedBytes != tc.used || v.QuotaUsedPct == nil || *v.QuotaUsedPct != events[0].Value {
					t.Fatalf("response disagrees with committed event: %v", v)
				}
			}
			book := traffic.New(h.store, h.clk, time.UTC, slog.Default())
			if err := book.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			engine := alert.New(alert.Config{TTL: 30 * time.Second, Location: time.UTC}, h.store, h.store.Evaluation(), h.live, h.clk, slog.Default())
			engine.SetTraffic(book)
			if err := engine.Load(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := engine.SweepTraffic(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(quotaEvents(t, h)) != 1 {
				t.Fatal("restart emitted a duplicate transition")
			}
		})
	}
}

func TestTrafficQuotaResetEditsAndCommitFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.clk.SetWall(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	h.login(t)
	id, _ := h.createNode(t, "quota")
	quotaNode(t, h, id, 100, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 1)
	_, err := h.admin.AdjustTraffic(t.Context(), connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: id, PeriodRx: 90}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.alerts.SaveRule(t.Context(), store.AlertRule{Name: "quota", Kind: store.KindTraffic, Enabled: true, AllNodes: true, Threshold: 80})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_traffic BEFORE UPDATE ON traffic BEGIN SELECT RAISE(ABORT,'traffic unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	quotaNode(t, h, id, 100, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 5)
	if len(quotaEvents(t, h)) != 1 {
		t.Fatal("failed Commit evaluated a rolled view")
	}
	if _, err := h.admin.GetTraffic(t.Context(), connect.NewRequest(&heronv1.GetTrafficRequest{})); err != nil {
		t.Fatal(err)
	}
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(quotaEvents(t, h)) != 1 {
		t.Fatal("stale committed state recovered")
	}
	if err := h.book.Flush(t.Context()); err == nil {
		t.Fatal("injected failure not observed")
	}
	if _, err := db.Exec("DROP TRIGGER reject_traffic"); err != nil {
		t.Fatal(err)
	}
	if err := h.book.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	quotaNode(t, h, id, 100, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 1)
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	events := quotaEvents(t, h)
	if len(events) != 2 || events[0].Transition != store.TransitionRecovered {
		t.Fatalf("reset sequence=%+v", events)
	}
	// 当前周期已经写入，墙钟倒退到其起点之前不能使用它转换状态。
	_, err = h.admin.AdjustTraffic(t.Context(), connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: id, PeriodRx: 90}))
	if err != nil {
		t.Fatal(err)
	}
	h.clk.SetWall(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	quotaNode(t, h, id, 1000, heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM, 1)
	if err := h.alerts.SweepOffline(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(quotaEvents(t, h)) != 3 {
		t.Fatal("clock rollback recovered a future observation")
	}
}
