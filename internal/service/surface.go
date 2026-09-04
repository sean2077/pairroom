package service

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/websession"
)

const (
	roomSurfacePrefix     = "/api/v1/rooms/"
	roomSurfaceMarker     = "/surface"
	surfaceFrameAncestors = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'self'; form-action 'self'"
	maxSurfaceBodyBytes   = attachment.MaxImageBytes + (1 << 20)
)

var surfaceStaticFiles = map[string]struct{}{
	"/favicon.svg":              {},
	"/styles.css":               {},
	"/ux.css":                   {},
	"/app.js":                   {},
	"/ux.js":                    {},
	"/room-shell.js":            {},
	"/richtext.js":              {},
	"/_pairroom/i18next.min.js": {},
	"/_pairroom/catalogs.js":    {},
	"/_pairroom/i18n.js":        {},
	"/_pairroom/theme.js":       {},
}

func isRoomSurfacePath(p string) bool {
	if !strings.HasPrefix(p, roomSurfacePrefix) {
		return false
	}
	rest := strings.TrimPrefix(p, roomSurfacePrefix)
	_, after, ok := strings.Cut(rest, roomSurfaceMarker)
	if !ok {
		return false
	}
	return after == "" || strings.HasPrefix(after, "/")
}

func splitRoomSurfacePath(p string) (roomID, remainder string, ok bool) {
	if !strings.HasPrefix(p, roomSurfacePrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, roomSurfacePrefix)
	roomID, after, found := strings.Cut(rest, roomSurfaceMarker)
	if !found || roomID == "" || strings.Contains(roomID, "/") {
		return "", "", false
	}
	switch after {
	case "", "/":
		return roomID, "/", true
	}
	if !strings.HasPrefix(after, "/") {
		return "", "", false
	}
	cleaned := path.Clean(after)
	if cleaned != "/" && !strings.HasPrefix(cleaned, "/") {
		return "", "", false
	}
	if strings.Contains(cleaned, "\\") {
		return "", "", false
	}
	return roomID, cleaned, true
}

func allowedSurfaceRequest(method, p string) bool {
	method = strings.ToUpper(method)
	switch p {
	case "/", "/index.html":
		return method == http.MethodGet || method == http.MethodHead
	case "/api/v1/session":
		return method == http.MethodGet || method == http.MethodHead || method == http.MethodPost || method == http.MethodDelete
	case "/api/v1/health", "/api/v1/snapshot", "/api/v1/events", "/api/v1/export", "/api/v1/git/status", "/api/v1/git/diff":
		return method == http.MethodGet || method == http.MethodHead
	case "/api/v1/messages":
		return method == http.MethodGet || method == http.MethodHead || method == http.MethodPost
	case "/api/v1/attachments":
		return method == http.MethodPost
	case "/api/v1/settings":
		return method == http.MethodPut
	}
	if _, ok := surfaceStaticFiles[p]; ok {
		return method == http.MethodGet || method == http.MethodHead
	}
	switch {
	case strings.HasPrefix(p, "/api/v1/attachments/"):
		return method == http.MethodGet || method == http.MethodHead || method == http.MethodDelete
	case strings.HasPrefix(p, "/api/v1/messages/") && strings.HasSuffix(p, "/retry"):
		return method == http.MethodPost
	case strings.HasPrefix(p, "/api/v1/messages/") && strings.HasSuffix(p, "/cancel"):
		return method == http.MethodPost
	case strings.HasPrefix(p, "/api/v1/participants/") && strings.HasSuffix(p, "/role"):
		return method == http.MethodPut
	case strings.HasPrefix(p, "/api/v1/participants/"):
		return method == http.MethodPost
	case strings.HasPrefix(p, "/api/v1/approvals/"):
		return method == http.MethodPost
	default:
		return false
	}
}

func applySurfaceFrameHeaders(header http.Header) {
	header.Del("X-Frame-Options")
	header.Del("Content-Security-Policy")
	header.Set("X-Frame-Options", "SAMEORIGIN")
	header.Set("Content-Security-Policy", surfaceFrameAncestors)
}

func (s *ManagementServer) roomSurface(w http.ResponseWriter, r *http.Request) {
	roomID, remainder, ok := splitRoomSurfacePath(r.URL.Path)
	if !ok {
		writeManagementError(w, http.StatusNotFound, "room surface path is invalid")
		return
	}
	if !allowedSurfaceRequest(r.Method, remainder) {
		writeManagementError(w, http.StatusNotFound, "room surface path is not allowed")
		return
	}
	room, exists := s.registry.Room(roomID)
	if !exists {
		s.writeError(w, ErrRoomNotFound)
		return
	}
	if room.Archived() {
		writeManagementJSON(w, http.StatusConflict, map[string]any{
			"error": "archived rooms cannot be opened until they are restored",
			"code":  "room_archived",
		})
		return
	}
	if remainder == "/api/v1/session" {
		s.surfaceSession(w, r)
		return
	}

	base, token, err := s.runtimes.ProxyTarget(roomID)
	if err != nil {
		s.writeSurfaceUnavailable(w, roomID, err)
		return
	}
	target, err := parseLoopbackHTTPBase(base)
	if err != nil {
		writeManagementError(w, http.StatusBadGateway, "room runtime endpoint is invalid")
		return
	}

	if r.Body != nil && r.ContentLength != 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxSurfaceBodyBytes)
	}

	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL = target.ResolveReference(&url.URL{Path: remainder, RawQuery: pr.In.URL.RawQuery, Fragment: ""})
			out.Host = target.Host
			out.Header.Del("Cookie")
			out.Header.Del("Origin")
			out.Header.Del("Referer")
			out.Header.Del("Authorization")
			out.Header.Del(websession.CSRFHeaderName)
			out.Header.Set("Authorization", "Bearer "+token)
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			applySurfaceFrameHeaders(resp.Header)
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, _ error) {
			writeManagementError(rw, http.StatusBadGateway, "room surface proxy failed")
		},
	}
	applySurfaceFrameHeaders(w.Header())
	proxy.ServeHTTP(w, r)
}

func (s *ManagementServer) surfaceSession(w http.ResponseWriter, r *http.Request) {
	applySurfaceFrameHeaders(w.Header())
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost:
		s.readBrowserSession(w, r)
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *ManagementServer) writeSurfaceUnavailable(w http.ResponseWriter, roomID string, err error) {
	status := s.runtimes.Status(roomID)
	code := http.StatusConflict
	message := err.Error()
	apiCode := "runtime_not_ready"
	switch {
	case strings.Contains(message, "numeric loopback") || strings.Contains(message, "must be http"):
		code = http.StatusBadGateway
		apiCode = "runtime_endpoint_invalid"
	}
	writeManagementJSON(w, code, map[string]any{
		"error": message,
		"code":  apiCode,
		"phase": status.Phase,
	})
}
