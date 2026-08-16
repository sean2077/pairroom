package service

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

//go:embed assets/*
var managementAssets embed.FS

type ManagementServerConfig struct {
	Registry    *Registry
	Runtimes    *RuntimeManager
	Provisioner BindingProvisioner
	Token       string
}

type ManagementServer struct {
	registry    *Registry
	runtimes    *RuntimeManager
	provisioner BindingProvisioner
	token       string
	http        *http.Server
	roomLocks   sync.Map // map[roomID]*sync.Mutex; Rooms are never permanently deleted
}

type ServiceSummary struct {
	Projects            int `json:"projects"`
	UnavailableProjects int `json:"unavailable_projects"`
	Rooms               int `json:"rooms"`
	ActiveRooms         int `json:"active_rooms"`
	ArchivedRooms       int `json:"archived_rooms"`
	PendingBindings     int `json:"pending_bindings"`
	RuntimeCapacityUsed int `json:"runtime_capacity_used"`
	ActiveRuntimes      int `json:"active_runtimes"`
	BusyRuntimes        int `json:"busy_runtimes"`
	QueuedRuntimes      int `json:"queued_runtimes"`
	FailedRuntimes      int `json:"failed_runtimes"`
	AttentionItems      int `json:"attention_items"`
}

type ServiceCapabilities struct {
	LegacyImport          bool `json:"legacy_import"`
	RuntimeSuspend        bool `json:"runtime_suspend"`
	RuntimePolicyMutation bool `json:"runtime_policy_mutation"`
	ProjectRemoval        bool `json:"project_removal"`
	RoomDeletion          bool `json:"room_deletion"`
	ServerPathBrowser     bool `json:"server_path_browser"`
}

type ServiceSnapshot struct {
	Version       string              `json:"version"`
	Commit        string              `json:"commit,omitempty"`
	BuildDate     string              `json:"build_date,omitempty"`
	DataRoot      string              `json:"data_root"`
	GeneratedAt   time.Time           `json:"generated_at"`
	Projects      []Project           `json:"projects"`
	Rooms         []Room              `json:"rooms"`
	Runtimes      []RuntimeStatus     `json:"runtimes"`
	RuntimePolicy RuntimePolicy       `json:"runtime_policy"`
	Summary       ServiceSummary      `json:"summary"`
	Capabilities  ServiceCapabilities `json:"capabilities"`
	Healthy       bool                `json:"healthy"`
	Diagnostic    string              `json:"diagnostic,omitempty"`
}

func NewManagementServer(cfg ManagementServerConfig) (*ManagementServer, error) {
	if cfg.Registry == nil {
		return nil, errors.New("service registry is required")
	}
	if cfg.Runtimes == nil {
		return nil, errors.New("Room Runtime Manager is required")
	}
	if cfg.Provisioner == nil {
		return nil, errors.New("binding provisioner is required")
	}
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		var err error
		token, err = randomServiceToken()
		if err != nil {
			return nil, err
		}
	}
	assets, err := fs.Sub(managementAssets, "assets")
	if err != nil {
		return nil, fmt.Errorf("open Management Shell assets: %w", err)
	}
	server := &ManagementServer{
		registry: cfg.Registry, runtimes: cfg.Runtimes,
		provisioner: cfg.Provisioner, token: token,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/service", server.readService)
	mux.HandleFunc("POST /api/v1/projects", server.registerProject)
	mux.HandleFunc("POST /api/v1/projects/{project}/rooms", server.provisionRoom)
	mux.HandleFunc("POST /api/v1/rooms/{room}/activate", server.activateRoom)
	mux.HandleFunc("POST /api/v1/rooms/{room}/suspend", server.suspendRoom)
	mux.HandleFunc("POST /api/v1/rooms/{room}/bindings", server.completeRoomBindings)
	mux.HandleFunc("PATCH /api/v1/rooms/{room}", server.renameRoom)
	mux.HandleFunc("POST /api/v1/rooms/{room}/archive", server.archiveRoom)
	mux.HandleFunc("POST /api/v1/rooms/{room}/restore", server.restoreRoom)
	mux.HandleFunc("POST /api/v1/import", server.importLegacy)
	mux.Handle("/", http.FileServer(http.FS(assets)))
	server.http = &http.Server{
		Handler:           server.securityHeaders(server.sameOrigin(server.authenticate(mux))),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       45 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return server, nil
}

func (s *ManagementServer) Handler() http.Handler { return s.http.Handler }
func (s *ManagementServer) Token() string         { return s.token }

func (s *ManagementServer) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("Management Shell listener is required")
	}
	err := s.http.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *ManagementServer) Shutdown(ctx context.Context) error {
	err := s.http.Shutdown(ctx)
	if err == nil {
		return nil
	}
	// Shutdown only waits for active handlers. Once its deadline expires, force
	// their request contexts closed before the caller begins Runtime shutdown;
	// otherwise a provisioning/archive handler could still mutate the Registry.
	return errors.Join(err, s.http.Close())
}

func (s *ManagementServer) BrowserURL(address net.Addr) string {
	return roomViewURL(address, s.token)
}

func (s *ManagementServer) readService(w http.ResponseWriter, _ *http.Request) {
	registry := s.registry.Snapshot(true)
	statuses := make(map[string]RuntimeStatus, len(registry.Rooms))
	for _, status := range s.runtimes.Statuses() {
		statuses[status.RoomID] = status
	}
	runtimes := make([]RuntimeStatus, 0, len(registry.Rooms))
	for _, room := range registry.Rooms {
		status, ok := statuses[room.ID]
		if !ok {
			status = RuntimeStatus{RoomID: room.ID, Phase: RuntimeSuspended}
		}
		runtimes = append(runtimes, status)
	}
	healthErr := s.registry.Healthy()
	payload := ServiceSnapshot{
		Version: version.Current, Commit: version.Commit, BuildDate: version.BuildDate,
		DataRoot: s.registry.Root(), GeneratedAt: time.Now().UTC(),
		Projects: registry.Projects, Rooms: registry.Rooms, Runtimes: runtimes,
		RuntimePolicy: s.runtimes.Policy(),
		Summary:       summarizeService(registry.Projects, registry.Rooms, runtimes),
		Capabilities: ServiceCapabilities{
			LegacyImport: true, RuntimeSuspend: true,
		},
		Healthy: healthErr == nil,
	}
	if healthErr != nil {
		payload.Diagnostic = healthErr.Error()
	}
	writeManagementJSON(w, http.StatusOK, payload)
}

func summarizeService(projects []Project, rooms []Room, runtimes []RuntimeStatus) ServiceSummary {
	summary := ServiceSummary{Projects: len(projects), Rooms: len(rooms)}
	for _, project := range projects {
		if !project.Available {
			summary.UnavailableProjects++
		}
	}
	for _, room := range rooms {
		if room.Archived() {
			summary.ArchivedRooms++
		} else {
			summary.ActiveRooms++
		}
		if room.HasBlockingPendingBindings() {
			summary.PendingBindings++
		}
	}
	for _, runtime := range runtimes {
		if runtime.OccupiesCapacity {
			summary.RuntimeCapacityUsed++
		}
		if runtime.Phase == RuntimeActive {
			summary.ActiveRuntimes++
		}
		if runtime.Busy {
			summary.BusyRuntimes++
		}
		switch runtime.Phase {
		case RuntimeQueued:
			summary.QueuedRuntimes++
		case RuntimeFailed:
			summary.FailedRuntimes++
		}
	}
	summary.AttentionItems = summary.UnavailableProjects + summary.PendingBindings + summary.FailedRuntimes
	return summary
}

func (s *ManagementServer) registerProject(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string `json:"path"`
	}
	if err := decodeManagementJSON(w, r, &request); err != nil {
		return
	}
	project, err := s.registry.RegisterProject(r.Context(), request.Path)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusCreated, project)
}

func (s *ManagementServer) provisionRoom(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name     string                        `json:"name"`
		Bindings map[model.ActorID]BindingSpec `json:"bindings"`
	}
	if err := decodeManagementJSON(w, r, &request); err != nil {
		return
	}
	room, err := s.registry.ProvisionRoom(r.Context(), ProvisionRequest{
		ProjectID: r.PathValue("project"), Name: request.Name, Bindings: request.Bindings,
	}, s.provisioner)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusCreated, room)
}

func (s *ManagementServer) activateRoom(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room")
	unlock := s.lockRoom(roomID)
	defer unlock()
	status, err := s.runtimes.RequestActivation(roomID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	code := http.StatusAccepted
	if status.Phase == RuntimeActive && status.URL != "" {
		code = http.StatusOK
	}
	writeManagementJSON(w, code, status)
}

func (s *ManagementServer) suspendRoom(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room")
	unlock := s.lockRoom(roomID)
	defer unlock()
	if _, ok := s.registry.Room(roomID); !ok {
		s.writeError(w, ErrRoomNotFound)
		return
	}
	if err := s.runtimes.Suspend(r.Context(), roomID); err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, s.runtimes.Status(roomID))
}

func (s *ManagementServer) completeRoomBindings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Bindings map[model.ActorID]BindingSpec `json:"bindings"`
	}
	if err := decodeManagementJSON(w, r, &request); err != nil {
		return
	}
	roomID := r.PathValue("room")
	unlock := s.lockRoom(roomID)
	defer unlock()
	if err := s.runtimes.WaitAndSuspend(r.Context(), roomID); err != nil {
		s.writeError(w, err)
		return
	}
	room, err := s.registry.CompleteBindings(r.Context(), roomID, request.Bindings, s.provisioner)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, room)
}

func (s *ManagementServer) renameRoom(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name"`
	}
	if err := decodeManagementJSON(w, r, &request); err != nil {
		return
	}
	roomID := r.PathValue("room")
	unlock := s.lockRoom(roomID)
	defer unlock()
	// Keeping control-plane appends outside an active Engine avoids introducing
	// a second live projection of the same append-only log. Active work is never
	// interrupted; this request waits for the safe Turn boundary.
	if err := s.runtimes.WaitAndSuspend(r.Context(), roomID); err != nil {
		s.writeError(w, err)
		return
	}
	room, err := s.registry.RenameRoom(r.Context(), roomID, request.Name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, room)
}

func (s *ManagementServer) archiveRoom(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room")
	unlock := s.lockRoom(roomID)
	defer unlock()
	if err := s.runtimes.WaitAndSuspend(r.Context(), roomID); err != nil {
		s.writeError(w, err)
		return
	}
	room, err := s.registry.ArchiveRoom(r.Context(), roomID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, room)
}

func (s *ManagementServer) restoreRoom(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room")
	unlock := s.lockRoom(roomID)
	defer unlock()
	room, err := s.registry.RestoreRoom(r.Context(), roomID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, room)
}

func (s *ManagementServer) importLegacy(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Path string `json:"path"`
	}
	if err := decodeManagementJSON(w, r, &request); err != nil {
		return
	}
	room, err := s.registry.ImportLegacy(r.Context(), request.Path)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusCreated, room)
}

func (s *ManagementServer) lockRoom(roomID string) func() {
	value, _ := s.roomLocks.LoadOrStore(roomID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (s *ManagementServer) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Query().Has("token") {
			writeManagementError(w, http.StatusUnauthorized, "query-string tokens are not accepted")
			return
		}
		authorization := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authorization, prefix) || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(strings.TrimPrefix(authorization, prefix))), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="PairRoom Service"`)
			writeManagementError(w, http.StatusUnauthorized, "a valid service bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *ManagementServer) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site != "" && site != "same-origin" && site != "none" {
				writeManagementError(w, http.StatusForbidden, "cross-site service requests are not allowed")
				return
			}
			if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
				writeManagementError(w, http.StatusForbidden, "request origin does not match the Management Shell")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *ManagementServer) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *ManagementServer) writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrProjectAlreadyRegistered), errors.Is(err, ErrBindingOwned), errors.Is(err, ErrRoomBindingPending):
		code = http.StatusConflict
	case errors.Is(err, ErrProjectNotFound), errors.Is(err, ErrRoomNotFound):
		code = http.StatusNotFound
	case errors.Is(err, ErrRegistryFailClosed), errors.Is(err, ErrRuntimeManagerClosed):
		code = http.StatusServiceUnavailable
	case errors.Is(err, ErrRuntimeBusy), errors.Is(err, ErrRuntimeCloseUncertain), errors.Is(err, ErrRuntimeDrainAborted):
		code = http.StatusConflict
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = http.StatusRequestTimeout
	default:
		// Validation and filesystem diagnostics are deliberately actionable in a
		// local-only control plane. Treat known user-input failures as 400.
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "required") || strings.Contains(message, "must ") ||
			strings.Contains(message, "invalid") || strings.Contains(message, "not an accessible git") ||
			strings.Contains(message, "unavailable") || strings.Contains(message, "could not be resumed") {
			code = http.StatusBadRequest
		}
	}
	writeManagementError(w, code, err.Error())
}

func decodeManagementJSON(w http.ResponseWriter, r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeManagementError(w, http.StatusBadRequest, "invalid JSON request: "+err.Error())
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values are not allowed")
		}
		writeManagementError(w, http.StatusBadRequest, "invalid JSON request: "+err.Error())
		return err
	}
	return nil
}

func writeManagementJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeManagementError(w http.ResponseWriter, status int, message string) {
	writeManagementJSON(w, status, map[string]string{"error": message})
}
