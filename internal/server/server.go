package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/room"
)

//go:embed assets/*
var embeddedAssets embed.FS

type Config struct {
	Engine *room.Engine
	Repo   string
	Token  string
}

type Server struct {
	engine *room.Engine
	repo   string
	token  string
	http   *http.Server
}

func New(cfg Config) (*Server, error) {
	if cfg.Engine == nil {
		return nil, errors.New("room engine is required")
	}
	assets, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		return nil, fmt.Errorf("open embedded assets: %w", err)
	}
	s := &Server{engine: cfg.Engine, repo: cfg.Repo, token: cfg.Token}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/snapshot", s.snapshot)
	mux.HandleFunc("GET /api/v1/events", s.events)
	mux.HandleFunc("POST /api/v1/messages", s.sendMessage)
	mux.HandleFunc("PUT /api/v1/settings", s.updateSettings)
	mux.HandleFunc("POST /api/v1/participants/{actor}/{action}", s.participantAction)
	mux.HandleFunc("PUT /api/v1/participants/{actor}/role", s.participantRole)
	mux.HandleFunc("POST /api/v1/approvals/{id}", s.resolveApproval)
	mux.HandleFunc("GET /api/v1/git/status", s.gitStatus)
	mux.HandleFunc("GET /api/v1/git/diff", s.gitDiff)
	mux.Handle("/", http.FileServer(http.FS(assets)))

	s.http = &http.Server{
		Handler:           s.securityHeaders(s.authenticate(s.sameOrigin(mux))),
		ReadHeaderTimeout: 10 * time.Second,
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

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": "0.1.0",
		"time":    time.Now().UTC(),
	})
}

func (s *Server) snapshot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Snapshot())
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
		if event.Seq <= last {
			continue
		}
		if err := writeSSE(w, event); err != nil {
			return
		}
		last = event.Seq
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
			if event.Seq <= last {
				continue
			}
			if err := writeSSE(w, event); err != nil {
				return
			}
			last = event.Seq
			flusher.Flush()
		}
	}
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
	var err error
	if request.Role == model.RoleDriver {
		err = s.engine.SwitchDriver(actor)
	} else {
		err = s.engine.SetRole(actor, request.Role)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Decision string `json:"decision"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.engine.ResolveApproval(ctx, r.PathValue("id"), request.Decision); err != nil {
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
	if s.token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token != s.token {
			writeError(w, http.StatusUnauthorized, "invalid PairRoom token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'")
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
	writeJSON(w, status, map[string]any{"error": message})
}

func writeSSE(w io.Writer, event model.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: pairroom\ndata: %s\n\n", event.Seq, data); err != nil {
		return err
	}
	return nil
}
