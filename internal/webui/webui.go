package webui

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/sean2077/pairroom/internal/version"
)

const Prefix = "/_pairroom/"

//go:embed assets/*
var assets embed.FS

func Handler() http.Handler {
	root, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(fmt.Errorf("open shared Web UI assets: %w", err))
	}
	return WithAssetETag(http.StripPrefix(Prefix, http.FileServer(http.FS(root))))
}

func Mount(mux *http.ServeMux) {
	mux.Handle(Prefix, Handler())
}

// WithAssetETag adds a version-keyed weak validator for embedded static
// assets. Embedded files carry no mtime, so http.FileServer answers every
// reload with a full 200; the asset set is fixed at build time, so a 304 for
// a matching build version is always correct. HTML documents are excluded so
// their existing no-store posture is untouched.
func WithAssetETag(next http.Handler) http.Handler {
	etag := `W/"pairroom-` + version.Current + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		staticAsset := (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
			path != "/" && !strings.HasSuffix(path, ".html")
		if !staticAsset {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		next.ServeHTTP(w, r)
	})
}
