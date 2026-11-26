package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gunzipBytes(t *testing.T, gz []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

func doReq(t *testing.T, h http.Handler, method, target string, headers map[string][]string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, vals := range headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func hasVaryToken(h http.Header, want string) bool {
	for _, line := range h.Values("Vary") {
		for item := range strings.SplitSeq(line, ",") {
			if strings.EqualFold(strings.TrimSpace(item), want) {
				return true
			}
		}
	}
	return false
}

func TestSelectEncoding(t *testing.T) {
	cases := []struct {
		name    string
		headers []string
		hasBr   bool
		hasGzip bool
		wantEnc string
		wantOK  bool
	}{
		{"缺省头返回原文", nil, true, true, encodingIdentity, true},
		{"空头返回原文", []string{""}, true, true, encodingIdentity, true},
		{"纯空白返回原文", []string{"  ,  "}, true, true, encodingIdentity, true},
		{"只接受 gzip", []string{"gzip"}, true, true, encodingGzip, true},
		{"x-gzip 视为 gzip", []string{"x-gzip"}, true, true, encodingGzip, true},
		{"只接受 br", []string{"br"}, true, true, encodingBr, true},
		{"同时接受 gzip 与 br 且同权重优先 br", []string{"gzip, br"}, true, true, encodingBr, true},
		{"多行头合并协商", []string{"gzip;q=0.5", "br;q=0.9"}, true, true, encodingBr, true},
		{"按 q 值优先权重更高者", []string{"gzip;q=1.0, br;q=0.5"}, true, true, encodingGzip, true},
		{"最小正 q 值仍优于隐式原文", []string{"gzip;q=0.001"}, true, true, encodingGzip, true},
		{"br 设为 q=0 退回 gzip", []string{"br;q=0, gzip"}, true, true, encodingGzip, true},
		{"全部压缩设 q=0 退回原文", []string{"br;q=0, gzip;q=0"}, true, true, encodingIdentity, true},
		{"显式请求 identity", []string{"identity"}, true, true, encodingIdentity, true},
		{"identity 权重高于压缩", []string{"identity;q=0.9, br;q=0.5, gzip;q=0.5"}, true, true, encodingIdentity, true},
		{"通配符按服务端偏好选 br", []string{"*"}, true, true, encodingBr, true},
		{"通配符在无 br 时选 gzip", []string{"*"}, false, true, encodingGzip, true},
		{"通配符与显式 br;q=0 结合选 gzip", []string{"br;q=0, *;q=0.5"}, true, true, encodingGzip, true},
		{"通配符与无压缩副本时选原文", []string{"*"}, false, false, encodingIdentity, true},
		{"显式排除 identity 且无压缩可接受返回 406", []string{"identity;q=0"}, true, true, "", false},
		{"显式排除全部编码返回 406", []string{"*;q=0"}, true, true, "", false},
		{"排除 identity 但有 gzip 可用", []string{"gzip, identity;q=0"}, false, true, encodingGzip, true},
		{"排除 identity 且仅有的 gzip 缺失返回 406", []string{"gzip, identity;q=0"}, false, false, "", false},
		{"*;q=0 排除未列出的 identity 但显式允许 identity", []string{"*;q=0, identity;q=1"}, true, true, encodingIdentity, true},
		{"未知编码回退隐式原文", []string{"deflate, zstd"}, true, true, encodingIdentity, true},
		{"未知编码加 *;q=0 无可用表示返回 406", []string{"deflate, *;q=0"}, true, true, "", false},
		{"大小写不敏感", []string{"BR;Q=0.8, GZIP;q=0.4"}, true, true, encodingBr, true},
		{"非法 q 值条目被忽略不误拒原文", []string{"gzip;q=1.5"}, true, true, encodingIdentity, true},
		{"小数位超过三位视为非法条目", []string{"gzip;q=0.1234"}, true, true, encodingIdentity, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, ok := selectEncoding(tc.headers, tc.hasBr, tc.hasGzip)
			if enc != tc.wantEnc || ok != tc.wantOK {
				t.Fatalf("selectEncoding(%q, br=%v, gzip=%v) = (%q, %v), want (%q, %v)",
					tc.headers, tc.hasBr, tc.hasGzip, enc, ok, tc.wantEnc, tc.wantOK)
			}
		})
	}
}

func precompressedFS(t *testing.T, dir string) (fstest.MapFS, map[string][]byte) {
	t.Helper()
	indexRaw := []byte("<!doctype html><meta charset=utf-8><div id=root>" + strings.Repeat("heron-page-", 120) + "</div>")
	jsRaw := []byte("export const payload = `" + strings.Repeat("console.log('heron-asset');\n", 120) + "`;\n")
	cssRaw := []byte("body{" + strings.Repeat("color:#111;margin:0;", 120) + "}")
	woff2Raw := []byte("wOF2" + strings.Repeat("\x00\x01\x02\x03", 128))
	pngRaw := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\xff\x00", 64))
	// br 测试样本使用独立于原文与 gzip 的确定字节，用于验证 br 协商、ETag 区分与 Range/HEAD 切片。
	indexBr := []byte("FAKE-BROTLI-INDEX-REPRESENTATION-BYTES-0123456789")
	jsBr := []byte("FAKE-BROTLI-JS-REPRESENTATION-BYTES-" + strings.Repeat("0123456789abcdef", 8))
	cssGz := gzipBytes(t, cssRaw)

	raws := map[string][]byte{
		"index.html":            indexRaw,
		"assets/app-abc.js":     jsRaw,
		"assets/app-abc.css":    cssRaw,
		"assets/font.woff2":     woff2Raw,
		"assets/logo.png":       pngRaw,
		"assets/app-abc.js.br":  jsBr,
		"index.html.br":         indexBr,
		"assets/app-abc.js.gz":  gzipBytes(t, jsRaw),
		"index.html.gz":         gzipBytes(t, indexRaw),
		"assets/app-abc.css.gz": cssGz,
	}

	m := fstest.MapFS{
		dir + "/index.html":            {Data: indexRaw},
		dir + "/index.html.gz":         {Data: raws["index.html.gz"]},
		dir + "/index.html.br":         {Data: indexBr},
		dir + "/assets/app-abc.js":     {Data: jsRaw},
		dir + "/assets/app-abc.js.gz":  {Data: raws["assets/app-abc.js.gz"]},
		dir + "/assets/app-abc.js.br":  {Data: jsBr},
		dir + "/assets/app-abc.css":    {Data: cssRaw},
		dir + "/assets/app-abc.css.gz": {Data: cssGz},
		dir + "/assets/font.woff2":     {Data: woff2Raw},
		dir + "/assets/logo.png":       {Data: pngRaw},
		dir + "/.gitkeep":              {},
	}
	return m, raws
}

// 管理面板与内置公开页经同一套 embedded/serveFiles 路径：两个挂载点上协商、Vary、ETag、304、HEAD、Range、406 行为必须一致。
func TestEmbeddedPrecompressionNegotiationAndValidators(t *testing.T) {
	for _, mount := range []struct {
		name, dir, prefix string
	}{{"panel", "dist", Prefix}, {"public", "dist-public", "/"}} {
		t.Run(mount.name, func(t *testing.T) {
			fsys, raws := precompressedFS(t, mount.dir)
			h := embedded(fsys, mount.dir, mount.prefix, notBuiltAdmin)

			jsURL := mount.prefix + "assets/app-abc.js"
			cssURL := mount.prefix + "assets/app-abc.css"
			indexURL := mount.prefix
			fallbackURL := mount.prefix + "nodes/42"

			// 1. 各种 Accept-Encoding 协商：br / gzip / identity / q=0 / 缺省 / *
			respBr, bodyBr := doReq(t, h, http.MethodGet, jsURL, map[string][]string{"Accept-Encoding": {"gzip, br"}})
			if respBr.StatusCode != http.StatusOK || respBr.Header.Get("Content-Encoding") != "br" {
				t.Fatalf("br: status=%d ce=%q", respBr.StatusCode, respBr.Header.Get("Content-Encoding"))
			}
			if !hasVaryToken(respBr.Header, "Accept-Encoding") {
				t.Fatalf("br: missing Vary: Accept-Encoding, got %v", respBr.Header.Values("Vary"))
			}
			if !strings.Contains(respBr.Header.Get("Content-Type"), "javascript") {
				t.Fatalf("br: Content-Type=%q, want javascript", respBr.Header.Get("Content-Type"))
			}
			if respBr.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
				t.Fatalf("br: Cache-Control=%q", respBr.Header.Get("Cache-Control"))
			}
			if !bytes.Equal(bodyBr, raws["assets/app-abc.js.br"]) {
				t.Fatalf("br body mismatch: got %d bytes, want %d", len(bodyBr), len(raws["assets/app-abc.js.br"]))
			}
			etagBr := respBr.Header.Get("ETag")

			respGz, bodyGz := doReq(t, h, http.MethodGet, jsURL, map[string][]string{"Accept-Encoding": {"gzip;q=1.0, br;q=0.5"}})
			if respGz.StatusCode != http.StatusOK || respGz.Header.Get("Content-Encoding") != "gzip" {
				t.Fatalf("gzip: status=%d ce=%q", respGz.StatusCode, respGz.Header.Get("Content-Encoding"))
			}
			if !hasVaryToken(respGz.Header, "Accept-Encoding") {
				t.Fatalf("gzip: missing Vary: Accept-Encoding, got %v", respGz.Header.Values("Vary"))
			}
			if !strings.Contains(respGz.Header.Get("Content-Type"), "javascript") {
				t.Fatalf("gzip: Content-Type=%q, want javascript", respGz.Header.Get("Content-Type"))
			}
			if got := gunzipBytes(t, bodyGz); !bytes.Equal(got, raws["assets/app-abc.js"]) {
				t.Fatalf("gunzipped body differs from original")
			}
			etagGz := respGz.Header.Get("ETag")

			for _, tc := range []struct {
				label string
				ae    []string
			}{
				{"缺省头", nil},
				{"identity", []string{"identity"}},
				{"gzip 与 br 均 q=0", []string{"gzip;q=0, br;q=0"}},
			} {
				respID, bodyID := doReq(t, h, http.MethodGet, jsURL, map[string][]string{"Accept-Encoding": tc.ae})
				if respID.StatusCode != http.StatusOK || respID.Header.Get("Content-Encoding") != "" {
					t.Fatalf("%s: status=%d ce=%q", tc.label, respID.StatusCode, respID.Header.Get("Content-Encoding"))
				}
				if !hasVaryToken(respID.Header, "Accept-Encoding") {
					t.Fatalf("%s: missing Vary: Accept-Encoding", tc.label)
				}
				if !bytes.Equal(bodyID, raws["assets/app-abc.js"]) {
					t.Fatalf("%s: identity body mismatch", tc.label)
				}
				etagID := respID.Header.Get("ETag")
				if etagID == "" || etagGz == "" || etagBr == "" {
					t.Fatalf("empty ETag: id=%q gz=%q br=%q", etagID, etagGz, etagBr)
				}
				if etagID == etagGz || etagID == etagBr || etagGz == etagBr {
					t.Fatalf("encodings must have distinct ETags: id=%q gz=%q br=%q", etagID, etagGz, etagBr)
				}
			}

			// 仅带 .gz 无 .br 的文件在 * 下应选 gzip；只请求 br 时回退原文且仍带 Vary。
			respStar, _ := doReq(t, h, http.MethodGet, cssURL, map[string][]string{"Accept-Encoding": {"*"}})
			if respStar.StatusCode != http.StatusOK || respStar.Header.Get("Content-Encoding") != "gzip" || !hasVaryToken(respStar.Header, "Accept-Encoding") {
				t.Fatalf("css *: status=%d ce=%q vary=%v", respStar.StatusCode, respStar.Header.Get("Content-Encoding"), respStar.Header.Values("Vary"))
			}
			respOnlyBr, bodyOnlyBr := doReq(t, h, http.MethodGet, cssURL, map[string][]string{"Accept-Encoding": {"br"}})
			if respOnlyBr.StatusCode != http.StatusOK || respOnlyBr.Header.Get("Content-Encoding") != "" || !hasVaryToken(respOnlyBr.Header, "Accept-Encoding") || !bytes.Equal(bodyOnlyBr, raws["assets/app-abc.css"]) {
				t.Fatalf("css br fallback: status=%d ce=%q vary=%v", respOnlyBr.StatusCode, respOnlyBr.Header.Get("Content-Encoding"), respOnlyBr.Header.Values("Vary"))
			}

			// 2. 406 Not Acceptable：显式排除全部可用表示时必须返回 406 而不回落 index.html。
			for _, ae := range []string{"identity;q=0", "*;q=0", "br;q=0, gzip;q=0, identity;q=0"} {
				resp406, _ := doReq(t, h, http.MethodGet, jsURL, map[string][]string{"Accept-Encoding": {ae}})
				if resp406.StatusCode != http.StatusNotAcceptable {
					t.Fatalf("AE=%q: status=%d, want 406", ae, resp406.StatusCode)
				}
				if !hasVaryToken(resp406.Header, "Accept-Encoding") {
					t.Fatalf("AE=%q: 406 missing Vary: Accept-Encoding", ae)
				}
				checkSecurityHeaders(t, resp406)
			}

			// 3. 条件请求 304 与跨编码 If-None-Match 必须区分：用原文 ETag 请求 gzip 不得命中 304。
			respID, _ := doReq(t, h, http.MethodGet, jsURL, nil)
			etagID := respID.Header.Get("ETag")

			for _, tc := range []struct {
				label, ae, matchETag, mismatchETag string
			}{
				{"br", "br", etagBr, etagID},
				{"gzip", "gzip", etagGz, etagID},
				{"identity", "", etagID, etagGz},
			} {
				hdr := map[string][]string{"If-None-Match": {tc.matchETag}}
				if tc.ae != "" {
					hdr["Accept-Encoding"] = []string{tc.ae}
				}
				resp304, body304 := doReq(t, h, http.MethodGet, jsURL, hdr)
				if resp304.StatusCode != http.StatusNotModified || len(body304) != 0 {
					t.Fatalf("304 %s: status=%d bodyLen=%d", tc.label, resp304.StatusCode, len(body304))
				}
				if resp304.Header.Get("ETag") != tc.matchETag || !hasVaryToken(resp304.Header, "Accept-Encoding") || resp304.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
					t.Fatalf("304 %s headers: etag=%q vary=%v cc=%q", tc.label, resp304.Header.Get("ETag"), resp304.Header.Values("Vary"), resp304.Header.Get("Cache-Control"))
				}

				mismatchHdr := map[string][]string{"If-None-Match": {tc.mismatchETag}}
				if tc.ae != "" {
					mismatchHdr["Accept-Encoding"] = []string{tc.ae}
				}
				resp200, body200 := doReq(t, h, http.MethodGet, jsURL, mismatchHdr)
				if resp200.StatusCode != http.StatusOK || len(body200) == 0 || resp200.Header.Get("ETag") != tc.matchETag {
					t.Fatalf("mismatch INM %s: status=%d etag=%q, want 200 %q", tc.label, resp200.StatusCode, resp200.Header.Get("ETag"), tc.matchETag)
				}
			}

			// 4. HEAD 请求：带与 GET 相同的编码、ETag、Vary、Content-Length，但无正文。
			respHead, bodyHead := doReq(t, h, http.MethodHead, jsURL, map[string][]string{"Accept-Encoding": {"gzip"}})
			if respHead.StatusCode != http.StatusOK || len(bodyHead) != 0 || respHead.Header.Get("Content-Encoding") != "gzip" || respHead.Header.Get("ETag") != etagGz || !hasVaryToken(respHead.Header, "Accept-Encoding") {
				t.Fatalf("HEAD gzip: status=%d len=%d ce=%q etag=%q vary=%v", respHead.StatusCode, len(bodyHead), respHead.Header.Get("Content-Encoding"), respHead.Header.Get("ETag"), respHead.Header.Values("Vary"))
			}
			if respHead.ContentLength != int64(len(raws["assets/app-abc.js.gz"])) {
				t.Fatalf("HEAD gzip Content-Length=%d, want %d", respHead.ContentLength, len(raws["assets/app-abc.js.gz"]))
			}
			if respGz.ContentLength != int64(len(raws["assets/app-abc.js.gz"])) {
				t.Fatalf("GET gzip Content-Length=%d, want %d", respGz.ContentLength, len(raws["assets/app-abc.js.gz"]))
			}
			resp412, body412 := doReq(t, h, http.MethodGet, jsURL, map[string][]string{"Accept-Encoding": {"gzip"}, "If-Match": {`"non-matching"`}})
			if resp412.StatusCode != http.StatusPreconditionFailed || len(body412) != 0 || resp412.Header.Get("Content-Length") != "" {
				t.Fatalf("412 gzip: status=%d len=%d cl=%q", resp412.StatusCode, len(body412), resp412.Header.Get("Content-Length"))
			}

			// 5. Range 请求：按所选编码后的字节切片，拼接两段后解压与原文逐字节一致；越界返回 416 且不带 Content-Encoding。
			respR1, part1 := doReq(t, h, http.MethodGet, jsURL, map[string][]string{"Accept-Encoding": {"gzip"}, "Range": {"bytes=0-15"}})
			respR2, part2 := doReq(t, h, http.MethodGet, jsURL, map[string][]string{"Accept-Encoding": {"gzip"}, "Range": {"bytes=16-"}})
			if respR1.StatusCode != http.StatusPartialContent || respR2.StatusCode != http.StatusPartialContent {
				t.Fatalf("Range gzip statuses: %d, %d", respR1.StatusCode, respR2.StatusCode)
			}
			if respR1.Header.Get("Content-Encoding") != "gzip" || respR1.Header.Get("ETag") != etagGz || !hasVaryToken(respR1.Header, "Accept-Encoding") {
				t.Fatalf("Range gzip headers: ce=%q etag=%q vary=%v", respR1.Header.Get("Content-Encoding"), respR1.Header.Get("ETag"), respR1.Header.Values("Vary"))
			}
			joined := append(slices.Clone(part1), part2...)
			if !bytes.Equal(gunzipBytes(t, joined), raws["assets/app-abc.js"]) {
				t.Fatalf("joined gzip ranges did not decompress to original")
			}
			resp416, _ := doReq(t, h, http.MethodGet, jsURL, map[string][]string{"Accept-Encoding": {"gzip"}, "Range": {"bytes=999999-"}})
			if resp416.StatusCode != http.StatusRequestedRangeNotSatisfiable || resp416.Header.Get("Content-Encoding") != "" {
				t.Fatalf("Range 416: status=%d ce=%q", resp416.StatusCode, resp416.Header.Get("Content-Encoding"))
			}

			// 6. 不可压缩资源（woff2、png）：即使客户端声明 br, gzip 也不带 Content-Encoding；但 identity;q=0 仍回 406。
			for _, asset := range []string{"assets/font.woff2", "assets/logo.png"} {
				u := mount.prefix + asset
				resp, body := doReq(t, h, http.MethodGet, u, map[string][]string{"Accept-Encoding": {"br, gzip"}})
				if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" || !bytes.Equal(body, raws[asset]) {
					t.Fatalf("%s: status=%d ce=%q len=%d", asset, resp.StatusCode, resp.Header.Get("Content-Encoding"), len(body))
				}
				respNoID, _ := doReq(t, h, http.MethodGet, u, map[string][]string{"Accept-Encoding": {"gzip, identity;q=0"}})
				if respNoID.StatusCode != http.StatusNotAcceptable {
					t.Fatalf("%s with identity;q=0: status=%d, want 406", asset, respNoID.StatusCode)
				}
			}

			// 7. 首页与 SPA 回落路径：保留 no-cache 与 text/html，同时按 Accept-Encoding 下发预压缩副本。
			for _, pageURL := range []string{indexURL, fallbackURL} {
				resp, body := doReq(t, h, http.MethodGet, pageURL, map[string][]string{"Accept-Encoding": {"gzip"}})
				if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" || !hasVaryToken(resp.Header, "Accept-Encoding") {
					t.Fatalf("%s: status=%d ce=%q cc=%q ct=%q vary=%v", pageURL, resp.StatusCode, resp.Header.Get("Content-Encoding"), resp.Header.Get("Cache-Control"), resp.Header.Get("Content-Type"), resp.Header.Values("Vary"))
				}
				if !bytes.Equal(gunzipBytes(t, body), raws["index.html"]) {
					t.Fatalf("%s: gunzipped index differs from original", pageURL)
				}
			}

			// 8. 预压缩伴生文件不作为独立 URL 直接暴露。
			for _, sidecar := range []string{"assets/app-abc.js.gz", "assets/app-abc.js.br"} {
				resp, _ := doReq(t, h, http.MethodGet, mount.prefix+sidecar, nil)
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("direct sidecar %s: status=%d, want 404", sidecar, resp.StatusCode)
				}
			}
		})
	}
}

func TestDirHandlerAcceptEncoding(t *testing.T) {
	site := t.TempDir()
	writeFile(t, filepath.Join(site, "index.html"), "site index")
	h, err := DirHandler(site)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := doReq(t, h, http.MethodGet, "/", map[string][]string{"Accept-Encoding": {"br, gzip"}})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" || string(body) != "site index" {
		t.Fatalf("DirHandler gzip accept: status=%d ce=%q body=%q", resp.StatusCode, resp.Header.Get("Content-Encoding"), body)
	}
	resp406, _ := doReq(t, h, http.MethodGet, "/", map[string][]string{"Accept-Encoding": {"identity;q=0"}})
	if resp406.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("DirHandler identity;q=0: status=%d, want 406", resp406.StatusCode)
	}
}

// 若当前工作树已执行过 make web，抽验嵌入产物中的全部 .gz 副本解压后与原文字节完全一致。
func TestBuiltEmbeddedGzipMatchesOriginal(t *testing.T) {
	for _, tc := range []struct {
		name string
		root fs.FS
		dir  string
	}{{"admin", adminDist, "dist"}, {"public", publicDist, "dist-public"}} {
		sub, err := fs.Sub(tc.root, tc.dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Stat(sub, "index.html"); err != nil {
			continue
		}
		checked := 0
		if err := fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".gz") {
				return err
			}
			origPath := strings.TrimSuffix(p, ".gz")
			orig, err := fs.ReadFile(sub, origPath)
			if err != nil {
				t.Fatalf("%s: missing original %s for %s: %v", tc.name, origPath, p, err)
			}
			gz, err := fs.ReadFile(sub, p)
			if err != nil {
				t.Fatalf("%s: read %s: %v", tc.name, p, err)
			}
			if !bytes.Equal(gunzipBytes(t, gz), orig) {
				t.Fatalf("%s: gunzip(%s) != %s", tc.name, p, origPath)
			}
			checked++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if checked == 0 {
			t.Fatalf("%s: built dist has index.html but zero .gz assets", tc.name)
		}
	}
}
