package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

const navigationOrderPath = "/api/v1/navigation-order"
const navigationOrderFile = "navigation-order.json"
const maxNavigationOrderBytes = 4 << 20

var errNavigationOrderStore = errors.New("navigation order is unavailable; repair navigation-order.json before retrying")
var errNavigationOrderMove = errors.New("invalid navigation move: use distinct existing items in the same Project and lifecycle group")

// NavigationOrder is Service-scoped display preference, not runtime scheduling
// or Room ownership. Keeping it outside the rebuildable Registry preserves it
// across restarts/index repair without appending cosmetic Room events.
type NavigationOrder struct {
	Schema   int                 `json:"schema"`
	Projects []string            `json:"projects"`
	Rooms    map[string][]string `json:"rooms"`
}

type NavigationMove struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	TargetID string `json:"target_id"`
	Position string `json:"position"`
}

func (r *Registry) NavigationOrder() (NavigationOrder, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.readNavigationOrderLocked()
}

func (r *Registry) readNavigationOrderLocked() (NavigationOrder, error) {
	order := NavigationOrder{Schema: 1, Projects: []string{}, Rooms: map[string][]string{}}
	path := filepath.Join(r.root, navigationOrderFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return order, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxNavigationOrderBytes {
		return NavigationOrder{}, errNavigationOrderStore
	}
	file, err := os.Open(path)
	if err != nil {
		return NavigationOrder{}, errNavigationOrderStore
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return NavigationOrder{}, errNavigationOrderStore
	}
	order = NavigationOrder{}
	decoder := json.NewDecoder(io.LimitReader(file, maxNavigationOrderBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&order); err != nil {
		return NavigationOrder{}, errNavigationOrderStore
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return NavigationOrder{}, errNavigationOrderStore
	}
	if order.Schema != 1 || order.Projects == nil || order.Rooms == nil || !validOrderIDs(order.Projects) {
		return NavigationOrder{}, errNavigationOrderStore
	}
	for project, ids := range order.Rooms {
		if !validOrderIDs([]string{project}) || ids == nil || !validOrderIDs(ids) {
			return NavigationOrder{}, errNavigationOrderStore
		}
	}
	return order, nil
}

func validOrderIDs(ids []string) bool {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || id == "." || id == ".." || len(id) > 128 || strings.ContainsAny(id, "/\\") || strings.ContainsFunc(id, unicode.IsSpace) || strings.ContainsFunc(id, unicode.IsControl) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// mergeOrder preserves explicit ranks, drops removed IDs, and appends newly
// discovered items in the stable Registry order. Hidden/filtered items are not
// lost when a client moves just one item relative to another.
func mergeOrder(saved, current []string) []string {
	available := make(map[string]bool, len(current))
	for _, id := range current {
		available[id] = true
	}
	result := make([]string, 0, len(current))
	for _, id := range saved {
		if available[id] {
			result = append(result, id)
			delete(available, id)
		}
	}
	for _, id := range current {
		if available[id] {
			result = append(result, id)
		}
	}
	return result
}

func (r *Registry) MoveNavigation(ctx context.Context, move NavigationMove) (NavigationOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return NavigationOrder{}, err
	}
	if err := r.healthyLocked(); err != nil {
		return NavigationOrder{}, err
	}
	if move.ID == move.TargetID || (move.Position != "before" && move.Position != "after") {
		return NavigationOrder{}, errNavigationOrderMove
	}
	projectID := ""
	switch move.Kind {
	case "project":
		if _, ok := r.projects[move.ID]; !ok {
			return NavigationOrder{}, errNavigationOrderMove
		}
		if _, ok := r.projects[move.TargetID]; !ok {
			return NavigationOrder{}, errNavigationOrderMove
		}
	case "room":
		room, ok := r.rooms[move.ID]
		target, targetOK := r.rooms[move.TargetID]
		if !ok || !targetOK || room.ProjectID != target.ProjectID || room.Archived() != target.Archived() {
			return NavigationOrder{}, errNavigationOrderMove
		}
		projectID = room.ProjectID
	default:
		return NavigationOrder{}, errNavigationOrderMove
	}
	order, err := r.readNavigationOrderLocked()
	if err != nil {
		return NavigationOrder{}, err
	}
	snapshot := RegistrySnapshot{}
	for _, project := range r.projects {
		snapshot.Projects = append(snapshot.Projects, project)
	}
	for _, room := range r.rooms {
		snapshot.Rooms = append(snapshot.Rooms, room)
	}
	snapshot = snapshot.Sorted()
	projects := make([]string, 0, len(snapshot.Projects))
	rooms := make(map[string][]string)
	for _, project := range snapshot.Projects {
		projects = append(projects, project.ID)
		rooms[project.ID] = []string{}
	}
	for _, room := range snapshot.Rooms {
		rooms[room.ProjectID] = append(rooms[room.ProjectID], room.ID)
	}
	order.Projects = mergeOrder(order.Projects, projects)
	for id, ids := range rooms {
		rooms[id] = mergeOrder(order.Rooms[id], ids)
	}
	order.Rooms = rooms
	ids := order.Projects
	if move.Kind == "room" {
		ids = order.Rooms[projectID]
	}
	from := slices.Index(ids, move.ID)
	ids = slices.Delete(ids, from, from+1)
	to := slices.Index(ids, move.TargetID)
	if move.Position == "after" {
		to++
	}
	ids = slices.Insert(ids, to, move.ID)
	if move.Kind == "room" {
		order.Rooms[projectID] = ids
	} else {
		order.Projects = ids
	}
	if err := r.writeNavigationOrderLocked(ctx, order); err != nil {
		return NavigationOrder{}, err
	}
	return order, nil
}

func (r *Registry) writeNavigationOrderLocked(ctx context.Context, order NavigationOrder) error {
	data, err := json.MarshalIndent(order, "", "  ")
	if err != nil || len(data)+1 > maxNavigationOrderBytes {
		return errNavigationOrderStore
	}
	file, err := os.CreateTemp(r.root, ".navigation-order-*.tmp")
	if err != nil {
		return errNavigationOrderStore
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return errNavigationOrderStore
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return errNavigationOrderStore
	}
	if err := file.Sync(); err != nil {
		return errNavigationOrderStore
	}
	if err := file.Close(); err != nil {
		return errNavigationOrderStore
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(r.root, navigationOrderFile)); err != nil {
		return errNavigationOrderStore
	}
	// On a directory-sync failure the rename may already be durable. The next
	// request reads disk afresh; never claim success or overwrite from stale memory.
	if err := syncDir(r.root); err != nil {
		return errNavigationOrderStore
	}
	return nil
}

func (s *ManagementServer) mountNavigationOrder(mux *http.ServeMux) {
	mux.HandleFunc("PATCH "+navigationOrderPath, func(w http.ResponseWriter, r *http.Request) {
		var move NavigationMove
		if err := decodeManagementJSON(w, r, &move); err != nil {
			return
		}
		order, err := s.registry.MoveNavigation(r.Context(), move)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, errNavigationOrderMove) {
				status = http.StatusBadRequest
			}
			// Report a stable, non-path-bearing error even when the Registry is poisoned.
			message := errNavigationOrderStore.Error()
			if status == http.StatusBadRequest {
				message = errNavigationOrderMove.Error()
			}
			writeManagementError(w, status, message)
			return
		}
		writeManagementJSON(w, http.StatusOK, order)
	})
}
