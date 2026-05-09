//go:build !dev

// Package webui serves the embedded LynxDB Web UI as a single-page application.
// In production builds, static assets are compiled into the binary via embed.FS.
// In dev builds (go build -tags dev), requests are proxied to a Vite dev server.
package webui

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

const Path = "/ui"

//go:embed all:dist
var distFS embed.FS

// Enabled reports whether embedded UI assets are available.
// Returns false when dist/ contains only the .gitkeep placeholder.
func Enabled() bool {
	entries, err := fs.ReadDir(distFS, "dist")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() != ".gitkeep" {
			return true
		}
	}
	return false
}

// Handler returns an http.Handler that serves the embedded SPA under Path.
// Static assets under /assets/ are served with immutable cache headers.
// All other paths fall back to index.html for client-side routing.
func Handler() http.Handler {
	sub, _ := fs.Sub(distFS, "dist")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, Path)
		if path == "" || path == "/" {
			path = "index.html"
		} else {
			path = strings.TrimPrefix(path, "/")
		}

		if f, err := sub.Open(path); err == nil {
			if strings.HasPrefix(path, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			serveFSFile(w, r, f)
			return
		}

		// SPA fallback: serve index.html for unmatched routes.
		f, err := sub.Open("index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		serveFSFile(w, r, f)
	})
}

func serveFSFile(w http.ResponseWriter, r *http.Request, f fs.File) {
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "web UI asset is not seekable", http.StatusInternalServerError)
		return
	}

	http.ServeContent(w, r, info.Name(), info.ModTime(), rs)
}
