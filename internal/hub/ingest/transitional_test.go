package ingest

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/api"
	"github.com/xjetry/heron/internal/hub/backup"
	"github.com/xjetry/heron/internal/hub/geo"
	"github.com/xjetry/heron/internal/hub/heartbeat"
	"github.com/xjetry/heron/internal/hub/nodeops"
	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/updates"
	"github.com/xjetry/heron/internal/testwait"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func transitionalReport(t *testing.T, tok string, round uint64) *connect.Request[heronv1.ReportRequest] {
	t.Helper()
	list := ifaceList(1000+100*round, 2000+200*round, 300+10*round, 400+20*round)
	epoch := strings.Repeat("a", 64)
	if epoch == agentwire.NetInterfaceSummary(list).Epoch {
		t.Fatal("test epoch must differ from the interface digest")
	}
	req := listReport(tok, &heronv1.Metrics{BootId: testBootID, NetRxTotal: proto.Uint64(9), NetTxTotal: proto.Uint64(8), NetCounterEpoch: epoch}, list)
	req.Msg.Update = &heronv1.UpdateStatus{Supported: true, Version: "v0.15.0"}
	req.Msg.UpdateHash = proto.Uint64(7)
	raw, err := proto.Marshal(req.Msg)
	if err != nil {
		t.Fatal(err)
	}
	raw = protowire.AppendTag(raw, 3, protowire.VarintType)
	raw = protowire.AppendVarint(raw, 7)
	raw = protowire.AppendTag(raw, 7, protowire.BytesType)
	raw = protowire.AppendBytes(raw, protowire.AppendVarint(nil, 1))
	msg := &heronv1.ReportRequest{}
	if err := proto.Unmarshal(raw, msg); err != nil {
		t.Fatal(err)
	}
	req.Msg = msg
	return req
}

func TestTransitionalReportAccepted(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	req := transitionalReport(t, tok, 0)
	if _, err := h.client.Report(t.Context(), req); err != nil {
		t.Fatalf("transitional report rejected: %v", err)
	}
	sum := agentwire.NetInterfaceSummary(req.Msg.NetInterfaces)
	e, ok := h.live.Get(id)
	if !ok || e.Metrics.GetNetRxTotal() != sum.Rx || e.Metrics.GetNetTxTotal() != sum.Tx || e.Metrics.GetNetCounterEpoch() != sum.Epoch {
		t.Fatalf("live totals = %v, want %d/%d %s", e.Metrics, sum.Rx, sum.Tx, sum.Epoch)
	}
}

type snapshotHeartbeat struct{}

func (snapshotHeartbeat) Status() heartbeat.Status { return heartbeat.Status{} }

func TestTransitionalReportLedgerAndSnapshot(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	for round := range uint64(2) {
		h.clk.Advance(h.svc.Interval())
		if _, err := h.client.Report(t.Context(), transitionalReport(t, tok, round)); err != nil {
			t.Fatalf("transitional report rejected: %v", err)
		}
	}
	if e, ok := h.book.Get(id); !ok || e.TotalRx != 110 || e.TotalTx != 220 || len(e.Baseline) != 2 || e.Baseline[0].Name != "eth0" || e.Baseline[0].Rx != 1100 || e.Baseline[0].Tx != 2200 || e.Baseline[1].Name != "eth1" || e.Baseline[1].Rx != 310 || e.Baseline[1].Tx != 420 {
		t.Fatalf("ledger = %+v, want 110/220 and per-interface baseline", e)
	}
	log := slog.Default()
	alerts := alert.New(alert.Config{TTL: 30 * time.Second, Location: time.UTC}, h.store, h.store.Evaluation(), h.live, h.clk, log)
	client := outbound.NewClient(alert.NotifyTimeout)
	notifier := alert.NewQueue(alert.QueueConfig{}, alert.QueueDeps{Store: h.store, Channels: alerts.Channels, Client: client, Clock: h.clk, Log: log})
	ops := nodeops.New(nodeops.Deps{Credentials: h.auth, Nodes: h.reg, Alerts: alerts, Traffic: h.book, State: h.svc, Log: log})
	svc := api.New(api.Config{TTL: 30 * time.Second, Location: time.UTC, Retention: store.DefaultRetention, Backups: backup.New(h.store, notifier, h.clk, log), Heartbeat: snapshotHeartbeat{}, Geo: geo.NewHTTP(client)}, api.Deps{Store: h.store, Auth: h.auth, Live: h.live, Nodes: ops, Traffic: h.book, Probes: h.reg, Alerts: alerts, Notifier: notifier, Clock: h.clk, Log: log})
	resp, err := svc.GetSnapshot(t.Context(), connect.NewRequest(&heronv1.GetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Nodes) != 1 {
		t.Fatalf("snapshot nodes = %v", resp.Msg.Nodes)
	}
	m := resp.Msg.Nodes[0].GetMetrics()
	sum := agentwire.NetInterfaceSummary(transitionalReport(t, tok, 1).Msg.NetInterfaces)
	if m.GetNetRxTotal() != sum.Rx || m.GetNetTxTotal() != sum.Tx || m.GetNetCounterEpoch() != sum.Epoch {
		t.Fatalf("snapshot totals = %v, want %d/%d %s", m, sum.Rx, sum.Tx, sum.Epoch)
	}
}

func TestTransitionalReportReceivesUpdateTask(t *testing.T) {
	var manager *updates.Manager
	h := newHubConfigured(t, filepath.Join(t.TempDir(), "t.db"), func(st *store.Store, clk *clock.Fake) Config {
		manager = updates.New(st, clk, slog.Default(), "v1.0.0")
		return Config{TTL: 30 * time.Second, Updates: manager}
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	id, tok := h.node(t)
	if _, err := h.client.Report(t.Context(), transitionalReport(t, tok, 0)); err != nil {
		t.Fatalf("first report rejected: %v", err)
	}
	testwait.Until(t, time.Millisecond, func() bool { return manager.Snapshot(id).Supported }, "first update status not persisted")
	queued, err := manager.Start(t.Context(), id, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(h.svc.Interval())
	resp, err := h.client.Report(t.Context(), transitionalReport(t, tok, 1))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Update != nil {
		t.Fatalf("queued update delivered before durable dispatch: %v", resp.Msg.Update)
	}
	testwait.Until(t, time.Millisecond, func() bool {
		rows, err := h.store.NodeUpdates(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return rows[id].GetTask().GetState() == "dispatched"
	}, "update not persisted as dispatched")
	h.clk.Advance(h.svc.Interval())
	resp, err = h.client.Report(t.Context(), transitionalReport(t, tok, 2))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Update.GetId() != queued.Id || resp.Msg.Update.GetState() != "dispatched" {
		t.Fatalf("update = %v, want dispatched %s", resp.Msg.Update, queued.Id)
	}
}

func TestReportResponseHasNoCapabilities(t *testing.T) {
	h := newHub(t)
	_, tok := h.node(t)
	resp, err := h.client.Report(t.Context(), report(tok, &heronv1.Metrics{}))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(resp.Msg)
	if err != nil {
		t.Fatal(err)
	}
	for len(raw) > 0 {
		num, _, n := protowire.ConsumeField(raw)
		if n < 0 {
			t.Fatal(protowire.ParseError(n))
		}
		if num == 6 {
			t.Fatal("response carries capability field 6")
		}
		raw = raw[n:]
	}
}

func checkTasksDigest(t *testing.T, kind string) {
	t.Helper()
	h := newHub(t)
	id, tok := h.node(t)
	h.task(t, id)
	current := h.reg.TasksFor(id)
	req := report(tok, &heronv1.Metrics{})

	if kind != "absent" {
		digest, err := agentwire.TasksDigest(current.Tasks)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "different" {
			digest = bytes.Repeat([]byte{9}, 32)
		}
		req.Msg.TasksDigest = digest
	}
	resp, err := h.client.Report(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "equal" {
		if resp.Msg.Tasks != nil {
			t.Fatalf("equal digest resent tasks: %v", resp.Msg.Tasks)
		}
	} else if !proto.Equal(resp.Msg.Tasks, current) {
		t.Fatalf("%s digest tasks = %v, want %v", kind, resp.Msg.Tasks, current)
	}
}
func TestReportTasksDigestAbsentSendsTasks(t *testing.T)  { checkTasksDigest(t, "absent") }
func TestReportTasksDigestEqualSendsNothing(t *testing.T) { checkTasksDigest(t, "equal") }
func TestReportTasksDigestDifferSendsTasks(t *testing.T)  { checkTasksDigest(t, "different") }

func TestReportWithoutListUsesMetricsTotals(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	m := &heronv1.Metrics{BootId: testBootID, NetRxTotal: proto.Uint64(123), NetTxTotal: proto.Uint64(456), NetCounterEpoch: strings.Repeat("a", 64)}
	if _, err := h.client.Report(t.Context(), report(tok, m)); err != nil {
		t.Fatal(err)
	}
	if e, ok := h.live.Get(id); !ok || !proto.Equal(e.Metrics, m) {
		t.Fatalf("metrics without list = %v, want %v", e.Metrics, m)
	}
}

func TestReportMalformedEpochStillRejected(t *testing.T) {
	h := newHub(t)
	_, tok := h.node(t)
	req := transitionalReport(t, tok, 0)
	req.Msg.Metrics.NetCounterEpoch = "malformed"
	if _, err := h.client.Report(t.Context(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed epoch accepted: %v", err)
	}
}
