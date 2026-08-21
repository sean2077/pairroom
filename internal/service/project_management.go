package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrProjectHasRooms = errors.New("project still owns rooms")

// ProjectHasRoomsError reports why a Project cannot be removed from the
// Registry. Archived Rooms intentionally count: archive is a reversible
// retention state, not permanent deletion, and Room Event Logs remain the
// authoritative source of Project identity during Registry rebuilds.
type ProjectHasRoomsError struct {
	ProjectID string
	RoomIDs   []string
}

func (e *ProjectHasRoomsError) Error() string {
	if e == nil {
		return ErrProjectHasRooms.Error()
	}
	count := len(e.RoomIDs)
	if count == 0 {
		return fmt.Sprintf("%v: %s", ErrProjectHasRooms, e.ProjectID)
	}
	const sampleLimit = 3
	sample := e.RoomIDs
	if len(sample) > sampleLimit {
		sample = sample[:sampleLimit]
	}
	detail := strings.Join(sample, ", ")
	if count > len(sample) {
		detail += fmt.Sprintf(" and %d more", count-len(sample))
	}
	return fmt.Sprintf("%v: project %s owns %d room(s): %s", ErrProjectHasRooms, e.ProjectID, count, detail)
}

func (e *ProjectHasRoomsError) Unwrap() error { return ErrProjectHasRooms }

// RemoveProject unregisters an empty Project from the Service Registry. It
// never removes the Git worktree or any PairRoom data. A Project that owns even
// one archived Room is rejected because Room Event Logs would reconstruct the
// Project on restart and because archive is deliberately non-destructive.
func (r *Registry) RemoveProject(ctx context.Context, projectID string) (Project, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return Project{}, errors.New("project ID is required")
	}

	// Serialize with provisioning and every other Registry mutation. This gives
	// remove-vs-provision a deterministic outcome: either removal wins and the
	// provision sees ErrProjectNotFound, or the Room commits and removal refuses.
	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.healthyLocked(); err != nil {
		return Project{}, err
	}
	project, ok := r.projects[projectID]
	if !ok {
		return Project{}, ErrProjectNotFound
	}

	roomIDs := make([]string, 0)
	for _, room := range r.rooms {
		if room.ProjectID == projectID {
			roomIDs = append(roomIDs, room.ID)
		}
	}
	if len(roomIDs) > 0 {
		sort.Strings(roomIDs)
		return Project{}, &ProjectHasRoomsError{ProjectID: projectID, RoomIDs: roomIDs}
	}

	// Removal is keyed only by the persisted Registry identity. Deliberately do
	// not resolve or stat project.Root here: unavailable or deleted worktrees
	// must remain removable from the control plane.
	indexedID, indexed := r.projectByRoot[project.Root]
	if !indexed || indexedID != project.ID {
		return Project{}, r.poisonLocked(fmt.Errorf("project root index is inconsistent for %s at %s", project.ID, project.Root))
	}
	delete(r.projects, project.ID)
	delete(r.projectByRoot, project.Root)
	published, err := r.writeCheckpointLocked()
	if err != nil {
		if published {
			return Project{}, r.poisonLocked(fmt.Errorf("project removal checkpoint was replaced but directory sync failed: %w", err))
		}
		r.projects[project.ID] = project
		r.projectByRoot[project.Root] = project.ID
		return Project{}, fmt.Errorf("persist project removal: %w", err)
	}
	return project, nil
}

// RefreshProject re-resolves one registered Project and persists its current
// availability diagnostic. Resolver failures are represented in the returned
// Project instead of failing the request; only Registry, persistence, lookup,
// or request-context failures are returned as errors.
func (r *Registry) RefreshProject(ctx context.Context, projectID string) (Project, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return Project{}, errors.New("project ID is required")
	}

	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}

	r.mu.RLock()
	if err := r.healthyLocked(); err != nil {
		r.mu.RUnlock()
		return Project{}, err
	}
	project, ok := r.projects[projectID]
	r.mu.RUnlock()
	if !ok {
		return Project{}, ErrProjectNotFound
	}

	refreshed := project
	resolved, resolveErr := r.resolver.Resolve(ctx, project.Root)
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}
	switch {
	case resolveErr != nil:
		refreshed.Available = false
		refreshed.Diagnostic = resolveErr.Error()
	case resolved.ID != project.ID || resolved.Root != project.Root:
		refreshed.Available = false
		refreshed.Diagnostic = fmt.Sprintf("Project Identity now resolves to %s at %s", resolved.ID, resolved.Root)
	default:
		refreshed.Available = true
		refreshed.Diagnostic = ""
	}
	if refreshed.Available == project.Available && refreshed.Diagnostic == project.Diagnostic {
		return refreshed, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.healthyLocked(); err != nil {
		return Project{}, err
	}
	current, ok := r.projects[projectID]
	if !ok {
		return Project{}, ErrProjectNotFound
	}
	// provisionMu prevents a concurrent Registry mutation, but retain the
	// defensive identity check so future call sites cannot silently overwrite a
	// changed canonical root.
	if current.Root != project.Root || current.ID != project.ID {
		return Project{}, r.poisonLocked(fmt.Errorf("project identity changed while refreshing %s", projectID))
	}
	r.projects[projectID] = refreshed
	published, err := r.writeCheckpointLocked()
	if err != nil {
		if published {
			return Project{}, r.poisonLocked(fmt.Errorf("project refresh checkpoint was replaced but directory sync failed: %w", err))
		}
		r.projects[projectID] = current
		return Project{}, fmt.Errorf("persist project refresh: %w", err)
	}
	return refreshed, nil
}
