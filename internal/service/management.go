package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/openbrowser"
	"github.com/sean2077/pairroom/internal/version"
	"github.com/sean2077/pairroom/internal/websession"
)

var openRoomInBrowser = openbrowser.Open

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
	sessions    *websession.Store
	http        *http.Server
	roomLocks   roomLockSet
}

type managementAuthMode uint8

const (
	managementAuthBearer managementAuthMode = iota + 1
	managementAuthBrowserSession
)

type managementAuthContextKey struct{}

type managementRequestAuth struct {
	Mode    managementAuthMode
	Session websession.Session
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
	PendingRoomCleanup  int `json:"pending_room_cleanup"`
}

type ServiceCapabilities struct {
	LegacyImport          bool `json:"legacy_import"`
	RuntimeSuspend        bool `json:"runtime_suspend"`
	RuntimePolicyMutation bool `json:"runtime_policy_mutation"`
	ProjectRefresh        bool `json:"project_refresh"`
	ProjectRemoval        bool `json:"project_removal"`
	RoomDeletion          bool `json:"room_deletion"`
	ServerPathBrowser     bool `json:"server_path_browser"`
	RoomSurface           bool `json:"room_surface"`
}

type ServiceSnapshot struct {
	Version       string                  `json:"version"`
	Commit        string                  `json:"commit,omitempty"`
	BuildDate     string                  `json:"build_date,omitempty"`
	DataRoot      string                  `json:"data_root"`
	GeneratedAt   time.Time               `json:"generated_at"`
	Projects      []Project               `json:"projects"`
	Rooms         []Room                  `json:"rooms"`
	Runtimes      []RuntimeStatus         `json:"runtimes"`
	RuntimePolicy RuntimePolicy           `json:"runtime_policy"`
	Summary       ServiceSummary          `json:"summary"`
	Capabilities  ServiceCapabilities     `json:"capabilities"`
	Healthy       bool                    `json:"healthy"`
	Diagnostic    string                  `json:"diagnostic,omitempty"`
	Maintenance   RoomDeletionMaintenance `json:"maintenance"`
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
	sessions, err := websession.New(managementSessionCookieName(cfg.Registry.Root()))
	if err != nil {
		return nil, err
	}
	server := &ManagementServer{
		registry: cfg.Registry, runtimes: cfg.Runtimes,
		provisioner: cfg.Provisioner, token: token, sessions: sessions,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/session", server.createBrowserSession)
	mux.HandleFunc("GET /api/v1/session", server.readBrowserSession)
	mux.HandleFunc("DELETE /api/v1/session", server.deleteBrowserSession)
	mux.HandleFunc("GET /api/v1/service", server.readService)
	mux.HandleFunc("POST /api/v1/projects", server.registerProject)
	mux.HandleFunc("POST /api/v1/projects/{project}/refresh", server.refreshProject)
	mux.HandleFunc("DELETE /api/v1/projects/{project}", server.removeProject)
	mux.HandleFunc("POST /api/v1/projects/{project}/rooms", server.provisionRoom)
	mux.HandleFunc("POST /api/v1/rooms/{room}/activate", server.activateRoom)
	mux.HandleFunc("POST /api/v1/rooms/{room}/open-browser", server.openRoomBrowser)
	mux.HandleFunc("/api/v1/rooms/{room}/surface", server.roomSurface)
	mux.HandleFunc("/api/v1/rooms/{room}/surface/{path...}", server.roomSurface)
	mux.HandleFunc("PATCH /api/v1/runtime-policy", server.updateRuntimePolicy)
	mux.HandleFunc("POST /api/v1/rooms/{room}/suspend", server.suspendRoom)
	mux.HandleFunc("POST /api/v1/rooms/{room}/bindings", server.completeRoomBindings)
	mux.HandleFunc("PATCH /api/v1/rooms/{room}", server.renameRoom)
	mux.HandleFunc("POST /api/v1/rooms/{room}/archive", server.archiveRoom)
	mux.HandleFunc("POST /api/v1/rooms/batch-archive", server.archiveRoomsBatch)
	mux.HandleFunc("POST /api/v1/rooms/{room}/restore", server.restoreRoom)
	mux.HandleFunc("DELETE /api/v1/rooms/{room}", server.removeRoom)
	mux.HandleFunc("POST /api/v1/rooms/batch-delete", server.removeRoomsBatch)
	mux.HandleFunc("POST /api/v1/maintenance/room-deletions/retry", server.retryRoomDeletionCleanup)
	mux.HandleFunc("POST /api/v1/import", server.importLegacy)
	mux.Handle("/", http.FileServer(http.FS(assets)))
	server.http = &http.Server{
		Handler:           server.securityHeaders(server.sameOrigin(server.authenticate(server.csrf(mux)))),
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

func (s *ManagementServer) createBrowserSession(w http.ResponseWriter, r *http.Request) {
	auth := managementAuthFromContext(r.Context())
	if auth.Mode != managementAuthBearer {
		writeManagementError(w, http.StatusForbidden, "a bearer bootstrap token is required")
		return
	}
	value, err := s.sessions.Create(w, r)
	if err != nil {
		writeManagementError(w, http.StatusInternalServerError, "create browser session: "+err.Error())
		return
	}
	writeManagementJSON(w, http.StatusCreated, map[string]any{
		"csrf_token": value.CSRFToken, "created_at": value.CreatedAt, "expires_at": value.ExpiresAt,
	})
}

func (s *ManagementServer) readBrowserSession(w http.ResponseWriter, r *http.Request) {
	auth := managementAuthFromContext(r.Context())
	if auth.Mode != managementAuthBrowserSession {
		writeManagementJSON(w, http.StatusOK, map[string]string{"mode": "bearer"})
		return
	}
	writeManagementJSON(w, http.StatusOK, map[string]any{
		"csrf_token": auth.Session.CSRFToken, "created_at": auth.Session.CreatedAt, "expires_at": auth.Session.ExpiresAt,
	})
}

func (s *ManagementServer) deleteBrowserSession(w http.ResponseWriter, r *http.Request) {
	s.sessions.Delete(w, r)
	w.WriteHeader(http.StatusNoContent)
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
		Version: version.Describe(), Commit: version.Commit, BuildDate: version.BuildDate,
		DataRoot: s.registry.Root(), GeneratedAt: time.Now().UTC(),
		Projects: registry.Projects, Rooms: registry.Rooms, Runtimes: runtimes,
		RuntimePolicy: s.runtimes.Policy(),
		Summary:       summarizeService(registry.Projects, registry.Rooms, runtimes),
		Capabilities: ServiceCapabilities{
			LegacyImport: true, RuntimeSuspend: true, RuntimePolicyMutation: true,
			ProjectRefresh: true, ProjectRemoval: true, RoomDeletion: true, RoomSurface: true,
		},
		Maintenance: s.registry.RoomDeletionMaintenance(),
		Healthy:     healthErr == nil,
	}
	payload.Summary.PendingRoomCleanup = payload.Maintenance.PendingCleanup
	maintenanceAttention := payload.Summary.PendingRoomCleanup
	if maintenanceAttention == 0 && payload.Maintenance.Diagnostic != "" {
		maintenanceAttention = 1
	}
	payload.Summary.AttentionItems = payload.Summary.UnavailableProjects + payload.Summary.PendingBindings + payload.Summary.FailedRuntimes + maintenanceAttention
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

func (s *ManagementServer) refreshProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.registry.RefreshProject(r.Context(), r.PathValue("project"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, project)
}

func (s *ManagementServer) removeProject(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ConfirmProjectID string `json:"confirm_project_id"`
	}
	if err := decodeManagementJSON(w, r, &request); err != nil {
		return
	}
	projectID := r.PathValue("project")
	if request.ConfirmProjectID != projectID {
		writeManagementError(w, http.StatusBadRequest, "confirm_project_id must exactly match the Project ID in the request path")
		return
	}
	if _, err := s.registry.RemoveProject(r.Context(), projectID); err != nil {
		s.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

func (s *ManagementServer) updateRuntimePolicy(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Limit *int `json:"limit"`
	}
	if err := decodeManagementJSON(w, r, &request); err != nil {
		return
	}
	if request.Limit == nil {
		writeManagementError(w, http.StatusBadRequest, "runtime-limit is required")
		return
	}
	policy, err := s.runtimes.SetLimit(*request.Limit)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, policy)
}

func (s *ManagementServer) openRoomBrowser(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room")
	room, ok := s.registry.Room(roomID)
	if !ok {
		s.writeError(w, ErrRoomNotFound)
		return
	}
	if room.Archived() {
		writeManagementJSON(w, http.StatusConflict, map[string]any{
			"error": "archived rooms cannot be opened in an external browser",
			"code":  "room_archived",
		})
		return
	}
	status := s.runtimes.Status(roomID)
	if status.Phase != RuntimeActive || strings.TrimSpace(status.URL) == "" {
		writeManagementJSON(w, http.StatusConflict, map[string]any{
			"error": "room runtime is not ready",
			"code":  "runtime_not_ready",
			"phase": status.Phase,
		})
		return
	}
	parsed, err := url.Parse(status.URL)
	if err != nil || parsed.User != nil {
		writeManagementError(w, http.StatusBadGateway, "room runtime URL is invalid")
		return
	}
	if _, err := parseLoopbackHTTPBase((&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: "/"}).String()); err != nil {
		writeManagementError(w, http.StatusBadGateway, "room runtime URL is not a numeric loopback endpoint")
		return
	}
	if err := openRoomInBrowser(status.URL); err != nil {
		writeManagementError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeManagementJSON(w, http.StatusOK, map[string]any{"opened": true})
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
	room, _, err := s.archiveRoomByID(r.Context(), r.PathValue("room"))
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

const maxRoomBatchSize = 100

type roomRemovalRequest struct {
	AcknowledgeDataLoss bool `json:"acknowledge_data_loss"`
}

type roomBatchRequest struct {
	RoomIDs []string `json:"room_ids"`
}

type roomBatchRemovalRequest struct {
	RoomIDs             []string `json:"room_ids"`
	AcknowledgeDataLoss bool     `json:"acknowledge_data_loss"`
}

type RoomBatchArchiveItem struct {
	RoomID string `json:"room_id"`
	Status string `json:"status"`
	Room   *Room  `json:"room,omitempty"`
	Error  string `json:"error,omitempty"`
	Code   string `json:"code,omitempty"`
}

type RoomBatchArchiveResult struct {
	Submitted         int                    `json:"submitted"`
	Processed         int                    `json:"processed"`
	Succeeded         int                    `json:"succeeded"`
	Failed            int                    `json:"failed"`
	AlreadyArchived   int                    `json:"already_archived,omitempty"`
	DuplicatesIgnored int                    `json:"duplicates_ignored,omitempty"`
	Results           []RoomBatchArchiveItem `json:"results"`
}

type RoomBatchRemovalItem struct {
	RoomID  string             `json:"room_id"`
	Status  string             `json:"status"`
	Removal *RoomRemovalResult `json:"removal,omitempty"`
	Error   string             `json:"error,omitempty"`
	Code    string             `json:"code,omitempty"`
}

type RoomBatchRemovalResult struct {
	Submitted         int                    `json:"submitted"`
	Processed         int                    `json:"processed"`
	Succeeded         int                    `json:"succeeded"`
	Failed            int                    `json:"failed"`
	DuplicatesIgnored int                    `json:"duplicates_ignored,omitempty"`
	Results           []RoomBatchRemovalItem `json:"results"`
}

func (s *ManagementServer) archiveRoomsBatch(w http.ResponseWriter, r *http.Request) {
	var request roomBatchRequest
	if err := decodeManagementJSON(w, r, &request); err != nil {
		return
	}
	roomIDs, duplicates, err := normalizeRoomBatch(request.RoomIDs)
	if err != nil {
		writeManagementError(w, http.StatusBadRequest, err.Error())
		return
	}

	result := RoomBatchArchiveResult{
		Submitted:         len(request.RoomIDs),
		Processed:         len(roomIDs),
		DuplicatesIgnored: duplicates,
		Results:           make([]RoomBatchArchiveItem, 0, len(roomIDs)),
	}
	for index, roomID := range roomIDs {
		if contextErr := r.Context().Err(); contextErr != nil {
			for _, pendingRoomID := range roomIDs[index:] {
				result.Results = append(result.Results, RoomBatchArchiveItem{
					RoomID: pendingRoomID, Status: "failed",
					Error: contextErr.Error(), Code: managementErrorCode(contextErr, "room_archive_failed"),
				})
				result.Failed++
			}
			break
		}

		room, alreadyArchived, archiveErr := s.archiveRoomByID(r.Context(), roomID)
		if archiveErr != nil {
			result.Results = append(result.Results, RoomBatchArchiveItem{
				RoomID: roomID, Status: "failed",
				Error: archiveErr.Error(), Code: managementErrorCode(archiveErr, "room_archive_failed"),
			})
			result.Failed++
			continue
		}
		roomCopy := room
		status := "archived"
		if alreadyArchived {
			status = "already_archived"
			result.AlreadyArchived++
		}
		result.Results = append(result.Results, RoomBatchArchiveItem{
			RoomID: roomID, Status: status, Room: &roomCopy,
		})
		result.Succeeded++
	}
	writeManagementJSON(w, http.StatusOK, result)
}

func (s *ManagementServer) archiveRoomByID(ctx context.Context, roomID string) (Room, bool, error) {
	unlock := s.lockRoom(roomID)
	defer unlock()
	room, ok := s.registry.Room(roomID)
	if !ok {
		return Room{}, false, ErrRoomNotFound
	}
	if room.Archived() {
		return room, true, nil
	}
	// Archive stops active work by default: it closes the Room mutation gate,
	// interrupts the current Agent Turn so the operator does not have to stop
	// it from inside the Room first, waits for the runtime to settle, and only
	// then suspends it before the lifecycle event is appended.
	if err := s.runtimes.InterruptAndSuspend(ctx, roomID); err != nil {
		return Room{}, false, err
	}
	archived, err := s.registry.ArchiveRoom(ctx, roomID)
	if err != nil {
		return Room{}, false, err
	}
	return archived, false, nil
}

func (s *ManagementServer) removeRoom(w http.ResponseWriter, r *http.Request) {
	var request roomRemovalRequest
	if err := decodeManagementJSON(w, r, &request); err != nil {
		return
	}
	if !request.AcknowledgeDataLoss {
		writeManagementError(w, http.StatusBadRequest, "acknowledge_data_loss must be true")
		return
	}
	roomID := r.PathValue("room")
	result, err := s.removeRoomByID(r.Context(), roomID)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, result)
}

func (s *ManagementServer) removeRoomsBatch(w http.ResponseWriter, r *http.Request) {
	var request roomBatchRemovalRequest
	if err := decodeManagementJSON(w, r, &request); err != nil {
		return
	}
	if !request.AcknowledgeDataLoss {
		writeManagementError(w, http.StatusBadRequest, "acknowledge_data_loss must be true")
		return
	}
	roomIDs, duplicates, err := normalizeRoomBatch(request.RoomIDs)
	if err != nil {
		writeManagementError(w, http.StatusBadRequest, err.Error())
		return
	}

	result := RoomBatchRemovalResult{
		Submitted:         len(request.RoomIDs),
		Processed:         len(roomIDs),
		DuplicatesIgnored: duplicates,
		Results:           make([]RoomBatchRemovalItem, 0, len(roomIDs)),
	}
	for index, roomID := range roomIDs {
		if contextErr := r.Context().Err(); contextErr != nil {
			for _, pendingRoomID := range roomIDs[index:] {
				result.Results = append(result.Results, RoomBatchRemovalItem{
					RoomID: pendingRoomID, Status: "failed",
					Error: contextErr.Error(), Code: managementErrorCode(contextErr, "room_deletion_failed"),
				})
				result.Failed++
			}
			break
		}

		removal, removalErr := s.removeRoomByID(r.Context(), roomID)
		if removalErr != nil {
			result.Results = append(result.Results, RoomBatchRemovalItem{
				RoomID: roomID, Status: "failed",
				Error: removalErr.Error(), Code: managementErrorCode(removalErr, "room_deletion_failed"),
			})
			result.Failed++
			continue
		}
		removalCopy := removal
		result.Results = append(result.Results, RoomBatchRemovalItem{
			RoomID: roomID, Status: "deleted", Removal: &removalCopy,
		})
		result.Succeeded++
	}
	writeManagementJSON(w, http.StatusOK, result)
}

func normalizeRoomBatch(submitted []string) ([]string, int, error) {
	if len(submitted) == 0 {
		return nil, 0, errors.New("room_ids must contain at least one Room ID")
	}
	if len(submitted) > maxRoomBatchSize {
		return nil, 0, fmt.Errorf("room_ids may contain at most %d entries", maxRoomBatchSize)
	}
	roomIDs := make([]string, 0, len(submitted))
	seen := make(map[string]struct{}, len(submitted))
	duplicates := 0
	for index, roomID := range submitted {
		if strings.TrimSpace(roomID) == "" {
			return nil, 0, fmt.Errorf("room_ids[%d] must not be empty", index)
		}
		if strings.TrimSpace(roomID) != roomID {
			return nil, 0, fmt.Errorf("room_ids[%d] must not contain surrounding whitespace", index)
		}
		if _, ok := seen[roomID]; ok {
			duplicates++
			continue
		}
		seen[roomID] = struct{}{}
		roomIDs = append(roomIDs, roomID)
	}
	return roomIDs, duplicates, nil
}

func (s *ManagementServer) removeRoomByID(ctx context.Context, roomID string) (RoomRemovalResult, error) {
	unlock := s.lockRoom(roomID)
	defer unlock()
	room, ok := s.registry.Room(roomID)
	if !ok {
		return RoomRemovalResult{}, ErrRoomNotFound
	}
	if !room.Archived() {
		return RoomRemovalResult{}, &RoomNotArchivedError{RoomID: room.ID, Lifecycle: room.Lifecycle}
	}
	if err := s.runtimes.PrepareRoomDeletion(ctx, roomID); err != nil {
		return RoomRemovalResult{}, err
	}
	result, err := s.registry.RemoveRoom(ctx, roomID)
	if err != nil {
		s.runtimes.AbortRoomDeletion(roomID)
		return RoomRemovalResult{}, err
	}
	s.runtimes.CommitRoomDeletion(roomID)
	return result, nil
}

func (s *ManagementServer) retryRoomDeletionCleanup(w http.ResponseWriter, r *http.Request) {
	maintenance, err := s.registry.RetryRoomDeletionCleanup(r.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, maintenance)
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
	return s.roomLocks.Lock(roomID)
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
		authorization := strings.TrimSpace(r.Header.Get("Authorization"))
		const prefix = "Bearer "
		if strings.HasPrefix(authorization, prefix) && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(strings.TrimPrefix(authorization, prefix))), []byte(s.token)) == 1 {
			next.ServeHTTP(w, withManagementAuth(r, managementRequestAuth{Mode: managementAuthBearer}))
			return
		}
		if value, ok := s.sessions.Get(w, r); ok {
			next.ServeHTTP(w, withManagementAuth(r, managementRequestAuth{Mode: managementAuthBrowserSession, Session: value}))
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="PairRoom Service"`)
		writeManagementError(w, http.StatusUnauthorized, "a valid service bearer token or browser session is required")
	})
}

func (s *ManagementServer) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		auth := managementAuthFromContext(r.Context())
		if auth.Mode != managementAuthBrowserSession {
			next.ServeHTTP(w, r)
			return
		}
		if !auth.Session.ValidCSRF(r.Header.Get(websession.CSRFHeaderName)) {
			writeManagementError(w, http.StatusForbidden, "missing or invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func managementAuthFromContext(ctx context.Context) managementRequestAuth {
	value, _ := ctx.Value(managementAuthContextKey{}).(managementRequestAuth)
	return value
}

func withManagementAuth(r *http.Request, value managementRequestAuth) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), managementAuthContextKey{}, value))
}

func managementSessionCookieName(root string) string {
	sum := sha256.Sum256([]byte(root))
	return "pairroom_management_" + hex.EncodeToString(sum[:8])
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
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		if isRoomSurfacePath(r.URL.Path) {
			applySurfaceFrameHeaders(w.Header())
		} else {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *ManagementServer) writeError(w http.ResponseWriter, err error) {
	var roomLifecycle *RoomNotArchivedError
	if errors.As(err, &roomLifecycle) {
		writeManagementJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(),
			"code":  "room_not_archived",
			"details": map[string]any{
				"room_id":   roomLifecycle.RoomID,
				"lifecycle": roomLifecycle.Lifecycle,
			},
		})
		return
	}

	var projectRooms *ProjectHasRoomsError
	if errors.As(err, &projectRooms) {
		const detailLimit = 20
		roomIDs := append([]string(nil), projectRooms.RoomIDs...)
		truncated := len(roomIDs) > detailLimit
		if truncated {
			roomIDs = roomIDs[:detailLimit]
		}
		writeManagementJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(),
			"code":  "project_has_rooms",
			"details": map[string]any{
				"project_id": projectRooms.ProjectID,
				"room_count": len(projectRooms.RoomIDs),
				"room_ids":   roomIDs,
				"truncated":  truncated,
			},
		})
		return
	}

	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrProjectAlreadyRegistered), errors.Is(err, ErrProjectHasRooms),
		errors.Is(err, ErrBindingOwned), errors.Is(err, ErrRoomBindingPending):
		code = http.StatusConflict
	case errors.Is(err, ErrProjectNotFound), errors.Is(err, ErrRoomNotFound):
		code = http.StatusNotFound
	case errors.Is(err, ErrRegistryFailClosed), errors.Is(err, ErrRuntimeManagerClosed):
		code = http.StatusServiceUnavailable
	case errors.Is(err, ErrRuntimeBusy), errors.Is(err, ErrRuntimeCloseUncertain), errors.Is(err, ErrRuntimeDrainAborted),
		errors.Is(err, ErrRuntimeRoomDeleting), errors.Is(err, ErrRoomNotArchived), errors.Is(err, ErrRuntimeNotReady):
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

func managementErrorCode(err error, fallback string) string {
	var roomLifecycle *RoomNotArchivedError
	if errors.As(err, &roomLifecycle) {
		return "room_not_archived"
	}
	switch {
	case errors.Is(err, ErrRoomNotFound):
		return "room_not_found"
	case errors.Is(err, ErrRegistryFailClosed):
		return "registry_unavailable"
	case errors.Is(err, ErrRuntimeManagerClosed):
		return "runtime_manager_closed"
	case errors.Is(err, ErrRuntimeBusy):
		return "runtime_busy"
	case errors.Is(err, ErrRuntimeCloseUncertain):
		return "runtime_close_uncertain"
	case errors.Is(err, ErrRuntimeDrainAborted):
		return "runtime_drain_aborted"
	case errors.Is(err, ErrRuntimeRoomDeleting):
		return "room_deletion_in_progress"
	case errors.Is(err, ErrRuntimeNotReady):
		return "runtime_not_ready"
	case errors.Is(err, ErrRoomNotArchived):
		return "room_not_archived"
	case errors.Is(err, context.Canceled):
		return "request_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "request_deadline_exceeded"
	default:
		return fallback
	}
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
