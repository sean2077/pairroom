package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
	"github.com/sean2077/pairroom/internal/websession"
	"github.com/sean2077/pairroom/internal/webui"
)

type nativeHostRuntime struct {
	room      Room
	project   Project
	engine    *relay.Engine
	media     *attachment.Store
	token     string
	baseURL   string
	sessions  *websession.Store
	http      *http.Server
	cancel    context.CancelFunc
	done      chan struct{}
	active    atomic.Int64
	last      atomic.Int64
	closeOnce sync.Once
	closeErr  error
}

func startNativeHostRuntime(ctx context.Context, registry *Registry, project Project, durable Room, host string) (_ RoomRuntime, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if durable.HostMode != model.HostNative || durable.Archived() {
		return nil, errors.New("native runtime requires an active native Room")
	}
	if err := durable.Validate(); err != nil {
		return nil, err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("native Room listener must use numeric loopback")
	}
	log, err := store.OpenExistingForRoom(durable.DataDir, durable.ID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			_ = log.Close()
		}
	}()
	media, err := attachment.Open(durable.DataDir, project.Root)
	if err != nil {
		return nil, err
	}
	kinds := map[model.ActorID]model.RuntimeKind{}
	for actor, selection := range durable.Agents {
		kinds[actor] = selection.Runtime
	}
	engine, err := relay.Open(relay.Config{RoomID: durable.ID, Store: log, Runtimes: kinds, Media: media, CommitBinding: func(b relay.Binding, appendFact func() error) error {
		return registry.commitNativeBinding(durable.ID, b, appendFact)
	}})
	if err != nil {
		return nil, err
	}
	token, err := randomServiceToken()
	if err != nil {
		return nil, err
	}
	sessions, err := websession.New("pairroom_native_" + relay.Digest(durable.ID)[:16])
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	n := &nativeHostRuntime{room: durable, project: project, engine: engine, media: media, token: token, baseURL: "http://" + listener.Addr().String(), sessions: sessions, cancel: cancel, done: make(chan struct{})}
	mux := http.NewServeMux()
	webui.Mount(mux)
	mux.HandleFunc("/", n.serve)
	n.http = &http.Server{BaseContext: func(net.Listener) context.Context { return runCtx }, Handler: n.boundary(mux), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() { _ = n.http.Serve(listener) }()
	go func() {
		defer close(n.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_ = engine.Reap()
			}
		}
	}()
	n.last.Store(time.Now().UnixNano())
	return n, nil
}
func (n *nativeHostRuntime) URL() string             { return n.baseURL + "/#token=" + url.QueryEscape(n.token) }
func (n *nativeHostRuntime) ProxyBaseURL() string    { return n.baseURL }
func (n *nativeHostRuntime) ProxyToken() string      { return n.token }
func (n *nativeHostRuntime) Busy() bool              { return n.engine.Busy() }
func (n *nativeHostRuntime) InUse() bool             { return n.active.Load() > 0 }
func (n *nativeHostRuntime) LastActivity() time.Time { return time.Unix(0, n.last.Load()) }
func (n *nativeHostRuntime) SetDraining(v bool)      { n.engine.SetDraining(v) }
func (n *nativeHostRuntime) acquire() func() {
	n.active.Add(1)
	n.last.Store(time.Now().UnixNano())
	return func() { n.active.Add(-1); n.last.Store(time.Now().UnixNano()) }
}
func (n *nativeHostRuntime) Close(ctx context.Context) error {
	n.closeOnce.Do(func() {
		n.engine.SetDraining(true)
		n.cancel()
		<-n.done
		n.closeErr = errors.Join(n.http.Shutdown(ctx), n.engine.Close())
		if n.closeErr != nil {
			_ = n.http.Close()
		}
	})
	return n.closeErr
}
func (n *nativeHostRuntime) boundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release := n.acquire()
		defer release()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		applySurfaceFrameHeaders(w.Header())
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Query().Has("token") {
			writeManagementError(w, 401, "query tokens are not accepted")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			writeManagementError(w, 403, "cross-origin request rejected")
			return
		}
		bearer := subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+n.token)) == 1
		session, authed := n.sessions.Get(w, r)
		if !bearer && !authed {
			writeManagementError(w, 401, "authenticated Room access required")
			return
		}
		if !bearer && r.Method != http.MethodGet && r.Method != http.MethodHead && !session.ValidCSRF(r.Header.Get(websession.CSRFHeaderName)) {
			writeManagementError(w, 403, "invalid CSRF token")
			return
		}
		if r.URL.Path == "/api/v1/session" {
			switch r.Method {
			case http.MethodPost:
				if !bearer {
					writeManagementError(w, 403, "bearer bootstrap required")
					return
				}
				value, err := n.sessions.Create(w, r)
				if err != nil {
					writeManagementError(w, 500, "session creation failed")
					return
				}
				writeManagementJSON(w, 201, map[string]any{"required": true, "csrf_token": value.CSRFToken})
			case http.MethodGet:
				writeManagementJSON(w, 200, map[string]any{"required": true, "csrf_token": session.CSRFToken})
			case http.MethodDelete:
				n.sessions.Delete(w, r)
				w.WriteHeader(204)
			default:
				writeManagementError(w, 405, "method not allowed")
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (n *nativeHostRuntime) snapshot() map[string]any {
	return map[string]any{"room": n.room, "relay": n.engine.Snapshot(), "protocol": protocol.NativeVersion, "config_notice": "Provider, model, effort, instructions and permissions are display-only here; configure them in the native harness.", "identities": model.ParticipantIdentities(map[model.ActorID]model.RuntimeKind{model.ActorClaude: n.room.Agents[model.ActorClaude].Runtime, model.ActorCodex: n.room.Agents[model.ActorCodex].Runtime})}
}
func (n *nativeHostRuntime) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		assets := map[string]string{"/": "native-host.html", "/index.html": "native-host.html", "/app.js": "native-host.js", "/styles.css": "native-host.css", "/favicon.svg": "favicon.svg"}
		if name, ok := assets[p]; ok {
			data, err := managementAssets.ReadFile("assets/" + name)
			if err != nil {
				writeManagementError(w, 404, "asset not found")
				return
			}
			switch {
			case strings.HasSuffix(name, ".js"):
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			case strings.HasSuffix(name, ".css"):
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			case strings.HasSuffix(name, ".svg"):
				w.Header().Set("Content-Type", "image/svg+xml")
			default:
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
			}
			if r.Method != http.MethodHead {
				_, _ = w.Write(data)
			}
			return
		}
		switch p {
		case "/api/v1/health":
			writeManagementJSON(w, 200, map[string]any{"ok": true, "host_mode": "native"})
			return
		case "/api/v1/snapshot", "/api/v1/export":
			writeManagementJSON(w, 200, n.snapshot())
			return
		case "/api/v1/messages":
			writeManagementJSON(w, 200, n.engine.Snapshot().Messages)
			return
		case "/api/v1/events":
			n.events(w, r)
			return
		}
		if strings.HasPrefix(p, "/api/v1/attachments/") {
			id := strings.TrimPrefix(p, "/api/v1/attachments/")
			meta, file, err := n.media.OpenFile(id)
			if err != nil {
				writeManagementError(w, 404, "attachment not found")
				return
			}
			defer file.Close()
			w.Header().Set("Content-Type", meta.MediaType)
			_, _ = io.Copy(w, file)
			return
		}
	}
	if r.Method == http.MethodPost {
		switch p {
		case "/api/v1/messages":
			var req relay.SendRequest
			if decodeNativeJSON(w, r, &req) != nil {
				return
			}
			m, err := n.engine.SendUser(req)
			nativeResult(w, m, err)
			return
		case "/api/v1/attachments":
			n.upload(w, r)
			return
		}
		if strings.HasPrefix(p, "/api/v1/messages/") {
			id, action, ok := strings.Cut(strings.TrimPrefix(p, "/api/v1/messages/"), "/")
			if ok {
				switch action {
				case "cancel":
					nativeResult(w, map[string]bool{"cancelled": true}, n.engine.Cancel(id))
					return
				case "retry":
					m, err := n.engine.Retry(id)
					nativeResult(w, m, err)
					return
				}
			}
		}
		if strings.HasPrefix(p, "/api/v1/participants/") {
			slot, action, ok := strings.Cut(strings.TrimPrefix(p, "/api/v1/participants/"), "/")
			if ok && action == "park" {
				var req struct {
					Enabled bool `json:"enabled"`
				}
				if decodeNativeJSON(w, r, &req) != nil {
					return
				}
				nativeResult(w, map[string]bool{"enabled": req.Enabled}, n.engine.Park(model.ActorID(slot), req.Enabled))
				return
			}
		}
	}
	// In particular there is no Interrupt, start, stop, permission projection or
	// adapter operation in Native hosting. A transport receipt is not acceptance.
	writeManagementError(w, http.StatusNotFound, "operation is not available in Native host mode")
}
func nativeResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, relay.ErrAuth) {
			status = 401
		}
		writeManagementError(w, status, err.Error())
		return
	}
	writeManagementJSON(w, 200, value)
}
func (n *nativeHostRuntime) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, attachment.MaxImageBytes+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		writeManagementError(w, 400, "multipart image required")
		return
	}
	part, err := reader.NextPart()
	if err != nil {
		writeManagementError(w, 400, "image part required")
		return
	}
	defer part.Close()
	image, err := n.media.SaveImage(part.FileName(), part, "native-relay")
	nativeResult(w, image, err)
}
func (n *nativeHostRuntime) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeManagementError(w, 500, "streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	var cursor uint64
	for {
		snapshot := n.engine.Snapshot()
		if cursor != snapshot.Sequence || cursor == 0 {
			data, _ := json.Marshal(map[string]any{"sequence": snapshot.Sequence})
			_, err := fmt.Fprintf(w, "id: %d\nevent: native\ndata: %s\n\n", snapshot.Sequence, data)
			if err != nil {
				return
			}
			flusher.Flush()
			cursor = snapshot.Sequence
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		err := n.engine.WaitChanges(ctx, cursor)
		cancel()
		if r.Context().Err() != nil || errors.Is(err, relay.ErrClosed) {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func parseRelayAuth(r *http.Request, slot model.ActorID) (relay.Auth, error) {
	auth := relay.Auth{Slot: slot, BindID: r.Header.Get("X-PairRoom-Bind"), SessionID: r.Header.Get("X-PairRoom-Session")}
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Relay ") {
		return auth, relay.ErrAuth
	}
	auth.Secret = strings.TrimPrefix(value, "Relay ")
	g, err := strconv.ParseUint(r.Header.Get("X-PairRoom-Generation"), 10, 64)
	if err != nil || g == 0 {
		return auth, relay.ErrAuth
	}
	auth.Generation = g
	return auth, nil
}
