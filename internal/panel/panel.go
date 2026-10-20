// Package panel is the operator console: static HTML, CSS and JavaScript
// embedded in the binary and served as a single-page app. It has no build
// step on purpose: what is in ui/ is what the browser gets, so a fix is a
// commit and a restart, and a contributor needs Go and nothing else.
//
// The panel talks to /api/v1 (docs/api.md) and nothing here knows about
// sites, manifests or the engine; it is a file server with the headers a
// console on a public domain must send.
package panel

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
)

//go:embed ui
var ui embed.FS

// AssetPrefix is where the embedded files answer. Everything else is the
// app shell, so a deep link (/sites/example.com) that someone pastes into a
// browser lands on index.html and the hash router does the rest.
const AssetPrefix = "/assets/"

// CSP is the policy every panel response carries. 'self' only, no inline:
// the whole UI is written so nothing needs 'unsafe-inline', and keeping it
// that way is what makes a stored string from a site name or a log line
// unable to run as script. connect-src covers fetch and EventSource; data:
// in img-src is for icons drawn as data URIs.
const CSP = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// FS is the embedded ui/ directory, exported for tests and for anyone who
// needs to read the catalog or the shell without serving it.
var FS fs.FS

func init() {
	sub, err := fs.Sub(ui, "ui")
	if err != nil {
		panic(err) // the directory is part of the binary; this cannot happen
	}
	FS = sub
}

// Handler serves the panel. GET and HEAD only. index.html answers every
// path outside AssetPrefix with Cache-Control: no-cache; assets answer with
// an ETag derived from their content, so a browser revalidates for free
// and never keeps a stale app.js across an upgrade. There is no build step
// that could put a hash in the file name, so the hash goes in the ETag.
func Handler() http.Handler {
	return &handler{etags: map[string]string{}}
}

type handler struct {
	mu    sync.Mutex
	etags map[string]string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Security-Policy", CSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")

	// The worker and the manifest answer at the root: a worker controls
	// only the paths under its own, and one at /assets/sw.js would control
	// /assets/ and none of the panel.
	switch r.URL.Path {
	case "/sw.js":
		w.Header().Set("Service-Worker-Allowed", "/")
		h.serve(w, r, "sw.js")
		return
	case "/manifest.webmanifest":
		h.serve(w, r, "manifest.webmanifest")
		return
	}
	if strings.HasPrefix(r.URL.Path, AssetPrefix) {
		name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, AssetPrefix)), "/")
		// index.html is the shell, not an asset: a module request that got
		// HTML back shows a syntax error with no clue in the console.
		if name == "" || name == "." || name == "index.html" {
			http.NotFound(w, r)
			return
		}
		h.serve(w, r, name)
		return
	}
	h.serve(w, r, "index.html")
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request, name string) {
	data, err := fs.ReadFile(FS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	tag := h.etag(name, data)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, tag) {
		w.Header().Set("ETag", tag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// Go's table knows these, but a stripped container may ship a bare
	// /etc/mime.types, and a module script served as text/plain is refused
	// by the browser. The common ones are pinned; the rest ask the table.
	var ct string
	switch path.Ext(name) {
	case ".js", ".mjs":
		ct = "text/javascript; charset=utf-8"
	case ".css":
		ct = "text/css; charset=utf-8"
	case ".json":
		ct = "application/json"
	case ".html":
		ct = "text/html; charset=utf-8"
	case ".svg":
		ct = "image/svg+xml"
	case ".webmanifest":
		ct = "application/manifest+json"
	case ".png":
		ct = "image/png"
	default:
		ct = mime.TypeByExtension(path.Ext(name))
		if ct == "" {
			ct = http.DetectContentType(data)
		}
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(data)
}

func (h *handler) etag(name string, data []byte) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if tag, ok := h.etags[name]; ok {
		return tag
	}
	sum := sha256.Sum256(data)
	tag := `"` + hex.EncodeToString(sum[:8]) + `"`
	h.etags[name] = tag
	return tag
}
