package enterpriseservice

import (
	"embed"
	"net/http"
)

//go:embed web/index.html web/app.js web/style.css
var landingFiles embed.FS

func (h *Handler) serveLanding(w http.ResponseWriter, r *http.Request) bool {
	var file, contentType string
	switch r.URL.Path {
	case "/enterprise/":
		file = "web/index.html"
		contentType = "text/html; charset=utf-8"
	case "/enterprise/assets/app.js":
		file = "web/app.js"
		contentType = "text/javascript; charset=utf-8"
	case "/enterprise/assets/style.css":
		file = "web/style.css"
		contentType = "text/css; charset=utf-8"
	default:
		return false
	}
	body, err := landingFiles.ReadFile(file)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "page_unavailable")
		return true
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return true
}
