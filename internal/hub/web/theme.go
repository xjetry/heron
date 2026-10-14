package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/theme"
)

// ThemeSource 按摘要读取不可变产物；代数在任何在线版本或选择变更提交后前进。
type ThemeSource interface {
	ThemeGeneration() uint64
	ThemeSelection(context.Context) (store.Theme, store.Theme, error)
	ThemeVersionFile(context.Context, string, string, string) (store.Theme, []byte, error)
}

// PreviewAccess 必须逐请求检查短期能力以及其所属管理员会话；能力不由 cookie 自动授予。
type PreviewAccess func(context.Context, string) (id, digest string, ok bool)

func ThemeHandler(src ThemeSource, builtin http.Handler, preview PreviewAccess, log *slog.Logger) http.Handler {
	return &themeHandler{src: src, builtin: builtin, preview: preview, log: log}
}

type themeHandler struct {
	src        ThemeSource
	builtin    http.Handler
	preview    PreviewAccess
	log        *slog.Logger
	mu         sync.Mutex
	gen        uint64
	loaded     bool
	current    store.Theme
	resources  []themeResource
	cacheBytes int
}

type themeResource struct {
	meta    store.Theme
	path    string
	content []byte
}

const themeCacheBytes = 32 << 20
const themeCacheEntries = 256

// 缓存按资源计量并同时限制字节和条目；冷读只加载目标文件，不因轮换摘要读取包内无关内容。
// 返回的字节此后只读；换代清空索引不会改变已经开始的响应。
func (h *themeHandler) snapshot(ctx context.Context, id, digest, rel string) (themeResource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	gen := h.src.ThemeGeneration()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if !h.loaded || h.gen != gen {
		current, _, err := h.src.ThemeSelection(ctx)
		if err != nil {
			return themeResource{}, err
		}
		h.current, h.gen, h.loaded, h.resources, h.cacheBytes = current, gen, true, nil, 0
	}
	if id == "" {
		return themeResource{meta: h.current}, nil
	}
	for i, p := range h.resources {
		if p.meta.ID == id && p.meta.Digest == digest && p.path == rel {
			copy(h.resources[i:], h.resources[i+1:])
			h.resources[len(h.resources)-1] = p
			return p, nil
		}
	}
	meta, content, err := h.src.ThemeVersionFile(ctx, id, digest, rel)
	if err != nil {
		return themeResource{}, err
	}
	p := themeResource{meta: meta, path: rel, content: content}
	if len(content) > themeCacheBytes {
		return p, nil
	}
	for len(h.resources) > 0 && (len(h.resources) >= themeCacheEntries || h.cacheBytes+len(content) > themeCacheBytes) {
		h.cacheBytes -= len(h.resources[0].content)
		h.resources[0] = themeResource{}
		h.resources = h.resources[1:]
	}
	h.resources = append(h.resources, p)
	h.cacheBytes += len(content)
	return p, nil
}

func (h *themeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		SandboxHeaders(w.Header())
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/_heron/themes/") || strings.HasPrefix(r.URL.Path, "/_heron/preview/") {
		h.servePackage(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/_heron/") {
		ThemeRuntimeHandler().ServeHTTP(w, r)
		return
	}
	p, err := h.snapshot(r.Context(), "", "", "")
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	if p.meta.ID == "" || theme.CheckExecutable(p.meta.SDK) != nil {
		h.builtin.ServeHTTP(w, r)
		return
	}
	ThemeShell(w, "/_heron/themes/"+p.meta.ID+"/"+p.meta.Digest+"/index.html")
}

func (h *themeHandler) servePackage(w http.ResponseWriter, r *http.Request) {
	SandboxHeaders(w.Header())
	var id, digest, rel string
	preview := strings.HasPrefix(r.URL.Path, "/_heron/preview/")
	if preview {
		token, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/_heron/preview/"), "/")
		if !ok || h.preview == nil {
			http.NotFound(w, r)
			return
		}
		id, digest, ok = h.preview(r.Context(), token)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rest == "" {
			ThemeShell(w, "/_heron/preview/"+token+"/files/index.html")
			return
		}
		rel, ok = strings.CutPrefix(rest, "files/")
		if !ok {
			http.NotFound(w, r)
			return
		}
	} else {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/_heron/themes/"), "/", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
			http.NotFound(w, r)
			return
		}
		id, digest, rel = parts[0], parts[1], parts[2]
	}
	if rel == "" {
		rel = "index.html"
	}
	if rel != path.Clean(rel) || hidden(rel) || strings.HasPrefix(rel, "/") {
		http.NotFound(w, r)
		return
	}
	p, err := h.snapshot(r.Context(), id, digest, rel)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.unavailable(w, r, err)
		return
	}
	if err != nil || theme.CheckExecutable(p.meta.SDK) != nil || (!preview && !p.meta.Published) {
		http.NotFound(w, r)
		return
	}
	// 不对管理 API 放行 CORS；这里只读不可变资源，opaque-origin 的模块和字体需要匿名 CORS。
	w.Header().Set("Access-Control-Allow-Origin", "*")
	typ := mime.TypeByExtension(path.Ext(rel))
	if typ == "" {
		typ = "application/octet-stream"
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("ETag", fmt.Sprintf(`"%s-%x"`, p.meta.Digest, sha256.Sum256([]byte(rel))))
	http.ServeContent(w, r, path.Base(rel), time.Time{}, bytes.NewReader(p.content))
}

func (h *themeHandler) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Error("reading theme failed", "err", err, "path", r.URL.Path)
	SandboxHeaders(w.Header())
	http.Error(w, "theme unavailable", http.StatusInternalServerError)
}
