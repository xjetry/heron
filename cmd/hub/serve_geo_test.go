package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/testwait"
)

// serve 启动国家查询器，交给它的是通知渠道那个不跟随重定向的出站客户端：开启查询并指向一个返回 302 的假服务，
// 节点经可信代理以公网地址上报之后，查询到达假服务，跳转目标从未被请求。查询器不启动时请求不会到达；给的是跟随
// 重定向的客户端时 /moved 会出现在请求里。
func TestServeRunsCountryLookupWithTheNoRedirectClient(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var paths []string
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/moved" {
			io.WriteString(w, "US")
			return
		}
		http.Redirect(w, r, "/moved", http.StatusFound)
	}))
	defer svc.Close()
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(paths)
	}

	db := filepath.Join(t.TempDir(), "hub.db")
	password := "initial sufficiently long password"
	if err := runPasswdWith([]string{"--db", db}, pipeWith(t, password+"\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	// 第一次启动：开启查询、建节点、让节点经可信代理以 8.8.8.8 上报。来源地址随分钟行刷出落盘，这里靠退出时的
	// 那次刷出写入；查询器先于刷出停止，所以这次启动不会查。
	url, _, stop := startTestHub(t, db, clk, "--trusted-proxies", "127.0.0.1/32")
	admin := heronv1connect.NewAdminServiceClient(ownedClient(t), url)
	ctx := context.Background()
	logged, err := admin.Login(ctx, connect.NewRequest(&heronv1.LoginRequest{Password: password}))
	if err != nil {
		t.Fatal(err)
	}
	cookies := (&http.Response{Header: logged.Header()}).Cookies()
	cookie := cookies[0].Name + "=" + cookies[0].Value
	update := connect.NewRequest(&heronv1.UpdateSettingsRequest{Settings: &heronv1.Settings{
		Theme: "auto", GeoEnabled: proto.Bool(true), GeoUrl: proto.String(svc.URL + "/{ip}/country"),
	}})
	update.Header().Set("Cookie", cookie)
	if _, err := admin.UpdateSettings(ctx, update); err != nil {
		t.Fatal(err)
	}
	create := connect.NewRequest(&heronv1.CreateNodeRequest{Name: "n"})
	create.Header().Set("Cookie", cookie)
	node, err := admin.CreateNode(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	report := connect.NewRequest(&heronv1.ReportRequest{Metrics: &heronv1.Metrics{CpuPct: proto.Float64(1)}})
	agent := heronv1connect.NewAgentServiceClient(ownedClient(t), url)
	registered, err := agent.Register(ctx, connect.NewRequest(&heronv1.RegisterRequest{Key: node.Msg.Token}))
	if err != nil {
		t.Fatal(err)
	}
	report.Header().Set("Authorization", "Bearer "+registered.Msg.Token)
	report.Header().Set("X-Forwarded-For", "8.8.8.8")
	if _, err := agent.Report(ctx, report); err != nil {
		t.Fatal(err)
	}
	stop()
	st, _, err := openOffline(db, false)
	if err != nil {
		t.Fatal(err)
	}
	n, err := st.GetNode(ctx, node.Msg.Node.Id)
	st.Close()
	if err != nil || n.LastSource != "8.8.8.8" {
		t.Fatalf("fixture: node after the first run = %+v %v, want last_source 8.8.8.8", n, err)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("fixture: the first run already queried %q", got)
	}

	// 第二次启动：查询器启动即巡检一轮，节点的来源地址已在库里、开关已开。
	_, _, stop = startTestHub(t, db, clk)
	testwait.Until(t, 10*time.Millisecond, func() bool { return len(seen()) > 0 }, "serve never queried the country service")
	stop()
	if got := seen(); !slices.Equal(got, []string{"/8.8.8.8/country"}) {
		t.Fatalf("country service saw %q, want only the lookup (the redirect must not be followed)", got)
	}
}
