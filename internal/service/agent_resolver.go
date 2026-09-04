package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/ccswitch"
	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

type RuntimeCatalogEntry struct {
	Runtime       model.RuntimeKind `json:"runtime"`
	DisplayName   string            `json:"display_name"`
	Available     bool              `json:"available"`
	Version       string            `json:"version,omitempty"`
	Diagnostic    string            `json:"diagnostic,omitempty"`
	DefaultModels []string          `json:"default_models,omitempty"`
}

type SafeCatalogError struct {
	Code   string            `json:"code"`
	Error  string            `json:"error"`
	Params map[string]string `json:"params,omitempty"`
}

type AgentCatalog struct {
	Schema        int                                    `json:"schema"`
	GeneratedAt   time.Time                              `json:"generated_at"`
	Runtimes      []RuntimeCatalogEntry                  `json:"runtimes"`
	Profiles      []ccswitch.ProfileSummary              `json:"profiles"`
	ProviderError *SafeCatalogError                      `json:"provider_error,omitempty"`
	Defaults      map[model.ActorID]model.AgentSelection `json:"defaults"`
}

type AgentResolverConfig struct {
	Defaults map[model.ActorID]model.AgentSelection
	Runtimes config.RuntimeTemplates
	CCSwitch *ccswitch.Reader
	Mock     bool
}

// AgentResolver owns the one-way boundary between durable, secret-free Room
// selections and ephemeral native process configuration. Every call resolves
// CC Switch again; no credential or materialization is cached.
type AgentResolver struct {
	defaults map[model.ActorID]model.AgentSelection
	runtimes config.RuntimeTemplates
	ccswitch *ccswitch.Reader
	mock     bool
}

func NewAgentResolver(cfg AgentResolverConfig) (*AgentResolver, error) {
	if cfg.CCSwitch == nil {
		return nil, errors.New("CC Switch reader is required")
	}
	r := &AgentResolver{defaults: cloneAgentSelections(cfg.Defaults), runtimes: cfg.Runtimes, ccswitch: cfg.CCSwitch, mock: cfg.Mock}
	if len(r.defaults) != 2 {
		return nil, errors.New("exactly two Agent defaults are required")
	}
	for _, actor := range model.SlotActors() {
		selection, ok := r.defaults[actor]
		if !ok {
			return nil, fmt.Errorf("%s default is required", model.SlotLabel(actor))
		}
		selection = selection.Normalized(actor)
		if err := selection.Validate(actor); err != nil {
			return nil, fmt.Errorf("%s default: %w", model.SlotLabel(actor), err)
		}
		r.defaults[actor] = selection
	}
	return r, nil
}

func (r *AgentResolver) DefaultSelections() map[model.ActorID]model.AgentSelection {
	if r == nil {
		return nil
	}
	return cloneAgentSelections(r.defaults)
}

func cloneAgentSelections(input map[model.ActorID]model.AgentSelection) map[model.ActorID]model.AgentSelection {
	if input == nil {
		return nil
	}
	output := make(map[model.ActorID]model.AgentSelection, len(input))
	for actor, selection := range input {
		output[actor] = selection
	}
	return output
}

func validateAgentSelections(input map[model.ActorID]model.AgentSelection) (map[model.ActorID]model.AgentSelection, error) {
	if len(input) != 2 {
		return nil, fmt.Errorf("exactly two Agent selections are required; got %d", len(input))
	}
	output := make(map[model.ActorID]model.AgentSelection, 2)
	for actor := range input {
		if !actor.ValidParticipant() {
			return nil, fmt.Errorf("unexpected Agent slot %q", actor)
		}
	}
	for _, actor := range model.SlotActors() {
		selection, ok := input[actor]
		if !ok {
			return nil, fmt.Errorf("%s selection is required", model.SlotLabel(actor))
		}
		selection = selection.Normalized(actor)
		if err := selection.Validate(actor); err != nil {
			return nil, fmt.Errorf("%s selection: %w", model.SlotLabel(actor), err)
		}
		output[actor] = selection
	}
	return output, nil
}

func (r *AgentResolver) Resolve(ctx context.Context, actor model.ActorID, selection model.AgentSelection, peerRuntime model.RuntimeKind, repo, dataDir string) (agent.Config, error) {
	if r == nil {
		return agent.Config{}, errors.New("Agent resolver is required")
	}
	selection = selection.Normalized(actor)
	if err := selection.Validate(actor); err != nil {
		return agent.Config{}, err
	}
	template := r.runtimes.For(selection.Runtime)
	if strings.TrimSpace(template.Command) == "" {
		return agent.Config{}, fmt.Errorf("no command template is configured for %s", selection.Runtime.DisplayName())
	}
	cfg := agent.Config{
		Actor: actor, Repo: repo, DataDir: dataDir, ClientVersion: version.Current,
		Command: template.Command, CommandArgs: append([]string(nil), template.Args...),
		Runtime: selection.Runtime, PeerRuntime: peerRuntime.Canonical(),
		Model: selection.Model, Effort: selection.Effort,
		PermissionMode: selection.PermissionMode, ApprovalPolicy: selection.ApprovalPolicy, Sandbox: selection.Sandbox,
		AdditionalInstructions: selection.Instructions,
		OrdinaryReviewerPolicy: selection.OrdinaryReviewerPolicy,
	}
	if selection.Provider.Source == model.ProviderNative {
		cfg.Provider = "native"
		return cfg, nil
	}
	materialized, err := r.ccswitch.Resolve(ctx, selection.Provider, selection.Runtime)
	if err != nil {
		return agent.Config{}, err
	}
	cfg.Provider = materialized.ProviderLabel
	cfg.Env = copyStringMap(materialized.Env)
	cfg.CommandArgs = append(cfg.CommandArgs, materialized.Args...)
	if cfg.Model == "" {
		cfg.Model = materialized.DefaultModel
	}
	if selection.Runtime.Canonical() == model.RuntimeGrok {
		overlay, effectiveModel, err := ccswitch.RenderGrokOverlay(materialized.Grok, selection.Model)
		if err != nil {
			return agent.Config{}, err
		}
		for _, value := range materialized.Env {
			if strings.TrimSpace(value) != "" && (strings.Contains(overlay, value) || strings.Contains(overlay, strconv.Quote(value))) {
				return agent.Config{}, errors.New("Grok Build provider overlay contains a credential")
			}
		}
		path, err := writeGrokOverlay(dataDir, actor, overlay)
		if err != nil {
			return agent.Config{}, err
		}
		if cfg.Env == nil {
			cfg.Env = make(map[string]string)
		}
		cfg.Env["GROK_CONFIG_PATH"] = path
		cfg.Model = effectiveModel
	}
	return cfg, nil
}

// ValidateSelections is the API-boundary validation pass. It normalizes both
// slots and re-reads every referenced CC Switch Profile immediately before the
// Registry begins provisioning, so stale browser catalog data is never trusted.
func (r *AgentResolver) ValidateSelections(ctx context.Context, input map[model.ActorID]model.AgentSelection) (map[model.ActorID]model.AgentSelection, error) {
	selections, err := validateAgentSelections(input)
	if err != nil {
		return nil, err
	}
	for _, actor := range model.SlotActors() {
		selection := selections[actor]
		if selection.Provider.Source != model.ProviderCCSwitch {
			continue
		}
		if _, err := r.ccswitch.Resolve(ctx, selection.Provider, selection.Runtime); err != nil {
			return nil, fmt.Errorf("%s Provider: %w", model.SlotLabel(actor), err)
		}
	}
	return selections, nil
}

func writeGrokOverlay(dataDir string, actor model.ActorID, content string) (string, error) {
	if strings.TrimSpace(dataDir) == "" || !filepath.IsAbs(dataDir) {
		return "", errors.New("an absolute Room data directory is required for the Grok Build provider overlay")
	}
	dir := filepath.Join(dataDir, "runtime", string(actor), "provider")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create Grok Build provider overlay directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("secure Grok Build provider overlay directory: %w", err)
	}
	sum := sha256.Sum256([]byte(content))
	path := filepath.Join(dir, "cc-switch-"+hex.EncodeToString(sum[:12])+".toml")
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != content {
			return "", errors.New("Grok Build provider overlay hash collision")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return "", fmt.Errorf("secure Grok Build provider overlay: %w", err)
		}
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect Grok Build provider overlay: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".cc-switch-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create Grok Build provider overlay: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("secure Grok Build provider overlay: %w", err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		return "", fmt.Errorf("write Grok Build provider overlay: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync Grok Build provider overlay: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close Grok Build provider overlay: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if existing, readErr := os.ReadFile(path); readErr == nil && string(existing) == content {
			return path, nil
		}
		return "", fmt.Errorf("publish Grok Build provider overlay: %w", err)
	}
	committed = true
	return path, nil
}

func (r *AgentResolver) Catalog(ctx context.Context) AgentCatalog {
	result := AgentCatalog{Schema: 1, GeneratedAt: time.Now().UTC(), Defaults: r.DefaultSelections()}
	ccCatalog, err := r.ccswitch.Catalog(ctx)
	if err != nil {
		result.ProviderError = safeCatalogError(err)
	} else {
		result.Profiles = ccCatalog.Profiles
	}
	modelsByRuntime := make(map[model.RuntimeKind][]string)
	for _, selection := range result.Defaults {
		if selection.Model != "" {
			modelsByRuntime[selection.Runtime] = append(modelsByRuntime[selection.Runtime], selection.Model)
		}
	}
	for _, profile := range result.Profiles {
		modelsByRuntime[profile.Runtime] = append(modelsByRuntime[profile.Runtime], profile.Models...)
	}
	type probeResult struct {
		entry RuntimeCatalogEntry
		index int
	}
	ch := make(chan probeResult, 3)
	for index, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
		go func(index int, kind model.RuntimeKind) {
			entry := RuntimeCatalogEntry{Runtime: kind, DisplayName: kind.DisplayName(), DefaultModels: uniqueCatalogStrings(modelsByRuntime[kind])}
			if r.mock {
				entry.Available, entry.Version = true, "mock"
				ch <- probeResult{entry, index}
				return
			}
			template := r.runtimes.For(kind)
			probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			probe, probeErr := agent.ProbeRuntime(probeCtx, agent.Config{Actor: model.ActorClaude, Runtime: kind, Command: template.Command, CommandArgs: template.Args})
			if probeErr != nil {
				entry.Diagnostic = probeErr.Error()
			} else {
				entry.Available, entry.Version = true, probe.Version
			}
			ch <- probeResult{entry, index}
		}(index, kind)
	}
	entries := make([]RuntimeCatalogEntry, 3)
	for range entries {
		probe := <-ch
		entries[probe.index] = probe.entry
	}
	result.Runtimes = entries
	return result
}

func safeCatalogError(err error) *SafeCatalogError {
	result := &SafeCatalogError{Code: ccswitch.CodeDatabaseUnreadable, Error: err.Error()}
	var typed *ccswitch.Error
	if errors.As(err, &typed) {
		result.Code, result.Params = typed.Code, typed.Params
	}
	return result
}

func uniqueCatalogStrings(values []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func copyStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
