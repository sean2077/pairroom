package service

import (
	"context"
	"errors"
	"net/http"

	"github.com/sean2077/pairroom/internal/projectsetup"
)

func (s *ManagementServer) mountProjectNativeSetup(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/projects/{project}/native-setup", s.projectNativeSetup)
	mux.HandleFunc("POST /api/v1/projects/{project}/native-setup", s.projectNativeSetup)
}

func (s *ManagementServer) projectNativeSetup(w http.ResponseWriter, request *http.Request) {
	var install *projectsetup.Request
	if request.Method == http.MethodPost {
		install = new(projectsetup.Request)
		if err := decodeManagementJSON(w, request, install); err != nil {
			return
		}
		if install.Component != "skill" && install.Component != "hook" {
			writeManagementError(w, http.StatusBadRequest, "select skill or hook")
			return
		}
		switch install.Runtime {
		case "claude", "codex", "grok", "gemini":
		default:
			writeManagementError(w, http.StatusBadRequest, "select a supported runtime")
			return
		}
	}
	status, err := s.registry.projectNativeSetup(request.Context(), request.PathValue("project"), install)
	if err != nil {
		switch {
		case errors.Is(err, projectsetup.ErrChanged), errors.Is(err, projectsetup.ErrConfirmation), errors.Is(err, projectsetup.ErrBlocked):
			writeManagementError(w, http.StatusConflict, err.Error())
		default:
			s.writeError(w, err)
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeManagementJSON(w, http.StatusOK, status)
}

func (r *Registry) projectNativeSetup(ctx context.Context, projectID string, install *projectsetup.Request) (projectsetup.Status, error) {
	// Preserve registry identity across inspection/install and concurrent removal.
	// Unlike RefreshProject, an inspection does not write a checkpoint.
	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return projectsetup.Status{}, err
	}
	r.mu.RLock()
	err := r.healthyLocked()
	project, ok := r.projects[projectID]
	r.mu.RUnlock()
	if err != nil {
		return projectsetup.Status{}, err
	}
	if !ok {
		return projectsetup.Status{}, ErrProjectNotFound
	}
	resolved, err := r.resolver.Resolve(ctx, project.Root)
	if err != nil || resolved.ID != project.ID || resolved.Root != project.Root {
		return projectsetup.Status{}, projectsetup.ErrBlocked
	}
	if err := ctx.Err(); err != nil {
		return projectsetup.Status{}, err
	}
	if install == nil {
		return projectsetup.Inspect(project.Root)
	}
	return projectsetup.Install(project.Root, *install)
}
