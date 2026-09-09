package service

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/execx"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

const diagnosticsPath = "/api/v1/diagnostics"

type diagnosticRequest struct {
	Mode    string        `json:"mode"`
	RoomID  string        `json:"room_id,omitempty"`
	Actor   model.ActorID `json:"actor,omitempty"`
	Confirm bool          `json:"confirm,omitempty"`
}

// DiagnosticReport is an allowlist projection suitable for local download. It
// intentionally excludes Room/Project names and IDs, paths, raw errors, native
// output, selections, environment, arguments, URLs and session identifiers.
type DiagnosticReport struct {
	Schema      int                     `json:"schema"`
	Version     string                  `json:"version"`
	Platform    string                  `json:"platform"`
	GeneratedAt time.Time               `json:"generated_at"`
	Mode        string                  `json:"mode"`
	Scope       string                  `json:"scope"`
	Checks      []agent.DiagnosticCheck `json:"checks"`
}

func (s *ManagementServer) runDiagnostics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request diagnosticRequest
	if decodeManagementJSON(w, r, &request) != nil {
		return
	}
	if request.Mode != "environment" && request.Mode != "runtime" {
		writeManagementError(w, http.StatusBadRequest, "mode must be environment or runtime")
		return
	}
	if (request.Actor != "" && !request.Actor.ValidParticipant()) || (request.Mode == "runtime" && (!request.Actor.ValidParticipant() || !request.Confirm)) {
		writeManagementError(w, http.StatusBadRequest, "runtime checks require an Agent slot and explicit confirmation")
		return
	}
	if !s.diagnosticsMu.TryLock() {
		writeManagementError(w, http.StatusConflict, "a diagnostic check is already running; retry after it finishes or is cancelled")
		return
	}
	defer s.diagnosticsMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
	defer cancel()
	report := DiagnosticReport{Schema: 1, Version: version.Current, Platform: runtime.GOOS + "/" + runtime.GOARCH, GeneratedAt: time.Now().UTC(), Mode: request.Mode, Scope: "service_defaults", Checks: []agent.DiagnosticCheck{}}
	add := func(id, status, code string) {
		report.Checks = append(report.Checks, agent.DiagnosticCheck{ID: id, Status: status, Code: code})
	}
	resolver := s.agentResolver
	var selections map[model.ActorID]model.AgentSelection
	if request.RoomID != "" {
		room, ok := s.registry.Room(request.RoomID)
		if !ok {
			writeManagementError(w, http.StatusNotFound, "Room not found")
			return
		}
		report.Scope, selections = "room", room.Agents
		// Never diagnose with guessed settings or resume bound sessions.
		if len(selections) != 2 && request.Mode == "runtime" {
			writeManagementError(w, http.StatusConflict, "this Room has no valid Agent selections; create a new Room")
			return
		}
	} else if resolver != nil {
		selections = resolver.DefaultSelections()
		profiles, err := s.registry.AgentPairProfiles()
		if err != nil {
			add("selection", "fail", "profile_unavailable")
			if request.Mode == "runtime" {
				writeManagementJSON(w, http.StatusOK, report)
				return
			}
			selections = nil
		}
		for _, profile := range profiles.Profiles {
			if profile.ID == profiles.DefaultProfileID {
				selections, report.Scope = profile.Agents, "default_profile"
				break
			}
		}
	}
	if request.Mode == "environment" {
		if executable, err := os.Executable(); err != nil || !diagnosticExecutableExists(executable) {
			add("application", "fail", "application_unavailable")
		} else {
			add("application", "pass", "application_available")
		}
		if s.registry.Healthy() != nil {
			add("registry", "fail", "registry_unhealthy")
		} else {
			add("registry", "pass", "registry_healthy")
		}
		if diagnosticWritable(s.registry.Root()) {
			add("storage", "pass", "storage_writable")
		} else {
			add("storage", "fail", "storage_unwritable")
		}
		if !diagnosticGit(ctx) {
			add("git", "fail", "git_unavailable")
		} else {
			add("git", "pass", "git_available")
		}
		snapshot := s.registry.Snapshot(true)
		unavailable := false
		for _, project := range snapshot.Projects {
			info, err := os.Stat(project.Root)
			unavailable = unavailable || !project.Available || err != nil || (info != nil && !info.IsDir())
		}
		if unavailable {
			add("projects", "warn", "project_unavailable")
		} else {
			add("projects", "pass", "projects_available")
		}
		failed, queued := false, false
		for _, status := range s.runtimes.Statuses() {
			failed = failed || status.Phase == RuntimeFailed
			queued = queued || status.Phase == RuntimeQueued
		}
		if failed {
			add("rooms", "warn", "room_failed")
		} else {
			add("rooms", "pass", "rooms_healthy")
		}
		if queued {
			add("capacity", "warn", "capacity_queued")
		} else {
			add("capacity", "pass", "capacity_available")
		}
	}
	if resolver == nil {
		add("selection", "skipped", "resolver_unavailable")
		writeManagementJSON(w, http.StatusOK, report)
		return
	}
	if len(selections) == 2 {
		var selectionErr error
		if request.Mode == "runtime" {
			_, selectionErr = validateAgentSelections(selections)
			selected := selections[request.Actor]
			if selectionErr == nil && selected.Provider.Source == model.ProviderCCSwitch {
				_, selectionErr = resolver.ccswitch.Resolve(ctx, selected.Provider, selected.Runtime)
			}
		} else {
			_, selectionErr = resolver.ValidateSelections(ctx, selections)
		}
		if selectionErr != nil {
			add("selection", "fail", "provider_unavailable")
			// A live check must not silently fall back to native/default credentials.
			if request.Mode == "runtime" {
				writeManagementJSON(w, http.StatusOK, report)
				return
			}
		} else {
			add("selection", "pass", "selection_valid")
		}
	}
	if request.Mode == "environment" {
		// Probe concurrently, preserve display order, and bound the whole metadata
		// phase. This does not authenticate or run a model turn.
		kinds := []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok}
		results := make(chan agent.DiagnosticCheck, len(kinds))
		for _, kind := range kinds {
			go func(kind model.RuntimeKind) {
				if resolver.mock {
					results <- agent.DiagnosticCheck{ID: "installation", Runtime: kind, Status: "skipped", Code: "mock"}
					return
				}
				template := resolver.runtimes.For(kind)
				probeCtx, stop := context.WithTimeout(ctx, 12*time.Second)
				defer stop()
				check := agent.CheckInstallation(probeCtx, agent.Config{Runtime: kind, Command: template.Command, CommandArgs: template.Args})
				selected := false
				for _, selection := range selections {
					selected = selected || selection.Runtime.Canonical() == kind
				}
				if !selected && check.Status == "fail" {
					check.Status = "warn"
				}
				results <- check
			}(kind)
		}
		byKind := make(map[model.RuntimeKind]agent.DiagnosticCheck)
		for range kinds {
			check := <-results
			byKind[check.Runtime] = check
		}
		for _, kind := range kinds {
			report.Checks = append(report.Checks, byKind[kind])
		}
		add("response", "skipped", "not_checked")
	} else if resolver.mock {
		add("response", "skipped", "mock")
	} else {
		selection, ok := selections[request.Actor]
		if !ok {
			writeManagementError(w, http.StatusConflict, "Agent selection unavailable")
			return
		}
		// Provider overlays are disposable too; Resolve never writes a real Room.
		overlay, err := os.MkdirTemp("", "pairroom-provider-check-")
		if err != nil {
			add("selection", "fail", "workspace_unavailable")
		} else {
			defer os.RemoveAll(overlay)
			cfg, err := resolver.Resolve(ctx, request.Actor, selection, selections[model.OtherParticipant(request.Actor)].Runtime, overlay, overlay)
			if err != nil {
				add("selection", "fail", "provider_unavailable")
			} else {
				check := agent.CheckInstallation(ctx, cfg)
				report.Checks = append(report.Checks, check)
				if check.Status == "pass" {
					report.Checks = append(report.Checks, agent.CheckRuntime(ctx, cfg)...)
				}
			}
		}
	}
	writeManagementJSON(w, http.StatusOK, report)
}

func diagnosticWritable(root string) bool {
	file, err := os.CreateTemp(root, ".diagnostic-*")
	if err != nil {
		return false
	}
	_, writeErr := file.WriteString("PairRoom storage check\n")
	syncErr := file.Sync()
	closeErr := file.Close()
	removeErr := os.Remove(file.Name())
	return writeErr == nil && syncErr == nil && closeErr == nil && removeErr == nil
}

func diagnosticExecutableExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func diagnosticGit(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--version")
	execx.NoConsole(cmd)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	return cmd.Run() == nil
}
