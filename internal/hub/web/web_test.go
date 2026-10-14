package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func builtFS(dir string) fstest.MapFS {
	return fstest.MapFS{
		dir + "/index.html":         {Data: []byte("<!doctype html><div id=root></div>")},
		dir + "/assets/app-abc.js":  {Data: []byte("console.log(1)")},
		dir + "/assets/app-abc.css": {Data: []byte("body{}")},
		dir + "/robots.txt":         {Data: []byte("User-agent: *")},
		dir + "/sub/page.txt":       {Data: []byte("sub page")},
		dir + "/.gitkeep":           {},
		dir + "/.env":               {Data: []byte("SECRET=embedded")},
		dir + "/.git/config":        {Data: []byte("[core] embedded")},
		dir + "/sub/.hidden.txt":    {Data: []byte("hidden page")},
		dir + "/assets/.hidden.js":  {Data: []byte("hidden asset")},
		dir + "/assetsx/a.js":       {Data: []byte("not under assets")},
	}
}

func get(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	resp := rec.Result()
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func responseBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func checkSecurityHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	wantCSP := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	if resp.Header.Get("Content-Security-Policy") != wantCSP || resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("security headers missing or incorrect: %v", resp.Header)
	}
}

// 面板与内置公开页是同一个核心、两个挂载点：同一组路径在两处得到同样的应答与同一套 CSP。
func TestEmbeddedServesFilesAndFallsBackToIndex(t *testing.T) {
	for _, mount := range []struct {
		name, dir, prefix string
	}{{"panel", "dist", Prefix}, {"public", "dist-public", "/"}} {
		h := embedded(builtFS(mount.dir), mount.dir, mount.prefix, notBuiltAdmin)
		for _, c := range []struct {
			path, wantBody, wantCache string
			wantStatus                int
		}{
			{"", "<div id=root>", "no-cache", 200},
			{"index.html", "<div id=root>", "no-cache", 200},
			{"nodes/7", "<div id=root>", "no-cache", 200},
			{"assets/app-abc.js", "console.log", "public, max-age=31536000, immutable", 200},
			{"assets/app-abc.css", "body{}", "public, max-age=31536000, immutable", 200},
			{"robots.txt", "User-agent: *", "no-cache", 200},
			{"sub/", "<div id=root>", "no-cache", 200},
			{"sub", "<div id=root>", "no-cache", 200},
			{"assets/../robots.txt", "User-agent: *", "no-cache", 200},
			{"sub/../robots.txt", "User-agent: *", "no-cache", 200},
			{"assets/missing.js", "404 page not found", "", 404},
			{"assets/", "404 page not found", "", 404},
			{"assets", "404 page not found", "", 404},
			// 点文件当作不存在：assets/ 下 404，其余回落 index.html；产物里的 .gitkeep 也不例外。
			{".gitkeep", "<div id=root>", "no-cache", 200},
			{".env", "<div id=root>", "no-cache", 200},
			{".git/config", "<div id=root>", "no-cache", 200},
			{"sub/.hidden.txt", "<div id=root>", "no-cache", 200},
			{"assets/.hidden.js", "404 page not found", "", 404},
			// assets 按整段比较：assetsx/ 下的文件不永久缓存，缺失时回落 index.html。
			{"assetsx/a.js", "not under assets", "no-cache", 200},
			{"assetsx/missing.js", "<div id=root>", "no-cache", 200},
		} {
			path := mount.prefix + c.path
			t.Run(mount.name+" "+path, func(t *testing.T) {
				resp := get(t, h, path)
				body := responseBody(t, resp)
				if resp.StatusCode != c.wantStatus || !strings.Contains(body, c.wantBody) {
					t.Fatalf("%s: status %d body %q", path, resp.StatusCode, body)
				}
				if c.wantCache != "" && resp.Header.Get("Cache-Control") != c.wantCache {
					t.Fatalf("%s: Cache-Control %q, want %q", path, resp.Header.Get("Cache-Control"), c.wantCache)
				}
				// 回落的 index.html 按 index.html 定类型，不按请求路径的扩展名：配合 nosniff，类型错了浏览器会把页面当纯文本。
				if c.wantBody == "<div id=root>" && resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
					t.Fatalf("%s: Content-Type %q for index.html", path, resp.Header.Get("Content-Type"))
				}
				checkSecurityHeaders(t, resp)
			})
		}
	}
}

// 关闸时按 serveFiles 的回落规则分流：开闸时回落到 index.html 的路径（前端路由、缺失的文件、点文件）关闸后得到说明页，
// 开闸时 404 的（assets/ 下缺失）关闸后仍是 404。逐路径对照两边，规则只在一侧改动时失败。这些路径都不对应可服务的
// 文件，开闸时的应答只由回落规则决定；存在的文件另列在后面，关闸时一律不经文件服务。
func TestPublicGateClosedFollowsTheFallbackRule(t *testing.T) {
	open := true
	gate := PublicGate(embedded(builtFS("dist-public"), "dist-public", "/", notBuiltPublic), func() bool { return open })
	checkClosed := func(t *testing.T, path string, wantNotFound bool) string {
		t.Helper()
		resp := get(t, gate, path)
		body := responseBody(t, resp)
		if wantNotFound {
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("closed %s: status %d body %q, want 404", path, resp.StatusCode, body)
			}
		} else if resp.StatusCode != http.StatusOK || body != closedPage || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("closed %s: status %d body %q type %q, want the closed page", path, resp.StatusCode, body, resp.Header.Get("Content-Type"))
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("closed %s: Cache-Control %q", path, resp.Header.Get("Cache-Control"))
		}
		checkSecurityHeaders(t, resp)
		return body
	}
	open = false
	body := checkClosed(t, "/nodes/7", false)
	// 分享出去的链接要能看出是站点关了而不是链接失效：说明页带品牌图形、原因与面板入口。
	for _, want := range []string{"<svg", "公开页已关闭", "站长暂时关闭了公开页", `href="` + Prefix + `"`, "管理员登录", `<meta name="viewport"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("closed page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "http") || strings.Contains(body, "@import") {
		t.Fatalf("closed page must not reference remote resources:\n%s", body)
	}
	var pages, notFound int
	for _, path := range []string{
		"/nodes/7", "/theme.js", "/sub", "/.env", "/assetsx/missing.js", "/assets/../nodes/7",
		"/assets", "/assets/", "/assets/missing.js", "/assets/.hidden.js", "/sub/../assets/missing.js",
	} {
		t.Run(path, func(t *testing.T) {
			open = true
			resp := get(t, gate, path)
			fallback := responseBody(t, resp)
			wantNotFound := resp.StatusCode == http.StatusNotFound
			if !wantNotFound && !strings.Contains(fallback, "<div id=root>") {
				t.Fatalf("open %s: status %d body %q, want index.html or 404", path, resp.StatusCode, fallback)
			}
			if wantNotFound {
				notFound++
			} else {
				pages++
			}
			open = false
			checkClosed(t, path, wantNotFound)
		})
	}
	if pages == 0 || notFound == 0 {
		t.Fatalf("paths cover %d fallback pages and %d not-found; both directions are needed", pages, notFound)
	}
	for _, c := range []struct {
		path         string
		wantNotFound bool
		leak         string
	}{
		{"/", false, "<div id=root>"},
		{"/index.html", false, "<div id=root>"},
		{"/robots.txt", false, "User-agent"},
		{"/assets/app-abc.js", true, "console.log"},
	} {
		t.Run("existing "+c.path, func(t *testing.T) {
			open = false
			if body := checkClosed(t, c.path, c.wantNotFound); strings.Contains(body, c.leak) {
				t.Fatalf("closed %s served the file: %q", c.path, body)
			}
		})
	}
}

func TestUnbuiltEmbeddedPagesExplainThemselves(t *testing.T) {
	for _, c := range []struct {
		dir, prefix, notBuilt, want string
	}{
		{"dist", Prefix, notBuiltAdmin, "The admin panel has not been built"},
		{"dist-public", "/", notBuiltPublic, "The public page has not been built"},
	} {
		h := embedded(fstest.MapFS{c.dir + "/.gitkeep": {}}, c.dir, c.prefix, c.notBuilt)
		resp := get(t, h, c.prefix+"nodes/7")
		body := responseBody(t, resp)
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, c.want) {
			t.Fatalf("%s: status %d body %q", c.dir, resp.StatusCode, body)
		}
		if resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("%s: unbuilt response headers: %v", c.dir, resp.Header)
		}
		checkSecurityHeaders(t, resp)
	}
}

// all:dist 与 all:dist-public 必须能匹配文件；源码检出靠入库的 .gitkeep 满足，构建后还会包含产物。
func TestEmbeddedDistExists(t *testing.T) {
	if _, err := adminDist.ReadDir("dist"); err != nil {
		t.Fatal(err)
	}
	if _, err := publicDist.ReadDir("dist-public"); err != nil {
		t.Fatal(err)
	}
}
