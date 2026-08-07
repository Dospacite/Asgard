package frontend

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed dist
var assets embed.FS

func Handler() http.Handler {
	dist, _ := fs.Sub(assets, "dist")
	index, _ := fs.ReadFile(dist, "index.html")
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "." {
			clean = "index.html"
		}
		if _, err := fs.Stat(dist, clean); err != nil || strings.HasPrefix(clean, "api/") || clean == "mcp" {
			clean = "index.html"
		}
		if ext := path.Ext(clean); ext != "" {
			if contentType := mime.TypeByExtension(ext); contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
		}
		if clean == "index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(index)
			return
		}
		clone := r.Clone(r.Context())
		clone.URL.Path = "/" + clean
		files.ServeHTTP(w, clone)
	})
}
