package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/sean2077/pairroom/internal/model"
)

type Agent struct {
	Command        string            `json:"command"`
	Args           []string          `json:"args,omitempty"`
	Model          string            `json:"model"`
	Effort         string            `json:"effort,omitempty"`
	PermissionMode string            `json:"permission_mode,omitempty"`
	ApprovalPolicy string            `json:"approval_policy,omitempty"`
	Sandbox        string            `json:"sandbox,omitempty"`
	Provider       string            `json:"provider,omitempty"`
	RuntimeEnv     map[string]string `json:"-"`
}

// CCConnectImport references cc-connect's existing provider source instead
// of copying credentials into PairRoom configuration. Providers can be
// filtered by name and optionally prefixed to avoid local-name collisions.
type CCConnectImport struct {
	Path      string   `json:"path,omitempty"`
	Providers []string `json:"providers,omitempty"`
	Prefix    string   `json:"prefix,omitempty"`
}

type ProviderModel struct {
	Model string `json:"model"`
	Alias string `json:"alias,omitempty"`
}

type CodexProvider struct {
	EnvKey      string            `json:"env_key,omitempty"`
	WireAPI     string            `json:"wire_api,omitempty"`
	HTTPHeaders map[string]string `json:"http_headers,omitempty"`
}

// Provider mirrors the useful, vendor-neutral subset of cc-connect's
// provider schema. APIKey is retained only in memory and is passed to the
// native process through its environment, never through command arguments.
type Provider struct {
	Name            string                     `json:"name"`
	APIKey          string                     `json:"api_key,omitempty"`
	BaseURL         string                     `json:"base_url,omitempty"`
	Model           string                     `json:"model,omitempty"`
	Models          []ProviderModel            `json:"models,omitempty"`
	Thinking        string                     `json:"thinking,omitempty"`
	Env             map[string]string          `json:"env,omitempty"`
	AgentTypes      []string                   `json:"agent_types,omitempty"`
	Endpoints       map[string]string          `json:"endpoints,omitempty"`
	AgentModels     map[string]string          `json:"agent_models,omitempty"`
	AgentModelLists map[string][]ProviderModel `json:"agent_model_lists,omitempty"`
	Codex           *CodexProvider             `json:"codex,omitempty"`
	ImportedFrom    string                     `json:"-"`
}

type ProviderSummary struct {
	Name         string   `json:"name"`
	AgentTypes   []string `json:"agent_types,omitempty"`
	BaseURL      string   `json:"base_url,omitempty"`
	Model        string   `json:"model,omitempty"`
	ImportedFrom string   `json:"imported_from,omitempty"`
}

type File struct {
	Listen              string            `json:"listen"`
	RoomName            string            `json:"room_name,omitempty"`
	RoutingMode         model.RoutingMode `json:"routing_mode"`
	MaxAgentHops        int               `json:"max_agent_hops"`
	StallWarningSeconds int               `json:"stall_warning_seconds"`
	AutoStart           bool              `json:"auto_start"`
	Token               string            `json:"token,omitempty"`
	Providers           []Provider        `json:"providers,omitempty"`
	CCConnect           *CCConnectImport  `json:"cc_connect,omitempty"`
	Claude              Agent             `json:"claude"`
	Codex               Agent             `json:"codex"`
}

func Defaults() File {
	return File{
		Listen:              "127.0.0.1:7332",
		RoomName:            "Claude × Codex",
		RoutingMode:         model.RoutingTurns,
		MaxAgentHops:        6,
		StallWarningSeconds: 300,
		AutoStart:           true,
		Claude:              Agent{Command: "claude", PermissionMode: "auto"},
		Codex:               Agent{Command: "codex", Effort: "high", ApprovalPolicy: "untrusted", Sandbox: "workspaceWrite"},
	}
}

func Load(path string) (File, error) {
	cfg := Defaults()
	if path == "" {
		if err := cfg.resolveProviderProfiles(""); err != nil {
			return File{}, err
		}
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return File{}, fmt.Errorf("decode config: %w", err)
	}
	if mode, ok := cfg.RoutingMode.Canonical(); ok {
		cfg.RoutingMode = mode
	}
	if err := cfg.resolveProviderProfiles(path); err != nil {
		return File{}, err
	}
	if err := cfg.Validate(); err != nil {
		return File{}, err
	}
	return cfg, nil
}

func (c File) Validate() error {
	if c.Listen == "" {
		return errors.New("listen address is required")
	}
	if mode, ok := c.RoutingMode.Canonical(); !ok || mode != model.RoutingTurns {
		return fmt.Errorf("invalid routing mode %q", c.RoutingMode)
	}
	if c.MaxAgentHops < 1 || c.MaxAgentHops > 30 {
		return errors.New("max_agent_hops must be between 1 and 30")
	}
	if c.StallWarningSeconds != -1 && (c.StallWarningSeconds < 30 || c.StallWarningSeconds > 86400) {
		return errors.New("stall_warning_seconds must be -1 (disabled) or between 30 and 86400")
	}
	if c.Claude.Command == "" || c.Codex.Command == "" {
		return errors.New("both agent commands are required")
	}
	return validateProviderNames(c.Providers)
}

func (c File) ProviderSummaries() []ProviderSummary {
	summaries := make([]ProviderSummary, 0, len(c.Providers))
	for _, provider := range c.Providers {
		summaries = append(summaries, ProviderSummary{
			Name: provider.Name, AgentTypes: append([]string(nil), provider.AgentTypes...),
			BaseURL: redactedProviderURL(provider.BaseURL), Model: provider.Model, ImportedFrom: provider.ImportedFrom,
		})
	}
	return summaries
}

func redactedProviderURL(value string) string {
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "redacted-invalid-url"
	}
	if parsed.User != nil {
		parsed.User = url.User("redacted")
	}
	if parsed.RawQuery != "" {
		parsed.RawQuery = "redacted"
	}
	if parsed.Fragment != "" {
		parsed.Fragment = "redacted"
	}
	return parsed.String()
}
