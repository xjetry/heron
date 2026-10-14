package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/testwait"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// publicDirFixture 造一个替换目录 site 与它旁边的 outside；outside 里的内容一个字节都不能经 site 被读到。
func publicDirFixture(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	site, outside := filepath.Join(base, "site"), filepath.Join(base, "outside")
	writeFile(t, filepath.Join(site, "index.html"), "site index")
	writeFile(t, filepath.Join(site, "assets", "app.js"), "console.log(1)")
	writeFile(t, filepath.Join(site, "inner.txt"), "inner file")
	writeFile(t, filepath.Join(site, "sub", "page.txt"), "sub page")
	writeFile(t, filepath.Join(site, "assetsx", "a.js"), "not under assets")
	writeFile(t, filepath.Join(outside, "secret.txt"), "outside secret")
	symlink(t, "inner.txt", filepath.Join(site, "in-link.txt"))
	symlink(t, "../outside/secret.txt", filepath.Join(site, "out-link.txt"))
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(site, "abs-link.txt"))
	symlink(t, "../outside", filepath.Join(site, "out-dir"))
	symlink(t, "../../outside/secret.txt", filepath.Join(site, "assets", "leak.js"))
	if err := syscall.Mkfifo(filepath.Join(site, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	return site
}

// serveWithin 在 testwait.Bound 内拿到响应：读在 FIFO 上挂住的缺陷以失败结束，而不是拖到测试超时。
func serveWithin(t *testing.T, h http.Handler, target string) (int, http.Header, string) {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		done <- rec
	}()
	select {
	case rec := <-done:
		return rec.Code, rec.Header(), rec.Body.String()
	case <-time.After(testwait.Bound):
		t.Fatalf("GET %s did not return within %v", target, testwait.Bound)
		return 0, nil, ""
	}
}

func TestDirHandlerServesOnlyRegularFilesInsideTheDirectory(t *testing.T) {
	h, err := DirHandler(publicDirFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path, want string
		status     int
	}{
		{"/", "site index", 200},
		{"/index.html", "site index", 200},
		{"/nodes/7", "site index", 200},
		{"/assets/app.js", "console.log(1)", 200},
		{"/inner.txt", "inner file", 200},
		{"/in-link.txt", "inner file", 200},
		{"/sub/page.txt", "sub page", 200},
		{"/sub/", "site index", 200},
		{"/sub", "site index", 200},
		{"/out-link.txt", "site index", 200},
		{"/abs-link.txt", "site index", 200},
		{"/out-dir/secret.txt", "site index", 200},
		{"/../outside/secret.txt", "site index", 200},
		{"/sub/../../outside/secret.txt", "site index", 200},
		{"/%2e%2e/outside/secret.txt", "site index", 200},
		{"/pipe", "site index", 200},
		{"/assets/leak.js", "404 page not found\n", 404},
		{"/assets/missing.js", "404 page not found\n", 404},
		{"/assets/", "404 page not found\n", 404},
		{"/assetsx/a.js", "not under assets", 200},
		{"/assetsx/missing.js", "site index", 200},
	} {
		t.Run(c.path, func(t *testing.T) {
			status, header, body := serveWithin(t, h, c.path)
			if status != c.status || body != c.want {
				t.Fatalf("status %d body %q, want %d %q", status, body, c.status, c.want)
			}
			if body == "site index" && header.Get("Content-Type") != "text/html; charset=utf-8" {
				t.Fatalf("Content-Type %q for index.html", header.Get("Content-Type"))
			}
			if header.Get("Content-Security-Policy") != "frame-ancestors 'none'" || header.Get("X-Content-Type-Options") != "nosniff" ||
				header.Get("Cache-Control") != "no-cache" || header.Get("Referrer-Policy") != "" {
				t.Fatalf("headers: %v", header)
			}
		})
	}
}

// 运维常把整个检出或构建目录当 --public-dir，里面的 .git/config、.env 不能被读到：路径任一段以 . 开头
// 就当作不存在，assets/ 下 404，其余回落 index.html。
func TestDirHandlerTreatsDotfilesAsMissing(t *testing.T) {
	site := t.TempDir()
	writeFile(t, filepath.Join(site, "index.html"), "site index")
	writeFile(t, filepath.Join(site, ".env"), "SECRET=dir")
	writeFile(t, filepath.Join(site, ".git", "config"), "[core] dir")
	writeFile(t, filepath.Join(site, ".well-known", "security.txt"), "Contact: dir")
	writeFile(t, filepath.Join(site, "sub", ".hidden.txt"), "hidden page")
	writeFile(t, filepath.Join(site, "assets", ".hidden.js"), "hidden asset")
	h, err := DirHandler(site)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path, want string
		status     int
	}{
		{"/.env", "site index", 200},
		{"/.git/config", "site index", 200},
		{"/.well-known/security.txt", "site index", 200},
		{"/sub/.hidden.txt", "site index", 200},
		{"/assets/.hidden.js", "404 page not found\n", 404},
	} {
		t.Run(c.path, func(t *testing.T) {
			if status, _, body := serveWithin(t, h, c.path); status != c.status || body != c.want {
				t.Fatalf("status %d body %q, want %d %q", status, body, c.status, c.want)
			}
		})
	}
}

func TestDirHandlerRefusesADirectoryWithoutAnIndexInside(t *testing.T) {
	base := t.TempDir()
	noIndex := filepath.Join(base, "no-index")
	writeFile(t, filepath.Join(noIndex, "other.html"), "x")
	escaping := filepath.Join(base, "escaping")
	writeFile(t, filepath.Join(base, "real-index.html"), "outside index")
	if err := os.MkdirAll(escaping, 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, "../real-index.html", filepath.Join(escaping, "index.html"))
	dirIndex := filepath.Join(base, "dir-index")
	if err := os.MkdirAll(filepath.Join(dirIndex, "index.html"), 0o755); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(base, "absolute")
	writeFile(t, filepath.Join(absolute, "real.html"), "inside index")
	symlink(t, filepath.Join(absolute, "real.html"), filepath.Join(absolute, "index.html"))
	// 每种情形都报出同一条约束，再附底层原因。
	for _, dir := range []string{filepath.Join(base, "missing"), noIndex, escaping, dirIndex, absolute} {
		want := "--public-dir " + dir + ": index.html must be a regular file inside the directory (it answers every path that is not a file): "
		if _, err := DirHandler(dir); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("DirHandler(%s) error = %v, want it to start with %q", dir, err, want)
		}
	}
}

// 每个请求重新打开目录：运维原子替换（rename）之后，下一个请求就读到新内容。
func TestDirHandlerFollowsAnAtomicallyReplacedDirectory(t *testing.T) {
	base := t.TempDir()
	site := filepath.Join(base, "site")
	writeFile(t, filepath.Join(site, "index.html"), "old index")
	h, err := DirHandler(site)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, body := serveWithin(t, h, "/"); body != "old index" {
		t.Fatalf("before replacement: %q", body)
	}
	next := filepath.Join(base, "next")
	writeFile(t, filepath.Join(next, "index.html"), "new index")
	if err := os.Rename(site, filepath.Join(base, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, site); err != nil {
		t.Fatal(err)
	}
	if _, _, body := serveWithin(t, h, "/"); body != "new index" {
		t.Fatalf("after replacement: %q", body)
	}
}
