package enterpriseservice

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed web/binding.html web/binding.js
var bindingPageFiles embed.FS

func (h *Handler) serveBindingApproval(w http.ResponseWriter, r *http.Request) bool {
	var file, contentType string
	if r.URL.Path == "/enterprise/assets/binding.js" {
		file = "web/binding.js"
		contentType = "text/javascript; charset=utf-8"
	} else {
		const prefix = "/enterprise/binding-requests/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			return false
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
		if len(parts) != 2 || !validID(parts[0]) || !validID(parts[1]) {
			return false
		}
		file = "web/binding.html"
		contentType = "text/html; charset=utf-8"
	}
	if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery {
		fail(w, http.StatusBadRequest, "invalid_binding_request")
		return true
	}
	body, err := bindingPageFiles.ReadFile(file)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "binding_unavailable")
		return true
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return true
}
