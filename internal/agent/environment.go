package agent

import (
	"errors"
	"runtime"
	"sort"
	"strings"
)

const claudeProviderManagedByHost = "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"

func claudeUsesCCSwitch(cfg Config) bool {
	return strings.HasPrefix(cfg.Provider, "cc-switch:")
}

// Claude 2.1.222 fixed host model selections losing to stale managed settings.
// This is the supported isolation baseline, not the flag's introduction date:
// https://github.com/anthropics/claude-code/blob/71cdddec623889d38af14b7a489670a03186f659/CHANGELOG.md#21222
func validateClaudeProviderVersion(cfg Config, version string) error {
	if claudeUsesCCSwitch(cfg) && !versionAtLeast(version, 2, 1, 222) {
		return errors.New("CC Switch Provider selection requires Claude Code 2.1.222 or newer for host-managed provider isolation; update Claude Code")
	}
	return nil
}

func claudeRuntimeEnv(base []string, cfg Config) []string {
	return claudeRuntimeEnvForOS(base, cfg, runtime.GOOS)
}

// A selected CC Switch Provider owns this child's model endpoint and auth.
// Removing conflicting inherited variables is only half the boundary: Claude
// normally applies settings.env over the child environment, including when a
// settings file changes. Its documented host flag preserves our selection while
// retaining native settings, tools, hooks, permissions, and session storage.
// https://code.claude.com/docs/en/env-vars
func claudeRuntimeEnvForOS(base []string, cfg Config, goos string) []string {
	if !claudeUsesCCSwitch(cfg) {
		return mergeRuntimeEnvForOS(base, cfg.Env, goos)
	}
	filtered := make([]string, 0, len(base))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if goos == "windows" {
			key = strings.ToUpper(key)
		}
		if !claudeProviderEnvironmentKey(key) {
			filtered = append(filtered, entry)
		}
	}
	// Copy before adding the host flag: Config.Env may be shared with diagnostics
	// and the other slot must never inherit this slot's materialization.
	overrides := make(map[string]string, len(cfg.Env)+1)
	for key, value := range cfg.Env {
		if key == claudeProviderManagedByHost || (goos == "windows" && strings.EqualFold(key, claudeProviderManagedByHost)) {
			continue
		}
		overrides[key] = value
	}
	overrides[claudeProviderManagedByHost] = "1"
	return mergeRuntimeEnvForOS(filtered, overrides, goos)
}

func claudeProviderEnvironmentKey(key string) bool {
	if strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "VERTEX_REGION_") ||
		(strings.HasPrefix(key, "CLAUDE_CODE_SKIP_") && strings.HasSuffix(key, "_AUTH")) {
		return true
	}
	switch key {
	case claudeProviderManagedByHost,
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_GATEWAY", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDE_CODE_OAUTH_SCOPES",
		"CLAUDE_CODE_API_KEY_HELPER_TTL_MS", "CLAUDE_CODE_SUBAGENT_MODEL",
		"CLAUDE_CODE_SUBAGENT_MODEL_FORCE", "AWS_BEARER_TOKEN_BEDROCK", "CLOUD_ML_REGION":
		return true
	default:
		// General AWS/Google credentials remain available to native tools. With
		// the protocol selectors removed they cannot select the model provider.
		// In particular, do not match CLAUDE_CODE_USE_* indiscriminately: it also
		// contains unrelated feature switches such as USE_POWERSHELL_TOOL.
		return false
	}
}

func mergeRuntimeEnv(base []string, overrides map[string]string) []string {
	return mergeRuntimeEnvForOS(base, overrides, runtime.GOOS)
}

// Windows environment names are case-insensitive. An inherited lower-case
// name must not win over an explicit per-participant credential after os/exec
// deduplicates Env. Keep Unix names case-sensitive and preserve Windows' hidden
// =C: drive-directory entries when reconstructing the environment.
func mergeRuntimeEnvForOS(base []string, overrides map[string]string, goos string) []string {
	if len(overrides) == 0 {
		return base
	}
	normalize := func(key string) string {
		if goos == "windows" {
			return strings.ToUpper(key)
		}
		return key
	}
	values := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		offset := strings.IndexByte(entry, '=')
		if offset == 0 && goos == "windows" {
			if next := strings.IndexByte(entry[1:], '='); next >= 0 {
				offset = next + 1
			}
		}
		if offset > 0 {
			values[normalize(entry[:offset])] = entry
		}
	}
	// Sorting makes even differently cased duplicate override keys deterministic.
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.TrimSpace(key) != "" {
			values[normalize(key)] = key + "=" + overrides[key]
		}
	}
	keys = keys[:0]
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	output := make([]string, 0, len(keys))
	for _, key := range keys {
		output = append(output, values[key])
	}
	return output
}
