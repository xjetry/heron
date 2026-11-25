package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/web"
	"github.com/xjetry/heron/internal/testdeps"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// --public-listen 与 HERON_PUBLIC_LISTEN 走与其它 flag 相同的来源规则；缺席时没有公开监听。
func TestParseServeOptionsPublicListen(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hub.db")
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"absent", nil, nil, ""},
		{"flag", []string{"--public-listen", "127.0.0.1:9101"}, nil, "127.0.0.1:9101"},
		{"environment", nil, map[string]string{"HERON_PUBLIC_LISTEN": "127.0.0.1:9102"}, "127.0.0.1:9102"},
		{"flag beats environment", []string{"--public-listen", "127.0.0.1:9103"}, map[string]string{"HERON_PUBLIC_LISTEN": "127.0.0.1:9102"}, "127.0.0.1:9103"},
		{"environment set empty", nil, map[string]string{"HERON_PUBLIC_LISTEN": ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseServeOptions(append([]string{"--db", db, "--timezone", "UTC"}, tc.args...), envOf(tc.env))
			if err != nil || opts.publicListen != tc.want {
				t.Fatalf("publicListen = %q, err = %v; want %q", opts.publicListen, err, tc.want)
			}
		})
	}
}

// 两个监听在同一端口上地址范围重叠时启动即拒绝：错误点名两个 flag 及其环境变量。net.Listen("tcp", …) 对任何
// 通配写法（空主机、0.0.0.0、::）都建双栈套接字，所以 0.0.0.0 也盖住 IPv6 地址；内核分配的端口（0）互不相同，各自给 0
// 不算重叠；同端口上不同的具体地址不重叠。
func TestParseServeOptionsRejectsOverlappingListeners(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hub.db")
	for _, tc := range []struct {
		listen, public string
		overlap        bool
	}{
		{"127.0.0.1:9000", "127.0.0.1:9000", true},
		{"127.0.0.1:9000", "localhost:9000", true},
		{":9000", "127.0.0.1:9000", true},
		{"127.0.0.1:9000", "0.0.0.0:9000", true},
		{"[::]:9000", "127.0.0.1:9000", true},
		{"[::1]:9000", ":9000", true},
		{"0.0.0.0:9000", "[::]:9000", true},
		{"[::ffff:127.0.0.1]:9000", "127.0.0.1:9000", true},
		{"127.0.0.1:0", "127.0.0.1:0", false},
		{"127.0.0.1:9000", "127.0.0.1:9001", false},
		{"127.0.0.1:9000", "127.0.0.2:9000", false},
		{"0.0.0.0:9000", "[::1]:9000", true},
		{"[::1]:9000", "127.0.0.1:9000", false},
	} {
		t.Run(tc.listen+" "+tc.public, func(t *testing.T) {
			_, err := parseServeOptions([]string{"--db", db, "--timezone", "UTC", "--listen", tc.listen, "--public-listen", tc.public}, noServeEnv)
			if !tc.overlap {
				if err != nil {
					t.Fatalf("err = %v, want the two listeners accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatal("overlapping listeners accepted")
			}
			for _, w := range []string{"--public-listen (HERON_PUBLIC_LISTEN)", "--listen (HERON_LISTEN)"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v, want it to name %s", err, w)
				}
			}
		})
	}
}

// 经真实入口：重叠的两个监听在打开数据库之前拒绝，不建库、不监听。
func TestServeRejectsOverlappingListenersBeforeOpeningTheDatabase(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hub.db")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := runServeWith(ctx, []string{"--db", db, "--timezone", "UTC", "--listen", "127.0.0.1:9000", "--public-listen", "127.0.0.1:9000"},
		clock.NewFake(time.Now()), discardLog())
	if err == nil || !strings.Contains(err.Error(), "--public-listen") {
		t.Fatalf("err = %v, want the overlap rejected", err)
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overlapping listeners touched the database: %v", err)
	}
}

const publicListenPassword = "public listen sufficiently long password"

// startPublicListenHub 在 db 上设好管理员密码、按 seed 写好库，再经真实 serve 同时开主监听与公开监听（都由内核分配端口），
// 返回两者的 base URL。
func startPublicListenHub(t *testing.T, seed func(*store.Store)) (mainBase, publicBase string) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "hub.db")
	if err := runPasswdWith([]string{"--db", db}, pipeWith(t, publicListenPassword+"\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		st, err := store.Open(db, clock.Real(), discardLog(), store.MigrateSchema)
		if err != nil {
			t.Fatal(err)
		}
		seed(st)
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	line, _, _ := startTestHubLine(t, db, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), "45s", "--public-listen", "127.0.0.1:0")
	var listen, publicListen string
	if err := json.Unmarshal(line["listen"], &listen); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(line["public_listen"], &publicListen); err != nil {
		t.Fatalf("startup line has no public_listen: %v", err)
	}
	return "http://" + listen, "http://" + publicListen
}

// get 以不跟随重定向的客户端发 GET，返回状态、头与正文。
func get(t *testing.T, url string) hostResponse {
	t.Helper()
	client := &http.Client{Transport: testdeps.OwnedTransport(t), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(url)
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

// 公开监听只服务公开侧：/healthz、不带管理入口的 PublicService、总闸后的公开页；管理面板、AdminService、AgentService
// 在那里一律 404，不落到公开页的客户端路由回落上。过程全集取自注册表，新加的服务没进主监听专属挂载点就会在这里现形。
// 主监听照旧服务全部路由，GetSite 带面板入口。
func TestServePublicListenServesOnlyThePublicSide(t *testing.T) {
	t.Parallel()
	mainBase, publicBase := startPublicListenHub(t, nil)

	for base, want := range map[string]string{mainBase: web.Prefix, publicBase: ""} {
		site, err := heronv1connect.NewPublicServiceClient(ownedClient(t), base).GetSite(t.Context(), connect.NewRequest(&heronv1.GetSiteRequest{}))
		if err != nil {
			t.Fatalf("GetSite on %s: %v", base, err)
		}
		if got := site.Msg.GetAdminPath(); got != want {
			t.Errorf("GetSite on %s: admin_path %q, want %q", base, got, want)
		}
	}

	if got := get(t, publicBase+"/healthz"); got.status != http.StatusOK || got.body != "ok\n" {
		t.Errorf("/healthz on the public listener: %d %q", got.status, got.body)
	}
	for _, path := range []string{"/admin/", "/admin/nodes/1", "/admin/assets/x.js"} {
		if got := get(t, publicBase+path); got.status != http.StatusNotFound {
			t.Errorf("%s on the public listener: %d, want 404", path, got.status)
		}
	}
	// 面板构建过回 200、没构建回 503，两者都带 CSP；公开监听上的 404 不带。
	if got := get(t, mainBase+"/admin/nodes/1"); (got.status != http.StatusOK && got.status != http.StatusServiceUnavailable) || got.header.Get("Content-Security-Policy") == "" {
		t.Errorf("/admin/nodes/1 on the main listener: %d, want the panel handler", got.status)
	}
	want := httptest.NewRecorder()
	web.PublicHandler().ServeHTTP(want, httptest.NewRequest(http.MethodGet, "/nodes/3", nil))
	if got := get(t, publicBase+"/nodes/3"); got.status != want.Code || got.body != want.Body.String() {
		t.Errorf("/nodes/3 on the public listener: %d %.80q, want the built-in public page", got.status, got.body)
	}

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
				resp, err := ownedClient(t).Post(publicBase+path, "application/json", strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if svc.FullName() == "heron.v1.PublicService" {
					// 应答必须来自 connect：落到公开页的过程也可能是 200，只是不是 JSON。
					if resp.StatusCode == http.StatusNotFound || resp.Header.Get("Content-Type") != "application/json" {
						t.Errorf("%s on the public listener: %d %q, want the public service to answer", path, resp.StatusCode, resp.Header.Get("Content-Type"))
					}
				} else if resp.StatusCode != http.StatusNotFound {
					t.Errorf("%s on the public listener: %d, want 404", path, resp.StatusCode)
				}
			}
		}
		return true
	})
	if count == 0 {
		t.Fatal("enumerated no procedures")
	}
}

// 已发布主题在公开监听上照常可读；主题的短期预览只在主监听可用：同一个有效能力在主监听打开预览，在公开监听 404。
func TestServePublicListenServesPublishedThemesButNotPreviews(t *testing.T) {
	t.Parallel()
	var installed store.Theme
	mainBase, publicBase := startPublicListenHub(t, func(st *store.Store) {
		installed = installTheme(t, st, "t", map[string]string{"index.html": "theme index", "app.js": "theme script"})
	})
	for _, base := range []string{mainBase, publicBase} {
		if got := get(t, base+themeFilePath(installed, "app.js")); got.status != http.StatusOK || got.body != "theme script" {
			t.Errorf("published theme resource on %s: %d %q", base, got.status, got.body)
		}
	}
	admin := heronv1connect.NewAdminServiceClient(ownedClient(t), mainBase)
	logged, err := admin.Login(t.Context(), connect.NewRequest(&heronv1.LoginRequest{Password: publicListenPassword}))
	if err != nil {
		t.Fatal(err)
	}
	req := connect.NewRequest(&heronv1.PreviewThemeRequest{Id: installed.ID, Digest: installed.Digest})
	req.Header().Set("Cookie", strings.Split(logged.Header().Get("Set-Cookie"), ";")[0])
	preview, err := admin.PreviewTheme(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	url := preview.Msg.GetUrl()
	if !strings.HasPrefix(url, "/_heron/preview/") {
		t.Fatalf("preview url %q", url)
	}
	file := strings.TrimSuffix(url, "/") + "/files/app.js"
	if got := get(t, mainBase+file); got.status != http.StatusOK || got.body != "theme script" {
		t.Fatalf("preview on the main listener: %d %q", got.status, got.body)
	}
	for _, path := range []string{url, file} {
		if got := get(t, publicBase+path); got.status != http.StatusNotFound {
			t.Errorf("preview %s on the public listener: %d, want 404", path, got.status)
		}
	}
}

// 总闸关闭时两个监听都回说明页；面板入口只在主监听的说明页上，公开监听的说明页不链到一个在那里 404 的路径。
func TestServePublicListenClosedPageHasNoPanelLink(t *testing.T) {
	t.Parallel()
	closed := false
	mainBase, publicBase := startPublicListenHub(t, func(st *store.Store) {
		if _, err := st.SaveSettings(context.Background(), store.SettingsUpdate{PublicEnabled: &closed}); err != nil {
			t.Fatal(err)
		}
	})
	link := `href="` + web.Prefix + `"`
	for base, wantLink := range map[string]bool{mainBase: true, publicBase: false} {
		got := get(t, base+"/nodes/3")
		if got.status != http.StatusOK || !strings.Contains(got.body, "公开页已关闭") {
			t.Fatalf("closed page on %s: %d %.80q", base, got.status, got.body)
		}
		if strings.Contains(got.body, link) != wantLink {
			t.Errorf("closed page on %s: panel link present = %v, want %v", base, !wantLink, wantLink)
		}
	}
}

// 两个监听挂的是同一个 PublicService 处理器：按来源的限流配额只有一份，在主监听用完后换到公开监听同样 429。
// 假时钟不走，桶不回补。
func TestServePublicListenSharesTheRateLimit(t *testing.T) {
	t.Parallel()
	mainBase, publicBase := startPublicListenHub(t, nil)
	const site = "/heron.v1.PublicService/GetSite?connect=v1&encoding=json&message=%7B%7D"
	for i := range 60 {
		if got := get(t, mainBase+site); got.status != http.StatusOK {
			t.Fatalf("request %d on the main listener: %d, want 200 within the burst", i, got.status)
		}
	}
	if got := get(t, publicBase+site); got.status != http.StatusTooManyRequests {
		t.Fatalf("public listener after the main listener used the burst: %d, want 429", got.status)
	}
}

// 快照字节缓存也只有一份：主监听取过快照后关闸，窗口内公开监听拿到的仍是同一份缓存字节，而不是另起一份缓存去读
// 已关闸的服务（那会是 404）。GetSite 不经快照缓存，用它确认关闸确已生效。假时钟不走，缓存窗口不过期。
func TestServePublicListenSharesTheSnapshotCache(t *testing.T) {
	t.Parallel()
	mainBase, publicBase := startPublicListenHub(t, nil)
	const snapshot = "/heron.v1.PublicService/GetSnapshot?connect=v1&encoding=json&message=%7B%7D"
	first := get(t, mainBase+snapshot)
	if first.status != http.StatusOK {
		t.Fatalf("GetSnapshot on the main listener: %d %q", first.status, first.body)
	}
	admin := heronv1connect.NewAdminServiceClient(ownedClient(t), mainBase)
	logged, err := admin.Login(t.Context(), connect.NewRequest(&heronv1.LoginRequest{Password: publicListenPassword}))
	if err != nil {
		t.Fatal(err)
	}
	cookie := strings.Split(logged.Header().Get("Set-Cookie"), ";")[0]
	read := connect.NewRequest(&heronv1.GetSettingsRequest{})
	read.Header().Set("Cookie", cookie)
	current, err := admin.GetSettings(t.Context(), read)
	if err != nil {
		t.Fatal(err)
	}
	settings := current.Msg.GetSettings()
	closed := false
	settings.PublicEnabled = &closed
	update := connect.NewRequest(&heronv1.UpdateSettingsRequest{Settings: settings})
	update.Header().Set("Cookie", cookie)
	if _, err := admin.UpdateSettings(t.Context(), update); err != nil {
		t.Fatal(err)
	}
	if got := get(t, publicBase+"/heron.v1.PublicService/GetSite?connect=v1&encoding=json&message=%7B%7D"); got.status != http.StatusNotFound {
		t.Fatalf("GetSite on the public listener after closing: %d, want 404", got.status)
	}
	if got := get(t, publicBase+snapshot); got.status != http.StatusOK || got.body != first.body {
		t.Fatalf("GetSnapshot on the public listener within the cache window: %d %q, want the cached %q", got.status, got.body, first.body)
	}
}
