package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

// Agent defines one Service default slot. It is snapshotted into every newly
// created Room; subsequent Service configuration changes never rewrite it.
// Executables and process arguments come from RuntimeTemplates, never a Room.
type Agent struct {
	Runtime                string                       `json:"runtime,omitempty"`
	Provider               model.ProviderRef            `json:"provider,omitempty"`
	Model                  string                       `json:"model,omitempty"`
	Effort                 string                       `json:"effort,omitempty"`
	PermissionMode         string                       `json:"permission_mode,omitempty"`
	ApprovalPolicy         string                       `json:"approval_policy,omitempty"`
	Sandbox                string                       `json:"sandbox,omitempty"`
	Instructions           string                       `json:"instructions,omitempty"`
	OrdinaryReviewerPolicy model.OrdinaryReviewerPolicy `json:"ordinary_reviewer_policy,omitempty"`
}

func (a Agent) RuntimeKind(slot model.ActorID) model.RuntimeKind {
	return model.ParseRuntimeKind(a.Runtime).CanonicalForSlot(slot)
}

func (a Agent) Selection(slot model.ActorID) model.AgentSelection {
	return model.AgentSelection{
		Runtime: a.RuntimeKind(slot), Provider: a.Provider, Model: a.Model,
		Effort: a.Effort, Instructions: a.Instructions,
		PermissionMode: a.PermissionMode, ApprovalPolicy: a.ApprovalPolicy, Sandbox: a.Sandbox,
		OrdinaryReviewerPolicy: a.OrdinaryReviewerPolicy,
	}.Normalized(slot)
}

type RuntimeTemplate struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

type RuntimeTemplates struct {
	Claude RuntimeTemplate `json:"claude"`
	Codex  RuntimeTemplate `json:"codex"`
	Grok   RuntimeTemplate `json:"grok"`
}

func (r RuntimeTemplates) For(kind model.RuntimeKind) RuntimeTemplate {
	switch kind.Canonical() {
	case model.RuntimeCodex:
		return cloneRuntimeTemplate(r.Codex)
	case model.RuntimeGrok:
		return cloneRuntimeTemplate(r.Grok)
	default:
		return cloneRuntimeTemplate(r.Claude)
	}
}

func cloneRuntimeTemplate(in RuntimeTemplate) RuntimeTemplate {
	in.Args = append([]string(nil), in.Args...)
	return in
}

type CCSwitch struct {
	// Database is empty for ~/.cc-switch/cc-switch.db. Any override must be
	// absolute so a daemon working-directory change cannot retarget it.
	Database string `json:"database,omitempty"`
}

type File struct {
	Listen              string           `json:"listen"`
	RoomName            string           `json:"room_name,omitempty"`
	StallWarningSeconds int              `json:"stall_warning_seconds"`
	AutoStart           bool             `json:"auto_start"`
	Token               string           `json:"token,omitempty"`
	CCSwitch            CCSwitch         `json:"cc_switch,omitempty"`
	Runtimes            RuntimeTemplates `json:"runtimes"`
	Claude              Agent            `json:"claude"`
	Codex               Agent            `json:"codex"`
}

func Defaults() File {
	return File{
		Listen:              "127.0.0.1:7332",
		RoomName:            "Claude × Codex",
		StallWarningSeconds: 300,
		AutoStart:           true,
		Runtimes: RuntimeTemplates{
			Claude: RuntimeTemplate{Command: "claude"},
			Codex:  RuntimeTemplate{Command: "codex"},
			Grok:   RuntimeTemplate{Command: "grok"},
		},
		Claude: Agent{Runtime: string(model.RuntimeClaude), Provider: model.NativeProviderRef(), PermissionMode: "yolo"},
		Codex:  Agent{Runtime: string(model.RuntimeCodex), Provider: model.NativeProviderRef(), ApprovalPolicy: "yolo", Sandbox: "danger-full-access"},
	}
}

type MigrationError struct{ Detail string }

func (e *MigrationError) Error() string { return e.Detail }

func Load(path string) (File, error) {
	cfg := Defaults()
	if path == "" {
		return cfg, cfg.Validate()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read config: %w", err)
	}
	raw, err := configObject(data)
	if err != nil {
		return File{}, fmt.Errorf("decode config: %w", err)
	}
	if raw == nil {
		return File{}, errors.New("config must contain one JSON object")
	}
	if err := rejectRemovedProviderConfig(raw); err != nil {
		return File{}, err
	}
	// Choose defaults in runtime identity, not historical slot identity, before
	// decoding explicit fields. Post-decode conversion cannot distinguish an
	// omitted policy from an explicit empty/native override.
	for _, entry := range []struct {
		key   string
		actor model.ActorID
		agent *Agent
	}{
		{"claude", model.ActorClaude, &cfg.Claude},
		{"codex", model.ActorCodex, &cfg.Codex},
	} {
		if value, ok := raw[entry.key]; ok {
			fields, err := configObject(value)
			if err != nil {
				return File{}, fmt.Errorf("%s config must be a JSON object", entry.key)
			}
			for _, key := range []string{"runtime", "permission_mode", "approval_policy", "sandbox"} {
				if bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
					return File{}, fmt.Errorf("%s.%s must be a string; use an empty string for native inheritance", entry.key, key)
				}
			}
			var selector struct {
				Runtime string `json:"runtime"`
			}
			if value := fields["runtime"]; value != nil {
				if err := json.Unmarshal(value, &selector.Runtime); err != nil {
					return File{}, fmt.Errorf("decode %s config: %w", entry.key, err)
				}
			}
			entry.agent.PermissionMode, entry.agent.ApprovalPolicy, entry.agent.Sandbox = "", "", ""
			if model.ParseRuntimeKind(selector.Runtime).CanonicalForSlot(entry.actor) == model.RuntimeCodex {
				entry.agent.ApprovalPolicy, entry.agent.Sandbox = "yolo", "danger-full-access"
			} else {
				entry.agent.PermissionMode = "yolo"
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return File{}, fmt.Errorf("decode config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return File{}, err
	}
	return cfg, nil
}

// Match encoding/json's case-insensitive struct fields, but reject ambiguous
// duplicate fields rather than merging two different runtime/policy objects.
func configObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name := strings.ToLower(key.(string))
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate config field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("expected exactly one JSON object")
	}
	return fields, nil
}

func rejectRemovedProviderConfig(raw map[string]json.RawMessage) error {
	if _, ok := raw["providers"]; ok {
		return &MigrationError{Detail: "legacy PairRoom providers configuration was removed; back up the PairRoom data root, migrate Service defaults to a CC Switch profile reference, and remove the top-level providers field (see docs/UPGRADING.md)"}
	}
	if _, ok := raw["cc_connect"]; ok {
		return &MigrationError{Detail: "legacy cc_connect imports were removed; back up the PairRoom data root, migrate Service defaults to a CC Switch profile reference, and remove cc_connect (see docs/UPGRADING.md)"}
	}
	for _, key := range []string{"claude", "codex"} {
		slot, err := configObject(raw[key])
		if err != nil {
			continue
		}
		if value, ok := slot["provider"]; ok && len(value) > 0 && value[0] == '"' {
			return &MigrationError{Detail: fmt.Sprintf("legacy %s.provider names were removed; replace the string with {\"source\":\"native\"} or a CC Switch ProviderRef (see docs/UPGRADING.md)", key)}
		}
		for _, removed := range []string{"command", "args"} {
			if _, ok := slot[removed]; ok {
				return &MigrationError{Detail: fmt.Sprintf("per-slot %s.%s was removed; configure runtimes.%s.%s instead (see docs/UPGRADING.md)", key, removed, key, removed)}
			}
		}
	}
	return nil
}

func (c *File) applyDefaults() {
	defaults := Defaults()
	if strings.TrimSpace(c.Claude.Runtime) == "" {
		c.Claude.Runtime = defaults.Claude.Runtime
	}
	if strings.TrimSpace(c.Codex.Runtime) == "" {
		c.Codex.Runtime = defaults.Codex.Runtime
	}
	if c.Claude.Provider.Source == "" {
		c.Claude.Provider = model.NativeProviderRef()
	}
	if c.Codex.Provider.Source == "" {
		c.Codex.Provider = model.NativeProviderRef()
	}
	if strings.TrimSpace(c.Runtimes.Claude.Command) == "" {
		c.Runtimes.Claude.Command = defaults.Runtimes.Claude.Command
	}
	if strings.TrimSpace(c.Runtimes.Codex.Command) == "" {
		c.Runtimes.Codex.Command = defaults.Runtimes.Codex.Command
	}
	if strings.TrimSpace(c.Runtimes.Grok.Command) == "" {
		c.Runtimes.Grok.Command = defaults.Runtimes.Grok.Command
	}
}

func (c File) DefaultSelections() map[model.ActorID]model.AgentSelection {
	return map[model.ActorID]model.AgentSelection{
		model.ActorClaude: c.Claude.Selection(model.ActorClaude),
		model.ActorCodex:  c.Codex.Selection(model.ActorCodex),
	}
}

func (c File) Validate() error {
	if c.Listen == "" {
		return errors.New("listen address is required")
	}
	if c.StallWarningSeconds != -1 && (c.StallWarningSeconds < 30 || c.StallWarningSeconds > 86400) {
		return errors.New("stall_warning_seconds must be -1 (disabled) or between 30 and 86400")
	}
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
		template := c.Runtimes.For(kind)
		if strings.TrimSpace(template.Command) == "" {
			return fmt.Errorf("runtimes.%s.command is required", kind)
		}
		if strings.ContainsRune(template.Command, '\x00') {
			return fmt.Errorf("runtimes.%s.command contains a NUL byte", kind)
		}
		for _, arg := range template.Args {
			if strings.ContainsRune(arg, '\x00') {
				return fmt.Errorf("runtimes.%s.args contains a NUL byte", kind)
			}
		}
		if err := validateRuntimeTemplateArgs(kind, template.Args); err != nil {
			return fmt.Errorf("runtimes.%s.args: %w", kind, err)
		}
	}
	for _, entry := range []struct {
		actor     model.ActorID
		selection model.AgentSelection
	}{
		{model.ActorClaude, c.Claude.Selection(model.ActorClaude)},
		{model.ActorCodex, c.Codex.Selection(model.ActorCodex)},
	} {
		if err := entry.selection.Validate(entry.actor); err != nil {
			return fmt.Errorf("%s default: %w", model.SlotLabel(entry.actor), err)
		}
	}
	if strings.TrimSpace(c.CCSwitch.Database) != "" && !filepath.IsAbs(c.CCSwitch.Database) {
		return errors.New("cc_switch.database must be an absolute path")
	}
	return nil
}

func validateRuntimeTemplateArgs(kind model.RuntimeKind, args []string) error {
	for index := 0; index < len(args); index++ {
		arg := strings.ToLower(strings.TrimSpace(args[index]))
		forbidden := false
		switch kind.Canonical() {
		case model.RuntimeClaude:
			forbidden = matchesOption(arg, "--model", "--effort", "--permission-mode", "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions")
		case model.RuntimeCodex:
			forbidden = matchesOption(arg, "--model", "-m", "--ask-for-approval", "-a", "--sandbox", "-s", "--dangerously-bypass-approvals-and-sandbox")
			if !forbidden {
				if value, ok := configOptionValue(args, index); ok {
					key := strings.ToLower(strings.TrimSpace(strings.SplitN(value, "=", 2)[0]))
					// TOML permits quoted and spaced dotted keys as well.
					key = strings.NewReplacer("\"", "", "'", "").Replace(key)
					root := strings.TrimSpace(strings.SplitN(key, ".", 2)[0])
					switch root {
					case "model", "model_provider", "model_providers", "model_reasoning_effort", "approval_policy", "sandbox_mode", "sandbox_workspace_write":
						forbidden = true
					}
				}
			}
		case model.RuntimeGrok:
			forbidden = matchesOption(arg, "--model", "-m", "--effort", "--permission-mode", "--always-approve", "--yolo", "--sandbox")
		}
		if forbidden {
			return fmt.Errorf("%q is a per-Room model or policy override; put it in AgentSelection instead", args[index])
		}
	}
	return nil
}

func matchesOption(value string, names ...string) bool {
	for _, name := range names {
		if value == name || strings.HasPrefix(value, name+"=") ||
			(len(name) == 2 && strings.HasPrefix(name, "-") && strings.HasPrefix(value, name)) {
			return true
		}
	}
	return false
}

func configOptionValue(args []string, index int) (string, bool) {
	arg := strings.TrimSpace(args[index])
	if arg == "-c" || arg == "--config" {
		if index+1 < len(args) {
			return args[index+1], true
		}
		return "", false
	}
	for _, prefix := range []string{"--config=", "-c=", "-c"} {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix), true
		}
	}
	return "", false
}
