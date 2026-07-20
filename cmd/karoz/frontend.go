package main

import (
	"embed"
	"log"
	"net/http"
	"strings"
)

//go:embed static
var staticFS embed.FS

// staticAssets serves the embedded css/js files. FileServerFS cleans the URL
// path before opening it, so traversal attempts like /static/../main.go
// resolve inside the embedded FS and 404 instead of escaping it.
var staticAssets = http.FileServerFS(staticFS)

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/":
		data, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			log.Printf("read index: %v", err)
			http.Error(w, "internal error", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	case strings.HasPrefix(r.URL.Path, "/static/"):
		staticAssets.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}
