package enterpriseservice

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed web/device.html web/device.js
var devicePageFiles embed.FS

func (h *Handler) serveDeviceApproval(w http.ResponseWriter, r *http.Request) bool {
	var file, contentType string
	if r.URL.Path == "/enterprise/assets/device.js" {
		file = "web/device.js"
		contentType = "text/javascript; charset=utf-8"
	} else {
		const prefix = "/enterprise/device-enrollments/"
		if !strings.HasPrefix(r.URL.Path, prefix) || !validID(strings.TrimPrefix(r.URL.Path, prefix)) {
			return false
		}
		file = "web/device.html"
		contentType = "text/html; charset=utf-8"
	}
	if r.Method != http.MethodGet || r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return true
	}
	body, err := devicePageFiles.ReadFile(file)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return true
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return true
}
