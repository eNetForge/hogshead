// Package ui embeds the built single-page web UI and serves it under /_ui/.
//
// The dist directory is produced by `npm run build` in the ui/ directory. A
// placeholder index.html is committed so that `go build` never depends on
// node; the placeholder simply tells the operator how to build the UI.
package ui

import (
	"embed"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

const (
	// Prefix is the URL prefix the UI is mounted under.
	Prefix = "/_ui"
	// appDir holds everything Vite emits; a miss there is a real 404, not a
	// client route, so stale asset references never receive index.html.
	appDir = "/_app/"
	// immutableDir holds Vite's content-hashed assets.
	immutableDir = appDir + "immutable/"

	cacheImmutable = "public, max-age=31536000, immutable"
	cacheNoStore   = "no-store"
	cacheDefault   = "public, max-age=3600"
)

// Handler serves the SPA. Requests for existing files are served from the
// embedded dist; everything else falls back to index.html so that client-side
// routes work on reload. Hashed assets under /_app/immutable/ get a long
// cache lifetime; index.html is never cached.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("ui: dist directory missing from embed: " + err.Error())
	}
	return &handler{fs: sub}
}

type handler struct {
	fs fs.FS
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := cleanPath(r.URL.Path)
	if p == "/" || p == "/index.html" {
		h.serveIndex(w, r)
		return
	}
	if h.serveFile(w, r, p) {
		return
	}
	if strings.HasPrefix(p, appDir) {
		http.NotFound(w, r)
		return
	}
	h.serveIndex(w, r)
}

// cleanPath strips the mount prefix and normalises the path to a rooted,
// dot-dot free form.
func cleanPath(p string) string {
	p = strings.TrimPrefix(p, Prefix)
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	return path.Clean(p)
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := h.fs.Open("index.html")
	if err != nil {
		http.Error(w, "ui not built", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "ui unreadable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", cacheNoStore)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader(string(data)))
}

// serveFile serves p from the embedded fs and reports whether it did.
func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, p string) bool {
	name := strings.TrimPrefix(p, "/")
	f, err := h.fs.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return false
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		return false
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if strings.HasPrefix(p, immutableDir) {
		w.Header().Set("Cache-Control", cacheImmutable)
	} else {
		w.Header().Set("Cache-Control", cacheDefault)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, time.Time{}, rs)
	return true
}
