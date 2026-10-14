package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/api"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/backup"
	"github.com/xjetry/heron/internal/hub/geo"
	"github.com/xjetry/heron/internal/hub/heartbeat"
	"github.com/xjetry/heron/internal/hub/ingest"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/nodeops"
	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
	"github.com/xjetry/heron/internal/hub/web"
	"github.com/xjetry/heron/internal/testdeps"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// publicProcedures 是匿名可达的全部过程：只有 PublicService。Register 与 Login 的凭据在请求体里，
// 用 {} 调用时由方法体返回 Unauthenticated（没有注册窗口、没有管理员），与其他过程一样断言 401。
var publicProcedures = map[string]bool{
	"/heron.v1.PublicService/GetSite":                  true,
	"/heron.v1.PublicService/GetSnapshot":              true,
	"/heron.v1.PublicService/QueryMetrics":             true,
	"/heron.v1.PublicService/QueryProbes":              true,
	"/heron.v1.PublicService/ListProbeComparisonNodes": true,
	"/heron.v1.PublicService/QueryProbeComparison":     true,
}

func newTestMux(t *testing.T) http.Handler {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newTestMuxOn(t, st, clk)
}

// newTestMuxOn 按 serve 的单域装配把 RPC、面板与沙箱主题挂到给定的库上。
func newTestMuxOn(t *testing.T, st *store.Store, clk clock.Clock) http.Handler {
	t.Helper()
	h, _ := newTestServicesOn(t, st, clk)
	return h
}

// newTestServicesOn 是 newTestMuxOn 的装配，另交出 AdminService，供直接调用方法体的用例使用。
func newTestServicesOn(t *testing.T, st *store.Store, clk clock.Clock) (http.Handler, *api.Service) {
	t.Helper()
	reg := probe.New(st, slog.Default())
	l := live.New(clk, 30*time.Second)
	book := traffic.New(st, clk, time.UTC, slog.Default())
	alerts := alert.New(alert.Config{TTL: 30 * time.Second, Location: time.UTC}, st, st.Evaluation(), l, clk, slog.Default())
	// 通知与国家查询的 HTTP 后端共用一个出站客户端，与 serve 的装配相同。
	client := outbound.NewClient(alert.NotifyTimeout)
	notifier := alert.NewQueue(alert.QueueConfig{}, alert.QueueDeps{Store: st, Channels: alerts.Channels, Client: client, Clock: clk, Log: slog.Default()})
	alerts.SetSender(notifier)
	a := auth.New(st, reg, notifier, clk, time.UTC, slog.Default())
	svc := ingest.New(ingest.Config{TTL: 30 * time.Second},
		ingest.Deps{Live: l, Store: st, Auth: a, Traffic: book, Tasks: reg, Clock: clk, Log: slog.Default()})
	ctx := context.Background()
	if err := errors.Join(a.Load(ctx), svc.Load(ctx), book.Load(ctx), reg.Load(ctx), alerts.Load(ctx)); err != nil {
		t.Fatal(err)
	}
	if err := notifier.Requeue(ctx); err != nil {
		t.Fatal(err)
	}
	admin := api.New(api.Config{Backups: backup.New(st, notifier, clk, slog.Default()), Heartbeat: heartbeat.New(heartbeatSource{st: st, live: l}, client, "test", clk, slog.Default()), TTL: 30 * time.Second, ReportInterval: 10 * time.Second, Location: time.UTC, Retention: store.DefaultRetention, Geo: geo.NewHTTP(client)},
		api.Deps{Store: st, Auth: a, Live: l, Nodes: nodeops.New(nodeops.Deps{Credentials: a, Nodes: reg, Alerts: alerts, Traffic: book, State: svc, Log: slog.Default()}),
			Traffic: book, Probes: reg, Alerts: alerts, Notifier: notifier, Clock: clk, Log: slog.Default()})
	pub := api.NewPublic(api.PublicConfig{ReportInterval: 10 * time.Second, Location: time.UTC},
		api.PublicDeps{Store: st, Live: l, Traffic: book, Probes: reg, Clock: clk, Log: slog.Default()})
	return newHandler(routes{
		agent: mountOf(svc.Handler()), admin: mountOf(admin.Handler()), public: mountOf(pub.Handler()),
		page: web.ThemeHandler(st, web.PublicHandler(), admin.ThemePreviewAccess, slog.Default()), publicEnabled: st.PublicEnabled,
	}), admin
}

// RPC 路径与 /admin/ 的优先级高于根路径的公开页；ServeMux 按最长前缀匹配，三者同时挂载时，RPC 仍必须经过服务自身的鉴权。
func TestMuxRoutesPanelAndRootAroundRPC(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newTestMux(t))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: testdeps.OwnedTransport(t), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"/", "/nodes/3"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable) || resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("%s: %d, want the public page handler (200 when built, 503 when not) with CSP", path, resp.StatusCode)
		}
	}
	resp, err := client.Get(srv.URL + "/admin/nodes/1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable) || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("/admin/nodes/1: %d, want the panel handler (200 when built, 503 when not) with CSP", resp.StatusCode)
	}
	resp, err = client.Post(srv.URL+"/heron.v1.AdminService/ListNodes", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("RPC path: %d, want 401 from the service, not the panel", resp.StatusCode)
	}
}

// 注册表提供方法全集，真实挂载点必须让所有未列入匿名清单的方法经过鉴权。
func TestMuxRejectsAnonymousProcedures(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newTestMux(t))
	t.Cleanup(srv.Close)
	seen := map[string]bool{}
	count := 0
	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		if file.Package() != "heron.v1" {
			return true
		}
		services := file.Services()
		for i := 0; i < services.Len(); i++ {
			svc := services.Get(i)
			for j := 0; j < svc.Methods().Len(); j++ {
				path := "/" + string(svc.FullName()) + "/" + string(svc.Methods().Get(j).Name())
				count++
				seen[path] = true
				t.Run(path, func(t *testing.T) {
					resp, err := srv.Client().Post(srv.URL+path, "application/json", strings.NewReader("{}"))
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					if publicProcedures[path] {
						// 401 说明被鉴权挡住。没挂载的过程落到根路径：根路径挂着公开页，得到的是 HTML（构建过 200，没构建 503），
						// 只看状态码分不出来，所以还要求应答是 connect 的 JSON（成功与错误都是 application/json）。
						if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound || resp.Header.Get("Content-Type") != "application/json" {
							t.Fatalf("%s: status %d %q, want the public service to answer anonymously", path, resp.StatusCode, resp.Header.Get("Content-Type"))
						}
						return
					}
					if resp.StatusCode != http.StatusUnauthorized {
						t.Fatalf("%s: status %d, want 401", path, resp.StatusCode)
					}
					var body struct {
						Code string `json:"code"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.Code != "unauthenticated" {
						t.Fatalf("%s: code %q, want unauthenticated", path, body.Code)
					}
				})
			}
		}
		return true
	})
	if count == 0 {
		t.Fatal("enumerated no procedures")
	}
	for path := range publicProcedures {
		if !seen[path] {
			t.Errorf("anonymous allowlist path not enumerated: %s", path)
		}
	}
}

// 无副作用标注决定一个过程是否接受 GET，只有 PublicService 标了：GET 到其余过程一律 405，
// 非公开过程不接受 GET，避免浏览器的简单跨站请求触发它们；公开过程接受 GET，浏览器与中间缓存才能按 Cache-Control 复用响应。
func TestMuxAcceptsGETOnlyOnPublicService(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newTestMux(t))
	t.Cleanup(srv.Close)
	count := 0
	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		if file.Package() != "heron.v1" {
			return true
		}
		for i := 0; i < file.Services().Len(); i++ {
			svc := file.Services().Get(i)
			for j := 0; j < svc.Methods().Len(); j++ {
				path := "/" + string(svc.FullName()) + "/" + string(svc.Methods().Get(j).Name())
				count++
				resp, err := srv.Client().Get(srv.URL + path + "?connect=v1&encoding=json&message=%7B%7D")
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				switch public := svc.FullName() == "heron.v1.PublicService"; {
				// 公开过程的应答必须来自 connect：没挂载的过程落到根路径的公开页，也可能是 200，只是不是 JSON。
				case public && (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Content-Type") != "application/json"):
					t.Errorf("%s: GET status %d %q, want the public service to answer (200, or 400 for an empty window)", path, resp.StatusCode, resp.Header.Get("Content-Type"))
				case !public && resp.StatusCode != http.StatusMethodNotAllowed:
					t.Errorf("%s: GET status %d, want 405", path, resp.StatusCode)
				}
			}
		}
		return true
	})
	if count == 0 {
		t.Fatal("enumerated no procedures")
	}
}
