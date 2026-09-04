package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/execx"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/room"
	"github.com/sean2077/pairroom/internal/version"
	"github.com/sean2077/pairroom/internal/websession"
	"github.com/sean2077/pairroom/internal/webui"
)

//go:embed assets/*
var embeddedAssets embed.FS

type Config struct {
	Engine                   *room.Engine
	Repo                     string
	Token                    string
	Attachments              *attachment.Store
	TranscriptBoundaryNotice string
	// SessionCookieName isolates browser sessions when multiple Room View
	// servers share a host on different ports. Browsers do not scope cookies by
	// port, so Service runtimes must provide a stable per-Room name.
	SessionCookieName string
}

type Server struct {
	engine   *room.Engine
	repo     string
	token    string
	media    *attachment.Store
	boundary string
	sessions *websession.Store
	limiter  *rateLimiter
	http     *http.Server
}

func New(cfg Config) (*Server, error) {
	if cfg.Engine == nil {
		return nil, errors.New("room engine is required")
	}
	assets, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		return nil, fmt.Errorf("open embedded assets: %w", err)
	}
	cookieName := strings.TrimSpace(cfg.SessionCookieName)
	if cookieName == "" {
		cookieName = browserSessionCookie
	}
	sessions, err := websession.New(cookieName)
	if err != nil {
		return nil, err
	}
	s := &Server{
		engine: cfg.Engine, repo: cfg.Repo, token: cfg.Token, media: cfg.Attachments,
		boundary: cfg.TranscriptBoundaryNotice,
		sessions: sessions, limiter: newRateLimiter(),
	}
	mux := http.NewServeMux()
	webui.Mount(mux)
	mux.HandleFunc("POST /api/v1/session", s.createBrowserSession)
	mux.HandleFunc("GET /api/v1/session", s.readBrowserSession)
	mux.HandleFunc("DELETE /api/v1/session", s.deleteBrowserSession)
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/snapshot", s.snapshot)
	mux.HandleFunc("GET /api/v1/messages", s.messages)
	mux.HandleFunc("GET /api/v1/events", s.events)
	mux.HandleFunc("POST /api/v1/messages", s.sendMessage)
	mux.HandleFunc("POST /api/v1/attachments", s.uploadAttachment)
	mux.HandleFunc("GET /api/v1/attachments/{id}", s.serveAttachment)
	mux.HandleFunc("DELETE /api/v1/attachments/{id}", s.deleteAttachment)
	mux.HandleFunc("POST /api/v1/messages/{id}/retry", s.retryMessage)
	mux.HandleFunc("POST /api/v1/messages/{id}/cancel", s.cancelMessage)
	mux.HandleFunc("GET /api/v1/export", s.exportRoom)
	mux.HandleFunc("PUT /api/v1/settings", s.updateSettings)
	mux.HandleFunc("POST /api/v1/participants/{actor}/{action}", s.participantAction)
	mux.HandleFunc("PUT /api/v1/participants/{actor}/role", s.participantRole)
	mux.HandleFunc("POST /api/v1/approvals/{id}", s.resolveApproval)
	mux.HandleFunc("GET /api/v1/git/status", s.gitStatus)
	mux.HandleFunc("GET /api/v1/git/diff", s.gitDiff)
	mux.Handle("/", http.FileServer(http.FS(assets)))

	s.http = &http.Server{
		Handler:           s.securityHeaders(s.sameOrigin(s.rateLimit(s.authenticate(s.csrf(mux))))),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.http.Handler }

func (s *Server) Serve(listenerAddr string) error {
	s.http.Addr = listenerAddr
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) createBrowserSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.token == "" {
		writeJSON(w, http.StatusOK, map[string]any{"required": false})
		return
	}
	auth := authFromContext(r.Context())
	if auth.Mode != authBearer {
		writeError(w, http.StatusForbidden, "a bearer bootstrap token is required")
		return
	}
	value, err := s.sessions.Create(w, r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create browser session: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"required": true, "csrf_token": value.CSRFToken,
		"created_at": value.CreatedAt, "expires_at": value.ExpiresAt,
	})
}

func (s *Server) readBrowserSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.token == "" {
		writeJSON(w, http.StatusOK, map[string]any{"required": false})
		return
	}
	auth := authFromContext(r.Context())
	if auth.Mode != authBrowserSession {
		writeJSON(w, http.StatusOK, map[string]any{"required": true, "mode": "bearer"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"required": true, "csrf_token": auth.Session.CSRFToken,
		"created_at": auth.Session.CreatedAt, "expires_at": auth.Session.ExpiresAt,
	})
}

func (s *Server) deleteBrowserSession(w http.ResponseWriter, r *http.Request) {
	s.sessions.Delete(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"version":    version.Describe(),
		"commit":     version.Commit,
		"build_date": version.BuildDate,
		"time":       time.Now().UTC(),
	})
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("message_limit"))
	var snapshot model.RoomSnapshot
	if limit > 0 {
		snapshot = s.engine.WindowedSnapshot(limit)
	} else {
		snapshot = s.engine.Snapshot()
	}
	if strings.TrimSpace(s.boundary) != "" {
		event, err := model.NewEvent(snapshot.Meta.ID, room.EventSystemNotice, model.ActorSystem, model.SystemNotice{
			Level: "info", Text: s.boundary,
		})
		if err == nil {
			// This presentation-only notice is intentionally outside the durable
			// Room sequence so Existing bindings and legacy imports remain
			// non-destructive. The Room Event Log still starts at the binding
			// boundary and never absorbs earlier vendor transcript content.
			event.Seq = 0
			snapshot.Events = append([]model.Event{event}, snapshot.Events...)
		}
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	before, err := strconv.ParseUint(r.URL.Query().Get("before_seq"), 10, 64)
	if err != nil && r.URL.Query().Get("before_seq") != "" {
		writeError(w, http.StatusBadRequest, "invalid before_seq")
		return
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil && r.URL.Query().Get("limit") != "" {
		writeError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	writeJSON(w, http.StatusOK, s.engine.MessagesPage(before, limit))
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	ch, cancel := s.engine.Subscribe()
	defer cancel()
	// Subscribe before snapshotting so events emitted during handoff are either
	// replayed from the snapshot tail or still buffered in ch. Sequence numbers
	// remove duplicates.
	snapshot := s.engine.Snapshot()
	last := since
	for _, event := range snapshot.Events {
		write, next := advanceSSECursor(event, last)
		if !write {
			continue
		}
		if err := writeSSE(w, event); err != nil {
			return
		}
		last = next
	}
	flusher.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": heartbeat\n\n")
			flusher.Flush()
		case event, ok := <-ch:
			if !ok {
				return
			}
			write, next := advanceSSECursor(event, last)
			if !write {
				continue
			}
			if err := writeSSE(w, event); err != nil {
				return
			}
			last = next
			flusher.Flush()
		}
	}
}

// advanceSSECursor keeps transient sequence-zero events live without letting
// them move the durable replay cursor backwards. Durable duplicates are still
// filtered across the snapshot-to-subscription handoff.
func advanceSSECursor(event model.Event, last uint64) (write bool, next uint64) {
	if event.Seq == 0 {
		return true, last
	}
	if event.Seq <= last {
		return false, last
	}
	return true, event.Seq
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var request room.SendRequest
	if err := decodeJSON(w, r, &request); err != nil {
		return
	}
	message, err := s.engine.Send(r.Context(), request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, message)
}

func (s *Server) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		writeError(w, http.StatusServiceUnavailable, "attachment storage is unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, attachment.MaxImageBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid image upload: "+err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "multipart field 'file' is required")
		return
	}
	defer file.Close()
	value, err := s.media.SaveImage(header.Filename, file, "user-upload")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, value)
}

func (s *Server) serveAttachment(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		writeError(w, http.StatusServiceUnavailable, "attachment storage is unavailable")
		return
	}
	value, file, err := s.media.OpenFile(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stat attachment: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", value.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": value.Name}))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	etag := `"sha256-` + value.SHA256 + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	http.ServeContent(w, r, value.Name, info.ModTime(), file)
}

func (s *Server) deleteAttachment(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		writeError(w, http.StatusServiceUnavailable, "attachment storage is unavailable")
		return
	}
	id := r.PathValue("id")
	if s.engine.AttachmentReferenced(id) {
		writeError(w, http.StatusConflict, "attachment is already part of the durable room transcript")
		return
	}
	if err := s.media.Remove(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) exportRoom(w http.ResponseWriter, r *http.Request) {
	snapshot := s.engine.Snapshot()
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "markdown"
	}
	base := safeFilename(snapshot.Meta.Name)
	if base == "" {
		base = "pairroom"
	}
	switch format {
	case "json":
		// A normal transcript export intentionally omits the bounded Inspector
		// event tail. Those events can contain verbose command output and vendor
		// diagnostics that are not part of the human conversation. Operators can
		// explicitly request a forensic export when they need that data.
		if r.URL.Query().Get("include_events") != "1" {
			snapshot.Events = nil
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, base))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(snapshot)
	case "md", "markdown":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.md"`, base))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, renderMarkdownTranscript(snapshot))
	default:
		writeError(w, http.StatusBadRequest, "export format must be markdown or json")
	}
}

func renderMarkdownTranscript(snapshot model.RoomSnapshot) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", snapshot.Meta.Name)
	fmt.Fprintf(&out, "- Repository: `%s`\n", strings.ReplaceAll(snapshot.Meta.Repo, "`", "'"))
	fmt.Fprintf(&out, "- Room ID: `%s`\n", snapshot.Meta.ID)
	fmt.Fprintf(&out, "- Created: %s\n", snapshot.Meta.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(&out, "- Exported: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&out, "- Turn policy: `%s`, max turns: %d\n\n", snapshot.Settings.RoutingMode, snapshot.Settings.MaxHops)

	out.WriteString("## Participants\n\n")
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		p := snapshot.Participants[actor]
		fmt.Fprintf(&out, "- **%s** — role `%s`, state `%s`", p.DisplayName, p.Role, p.State)
		if p.Runtime.Version != "" {
			fmt.Fprintf(&out, ", runtime `%s`", p.Runtime.Version)
		}
		if p.Model != "" {
			fmt.Fprintf(&out, ", model `%s`", p.Model)
		}
		out.WriteString("\n")
	}

	out.WriteString("\n## Conversation\n\n")
	for _, message := range snapshot.Messages {
		fmt.Fprintf(&out, "### %s · %s\n\n", message.From.DisplayName(), message.CreatedAt.Format(time.RFC3339))
		if message.RetryOf != "" {
			fmt.Fprintf(&out, "_Retry of `%s`_\n\n", message.RetryOf)
		}
		for _, line := range strings.Split(strings.ReplaceAll(message.Text, "\r\n", "\n"), "\n") {
			fmt.Fprintf(&out, "> %s\n", line)
		}
		if len(message.Attachments) > 0 {
			out.WriteString("\nAttachments:\n")
			for _, value := range message.Attachments {
				fmt.Fprintf(&out, "- `%s` — %s, %d bytes, `%s`\n", strings.ReplaceAll(value.Name, "`", "'"), value.MediaType, value.Size, value.ID)
			}
		}
		out.WriteString("\n")
		for _, target := range message.To {
			if !target.ValidParticipant() {
				continue
			}
			fmt.Fprintf(&out, "- %s: delivery `%s`, processing `%s`", target.DisplayName(), message.Delivery[target], message.Processing[target])
			if detail := message.ProcessingDetail[target]; detail != "" {
				fmt.Fprintf(&out, " — %s", strings.ReplaceAll(detail, "\n", " "))
			}
			out.WriteString("\n")
		}
		out.WriteString("\n")
	}
	return out.String()
}

func safeFilename(value string) string {
	value = strings.TrimSpace(value)
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out.WriteRune(r)
		case r == ' ' || r == '×':
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}

func (s *Server) retryMessage(w http.ResponseWriter, r *http.Request) {
	var request room.RetryRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &request); err != nil {
			return
		}
	}
	message, err := s.engine.Retry(r.Context(), r.PathValue("id"), request)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, message)
}

func (s *Server) cancelMessage(w http.ResponseWriter, r *http.Request) {
	var request room.CancelRequest
	if err := decodeJSON(w, r, &request); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.engine.CancelMessage(ctx, r.PathValue("id"), request.Target); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var settings model.RoomSettings
	if err := decodeJSON(w, r, &settings); err != nil {
		return
	}
	if err := s.engine.UpdateSettings(settings); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) participantAction(w http.ResponseWriter, r *http.Request) {
	actor := model.ActorID(strings.ToLower(r.PathValue("actor")))
	if !actor.ValidParticipant() {
		writeError(w, http.StatusBadRequest, "participant must be claude or codex")
		return
	}
	action := strings.ToLower(r.PathValue("action"))
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	var err error
	switch action {
	case "start":
		err = s.engine.StartAgent(ctx, actor)
	case "stop":
		err = s.engine.StopAgent(ctx, actor)
	case "restart":
		err = s.engine.RestartAgent(ctx, actor)
	case "interrupt":
		err = s.engine.Interrupt(ctx, actor)
	default:
		writeError(w, http.StatusNotFound, "unknown participant action")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "actor": actor, "action": action})
}

func (s *Server) participantRole(w http.ResponseWriter, r *http.Request) {
	actor := model.ActorID(strings.ToLower(r.PathValue("actor")))
	var request struct {
		Role model.ParticipantRole `json:"role"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	var err error
	if request.Role == model.RoleDriver {
		err = s.engine.SwitchDriver(ctx, actor)
	} else {
		err = s.engine.SetRole(ctx, actor, request.Role)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request) {
	var request model.ApprovalResolution
	if err := decodeJSON(w, r, &request); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.engine.ResolveApproval(ctx, r.PathValue("id"), request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) gitStatus(w http.ResponseWriter, r *http.Request) {
	output, err := s.runGit(r.Context(), "status", "--short", "--branch")
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": output})
}

func (s *Server) gitDiff(w http.ResponseWriter, r *http.Request) {
	args := []string{"diff", "--no-ext-diff", "--src-prefix=a/", "--dst-prefix=b/"}
	if r.URL.Query().Get("staged") == "1" {
		args = append(args, "--staged")
	}
	output, err := s.runGit(r.Context(), args...)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": output})
}

func (s *Server) runGit(parent context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	execx.NoConsole(cmd)
	cmd.Dir = s.repo
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if s.token == "" {
			next.ServeHTTP(w, withAuth(r, requestAuth{Mode: authNone}))
			return
		}

		header := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(header, "Bearer ") {
			candidate := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
			if constantTimeEqual(candidate, s.token) {
				next.ServeHTTP(w, withAuth(r, requestAuth{Mode: authBearer}))
				return
			}
		}
		if value, ok := s.sessions.Get(w, r); ok {
			next.ServeHTTP(w, withAuth(r, requestAuth{Mode: authBrowserSession, Session: value}))
			return
		}
		writeError(w, http.StatusUnauthorized, "PairRoom authentication is required")
	})
}

func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		auth := authFromContext(r.Context())
		if auth.Mode != authBrowserSession {
			next.ServeHTTP(w, r)
			return
		}
		if !auth.Session.ValidCSRF(r.Header.Get(csrfHeaderName)) {
			writeError(w, http.StatusForbidden, "missing or invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		allowed, retryAfter := s.limiter.allow(requestClientKey(r))
		if !allowed {
			seconds := int(retryAfter.Round(time.Second).Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeError(w, http.StatusTooManyRequests, "PairRoom API rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A tokenless PairRoom is deliberately local-only. Checking the Host header
		// closes the DNS-rebinding path where a malicious website resolves its own
		// hostname to 127.0.0.1 and then issues same-origin requests to the daemon.
		if s.token == "" && !isLoopbackRequestHost(r.Host) {
			writeError(w, http.StatusForbidden, "tokenless PairRoom accepts only loopback hosts")
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(parsed.Host, r.Host) {
				writeError(w, http.StatusForbidden, "cross-origin request rejected")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackRequestHost(value string) bool {
	host := strings.TrimSpace(value)
	if host == "" {
		return false
	}
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data: blob:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request must contain one JSON value")
		return errors.New("trailing JSON data")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	code := "request_failed"
	switch status {
	case http.StatusBadRequest:
		code = "invalid_request"
	case http.StatusUnauthorized:
		code = "authentication_required"
	case http.StatusForbidden:
		code = "request_forbidden"
	case http.StatusNotFound:
		code = "not_found"
	case http.StatusConflict:
		code = "request_conflict"
	case http.StatusRequestEntityTooLarge:
		code = "request_too_large"
	case http.StatusTooManyRequests:
		code = "rate_limited"
	case http.StatusInternalServerError:
		code = "internal_error"
	}
	writeJSON(w, status, map[string]any{"error": message, "code": code})
}

func writeSSE(w io.Writer, event model.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if event.Seq > 0 {
		_, err = fmt.Fprintf(w, "id: %d\nevent: pairroom\ndata: %s\n\n", event.Seq, data)
	} else {
		_, err = fmt.Fprintf(w, "event: pairroom\ndata: %s\n\n", data)
	}
	return err
}
