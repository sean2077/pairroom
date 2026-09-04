package webui

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

const Prefix = "/_pairroom/"

//go:embed assets/*
var assets embed.FS

func Handler() http.Handler {
	root, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(fmt.Errorf("open shared Web UI assets: %w", err))
	}
	return http.StripPrefix(Prefix, http.FileServer(http.FS(root)))
}

func Mount(mux *http.ServeMux) {
	mux.Handle(Prefix, Handler())
}
