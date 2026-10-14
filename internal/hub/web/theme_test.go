package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
)

type fakeThemeSource struct {
	gen       uint64
	current   store.Theme
	versions  map[string]themePackage
	reads     int
	err       error
	afterRead func()
}

type themePackage struct {
	meta  store.Theme
	files map[string][]byte
}

func (f *fakeThemeSource) ThemeGeneration() uint64 { return f.gen }
func (f *fakeThemeSource) ThemeSelection(ctx context.Context) (store.Theme, store.Theme, error) {
	if ctx.Err() != nil {
		return store.Theme{}, store.Theme{}, ctx.Err()
	}
	cur := f.current
	if f.afterRead != nil {
		hook := f.afterRead
		f.afterRead = nil
		hook()
	}
	return cur, store.Theme{}, f.err
}
func (f *fakeThemeSource) ThemeVersionFile(ctx context.Context, id, digest, path string) (store.Theme, []byte, error) {
	f.reads++
	if ctx.Err() != nil {
		return store.Theme{}, nil, ctx.Err()
	}
	if f.err != nil {
		return store.Theme{}, nil, f.err
	}
	p, ok := f.versions[digest]
	if !ok || p.meta.ID != id {
		return store.Theme{}, nil, store.ErrNotFound
	}
	content, ok := p.files[path]
	if !ok {
		return store.Theme{}, nil, store.ErrNotFound
	}
	return p.meta, append([]byte{}, content...), nil
}
func testThemeSource() *fakeThemeSource {
	meta := store.Theme{ID: "night", Digest: strings.Repeat("a", 64), SDK: 1, Published: true}
	return &fakeThemeSource{current: meta, versions: map[string]themePackage{meta.Digest: {meta: meta, files: map[string][]byte{"index.html": []byte("theme HTML"), "app.js": []byte("module"), "view.svg": []byte("<svg/>"), "data.unknown": []byte("<script/>")}}}}
}

var builtinSentinel = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "builtin") })

func themePath(meta store.Theme, file string) string {
	return "/_heron/themes/" + meta.ID + "/" + meta.Digest + "/" + file
}

func TestThemeDocumentsAreAlwaysSandboxed(t *testing.T) {
	src := testThemeSource()
	h := ThemeHandler(src, builtinSentinel, nil, slog.Default())
	for _, url := range []string{"/", "/nodes/3"} {
		code, _, body := serveWithin(t, h, url)
		if code != 200 || !strings.Contains(body, `sandbox="allow-scripts"`) || !strings.Contains(body, themePath(src.current, "index.html")) || strings.Contains(body, "theme HTML") {
			t.Fatalf("shell %s: %d %s", url, code, body)
		}
	}
	for _, file := range []string{"index.html", "app.js", "view.svg", "data.unknown", "missing.html", "assets/missing.js", ".secret"} {
		code, hdr, _ := serveWithin(t, h, themePath(src.current, file))
		if code != 200 && code != 404 {
			t.Fatalf("file %s: %d", file, code)
		}
		if !strings.Contains(hdr.Get("Content-Security-Policy"), "sandbox allow-scripts;") || strings.Contains(hdr.Get("Content-Security-Policy"), "allow-same-origin") || hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("Cache-Control") != "no-store" {
			t.Fatalf("unsafe %s: %v", file, hdr)
		}
		if file == "data.unknown" && hdr.Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("sniffed unknown type: %v", hdr)
		}
	}
	r := httptest.NewRequest("GET", themePath(src.current, "index.html"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 304 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox allow-scripts") {
		t.Fatalf("unsafe cache response: %v", w)
	}
}

func TestThemePublishedAdmissionAndPreview(t *testing.T) {
	src := testThemeSource()
	meta := src.current
	meta.Published = false
	p := src.versions[meta.Digest]
	p.meta = meta
	src.versions[meta.Digest] = p
	valid := true
	h := ThemeHandler(src, builtinSentinel, func(_ context.Context, token string) (string, string, bool) {
		return meta.ID, meta.Digest, valid && token == "cap"
	}, slog.Default())
	if code, _, _ := serveWithin(t, h, themePath(meta, "index.html")); code != 404 {
		t.Fatalf("unpublished is public: %d", code)
	}
	if code, _, body := serveWithin(t, h, "/_heron/preview/cap/files/index.html"); code != 200 || body != "theme HTML" {
		t.Fatalf("preview: %d %s", code, body)
	}
	valid = false
	if code, _, _ := serveWithin(t, h, "/_heron/preview/cap/files/index.html"); code != 404 {
		t.Fatalf("revoked preview: %d", code)
	}
	valid = true
	p.meta.SDK = 0
	p.meta.Published = true
	src.versions[meta.Digest] = p
	src.current = p.meta
	src.gen++
	for _, url := range []string{themePath(meta, "index.html"), "/_heron/preview/cap/files/index.html"} {
		if code, _, _ := serveWithin(t, h, url); code != 404 {
			t.Fatalf("old SDK executed: %s %d", url, code)
		}
	}
	if _, _, body := serveWithin(t, h, "/"); body != "builtin" {
		t.Fatalf("old SDK fallback: %s", body)
	}
}

func TestThemeResourceRequiresCompleteIdentity(t *testing.T) {
	src := testThemeSource()
	h := ThemeHandler(src, builtinSentinel, nil, slog.Default())
	for _, url := range []string{
		"/_heron/themes//" + src.current.Digest + "/index.html",
		"/_heron/themes/" + src.current.ID + "//index.html",
	} {
		if code, _, _ := serveWithin(t, h, url); code != 404 {
			t.Fatalf("incomplete identity %s: %d", url, code)
		}
	}
}

func TestThemeSnapshotGenerationAndFailure(t *testing.T) {
	src := testThemeSource()
	h := ThemeHandler(src, builtinSentinel, nil, slog.Default())
	first := src.current
	src.afterRead = func() { src.current = store.Theme{}; src.gen++ }
	if _, _, body := serveWithin(t, h, "/"); !strings.Contains(body, first.Digest) {
		t.Fatal("current read lost its snapshot")
	}
	if _, _, body := serveWithin(t, h, "/"); body != "builtin" {
		t.Fatal("stale generation hid deletion")
	}
	src.err = errors.New("disk failure")
	src.gen++
	if code, _, _ := serveWithin(t, h, "/"); code != 500 {
		t.Fatalf("error became fallback: %d", code)
	}
	if code, hdr, _ := serveWithin(t, h, themePath(first, "index.html")); code != 500 || !strings.Contains(hdr.Get("Content-Security-Policy"), "sandbox allow-scripts") {
		t.Fatalf("resource read error masked as missing: %d %v", code, hdr)
	}
	src.err = nil
	src.current = first
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil).WithContext(gone))
	if w.Code != 200 || !strings.Contains(w.Body.String(), first.Digest) {
		t.Fatalf("cancelled trigger lost shared reload: %d", w.Code)
	}
}

func TestThemeConcurrentResourcesReuseSnapshot(t *testing.T) {
	src := testThemeSource()
	h := ThemeHandler(src, builtinSentinel, nil, slog.Default())
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			code, _, body := serveWithin(t, h, themePath(src.current, "app.js"))
			if code != 200 || body != "module" {
				t.Errorf("resource: %d %s", code, body)
			}
		})
	}
	wg.Wait()
	if src.reads != 1 {
		t.Fatalf("package reads=%d", src.reads)
	}
	delete(src.versions, src.current.Digest)
	src.gen++
	if code, _, _ := serveWithin(t, h, themePath(src.current, "app.js")); code != 404 {
		t.Fatalf("deleted package cached: %d", code)
	}
}

func TestThemeHandlerAllocationPerRequestIsIndependentOfFileSize(t *testing.T) {
	perRequest := func(size int) float64 {
		src := testThemeSource()
		p := src.versions[src.current.Digest]
		p.files["file.bin"] = []byte(strings.Repeat("x", size))
		return allocBytesPerRequest(t, ThemeHandler(src, builtinSentinel, nil, slog.Default()), themePath(src.current, "file.bin"), int64(size))
	}
	small, big := perRequest(1024), perRequest(16<<20)
	if big > 2*small+4096 || big > (16<<20)/256 {
		t.Fatalf("per-request allocation small=%f big=%f", small, big)
	}
}

func TestThemeResourceAllocationIgnoresUnrelatedPackageFiles(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), clock.Real(), slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var paths []string
	for i := range 3 {
		id := fmt.Sprintf("theme%d", i)
		meta, err := s.PutTheme(t.Context(), store.Theme{ID: id, SDK: 1}, []store.ThemeFile{
			{Path: "index.html", Content: []byte("index")}, {Path: "app.js", Content: []byte("module")},
			{Path: "unused.bin", Content: make([]byte, 8<<20)},
		}, []byte(id), false, 20)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.EnableTheme(t.Context(), id, meta.Digest); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, themePath(meta, "app.js"))
	}
	measure := func(versions int, cold bool) uint64 {
		h := ThemeHandler(s, builtinSentinel, nil, slog.Default())
		read := func(i int) {
			if cold {
				h = ThemeHandler(s, builtinSentinel, nil, slog.Default())
			}
			code, _, body := serveWithin(t, h, paths[i%versions])
			if code != 200 || body != "module" {
				t.Fatalf("resource=%d %q", code, body)
			}
		}
		for i := range versions {
			read(i)
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for i := range 9 {
			read(i)
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / 9
	}
	single, rotated, cold := measure(1, false), measure(3, false), measure(1, true)
	t.Logf("bytes/request single=%d three=%d cold=%d", single, rotated, cold)
	if rotated > single+(1<<20) || cold > single+(1<<20) {
		t.Fatalf("small resource loaded unrelated package: single=%d three=%d cold=%d", single, rotated, cold)
	}
}

func TestThemeResourceCacheIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name        string
		size, count int
	}{
		{"bytes", 16 << 20, 3}, {"entries", 0, 257},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := testThemeSource()
			p := src.versions[src.current.Digest]
			h := ThemeHandler(src, builtinSentinel, nil, slog.Default()).(*themeHandler)
			for i := range tc.count {
				file := fmt.Sprintf("file%d.bin", i)
				p.files[file] = make([]byte, tc.size)
				w := &discardWriter{header: http.Header{}}
				h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, themePath(src.current, file), nil))
				if w.code != 200 || w.n != int64(tc.size) {
					t.Fatalf("response=%d %d", w.code, w.n)
				}
			}
			if h.cacheBytes > 32<<20 || len(h.resources) > 256 {
				t.Fatalf("unbounded cache: %d bytes %d entries", h.cacheBytes, len(h.resources))
			}
		})
	}
}

// discardWriter 丢弃响应体且不按响应大小分配：ReadFrom 交给 io.Discard（它的缓冲取自池）。httptest.ResponseRecorder
// 会把整个响应体缓冲下来，用它量分配，量到的是它自己。
type discardWriter struct {
	header http.Header
	code   int
	n      int64
}

func (d *discardWriter) Header() http.Header { return d.header }
func (d *discardWriter) WriteHeader(code int) {
	if d.code == 0 {
		d.code = code
	}
}
func (d *discardWriter) Write(p []byte) (int, error) {
	d.WriteHeader(http.StatusOK)
	d.n += int64(len(p))
	return len(p), nil
}
func (d *discardWriter) ReadFrom(r io.Reader) (int64, error) {
	d.WriteHeader(http.StatusOK)
	n, err := io.Copy(io.Discard, r)
	d.n += n
	return n, err
}

// allocBytesPerRequest 是 GET path 每个请求在堆上新分配的平均字节数（runtime.MemStats.TotalAlloc 之差），不含第一次请求
// 建快照的那一次。
func allocBytesPerRequest(t *testing.T, h http.Handler, path string, size int64) float64 {
	t.Helper()
	serve := func() *discardWriter {
		w := &discardWriter{header: http.Header{}}
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	if w := serve(); w.code != http.StatusOK || w.n != size {
		t.Fatalf("GET %s: %d, %d bytes, want 200 with %d bytes", path, w.code, w.n, size)
	}
	const runs = 20
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		serve()
	}
	runtime.ReadMemStats(&after)
	return float64(after.TotalAlloc-before.TotalAlloc) / runs
}
