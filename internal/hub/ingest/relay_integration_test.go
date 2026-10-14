package ingest

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
	"github.com/xjetry/heron/internal/hub/updates"
	"github.com/xjetry/heron/internal/hubclient"
	"github.com/xjetry/heron/internal/releasesig/sigtest"
	"github.com/xjetry/heron/internal/update"
)

// relayVersion 与 relayBinary 也被 internal/update/relay_accept_test.go 按值引用（两个包不能互相 import 测试代码）。
const (
	relayVersion = "v9.0.0"
	relayBinary  = "relay-accept-agent"
)

func signedAgent(arch string, archive []byte) update.Artifacts {
	sums := sigtest.Sums("heron-agent_linux_"+arch+".tar.gz", archive)
	return update.Artifacts{Sums: sums, Signature: sigtest.Sign(relayVersion, sums), Archive: archive}
}

func testTrusted() []ed25519.PublicKey { pub, _ := sigtest.Key(); return []ed25519.PublicKey{pub} }

// acceptVerify 是 hub 侧预验签：与节点更新器调用同一个 Accept，判定规则只有一套；两侧受信公钥相同时（本用例），hub 验过的产物节点也验得过。
func acceptVerify(version, arch string, a update.Artifacts) error {
	_, err := update.Accept(testTrusted(), "agent", arch, version, a)
	return err
}

// relayHub 起一套带中转的 hub（构造顺序同 newHubWith），并让一个节点处于 dispatched 的更新任务上。
// official 按请求的架构给出假官方产物；listen 为空时用 httptest 的随机端口，否则监听该地址（隔离验收）。
func relayHub(t *testing.T, official func(arch string) update.Artifacts, verify updates.VerifyFunc, listen string) (*hub, string, *heronv1.UpdateTask) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := probe.New(st, slog.Default())
	a := auth.New(st, reg, nil, clk, time.UTC, slog.Default())
	l := live.New(clk, 30*time.Second)
	book := traffic.New(st, clk, time.UTC, slog.Default())
	manager := updates.New(st, clk, slog.Default(), relayVersion)
	relay := updates.NewRelay(manager, func(_ context.Context, _ string, arch string) (update.Artifacts, error) {
		return official(arch), nil
	}, verify, clk)
	svc := New(Config{TTL: 30 * time.Second, Updates: manager, Releases: relay},
		Deps{Live: l, Store: st, Auth: a, Traffic: book, Tasks: reg, Clock: clk, Log: slog.Default()})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := errors.Join(a.Load(ctx), svc.Load(ctx), book.Load(ctx), reg.Load(ctx), manager.Load(ctx)); err != nil {
		t.Fatal(err)
	}
	go manager.Run(ctx)
	mux := http.NewServeMux()
	mux.Handle(svc.Handler())
	srv := httptest.NewUnstartedServer(mux)
	if listen != "" {
		srv.Listener.Close()
		if srv.Listener, err = net.Listen("tcp", listen); err != nil {
			t.Fatal(err)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	h := &hub{svc: svc, srv: srv, client: hubclient.New(srv.URL, 5*time.Second, agentwire.MaxResponseBytes), clk: clk, store: st, auth: a, live: l, book: book, reg: reg}
	id, tok := h.node(t)
	reportUpdate := func() {
		req := report(tok, &heronv1.Metrics{CpuPct: proto.Float64(1)})
		req.Msg.Update = &heronv1.UpdateStatus{Supported: true, Version: "v1.0.0"}
		req.Msg.UpdateHash = proto.Uint64(7)
		if _, err := h.client.Report(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(ok func(*heronv1.UpdateStatus) bool) *heronv1.UpdateStatus {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if s := manager.Snapshot(id); ok(s) {
				return s
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("update state did not converge: %v", manager.Snapshot(id))
		return nil
	}
	// Start 要求节点已报过支持在线更新，dispatched 要求任务创建后节点再上报一次（Manager.flush）。
	reportUpdate()
	waitFor(func(s *heronv1.UpdateStatus) bool { return s.GetSupported() })
	if _, err := manager.Start(ctx, id, relayVersion); err != nil {
		t.Fatal(err)
	}
	reportUpdate()
	s := waitFor(func(s *heronv1.UpdateStatus) bool { return s.GetTask().GetState() == "dispatched" })
	return h, tok, s.GetTask()
}

// fetchCtx 给取回调用一个期限：更新器的来源不设自己的总时限，没有期限的 ctx 会被拒绝。
func fetchCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// hubSourceFor 以节点 token 构造更新器的 hub 来源；配置里的地址就是这套 hub 自己。
func hubSourceFor(h *hub, tok string) *update.HubSource {
	return update.NewHubSource(func() ([]byte, error) {
		return []byte(`{"hub":"` + h.srv.URL + `","token":"` + tok + `","name":"n"}`), nil
	})
}

// 全链路在进程内走真实组件：ingest 的 GetRelease 处理器、Manager 与 Relay（假官方源、测试公钥）、
// 更新器的 HubSource 与 Accept，任何一层被替掉都会让这里的断言失去对象。
func TestRelayEndToEnd(t *testing.T) {
	archive := sigtest.Archive("agent", []byte(relayBinary))
	h, tok, task := relayHub(t, func(arch string) update.Artifacts { return signedAgent(arch, archive) }, acceptVerify, "")
	a, err := hubSourceFor(h, tok).Fetch(fetchCtx(t), update.Request{ID: task.Id, Version: task.Version}, "agent", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	bin, err := update.Accept(testTrusted(), "agent", "amd64", task.Version, a)
	if err != nil || string(bin) != relayBinary {
		t.Fatalf("bin=%q err=%v", bin, err)
	}
}

// tampered 的清单与签名都合法，但归档字节被换掉：只有"取回后逐字节验摘要"这一步能发现。
func tampered(arch string) update.Artifacts {
	a := signedAgent(arch, sigtest.Archive("agent", []byte(relayBinary)))
	a.Archive = sigtest.Archive("agent", []byte("evil"))
	return a
}

// hub 预验签让坏产物在 hub 处就报错，不必每个节点各下一遍才失败；预验签失败经 releaseError 归为 Unavailable。
func TestRelayHubPreverifyRejectsTamperedOfficial(t *testing.T) {
	h, tok, task := relayHub(t, tampered, acceptVerify, "")
	_, err := hubSourceFor(h, tok).Fetch(fetchCtx(t), update.Request{ID: task.Id, Version: task.Version}, "agent", "amd64")
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("err = %v, want Unavailable from the hub's preverify", err)
	}
}

// 失守的 hub 跳过预验签、把篡改的归档原样转发：节点上的 Accept 仍拒绝。
func TestUpdaterRejectsForgedHub(t *testing.T) {
	h, tok, task := relayHub(t, tampered, func(string, string, update.Artifacts) error { return nil }, "")
	a, err := hubSourceFor(h, tok).Fetch(fetchCtx(t), update.Request{ID: task.Id, Version: task.Version}, "agent", "amd64")
	if err != nil {
		t.Fatalf("the forged hub should serve its bytes: %v", err)
	}
	if _, err := update.Accept(testTrusted(), "agent", "amd64", task.Version, a); err == nil {
		t.Fatal("bytes from a hub that skipped verification were accepted")
	}
}
