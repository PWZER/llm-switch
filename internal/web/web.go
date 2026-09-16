// Package web serves the embedded admin UI (built by `make frontend` into
// web/dist) with SPA fallback for client-side routes.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler returns the SPA handler. In -web-dev mode the caller skips mounting
// this so the browser hits the Vite dev server directly.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("web: embedded dist missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(sub, path); err != nil {
			// Unknown path without extension -> SPA client route.
			if !strings.Contains(path, ".") {
				r.URL.Path = "/"
			} else {
				http.NotFound(w, r)
				return
			}
		}
		// Hashed assets are immutable; index.html must always revalidate.
		if strings.HasPrefix(path, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}
