package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testdeps"
	"github.com/xjetry/heron/internal/testwait"
)

func startAlertHub(t *testing.T, clk clock.Clock, seed func(*store.Store), flags ...string) (heronv1connect.AdminServiceClient, serveEvents, func()) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "hub.db")
	password := "alert delivery sufficiently long password"
	if err := runPasswdWith([]string{"--db", db}, pipeWith(t, password+"\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		st, err := store.Open(db, clk, slog.New(slog.NewTextHandler(io.Discard, nil)), store.MigrateSchema)
		if err != nil {
			t.Fatal(err)
		}
		seed(st)
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	url, events, stop := startTestHubWithTTL(t, db, clk, minTTL.String(), flags...)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := heronv1connect.NewAdminServiceClient(&http.Client{Transport: testdeps.OwnedTransport(t), Jar: jar, Timeout: testwait.Bound}, url)
	if _, err := client.Login(t.Context(), connect.NewRequest(&heronv1.LoginRequest{Password: password})); err != nil {
		t.Fatal(err)
	}
	return client, events, stop
}

func alertReceiver(t *testing.T) (string, <-chan string) {
	t.Helper()
	bodies := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		bodies <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, bodies
}

// awaitDelivered 等待指定 transition 的事件送达；列表还可能包含不投递的系统审计。
func awaitDelivered(t *testing.T, client heronv1connect.AdminServiceClient, bodies <-chan string, transition string, timeout time.Duration) {
	t.Helper()
	started := time.Now()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		events, err := client.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events.Msg.Events {
			if ev.Transition == transition && len(ev.Deliveries) == 1 && ev.Deliveries[0].Ok {
				select {
				case body := <-bodies:
					var payload struct {
						Transition string `json:"transition"`
					}
					if err := json.Unmarshal([]byte(body), &payload); err != nil || payload.Transition != transition {
						t.Fatalf("webhook body=%s err=%v", body, err)
					}
				default:
					t.Fatal("delivery marked ok without webhook body")
				}
				t.Logf("%s delivered after %s", transition, time.Since(started))
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("%s was not delivered within %s: %v", transition, timeout, events.Msg)
		case <-ticker.C:
		}
	}
}

func TestServeDeliversOfflineAlerts(t *testing.T) {
	t.Parallel()
	url, bodies := alertReceiver(t)
	// 巡检 ticker 使用真实时间；真实 Mono 同步推进，才能从未上报走到 TTL。
	client, _, _ := startAlertHub(t, clock.Real(), nil)
	if _, err := client.CreateNode(t.Context(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: "unseen"})); err != nil {
		t.Fatal(err)
	}
	c, err := client.SaveNotifyChannel(t.Context(), connect.NewRequest(&heronv1.SaveNotifyChannelRequest{Channel: &heronv1.NotifyChannel{Name: "hook", Kind: heronv1.ChannelKind_CHANNEL_KIND_WEBHOOK, Webhook: &heronv1.WebhookConfig{Url: url}}}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SaveAlertRule(t.Context(), connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: &heronv1.AlertRule{Name: "offline", Kind: heronv1.AlertKind_ALERT_KIND_OFFLINE, Enabled: true, AllNodes: true, ChannelIds: []int64{c.Msg.Channel.Id}}}))
	if err != nil {
		t.Fatal(err)
	}
	// 节点要先过 HERON_OFFLINE_AFTER（minTTL），再赶上 OfflineSweepEvery 的巡检才会投递。
	// 上界只覆盖这两段之后的挂死，不把“多久内必须送达”当成被测性质。
	awaitDelivered(t, client, bodies, "firing", minTTL+alert.OfflineSweepEvery+testwait.Bound)
}

func seedAlertChannel(t *testing.T, st *store.Store, url string) (int64, int64) {
	t.Helper()
	id, _, err := st.CreateNode(t.Context(), store.NewNode{Name: "n"}, []byte("token hash"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]string{"url": url, "method": "POST"})
	c, err := st.SaveNotifyChannel(t.Context(), store.NotifyChannel{Name: "hook", Kind: store.ChannelWebhook, Config: string(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	return id, c.ID
}

func TestServeRequeuesPendingNotifications(t *testing.T) {
	t.Parallel()
	url, bodies := alertReceiver(t)
	client, _, _ := startAlertHub(t, clock.Real(), func(st *store.Store) {
		id, channel := seedAlertChannel(t, st, url)
		r, err := st.SaveAlertRule(t.Context(), store.AlertRule{Name: "offline", Kind: store.KindOffline, AllNodes: true})
		if err != nil {
			t.Fatal(err)
		}
		_, err = st.RecordTransition(t.Context(), r.ID, id, store.StateFiring, "", time.Time{}, store.AlertEvent{Transition: store.TransitionFiring, At: time.Now()}, []store.DeliveryTarget{{ChannelID: channel}})
		if err != nil {
			t.Fatal(err)
		}
	})
	awaitDelivered(t, client, bodies, "firing", testwait.Bound)
}

func TestServeEvaluatesProbeAlerts(t *testing.T) {
	t.Parallel()
	url, bodies := alertReceiver(t)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 59, 999000000, time.UTC))
	client, _, _ := startAlertHub(t, clk, func(st *store.Store) {
		id, channel := seedAlertChannel(t, st, url)
		task, _, err := st.SaveProbeTask(t.Context(), &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "127.0.0.1", IntervalS: 5, TimeoutMs: 1000}, store.NodeSelector{NodeIDs: []int64{id}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = st.SaveAlertRule(t.Context(), store.AlertRule{Name: "loss", Kind: store.KindProbe, Enabled: true, AllNodes: true, TaskID: task.Task.Id, Metric: store.MetricLossPct, Threshold: 100, ForMinutes: 1, ChannelIDs: []int64{channel}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = st.WriteMinuteBatch(t.Context(), metric.Batch{Probes: []metric.ProbeRow{{NodeID: id, TS: clk.Now().Truncate(time.Minute).Add(-time.Minute).Unix(), TaskID: task.Task.Id, Bucket: &metric.ProbeBucket{Sent: 1, Lost: 1}}}})
		if err != nil {
			t.Fatal(err)
		}
	})
	// 墙钟固定，评估分钟由它决定；真实计时器只提供 3.001s 等待。
	// 首次读钟与测试线程拨钟没有同步点，拨钟可能让协程等下一分钟，所以不推进墙钟。
	awaitDelivered(t, client, bodies, "firing", testwait.Bound)
}

func TestServePrunesAlertEvents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		retention time.Duration
		flags     []string
	}{
		{"default", 90 * 24 * time.Hour, nil},
		{"configured", 3 * 24 * time.Hour, []string{"--retention-alert-events", "72h"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 59, 999000000, time.UTC))
			var nodeID, retainedID int64
			client, events, _ := startAlertHub(t, clk, func(st *store.Store) {
				id, channel := seedAlertChannel(t, st, "http://127.0.0.1:1")
				nodeID = id
				r, err := st.SaveAlertRule(t.Context(), store.AlertRule{Name: "offline", Kind: store.KindOffline, AllNodes: true})
				if err != nil {
					t.Fatal(err)
				}
				for _, age := range []time.Duration{tc.retention + 24*time.Hour, tc.retention - 24*time.Hour} {
					ev, err := st.RecordTransition(t.Context(), r.ID, id, store.StateFiring, "", time.Time{}, store.AlertEvent{Transition: store.TransitionFiring, At: clk.Now().Add(-age)}, []store.DeliveryTarget{{ChannelID: channel}})
					if err != nil {
						t.Fatal(err)
					}
					retainedID = ev.ID
					if _, err := st.BeginBatchAttempt(t.Context(), ev.Deliveries[0].BatchID, []int64{ev.Deliveries[0].ID}); err != nil {
						t.Fatal(err)
					}
					if err := st.UpdateBatch(t.Context(), ev.Deliveries[0].BatchID, store.DeliveryResult{OK: true, Done: true, DeliveredAt: clk.Now()}); err != nil {
						t.Fatal(err)
					}
				}
			}, tc.flags...)
			deadline := time.NewTimer(testwait.Bound)
			defer deadline.Stop()
			for {
				select {
				case event := <-events:
					if string(event["msg"]) != `"pruned expired alert events"` {
						continue
					}
					got, err := client.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{NodeId: nodeID}))
					if err != nil || len(got.Msg.Events) != 1 || got.Msg.Events[0].Id != retainedID || len(got.Msg.Events[0].Deliveries) != 1 {
						t.Fatalf("retained events=%v err=%v", got, err)
					}
					return
				case <-deadline.C:
					t.Fatal("maintenance did not prune alert events")
				}
			}
		})
	}
}

// hub 启动即做一次到期扫描，天界取 --timezone：UTC 16:30 在上海已是 9 月 25 日，9 月 24 日到期、开着按月自动续期的
// 节点推后到 10 月 24 日；按 UTC 算今天仍是 9 月 24 日，不会推后。"node expiry renewed" 只在推后的日期落库之后记。
func TestServeRenewsExpiryAtStartupInTheHubZone(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 24, 16, 30, 0, 0, time.UTC))
	_, events, _ := startAlertHub(t, clk, func(st *store.Store) {
		id, _, err := st.CreateNode(t.Context(), store.NewNode{Name: "renewing"}, make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		b := store.Billing{Cycle: store.CycleMonthly, ExpiresOn: "2026-09-24", AutoRenew: true}
		if _, err := st.UpdateNode(t.Context(), id, store.NodeEdit{Name: "renewing", TrafficResetDay: 1, Billing: b}); err != nil {
			t.Fatal(err)
		}
	}, "--timezone", "Asia/Shanghai")
	deadline := time.NewTimer(testwait.Bound)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if string(event["msg"]) != `"node expiry renewed"` {
				continue
			}
			if from, to := string(event["from"]), string(event["to"]); from != `"2026-09-24"` || to != `"2026-10-24"` {
				t.Fatalf("renewed from %s to %s, want 2026-09-24 to 2026-10-24", from, to)
			}
			return
		case <-deadline.C:
			t.Fatal("the startup expiry sweep did not renew the node")
		}
	}
}

// serve 把 --timezone 交给管理端与公开端：UTC 16:30 在上海已是 9 月 25 日，9 月 30 日到期还剩 5 天（按 UTC 是 6 天）。
func TestServeReportsDaysLeftInTheHubZone(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 24, 16, 30, 0, 0, time.UTC))
	db := filepath.Join(t.TempDir(), "hub.db")
	password := "days left sufficiently long password"
	if err := runPasswdWith([]string{"--db", db}, pipeWith(t, password+"\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(db, clk, slog.New(slog.NewTextHandler(io.Discard, nil)), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := st.CreateNode(t.Context(), store.NewNode{Name: "zoned"}, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateNode(t.Context(), id, store.NodeEdit{Name: "zoned", Public: true, TrafficResetDay: 1, Billing: store.Billing{ExpiresOn: "2026-09-30"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	url, _, _ := startTestHub(t, db, clk, "--timezone", "Asia/Shanghai")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	admin := heronv1connect.NewAdminServiceClient(&http.Client{Transport: testdeps.OwnedTransport(t), Jar: jar, Timeout: testwait.Bound}, url)
	if _, err := admin.Login(t.Context(), connect.NewRequest(&heronv1.LoginRequest{Password: password})); err != nil {
		t.Fatal(err)
	}
	nodes, err := admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || len(nodes.Msg.GetNodes()) != 1 || nodes.Msg.GetNodes()[0].GetBilling().GetDaysLeft() != 5 {
		t.Fatalf("admin ListNodes = %v %v, want days_left 5", nodes, err)
	}
	public := heronv1connect.NewPublicServiceClient(&http.Client{Transport: testdeps.OwnedTransport(t), Timeout: testwait.Bound}, url)
	snap, err := public.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil || len(snap.Msg.GetNodes()) != 1 || snap.Msg.GetNodes()[0].GetBilling().GetDaysLeft() != 5 {
		t.Fatalf("public GetSnapshot = %v %v, want days_left 5", snap, err)
	}
}
