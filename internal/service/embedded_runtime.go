package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/room"
	"github.com/sean2077/pairroom/internal/server"
	"github.com/sean2077/pairroom/internal/store"
	"github.com/sean2077/pairroom/internal/version"
	"github.com/sean2077/pairroom/internal/workspace"
)

// EmbeddedRuntimeConfig controls the per-Room resources created by the
// service. Each active Room still uses the existing v1 Engine, adapters,
// attachment store, workspace manager, and Room View HTTP handler.
type EmbeddedRuntimeConfig struct {
	ListenHost          string
	Mock                bool
	AutoStart           bool
	RoutingMode         model.RoutingMode
	MaxAgentHops        int
	StallWarningSeconds int
	Claude              agent.Config
	Codex               agent.Config
	DrainPollInterval   time.Duration
}

const uncorrelatedRuntimeErrorNotice = "native runtime reported an error outside a PairRoom-authored turn"

// transcriptBoundaryFactory keeps service-owned Room history separate from a
// vendor's native transcript. Native session resume is still allowed, but any
// transcript-bearing event emitted before PairRoom has submitted a correlated
// Room input is discarded rather than persisted or published over SSE.
func transcriptBoundaryFactory(factory agent.Factory) agent.Factory {
	return func(cfg agent.Config, sink agent.EventSink) agent.Adapter {
		expectedSession := strings.TrimSpace(cfg.SessionID)
		return factory(cfg, func(event model.RuntimeEvent) {
			filtered, ok := filterTranscriptBoundaryEvent(expectedSession, event)
			if ok {
				sink(filtered)
			}
		})
	}
}

func filterTranscriptBoundaryEvent(expectedSession string, event model.RuntimeEvent) (model.RuntimeEvent, bool) {
	if strings.TrimSpace(event.CorrelationID) != "" {
		// A correlation ID is assigned only after PairRoom submits a Room-owned
		// input, so transcript data for that turn is inside the durable boundary.
		return event, true
	}

	event.CorrelationID = ""
	event.TurnID = ""
	event.ItemID = ""
	event.Approval = nil
	switch event.Kind {
	case model.RuntimeSession:
		if expectedSession == "" || strings.TrimSpace(event.SessionID) != expectedSession {
			return model.RuntimeEvent{}, false
		}
		event.SessionID = expectedSession
		event.Text = ""
		event.Name = ""
		event.State = ""
		event.Runtime = nil
		event.Data = nil
		return event, true

	case model.RuntimeInfoUpdated:
		event.SessionID = ""
		event.Text = ""
		event.Name = ""
		event.State = ""
		event.Data = nil
		if event.Runtime != nil {
			info := *event.Runtime
			info.Capabilities = append([]string(nil), info.Capabilities...)
			// Warnings and opaque vendor data can include raw CLI output. Runtime
			// capability metadata is safe, but those fields stay outside the Room.
			info.Warnings = nil
			info.Data = nil
			event.Runtime = &info
		}
		return event, true

	case model.RuntimeState:
		event.SessionID = ""
		event.Name = ""
		event.Runtime = nil
		event.Data = nil
		if event.State == model.StateError {
			event.Text = uncorrelatedRuntimeErrorNotice
		} else {
			event.Text = ""
		}
		return event, true

	case model.RuntimeError:
		event.SessionID = ""
		event.Text = uncorrelatedRuntimeErrorNotice
		event.Name = ""
		event.State = ""
		event.Runtime = nil
		event.Data = nil
		return event, true

	default:
		// Text, final answers, tool events, plans, diffs, usage, approvals,
		// command output, and logs are transcript-bearing unless PairRoom can
		// prove that they belong to a Room-authored correlated turn.
		return model.RuntimeEvent{}, false
	}
}

// EmbeddedRuntimeFactory returns an in-process RuntimeFactory. The manager
// controls how many of these factories may be active simultaneously.
func EmbeddedRuntimeFactory(registry *Registry, cfg EmbeddedRuntimeConfig) RuntimeFactory {
	return func(ctx context.Context, durableRoom Room) (RoomRuntime, error) {
		if registry == nil {
			return nil, errors.New("service registry is required")
		}
		project, ok := registry.Project(durableRoom.ProjectID)
		if !ok {
			return nil, ErrProjectNotFound
		}
		if !project.Available {
			return nil, fmt.Errorf("project is unavailable: %s", project.Diagnostic)
		}
		return startEmbeddedRuntime(ctx, project, durableRoom, cfg)
	}
}

type uncertainRuntime struct {
	roomID string
	cause  error
}

func (r *uncertainRuntime) URL() string                 { return "" }
func (r *uncertainRuntime) Busy() bool                  { return false }
func (r *uncertainRuntime) Close(context.Context) error { return r.cause }

func failedEmbeddedStart(roomID string, engine *room.Engine, cancel context.CancelFunc, startErr error) (RoomRuntime, error) {
	if cancel != nil {
		cancel()
	}
	if engine == nil {
		return nil, startErr
	}
	if closeErr := engine.Close(); closeErr != nil {
		combined := errors.Join(startErr, fmt.Errorf("clean up failed Room runtime: %w", closeErr))
		return &uncertainRuntime{roomID: roomID, cause: combined}, combined
	}
	return nil, startErr
}

type embeddedRuntime struct {
	roomID string
	url    string
	engine *room.Engine
	cancel context.CancelFunc

	http      *http.Server
	listener  net.Listener
	serveDone chan error

	requestMu       sync.Mutex
	managerDraining bool
	closeDraining   bool
	admissionClosed bool
	activeMutations int

	closeMu      sync.Mutex
	closeAttempt chan struct{}
	closed       bool
	closeErr     error
	poll         time.Duration
	lastActivity atomic.Int64
}

func startEmbeddedRuntime(startCtx context.Context, project Project, durableRoom Room, cfg EmbeddedRuntimeConfig) (_ RoomRuntime, resultErr error) {
	if err := startCtx.Err(); err != nil {
		return nil, err
	}
	if durableRoom.Archived() {
		return nil, errors.New("archived Room cannot be activated")
	}
	if durableRoom.HasPendingBindings() {
		return nil, ErrRoomBindingPending
	}
	if err := durableRoom.Validate(); err != nil {
		return nil, fmt.Errorf("validate durable Room projection: %w", err)
	}
	if cfg.ListenHost == "" {
		cfg.ListenHost = "127.0.0.1"
	}
	if cfg.ListenHost != "127.0.0.1" && cfg.ListenHost != "::1" && !strings.EqualFold(cfg.ListenHost, "localhost") {
		return nil, errors.New("Room runtimes must listen on loopback")
	}
	if cfg.RoutingMode == "" {
		cfg.RoutingMode = model.DefaultRoomSettings().RoutingMode
	}
	if !cfg.RoutingMode.Valid() {
		return nil, fmt.Errorf("invalid routing mode %q", cfg.RoutingMode)
	}
	if cfg.MaxAgentHops == 0 {
		cfg.MaxAgentHops = model.DefaultRoomSettings().MaxHops
	}
	if cfg.MaxAgentHops < 1 || cfg.MaxAgentHops > 30 {
		return nil, errors.New("max agent hops must be between 1 and 30")
	}
	if cfg.StallWarningSeconds == 0 {
		cfg.StallWarningSeconds = model.DefaultRoomSettings().StallWarningSeconds
	}
	if cfg.StallWarningSeconds != -1 && (cfg.StallWarningSeconds < 30 || cfg.StallWarningSeconds > 86400) {
		return nil, errors.New("stall warning seconds must be -1 or between 30 and 86400")
	}
	if cfg.DrainPollInterval <= 0 {
		cfg.DrainPollInterval = 100 * time.Millisecond
	}

	eventStore, err := store.Open(durableRoom.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open Room store: %w", err)
	}
	storeOwned := true
	defer func() {
		if resultErr != nil && storeOwned {
			_ = eventStore.Close()
		}
	}()

	attachmentStore, err := attachment.Open(durableRoom.DataDir, project.Root)
	if err != nil {
		return nil, fmt.Errorf("open Room attachment store: %w", err)
	}
	workspaceManager, err := workspace.New(project.Root, durableRoom.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open Room workspace manager: %w", err)
	}

	claudeFactory := agent.ClaudeFactory
	codexFactory := agent.CodexFactory
	if cfg.Mock {
		claudeFactory = agent.MockFactory
		codexFactory = agent.MockFactory
	}
	claudeFactory = transcriptBoundaryFactory(claudeFactory)
	codexFactory = transcriptBoundaryFactory(codexFactory)
	claudeCfg := cfg.Claude
	claudeCfg.ClientVersion = version.Current
	claudeCfg.SessionID = durableRoom.Bindings[model.ActorClaude].SessionID
	claudeCfg.RequireExactSession = true
	codexCfg := cfg.Codex
	codexCfg.ClientVersion = version.Current
	codexCfg.SessionID = durableRoom.Bindings[model.ActorCodex].SessionID
	codexCfg.RequireExactSession = true

	engine, err := room.New(room.Config{
		Name: durableRoom.Name,
		Repo: project.Root,
		Settings: model.RoomSettings{
			RoutingMode:         cfg.RoutingMode,
			MaxHops:             cfg.MaxAgentHops,
			StallWarningSeconds: cfg.StallWarningSeconds,
		},
		Store:         eventStore,
		ClaudeFactory: claudeFactory,
		CodexFactory:  codexFactory,
		ClaudeConfig:  claudeCfg,
		CodexConfig:   codexCfg,
		Attachments:   attachmentStore,
		Workspaces:    workspaceManager,
		AutoStart:     cfg.AutoStart,
	})
	if err != nil {
		return nil, fmt.Errorf("restore Room engine: %w", err)
	}
	// The Engine owns the event store after construction, including all error
	// paths through Engine.Close.
	storeOwned = false

	// Engine lifetime is independent from the manager's startup context. The
	// manager may cancel an in-progress factory during service shutdown, but it
	// must never use that cancellation to interrupt a successfully activated
	// Room turn.
	runtimeCtx, runtimeCancel := context.WithCancel(context.Background())
	if err := engine.Start(runtimeCtx); err != nil {
		return failedEmbeddedStart(durableRoom.ID, engine, runtimeCancel, fmt.Errorf("start Room engine: %w", err))
	}

	token, err := randomServiceToken()
	if err != nil {
		return failedEmbeddedStart(durableRoom.ID, engine, runtimeCancel, err)
	}
	roomServer, err := server.New(server.Config{
		Engine: engine, Repo: project.Root, Token: token, Attachments: attachmentStore,
		TranscriptBoundaryNotice: durableRoom.TranscriptBoundaryNotice,
		SessionCookieName:        roomSessionCookieName(durableRoom.ID),
	})
	if err != nil {
		return failedEmbeddedStart(durableRoom.ID, engine, runtimeCancel, fmt.Errorf("create Room server: %w", err))
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.ListenHost, "0"))
	if err != nil {
		return failedEmbeddedStart(durableRoom.ID, engine, runtimeCancel, fmt.Errorf("listen for Room View: %w", err))
	}

	runtime := &embeddedRuntime{
		roomID:    durableRoom.ID,
		engine:    engine,
		cancel:    runtimeCancel,
		listener:  listener,
		serveDone: make(chan error, 1),
		poll:      cfg.DrainPollInterval,
	}
	runtime.lastActivity.Store(time.Now().UTC().UnixNano())
	runtime.url = roomViewURL(listener.Addr(), token)
	runtime.http = &http.Server{
		Handler:           runtime.drainHandler(roomServer.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		err := runtime.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			err = nil
		}
		runtime.serveDone <- err
	}()

	select {
	case <-startCtx.Done():
		if closeErr := runtime.Close(context.Background()); closeErr != nil {
			combined := errors.Join(startCtx.Err(), fmt.Errorf("clean up canceled Room runtime: %w", closeErr))
			return runtime, combined
		}
		return nil, startCtx.Err()
	default:
	}
	return runtime, nil
}

func (r *embeddedRuntime) URL() string { return r.url }

func (r *embeddedRuntime) LastActivity() time.Time {
	if r == nil {
		return time.Time{}
	}
	value := r.lastActivity.Load()
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func (r *embeddedRuntime) Busy() bool {
	if r == nil || r.engine == nil {
		return false
	}
	return snapshotBusy(r.engine.Snapshot())
}

func snapshotBusy(snapshot model.RoomSnapshot) bool {
	for _, participant := range snapshot.Participants {
		if participant.CurrentTurn != "" {
			return true
		}
		switch participant.State {
		case model.StateStarting, model.StateWorking, model.StateWaiting:
			return true
		}
	}
	for _, approval := range snapshot.Approvals {
		if approval.Status == "pending" {
			return true
		}
	}
	for _, message := range snapshot.Messages {
		for _, state := range message.Processing {
			if state == model.ProcessingWaiting || state == model.ProcessingWorking {
				return true
			}
		}
	}
	return false
}

func (r *embeddedRuntime) SetDraining(value bool) {
	if r == nil {
		return
	}
	r.requestMu.Lock()
	if !r.admissionClosed {
		r.managerDraining = value
	}
	r.requestMu.Unlock()
}

func (r *embeddedRuntime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		r.closeMu.Lock()
		if r.closed {
			err := r.closeErr
			r.closeMu.Unlock()
			return err
		}
		if attempt := r.closeAttempt; attempt != nil {
			r.closeMu.Unlock()
			select {
			case <-ctx.Done():
				return errors.Join(ErrRuntimeDrainAborted, ctx.Err())
			case <-attempt:
				continue
			}
		}
		attempt := make(chan struct{})
		r.closeAttempt = attempt
		r.closeMu.Unlock()

		err, terminal := r.close(ctx)

		r.closeMu.Lock()
		if terminal {
			r.closed = true
			r.closeErr = err
		}
		r.closeAttempt = nil
		close(attempt)
		r.closeMu.Unlock()
		return err
	}
}

// close first establishes a mutation drain while leaving the Room View
// reachable for the few controls that can settle existing work. It crosses the
// irreversible close boundary only after every admitted mutation and native
// turn is idle. A context cancellation before that point is retryable and does
// not interrupt the native runtime.
func (r *embeddedRuntime) close(ctx context.Context) (error, bool) {
	r.requestMu.Lock()
	if r.admissionClosed {
		r.requestMu.Unlock()
		return nil, true
	}
	r.closeDraining = true
	r.requestMu.Unlock()

	poll := r.poll
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		r.requestMu.Lock()
		if r.activeMutations == 0 && !r.Busy() {
			// From this point no approval/cancel/interrupt handler may enter. HTTP
			// readers can be terminated safely because they do not own native work.
			r.admissionClosed = true
			r.requestMu.Unlock()
			break
		}
		r.requestMu.Unlock()

		select {
		case <-ctx.Done():
			r.requestMu.Lock()
			r.closeDraining = false
			r.requestMu.Unlock()
			return errors.Join(ErrRuntimeDrainAborted, ctx.Err()), false
		case <-ticker.C:
		}
	}

	var result error
	if r.http != nil {
		if err := r.http.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, fmt.Errorf("close Room HTTP server: %w", err))
		}
	} else if r.listener != nil {
		if err := r.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, fmt.Errorf("close Room listener: %w", err))
		}
	}
	if r.serveDone != nil {
		if err := <-r.serveDone; err != nil {
			result = errors.Join(result, fmt.Errorf("serve Room View: %w", err))
		}
	}
	if r.engine != nil {
		if err := r.engine.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	if r.cancel != nil {
		r.cancel()
	}
	return result, true
}

func (r *embeddedRuntime) drainHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		r.lastActivity.Store(time.Now().UTC().UnixNano())
		mutating := request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions
		if mutating {
			r.requestMu.Lock()
			draining := r.managerDraining || r.closeDraining
			if r.admissionClosed || (draining && !drainControlRequest(request)) {
				r.requestMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"Room runtime is suspending"}` + "\n"))
				return
			}
			r.activeMutations++
			r.requestMu.Unlock()
			defer func() {
				r.requestMu.Lock()
				r.activeMutations--
				r.requestMu.Unlock()
			}()
		}
		next.ServeHTTP(w, request)
	})
}

// drainControlRequest admits only mutations that cannot create a new turn and
// are necessary to settle work already owned by the Room. Authentication can
// also be established so a newly reloaded local tab can issue those controls.
func drainControlRequest(request *http.Request) bool {
	if request == nil {
		return false
	}
	path := request.URL.Path
	if request.Method == http.MethodPost && path == "/api/v1/session" {
		return true
	}
	if request.Method != http.MethodPost {
		return false
	}
	if singlePathID(path, "/api/v1/approvals/") {
		return true
	}
	if pathAction(path, "/api/v1/participants/", "interrupt") {
		return true
	}
	return pathAction(path, "/api/v1/messages/", "cancel")
}

func singlePathID(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	remainder := strings.TrimPrefix(path, prefix)
	return remainder != "" && !strings.Contains(remainder, "/")
}

func pathAction(path, prefix, action string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] == action
}

func roomSessionCookieName(roomID string) string {
	sum := sha256.Sum256([]byte(roomID))
	return "pairroom_session_" + hex.EncodeToString(sum[:8])
}

func roomViewURL(address net.Addr, token string) string {
	host := address.String()
	if tcp, ok := address.(*net.TCPAddr); ok {
		host = net.JoinHostPort(tcp.IP.String(), fmt.Sprint(tcp.Port))
	}
	value := url.URL{Scheme: "http", Host: host, Path: "/"}
	value.Fragment = "token=" + url.QueryEscape(token)
	return value.String()
}

func randomServiceToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate service token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
