package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/sean2077/pairroom/internal/model"
)

const (
	agentPairProfilesPath = "/api/v1/agent-pair-profiles"
	agentPairProfilesFile = "agent-pair-profiles.json"
	maxAgentPairProfiles  = 100
	maxPairProfilesBytes  = 16 << 20
)

var (
	errInvalidAgentPairProfile  = errors.New("invalid Agent pair profile")
	errAgentPairProfileNotFound = errors.New("Agent pair profile not found")
	errAgentPairProfileConflict = errors.New("Agent pair profile name already exists")
	errAgentPairProfilesLimit   = errors.New("at most 100 Agent pair profiles may be saved")
	errAgentPairProfilesStore   = errors.New("Agent pair profiles are unavailable; repair the profile file before retrying")
)

// AgentPairProfile is a reusable, secret-free pair of selections, not a Room or
// a CC Switch Provider Profile. No Project, Binding, or collaboration is saved.
type AgentPairProfile struct {
	ID     string                                 `json:"id"`
	Name   string                                 `json:"name"`
	Agents map[model.ActorID]model.AgentSelection `json:"agents"`
}

type AgentPairProfileCatalog struct {
	Schema           int                `json:"schema"`
	DefaultProfileID string             `json:"default_profile_id"`
	Profiles         []AgentPairProfile `json:"profiles"`
}

type AgentPairProfileInput struct {
	Name      string                                 `json:"name"`
	Agents    map[model.ActorID]model.AgentSelection `json:"agents"`
	IsDefault bool                                   `json:"is_default"`
}

func validateAgentPairProfileName(name string) error {
	if strings.TrimSpace(name) == "" || len(name) > 160 || strings.ContainsFunc(name, unicode.IsControl) {
		return errors.New("Agent pair profile name must contain 1–160 bytes and no control characters")
	}
	return nil
}

// The profile file is user configuration, not part of the rebuildable Registry
// checkpoint. Read it afresh under the Registry lock: failed writes cannot leave
// an optimistic in-memory value, and rebuilding the index never drops profiles.
func (r *Registry) AgentPairProfiles() (AgentPairProfileCatalog, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.readAgentPairProfilesLocked()
}

func (r *Registry) readAgentPairProfilesLocked() (AgentPairProfileCatalog, error) {
	catalog := AgentPairProfileCatalog{Schema: 1, Profiles: []AgentPairProfile{}}
	path := filepath.Join(r.root, agentPairProfilesFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return catalog, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPairProfilesBytes {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	file, err := os.Open(path)
	if err != nil {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	catalog = AgentPairProfileCatalog{}
	decoder := json.NewDecoder(io.LimitReader(file, maxPairProfilesBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	if catalog.Schema != 1 || catalog.Profiles == nil || len(catalog.Profiles) > maxAgentPairProfiles {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	ids, names := map[string]bool{}, []string{}
	for i, profile := range catalog.Profiles {
		if profile.ID == "" || profile.ID == "." || profile.ID == ".." || len(profile.ID) > 128 || strings.ContainsAny(profile.ID, "/\\") || strings.ContainsFunc(profile.ID, unicode.IsSpace) || strings.ContainsFunc(profile.ID, unicode.IsControl) || ids[profile.ID] {
			return AgentPairProfileCatalog{}, errAgentPairProfilesStore
		}
		if validateAgentPairProfileName(profile.Name) != nil || profile.Name != strings.TrimSpace(profile.Name) {
			return AgentPairProfileCatalog{}, errAgentPairProfilesStore
		}
		for _, name := range names {
			if strings.EqualFold(name, profile.Name) {
				return AgentPairProfileCatalog{}, errAgentPairProfilesStore
			}
		}
		agents, err := validateAgentSelections(profile.Agents)
		if err != nil {
			return AgentPairProfileCatalog{}, errAgentPairProfilesStore
		}
		catalog.Profiles[i].Agents = agents
		ids[profile.ID] = true
		names = append(names, profile.Name)
	}
	if catalog.DefaultProfileID != "" && !ids[catalog.DefaultProfileID] {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	return catalog, nil
}

func (r *Registry) mutateAgentPairProfiles(ctx context.Context, change func(*AgentPairProfileCatalog) error) (AgentPairProfileCatalog, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return AgentPairProfileCatalog{}, err
	}
	if err := r.healthyLocked(); err != nil {
		return AgentPairProfileCatalog{}, err
	}
	catalog, err := r.readAgentPairProfilesLocked()
	if err != nil {
		return AgentPairProfileCatalog{}, err
	}
	if err := change(&catalog); err != nil {
		return AgentPairProfileCatalog{}, err
	}
	sort.Slice(catalog.Profiles, func(i, j int) bool { return catalog.Profiles[i].Name < catalog.Profiles[j].Name })
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil || len(data)+1 > maxPairProfilesBytes {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	tmp, err := os.CreateTemp(r.root, ".agent-pair-profiles-*.tmp")
	if err != nil {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	if err := tmp.Sync(); err != nil {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	if err := tmp.Close(); err != nil {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	if err := ctx.Err(); err != nil {
		return AgentPairProfileCatalog{}, err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(r.root, agentPairProfilesFile)); err != nil {
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	if err := syncDir(r.root); err != nil {
		// The rename may already be durable. Never report success or roll back
		// from stale memory; the next read reports the actual on-disk state.
		return AgentPairProfileCatalog{}, errAgentPairProfilesStore
	}
	return catalog, nil
}

func (r *Registry) SaveAgentPairProfile(ctx context.Context, id string, input AgentPairProfileInput) (AgentPairProfileCatalog, error) {
	if err := validateAgentPairProfileName(input.Name); err != nil {
		return AgentPairProfileCatalog{}, fmt.Errorf("%w: %v", errInvalidAgentPairProfile, err)
	}
	name := strings.TrimSpace(input.Name)
	agents, err := validateAgentSelections(input.Agents)
	if err != nil {
		return AgentPairProfileCatalog{}, fmt.Errorf("%w: %v", errInvalidAgentPairProfile, err)
	}
	for actor, selection := range agents {
		// Retired role-policy metadata is not a reusable native override.
		selection.OrdinaryReviewerPolicy = ""
		agents[actor] = selection
	}
	return r.mutateAgentPairProfiles(ctx, func(catalog *AgentPairProfileCatalog) error {
		index := -1
		for i, profile := range catalog.Profiles {
			if profile.ID == id {
				index = i
			} else if strings.EqualFold(profile.Name, name) {
				return errAgentPairProfileConflict
			}
		}
		if id != "" && index < 0 {
			return errAgentPairProfileNotFound
		}
		if id == "" {
			if len(catalog.Profiles) >= maxAgentPairProfiles {
				return errAgentPairProfilesLimit
			}
			id = model.NewID("pair")
			index = len(catalog.Profiles)
			catalog.Profiles = append(catalog.Profiles, AgentPairProfile{})
		}
		catalog.Profiles[index] = AgentPairProfile{ID: id, Name: name, Agents: agents}
		if input.IsDefault {
			catalog.DefaultProfileID = id
		} else if catalog.DefaultProfileID == id {
			catalog.DefaultProfileID = ""
		}
		return nil
	})
}

func (r *Registry) SetDefaultAgentPairProfile(ctx context.Context, id string) (AgentPairProfileCatalog, error) {
	return r.mutateAgentPairProfiles(ctx, func(catalog *AgentPairProfileCatalog) error {
		if id != "" {
			found := false
			for _, profile := range catalog.Profiles {
				found = found || profile.ID == id
			}
			if !found {
				return errAgentPairProfileNotFound
			}
		}
		catalog.DefaultProfileID = id
		return nil
	})
}

func (r *Registry) DeleteAgentPairProfile(ctx context.Context, id string) (AgentPairProfileCatalog, error) {
	return r.mutateAgentPairProfiles(ctx, func(catalog *AgentPairProfileCatalog) error {
		for i, profile := range catalog.Profiles {
			if profile.ID == id {
				catalog.Profiles = append(catalog.Profiles[:i], catalog.Profiles[i+1:]...)
				if catalog.DefaultProfileID == id {
					catalog.DefaultProfileID = ""
				}
				return nil
			}
		}
		return errAgentPairProfileNotFound
	})
}

// Resolve once at creation. Existing Rooms retain their own immutable copy;
// changing or deleting a profile cannot reconfigure a native session.
func (r *Registry) agentPairProfileSelections(id string) (map[model.ActorID]model.AgentSelection, error) {
	catalog, err := r.AgentPairProfiles()
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = catalog.DefaultProfileID
	}
	if id == "" {
		return nil, nil
	}
	for _, profile := range catalog.Profiles {
		if profile.ID == id {
			return cloneAgentSelections(profile.Agents), nil
		}
	}
	return nil, errAgentPairProfileNotFound
}

func (s *ManagementServer) mountAgentPairProfiles(mux *http.ServeMux) {
	mux.HandleFunc("GET "+agentPairProfilesPath, func(w http.ResponseWriter, r *http.Request) {
		catalog, err := s.registry.AgentPairProfiles()
		s.writeAgentPairProfiles(w, http.StatusOK, catalog, err)
	})
	save := func(w http.ResponseWriter, r *http.Request) {
		var input AgentPairProfileInput
		if err := decodeManagementJSON(w, r, &input); err != nil {
			return
		}
		catalog, err := s.registry.SaveAgentPairProfile(r.Context(), r.PathValue("profile"), input)
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		s.writeAgentPairProfiles(w, status, catalog, err)
	}
	mux.HandleFunc("POST "+agentPairProfilesPath, save)
	mux.HandleFunc("PUT "+agentPairProfilesPath+"/{profile}", save)
	mux.HandleFunc("PATCH "+agentPairProfilesPath+"/default", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ProfileID *string `json:"profile_id"`
		}
		if err := decodeManagementJSON(w, r, &input); err != nil {
			return
		}
		if input.ProfileID == nil {
			writeManagementError(w, http.StatusBadRequest, "profile_id is required; use an empty string to clear the default")
			return
		}
		catalog, err := s.registry.SetDefaultAgentPairProfile(r.Context(), *input.ProfileID)
		s.writeAgentPairProfiles(w, http.StatusOK, catalog, err)
	})
	mux.HandleFunc("DELETE "+agentPairProfilesPath+"/{profile}", func(w http.ResponseWriter, r *http.Request) {
		catalog, err := s.registry.DeleteAgentPairProfile(r.Context(), r.PathValue("profile"))
		s.writeAgentPairProfiles(w, http.StatusOK, catalog, err)
	})
}

func (s *ManagementServer) writeAgentPairProfiles(w http.ResponseWriter, status int, catalog AgentPairProfileCatalog, err error) {
	if err == nil {
		writeManagementJSON(w, status, catalog)
		return
	}
	switch {
	case errors.Is(err, errInvalidAgentPairProfile):
		writeManagementError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errAgentPairProfileNotFound):
		writeManagementError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errAgentPairProfileConflict), errors.Is(err, errAgentPairProfilesLimit):
		writeManagementError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errAgentPairProfilesStore):
		writeManagementError(w, http.StatusServiceUnavailable, err.Error())
	default:
		s.writeError(w, fmt.Errorf("Agent pair profile: %w", err))
	}
}
