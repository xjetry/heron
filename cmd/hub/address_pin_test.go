package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hubclient"
	"google.golang.org/protobuf/proto"
)

// 从用户可见的入口走一遍手填出口地址到 agent 的路径：管理端 UpdateNode 保存之后，该节点的下一次 Report 应答就让
// agent 停用该族探测，清空即恢复；hub 重启后按库里的手填照旧下发。经的是 serve 的真实装配（api → nodeops →
// ingest 的内存态，启动时 ingest.Load），不是某个包的替身。
func TestServeHandsAddressPinsToAgent(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hub.db")
	password := "initial sufficiently long password"
	if err := runPasswdWith([]string{"--db", db}, pipeWith(t, password+"\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx := context.Background()
	url, _, stop := startTestHub(t, db, clk)
	client := heronv1connect.NewAdminServiceClient(ownedClient(t), url)
	logged, err := client.Login(ctx, connect.NewRequest(&heronv1.LoginRequest{Password: password}))
	if err != nil {
		t.Fatal(err)
	}
	cookie := logged.Header().Get("Set-Cookie")
	create := connect.NewRequest(&heronv1.CreateNodeRequest{Name: "n"})
	create.Header().Set("Cookie", cookie)
	node, err := client.CreateNode(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	agent := hubclient.New(url, 5*time.Second, agentwire.MaxResponseBytes)
	registered, err := agent.Register(ctx, connect.NewRequest(&heronv1.RegisterRequest{Key: node.Msg.Token}))
	if err != nil {
		t.Fatal(err)
	}
	detection := func(agent heronv1connect.AgentServiceClient) *heronv1.NetworkDetection {
		t.Helper()
		req := connect.NewRequest(&heronv1.ReportRequest{Metrics: &heronv1.Metrics{}})
		req.Header().Set("Authorization", "Bearer "+registered.Msg.Token)
		resp, err := agent.Report(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetDetection()
	}
	pin := func(v4, v6 string) {
		t.Helper()
		req := connect.NewRequest(&heronv1.UpdateNodeRequest{Id: node.Msg.Node.Id, Name: "n", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), Ipv4Pin: v4, Ipv6Pin: v6})
		req.Header().Set("Cookie", cookie)
		if _, err := client.UpdateNode(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if d := detection(agent); d.GetSkipIpv4() || d.GetSkipIpv6() {
		t.Fatalf("detection before any pin = %v", d)
	}
	pin("8.8.8.8", "")
	if d := detection(agent); !d.GetSkipIpv4() || d.GetSkipIpv6() {
		t.Fatalf("detection after pinning IPv4 = %v, want IPv4 skipped", d)
	}
	pin("", "")
	if d := detection(agent); d.GetSkipIpv4() || d.GetSkipIpv6() {
		t.Fatalf("detection after clearing = %v, want both probed", d)
	}
	pin("", "2606:4700::1111")
	stop()
	url, _, _ = startTestHub(t, db, clk)
	if d := detection(hubclient.New(url, 5*time.Second, agentwire.MaxResponseBytes)); d.GetSkipIpv4() || !d.GetSkipIpv6() {
		t.Fatalf("detection after restart = %v, want IPv6 skipped from the stored pin", d)
	}
}
