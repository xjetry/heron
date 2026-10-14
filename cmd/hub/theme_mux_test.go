package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/web"
	"github.com/xjetry/heron/internal/testdeps"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func newThemeTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(newTestMuxOn(t, st, clk))
	t.Cleanup(srv.Close)
	return srv, st
}

// 挂载点用例直接安装已解析文件，包解析与 SDK 准入由 theme 和 api 的测试独立覆盖。
func putMuxTheme(t *testing.T, st *store.Store, id string, files map[string]string) store.Theme {
	t.Helper()
	var list []store.ThemeFile
	for p, c := range files {
		list = append(list, store.ThemeFile{Path: p, Content: []byte(c)})
	}
	content, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := st.PutTheme(t.Context(), store.Theme{ID: id, Name: id, Version: "1", SDK: 1, UploadedAt: time.Unix(100, 0)}, list, content, false, 20)
	if err != nil {
		t.Fatal(err)
	}
	return installed
}

func installTheme(t *testing.T, st *store.Store, id string, files map[string]string) store.Theme {
	t.Helper()
	installed := putMuxTheme(t, st, id, files)
	if err := st.EnableTheme(t.Context(), id, installed.Digest); err != nil {
		t.Fatal(err)
	}
	return installed
}

type hostResponse struct {
	status int
	header http.Header
	body   string
}

func hostDo(t *testing.T, srv *httptest.Server, method, host, path, body string, header http.Header) hostResponse {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header[k] = v
	}
	client := &http.Client{Transport: testdeps.OwnedTransport(t), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return hostResponse{resp.StatusCode, resp.Header, string(b)}
}

func builtinPublic(path string) hostResponse {
	rec := httptest.NewRecorder()
	web.PublicHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return hostResponse{rec.Code, rec.Header(), rec.Body.String()}
}

func themeFilePath(t store.Theme, rel string) string {
	return "/_heron/themes/" + t.ID + "/" + t.Digest + "/" + rel
}

func assertThemeShell(t *testing.T, got hostResponse, installed store.Theme) {
	t.Helper()
	if got.status != http.StatusOK || !strings.Contains(got.body, themeFilePath(installed, "index.html")) || !strings.Contains(got.body, "sandbox=\"allow-scripts\"") || strings.Contains(got.body, "allow-same-origin") || strings.Contains(got.body, "theme index") {
		t.Fatalf("expected isolated shell for %s: status=%d body=%.200q", installed.Digest, got.status, got.body)
	}
	if got.header.Get("Cache-Control") != "no-store" {
		t.Fatal("theme shell was cacheable across selection changes")
	}
}

// 任意 Host 的同一根入口都显示所选主题；管理页面与 RPC 保持明确挂载，不由包内路径覆盖。
func TestSameDomainThemeKeepsPanelAndRPCMounts(t *testing.T) {
	t.Parallel()
	srv, st := newThemeTestServer(t)
	installed := installTheme(t, st, "t", map[string]string{"index.html": "theme index", "assets/app.js": "theme script", "admin/index.html": "shadow panel", "heron.v1.PublicService/GetSite": "shadow rpc"})
	for _, host := range []string{"panel.test", "status.test:8443", strings.TrimPrefix(srv.URL, "http://")} {
		for _, path := range []string{"/", "/nodes/3"} {
			assertThemeShell(t, hostDo(t, srv, http.MethodGet, host, path, "", nil), installed)
		}
		panel := hostDo(t, srv, http.MethodGet, host, "/admin/", "", nil)
		if (panel.status != http.StatusOK && panel.status != http.StatusServiceUnavailable) || !strings.Contains(panel.header.Get("Content-Security-Policy"), "default-src 'self'") || strings.Contains(panel.body, "shadow panel") {
			t.Fatalf("panel mount shadowed: %+v", panel)
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			path, body := "/heron.v1.PublicService/GetSite", "{}"
			if method == http.MethodGet {
				path += "?connect=v1&encoding=json&message=%7B%7D"
				body = ""
			}
			got := hostDo(t, srv, method, host, path, body, nil)
			var site map[string]any
			if got.status != http.StatusOK || json.Unmarshal([]byte(got.body), &site) != nil || site["adminPath"] != web.Prefix {
				t.Fatalf("public RPC/admin path: %+v", got)
			}
		}
		if got := hostDo(t, srv, http.MethodPost, host, "/heron.v1.AdminService/ListNodes", "{}", nil); got.status != http.StatusUnauthorized {
			t.Fatalf("admin auth bypass: %+v", got)
		}
	}
}

// 主题资源即使直接导航也带 CSP sandbox；只有不可变公开资源开放匿名 CORS。
func TestSameDomainThemeFilesRemainSandboxed(t *testing.T) {
	t.Parallel()
	srv, st := newThemeTestServer(t)
	installed := installTheme(t, st, "t", map[string]string{"index.html": "theme index", "assets/app.js": "theme script", "preview.svg": "<svg/>"})
	for rel, body := range map[string]string{"index.html": "theme index", "assets/app.js": "theme script", "preview.svg": "<svg/>"} {
		path := themeFilePath(installed, rel)
		got := hostDo(t, srv, http.MethodGet, "panel.test", path, "", http.Header{"Origin": {"null"}})
		if got.status != http.StatusOK || got.body != body {
			t.Fatalf("resource %s: %+v", path, got)
		}
		csp := got.header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "sandbox allow-scripts;") || strings.Contains(csp, "allow-same-origin") || !strings.Contains(csp, "connect-src 'none'") || !strings.Contains(csp, "worker-src 'none'") || got.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("resource isolation lost: %v", got.header)
		}
		if got.header.Get("Access-Control-Allow-Origin") != "*" || got.header.Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("resource CORS must be anonymous: %v", got.header)
		}
		etag := got.header.Get("ETag")
		if etag == "" {
			t.Fatal("immutable resource lacks ETag")
		}
		cached := hostDo(t, srv, http.MethodGet, "panel.test", path, "", http.Header{"If-None-Match": {etag}})
		if cached.status != http.StatusNotModified || !strings.Contains(cached.header.Get("Content-Security-Policy"), "sandbox allow-scripts;") {
			t.Fatal("conditional response lost sandbox policy")
		}
	}
}

// 注册表枚举全部管理过程；Origin:null 和跨域来源都在请求体与身份处理前拒绝，不靠浏览器读响应失败兜底。
func TestSandboxOriginCannotReachAnyAdminProcedure(t *testing.T) {
	t.Parallel()
	srv, st := newThemeTestServer(t)
	installTheme(t, st, "t", map[string]string{"index.html": "theme index"})
	count, public := 0, 0
	protoregistry.GlobalFiles.RangeFilesByPackage("heron.v1", func(file protoreflect.FileDescriptor) bool {
		for i := 0; i < file.Services().Len(); i++ {
			svc := file.Services().Get(i)
			for j := 0; j < svc.Methods().Len(); j++ {
				path := "/" + string(svc.FullName()) + "/" + string(svc.Methods().Get(j).Name())
				count++
				for _, origin := range []string{"null", "https://foreign.example"} {
					got := hostDo(t, srv, http.MethodPost, "panel.test", path, "{}", http.Header{"Origin": {origin}})
					if publicProcedures[path] {
						if origin == "null" {
							public++
						}
						if got.status == http.StatusUnauthorized || got.status == http.StatusNotFound || got.header.Get("Content-Type") != "application/json" {
							t.Fatalf("public RPC not mounted: %s %+v", path, got)
						}
					} else {
						want := http.StatusUnauthorized
						if svc.FullName() == "heron.v1.AdminService" {
							want = http.StatusForbidden
						}
						if got.status != want {
							t.Fatalf("%s origin %s: status %d want %d", path, origin, got.status, want)
						}
						if got.header.Get("Access-Control-Allow-Origin") != "" || got.header.Get("Access-Control-Allow-Credentials") != "" {
							t.Fatal("management CORS was broadened")
						}
					}
				}
			}
		}
		return true
	})
	if count <= public || public != len(publicProcedures) {
		t.Fatalf("incomplete procedure inventory: all=%d public=%d", count, public)
	}
}

func TestThemeSelectionFallbackAndImmutableVersions(t *testing.T) {
	t.Parallel()
	srv, st := newThemeTestServer(t)
	get := func(path string) hostResponse { return hostDo(t, srv, http.MethodGet, "panel.test", path, "", nil) }
	assertBuiltin := func() {
		t.Helper()
		for _, path := range []string{"/", "/nodes/3"} {
			got, want := get(path), builtinPublic(path)
			if got.status != want.status || got.body != want.body || got.header.Get("Content-Security-Policy") != want.header.Get("Content-Security-Policy") {
				t.Fatalf("builtin fallback %s: %+v", path, got)
			}
		}
	}
	enable := func(v store.Theme) {
		t.Helper()
		if err := st.EnableTheme(t.Context(), v.ID, v.Digest); err != nil {
			t.Fatal(err)
		}
	}
	assertBuiltin()
	first := putMuxTheme(t, st, "t", map[string]string{"index.html": "theme first", "app.js": "first script"})
	assertBuiltin()
	if got := get(themeFilePath(first, "index.html")); got.status != http.StatusNotFound {
		t.Fatal("unpublished package was publicly executable")
	}
	enable(first)
	assertThemeShell(t, get("/"), first)
	second := putMuxTheme(t, st, "t", map[string]string{"index.html": "theme second", "app.js": "second script"})
	assertThemeShell(t, get("/"), first)
	enable(second)
	assertThemeShell(t, get("/"), second)
	for _, v := range []struct {
		theme   store.Theme
		content string
	}{{first, "first script"}, {second, "second script"}} {
		if got := get(themeFilePath(v.theme, "app.js")); got.status != http.StatusOK || got.body != v.content {
			t.Fatalf("versioned asset mixed: %+v", got)
		}
	}
	enable(first)
	assertThemeShell(t, get("/"), first)
	if err := st.EnableTheme(t.Context(), "", ""); err != nil {
		t.Fatal(err)
	}
	assertBuiltin()
	enable(second)
	if err := st.DeleteTheme(t.Context(), "t"); err != nil {
		t.Fatal(err)
	}
	assertBuiltin()
	if got := get(themeFilePath(first, "index.html")); got.status != http.StatusNotFound {
		t.Fatal("removed package remained reachable through resource cache")
	}
}

func TestSameDomainThemeObeysPublicSwitch(t *testing.T) {
	t.Parallel()
	srv, st := newThemeTestServer(t)
	installed := installTheme(t, st, "t", map[string]string{"index.html": "theme index", "app.js": "theme script"})
	get := func(path string) hostResponse { return hostDo(t, srv, http.MethodGet, "panel.test", path, "", nil) }
	for _, enabled := range []bool{false, true} {
		if _, err := st.SaveSettings(t.Context(), store.SettingsUpdate{PublicEnabled: &enabled}); err != nil {
			t.Fatal(err)
		}
		panel := get("/admin/")
		if panel.status != http.StatusOK && panel.status != http.StatusServiceUnavailable {
			t.Fatal("public switch disabled admin recovery")
		}
		if !enabled {
			for _, path := range []string{"/", "/nodes/3", themeFilePath(installed, "index.html"), themeFilePath(installed, "app.js"), "/_heron/theme-shell.js", "/_heron/theme-sdk.js"} {
				got := get(path)
				if got.status != http.StatusOK || !strings.Contains(got.body, "公开页已关闭") || strings.Contains(got.body, "theme index") || got.header.Get("Cache-Control") != "no-store" {
					t.Fatalf("closed path %s exposed theme: %+v", path, got)
				}
			}
			for path := range publicProcedures {
				got := hostDo(t, srv, http.MethodPost, "panel.test", path, "{}", nil)
				if got.status != http.StatusNotFound {
					t.Fatalf("closed public RPC %s: %+v", path, got)
				}
			}
			if got := get("/assets/x.js"); got.status != http.StatusNotFound {
				t.Fatal("closed assets directory exposed content")
			}
		} else {
			assertThemeShell(t, get("/nodes/3"), installed)
			if got := get(themeFilePath(installed, "app.js")); got.status != http.StatusOK || got.body != "theme script" {
				t.Fatal("reopened theme resource unavailable")
			}
		}
	}
	if current, _, err := st.ThemeSelection(t.Context()); err != nil || current.Digest != installed.Digest {
		t.Fatal("public switch altered selected version")
	}
}

func TestNewHandlerKeepsStaticSurfaceBehindPublicSwitch(t *testing.T) {
	t.Parallel()
	var open atomic.Bool
	var calls atomic.Int64
	r := routes{
		agent:         mountOf("/heron.v1.AgentService/", http.NotFoundHandler()),
		admin:         mountOf("/heron.v1.AdminService/", http.NotFoundHandler()),
		public:        mountOf("/heron.v1.PublicService/", http.NotFoundHandler()),
		page:          http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); io.WriteString(w, "static surface") }),
		publicEnabled: open.Load,
	}
	h := newHandler(r)
	paths := []string{"/", "/nodes/3", "/assets/x.js", "/_heron/themes/t/digest/index.html", "/_heron/preview/token/files/index.html"}
	serveAll := func() {
		for _, path := range paths {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		}
	}
	serveAll()
	if calls.Load() != 0 {
		t.Fatal("closed public switch called static handler")
	}
	open.Store(true)
	serveAll()
	if calls.Load() != int64(len(paths)) {
		t.Fatalf("open static handler calls=%d want=%d", calls.Load(), len(paths))
	}
	r.publicEnabled = nil
	defer func() {
		if recover() == nil {
			t.Fatal("newHandler accepted a missing public switch")
		}
	}()
	newHandler(r)
}
