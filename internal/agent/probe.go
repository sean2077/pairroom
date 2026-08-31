package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// ProbeResult is deliberately small and vendor-neutral. It is used both by
// runtime adapters and by `pairroom doctor`, so the UI reports what was actually
// found on the machine instead of only echoing configured values.
type ProbeResult struct {
	Actor          model.ActorID   `json:"actor"`
	Command        string          `json:"command"`
	Path           string          `json:"path"`
	Version        string          `json:"version,omitempty"`
	VersionLine    string          `json:"version_line,omitempty"`
	Protocol       string          `json:"protocol"`
	Capabilities   []string        `json:"capabilities,omitempty"`
	Warnings       []string        `json:"warnings,omitempty"`
	SupportedFlags map[string]bool `json:"-"`
}

var semanticVersionPattern = regexp.MustCompile(`\bv?(\d+)\.(\d+)\.(\d+)(?:[-+][0-9A-Za-z.-]+)?\b`)

// ProbeRuntime performs non-destructive executable and protocol-surface checks.
// It never authenticates, creates a vendor conversation, or reads repository
// files. A short timeout keeps room startup responsive when a wrapper command
// is broken.
func ProbeRuntime(parent context.Context, cfg Config) (ProbeResult, error) {
	actor := cfg.Actor
	command := strings.TrimSpace(cfg.Command)
	if command == "" {
		switch actor {
		case model.ActorClaude:
			command = "claude"
		case model.ActorCodex:
			command = "codex"
		default:
			return ProbeResult{}, fmt.Errorf("unsupported runtime actor %q", actor)
		}
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("locate %s runtime %q: %w", actor.DisplayName(), command, err)
	}

	ctx, cancel := context.WithTimeout(parent, 6*time.Second)
	defer cancel()
	versionLine, err := runProbeCommand(ctx, path, []string{"--version"}, actor)
	if err != nil {
		return ProbeResult{}, err
	}
	result := ProbeResult{
		Actor: actor, Command: command, Path: path,
		Version: extractSemanticVersion(versionLine), VersionLine: firstNonEmptyLine(versionLine),
	}

	switch actor {
	case model.ActorClaude:
		result.Protocol = "claude-stream-json"
		result.SupportedFlags = make(map[string]bool)
		helpCtx, helpCancel := context.WithTimeout(parent, 6*time.Second)
		help, helpErr := runProbeCommand(helpCtx, path, []string{"--help"}, actor)
		helpCancel()

		// input/output stream-json are the protocol's hard requirements, but the
		// official CLI reference explicitly notes that `claude --help` is not an
		// exhaustive flag inventory. Treat these documented flags as available and
		// let process startup provide the authoritative compatibility result instead
		// of rejecting a valid wrapper or release based on incomplete help output.
		for _, flag := range []string{"--input-format", "--output-format", "--permission-prompt-tool"} {
			result.SupportedFlags[flag] = true
		}
		optional := []string{
			"--append-system-prompt", "--append-system-prompt-file",
			"--include-partial-messages", "--replay-user-messages",
			"--forward-subagent-text", "--include-hook-events",
			"--model", "--permission-mode", "--disallowedTools",
			"--resume", "--session-id", "--verbose", "--add-dir",
		}
		if helpErr != nil {
			result.Warnings = append(result.Warnings, "could not inspect optional Claude Code flags: "+helpErr.Error())
			// These long-standing documented flags are safe fallbacks when a
			// wrapper suppresses help output. Newer telemetry flags stay disabled.
			for _, flag := range []string{
				"--append-system-prompt", "--include-partial-messages", "--replay-user-messages",
				"--model", "--permission-mode", "--disallowedTools",
				"--resume", "--session-id", "--verbose", "--add-dir",
			} {
				result.SupportedFlags[flag] = true
			}
		} else {
			for _, flag := range optional {
				result.SupportedFlags[flag] = strings.Contains(help, flag)
			}
		}

		result.Capabilities = []string{"stream-json", "queued-input", "tool-events"}
		if result.SupportedFlags["--resume"] {
			result.Capabilities = append(result.Capabilities, "session-resume")
		} else {
			result.Warnings = append(result.Warnings, "Claude Code does not advertise --resume; native session recovery will start a fresh session")
		}
		if result.SupportedFlags["--add-dir"] {
			result.Capabilities = append(result.Capabilities, "additional-directories")
		}
		// The native initialize handshake is negotiated after process start. The
		// permission-prompt-tool=stdio flag is the documented switch used by the
		// official Agent SDK to make tool approvals arrive on that control channel.
		result.Capabilities = append(result.Capabilities, "control-handshake-pending")
		if result.SupportedFlags["--include-partial-messages"] {
			result.Capabilities = append(result.Capabilities, "partial-messages")
		}
		if result.SupportedFlags["--forward-subagent-text"] || versionAtLeast(result.Version, 2, 1, 211) {
			result.SupportedFlags["--forward-subagent-text"] = true
			result.Capabilities = append(result.Capabilities, "subagent-text-forwarding")
		} else if result.Version == "" {
			result.Warnings = append(result.Warnings, "could not parse Claude Code version; optional subagent text forwarding is disabled")
		} else {
			result.Warnings = append(result.Warnings, "Claude Code before 2.1.211 does not support --forward-subagent-text; PairRoom will run without forwarded subagent text")
		}
		if result.SupportedFlags["--include-hook-events"] {
			result.Capabilities = append(result.Capabilities, "hook-events")
		}
	case model.ActorCodex:
		result.Protocol = "codex-app-server-jsonrpc"
		helpCtx, helpCancel := context.WithTimeout(parent, 6*time.Second)
		defer helpCancel()
		if _, err := runProbeCommand(helpCtx, path, []string{"app-server", "--help"}, actor); err != nil {
			return ProbeResult{}, fmt.Errorf("verify Codex app-server: %w", err)
		}
		result.Capabilities = []string{
			"app-server", "thread-resume", "turn-start", "turn-steer", "turn-interrupt",
			"command-approval", "file-approval", "permission-approval", "plan-events", "diff-events", "usage-events",
		}
	default:
		return ProbeResult{}, fmt.Errorf("unsupported runtime actor %q", actor)
	}
	result.Capabilities = mergeUniqueStrings(result.Capabilities)
	result.Warnings = mergeUniqueStrings(result.Warnings)
	return result, nil
}

func runProbeCommand(ctx context.Context, path string, args []string, actor model.ActorID) (string, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	switch actor {
	case model.ActorClaude:
		cmd.Env = envWithout("CLAUDECODE")
	case model.ActorCodex:
		cmd.Env = envWithout("CODEX_INTERNAL_ORIGINATOR")
	}
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if ctx.Err() != nil {
		return text, fmt.Errorf("probe %s: %w", actor.DisplayName(), ctx.Err())
	}
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("probe %s (%s): %s", actor.DisplayName(), strings.Join(args, " "), firstNonEmptyLine(text))
	}
	return text, nil
}

func (p ProbeResult) RuntimeInfo(cfg Config) model.RuntimeInfo {
	data, _ := json.Marshal(map[string]any{
		"command":      p.Command,
		"path":         p.Path,
		"version_line": p.VersionLine,
		"warnings":     p.Warnings,
	})
	return model.RuntimeInfo{
		Available: true, Command: p.Command, Path: p.Path,
		Protocol: p.Protocol, Version: p.Version, Provider: cfg.Provider, Model: cfg.Model,
		PermissionMode: cfg.PermissionMode, ApprovalPolicy: cfg.ApprovalPolicy, Sandbox: cfg.Sandbox,
		Capabilities: append([]string(nil), p.Capabilities...), Warnings: append([]string(nil), p.Warnings...),
		ProbedAt: time.Now().UTC(), Data: data,
	}
}

func emitRuntimeInfo(sink EventSink, actor model.ActorID, info model.RuntimeInfo) {
	event := runtimeEvent(actor, model.RuntimeInfoUpdated)
	event.Runtime = &info
	encoded, _ := json.Marshal(info)
	event.Data = encoded
	sink(event)
}

func capabilityNames(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err == nil {
		sort.Strings(values)
		return values
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	for key, value := range object {
		enabled := true
		if boolean, ok := value.(bool); ok {
			enabled = boolean
		}
		if enabled {
			values = append(values, key)
		}
	}
	sort.Strings(values)
	return values
}

func mergeUniqueStrings(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var merged []string
	for _, group := range groups {
		for _, value := range group {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			merged = append(merged, value)
		}
	}
	sort.Strings(merged)
	return merged
}

// diagnosticStrings extracts human-readable startup warnings without keeping
// the complete vendor init payload. Init events can contain local paths and
// plugin/MCP metadata that are useful in the live Inspector but unnecessary in
// the long-lived participant summary and room exports.
func diagnosticStrings(raw json.RawMessage, keys ...string) []string {
	if len(raw) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	var values []string
	for _, key := range keys {
		value := object[key]
		if len(value) == 0 || string(value) == "null" {
			continue
		}
		var text string
		if json.Unmarshal(value, &text) == nil && strings.TrimSpace(text) != "" {
			values = append(values, key+": "+strings.TrimSpace(text))
			continue
		}
		var texts []string
		if json.Unmarshal(value, &texts) == nil {
			for _, item := range texts {
				if strings.TrimSpace(item) != "" {
					values = append(values, key+": "+strings.TrimSpace(item))
				}
			}
			continue
		}
		// Preserve enough context for operators without copying arbitrarily large
		// nested diagnostics into the participant snapshot.
		compact := strings.TrimSpace(string(value))
		if len(compact) > 400 {
			compact = compact[:400] + "…"
		}
		if compact != "" && compact != "{}" && compact != "[]" {
			values = append(values, key+": "+compact)
		}
	}
	return mergeUniqueStrings(values)
}

func extractSemanticVersion(value string) string {
	match := semanticVersionPattern.FindStringSubmatch(value)
	if len(match) == 0 {
		return ""
	}
	return strings.TrimPrefix(match[0], "v")
}

func versionAtLeast(value string, major, minor, patch int) bool {
	match := semanticVersionPattern.FindStringSubmatch(value)
	if len(match) != 4 {
		return false
	}
	parts := []int{major, minor, patch}
	for i := 1; i <= 3; i++ {
		got, err := strconv.Atoi(match[i])
		if err != nil {
			return false
		}
		want := parts[i-1]
		if got > want {
			return true
		}
		if got < want {
			return false
		}
	}
	return true
}

func firstNonEmptyLine(value string) string {
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return "ok"
}
