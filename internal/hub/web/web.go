// Package web 服务 hub 的静态页面：嵌入的管理面板（/admin/）、嵌入的内置公开页（/）、运维用 --public-dir 指定的
// 替换目录，以及同域沙箱承载的第三方主题（存在库里）。
//
// 两份嵌入产物由 Vite 构建到本包的 dist 与 dist-public 目录，不入库；目录里只保证有一个占位文件，
// 所以 embed 永远成立，而"有没有真的构建过"由 index.html 是否存在判定。
package web

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var adminDist embed.FS

//go:embed all:dist-public
var publicDist embed.FS

// Prefix 是面板的挂载路径；Vite 的 base 与之一致，产物里的资源引用都以它开头。
const Prefix = "/admin/"

// csp 是面板与内置公开页共用的策略。style-src 带 'unsafe-inline'，只因为公开页把站点设置的自定义 CSS
// 写进一个 <style> 元素；元素的 style 经 CSSOM 写入（React 的 style 属性、uPlot），不受 style-src 约束。
// img-src 带 data:：logo 是 data: URL。
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

const notBuiltAdmin = `<!doctype html><meta charset="utf-8"><title>Heron</title>
<p>The admin panel has not been built into this binary. Run <code>make web</code> before <code>go build</code>, or use a release build.</p>`

const notBuiltPublic = `<!doctype html><meta charset="utf-8"><title>Heron</title>
<p>The public page has not been built into this binary. Run <code>make web</code> before <code>go build</code>, or use a release build.</p>`

// Handler 服务 /admin/ 下的管理面板。
func Handler() http.Handler { return embedded(adminDist, "dist", Prefix, notBuiltAdmin) }

// PublicHandler 服务挂在 / 的内置公开页。
func PublicHandler() http.Handler { return embedded(publicDist, "dist-public", "/", notBuiltPublic) }

// closedPage 是总闸关闭时 assets/ 之外的公开路径得到的说明页，带品牌图形、关闭原因与面板入口。
// PublicGate 在关闭时对 assets/ 返回 404，所以 SVG 与样式全部内联，不依赖前端产物。
// cmd/hub/serve.go 将面板与公开页挂在同一个 mux，入口直接使用 Prefix；颜色跟随系统明暗。
const closedPage = `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>公开页已关闭</title>
<style>
:root{color-scheme:light dark;--bg:light-dark(#f6f7f9,#0b0d12);--fg:light-dark(#111827,#e6e8ee);--muted:light-dark(#4b5563,#9aa3b5);--accent:light-dark(#2563eb,#5b9bf8);--bird:light-dark(#214e57,#a9d4d6)}
body{margin:0;min-height:100dvh;display:grid;place-items:center;font:14px/1.5 Inter,system-ui,-apple-system,"PingFang SC","Noto Sans SC",sans-serif;color:var(--fg);background:var(--bg)}
main{text-align:center;padding:24px}svg{width:64px;height:64px}h1{margin:12px 0 4px;font-size:22px;font-weight:600}p{margin:0 0 16px;color:var(--muted)}a{color:var(--accent);text-decoration:none}
</style>
<main>
<svg viewBox="0 0 64 64" fill="none" aria-hidden="true"><path fill="var(--bird)" d="M7 43c4-10 11-17 21-19l5-1c4-1 5-3 3-6l-3-4c-3-5 0-10 5-10 4 0 6 2 7 5l12 4-14 2c-2-1-3-2-3-4-2 0-3 1-2 3l4 5c4 6 1 11-5 14-2 10-10 15-20 13L7 43Z"/><path d="M12 40c5-7 11-10 19-11-3 7-9 11-19 11Z" fill="#fff" fill-opacity=".24"/><path stroke="var(--bird)" d="m24 45-2 13h-7m17-14 4 8-5 6h7" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"/><circle cx="40" cy="7.5" r="1.5" fill="#e5ad64"/></svg>
<h1>公开页已关闭</h1>
<p>站长暂时关闭了公开页，这个链接本身没有失效。</p>
<a href="` + Prefix + `">管理员登录</a>
</main>`

// PublicGate 统一包住 RPC 之外的公开静态面，包括内置页、可信目录、主题容器、历史产物及预览。
// 管理面板和 RPC 由更具体路由承载；关闭时不调用下游，也不读主题库。
//
// 关闭时的分流沿用 serveFiles 的回落规则，同一个 relPath 与 underAssets：assets/ 下 404，其余路径回说明页。
// 开闸时 serveFiles 把 assets/ 之外缺失的路径交给客户端路由，分享出去的 /nodes/3 是一个页面；两处规则一旦分叉，
// 同一个前端路由关闸后就成了 404，访客分不清是站点关了还是链接失效。说明页与 404 都带内置页的安全头，
// 不随来源（--public-dir 与主题共用的 customHeaders）放宽。
func PublicGate(next http.Handler, enabled func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if enabled() {
			next.ServeHTTP(w, r)
			return
		}
		builtinHeaders(w.Header())
		w.Header().Set("Cache-Control", "no-store")
		if underAssets(relPath(r.URL.Path, "/")) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, closedPage)
	})
}

func builtinHeaders(h http.Header) {
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// embedded 服务一份嵌入产物：assets/ 下的文件名带内容哈希，可以永久缓存，其余 no-cache。
// 没构建过（没有 index.html）时每个路径都回答一页说明。
func embedded(root fs.FS, dir, prefix, notBuilt string) http.Handler {
	sub, err := fs.Sub(root, dir)
	if err != nil {
		panic(err)
	}
	if info, err := fs.Stat(sub, "index.html"); err != nil || !info.Mode().IsRegular() {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			h := w.Header()
			builtinHeaders(h)
			h.Set("Cache-Control", "no-cache")
			h.Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, notBuilt)
		})
	}
	cacheFor := func(rel string) string {
		if underAssets(rel) {
			return "public, max-age=31536000, immutable"
		}
		return "no-cache"
	}
	return serveFiles(prefix, builtinHeaders, cacheFor, sub.Open)
}

// opener 打开 rel：rel 已按 URL 路径语义清理，相对挂载根，不以 / 开头，不含 ..。
// 打开不得阻塞：FIFO 在没有写端时挂住的是 open 本身，serveFiles 在打开之后才看文件类型，兜不住这一步。
// DirHandler 以 O_NONBLOCK 打开；embed 里没有特殊文件；ThemeHandler 从内存里的快照打开。新增来源要自己满足这一条。
type opener func(rel string) (fs.File, error)

// entityTagger 是来源能给出的强校验器：打开的文件实现它时，serveRegular 把它作为 ETag 发出，ServeContent 据
// If-None-Match 回 304。值必须随内容变化而变化（主题按内容哈希算）；给不出这样的值的来源不实现它，不发 ETag。
type entityTagger interface {
	EntityTag() string
}

// serveFiles 是各处静态服务共用的核心。命中普通文件就返回它；rel 在 assets 之下（underAssets）而未命中时返回 404——
// 用 HTML 回应 script 标签会被浏览器按 MIME 拒绝，404 才能让缺失可见；其余路径回落到 index.html，交给客户端路由。
// 不在特殊文件上挂住靠两条各自的事实：打开本身不阻塞由 opener 保证；打开之后不是普通文件（目录、FIFO、设备）
// 就当作不存在、不读，所以任何来源都不列目录，也不从 FIFO 与设备读。
// 点文件同样当作不存在：路径任一段以 . 开头就不打开（hidden），.git/config、.env 这类运维放目录时顺手带进来的
// 文件因此不经任何来源服务。判定只看请求路径里各段的名字，符号链接按链接自己的名字算。.well-known/ 也在其列，
// 需要它的（ACME 校验、security.txt）由反向代理提供。
func serveFiles(prefix string, headers func(http.Header), cacheFor func(rel string) string, open opener) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers(w.Header())
		rel := requestRel(r.URL.Path, prefix)
		if !hidden(rel) && serveRegular(w, r, open, rel, cacheFor(rel)) {
			return
		}
		if underAssets(rel) {
			http.NotFound(w, r)
			return
		}
		if !serveRegular(w, r, open, "index.html", "no-cache") {
			http.Error(w, "index.html unreadable", http.StatusInternalServerError)
		}
	})
}

// underAssets 报告 rel 是否是 assets 目录本身或在它之下，按整段比较：assetsx/ 不算。缺失时 404、嵌入产物的
// 永久缓存与总闸关闭时的 404（PublicGate）都按它判定，同一个判定只有这一种写法。
func underAssets(rel string) bool {
	return rel == "assets" || strings.HasPrefix(rel, "assets/")
}

// hidden 报告 rel 是否有一段以 . 开头。rel 来自 relPath：经 path.Clean 清理，没有 . 与 .. 段，也不以 / 开头，
// 所以以 . 开头的段只能是点文件或点目录的名字。
func hidden(rel string) bool {
	return strings.HasPrefix(rel, ".") || strings.Contains(rel, "/.")
}

// requestRel 是请求路径对应的文件名：挂载根本身是 index.html。serveFiles 打开的只有它与回落用的 index.html。
func requestRel(urlPath, prefix string) string {
	if rel := relPath(urlPath, prefix); rel != "" {
		return rel
	}
	return "index.html"
}

// relPath 先按 URL 路径语义清理再去掉挂载前缀。各来源对 . 与 .. 的处理不同（embed 经 fs.ValidPath 一律拒绝，
// os.Root 接受不越界的 ..，主题按库里的键精确匹配），先清理，同一个 URL 在各处才落到同一个文件名，assets/ 的 404 判定
// 也看清理后的路径。越界由来源自己拒绝（fs.ValidPath、os.Root；主题的键由 theme.Parse 限定为包内规范路径），不靠这里。清理后不在前缀之下的（如 /admin 本身）按挂载根处理。
func relPath(urlPath, prefix string) string {
	if rest, ok := strings.CutPrefix(path.Clean("/"+urlPath), prefix); ok {
		return rest
	}
	return ""
}

// serveRegular 在 rel 是普通文件时写出它并返回 true；打不开或不是普通文件时返回 false，什么都不写。
func serveRegular(w http.ResponseWriter, r *http.Request, open opener, rel, cacheControl string) bool {
	f, err := open(rel)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	content, ok := f.(io.ReadSeeker)
	if !ok {
		return false
	}
	w.Header().Set("Cache-Control", cacheControl)
	if e, ok := f.(entityTagger); ok {
		w.Header().Set("ETag", e.EntityTag())
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), content)
	return true
}
