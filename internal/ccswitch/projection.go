package ccswitch

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

// Provider-owned fields were checked against CC Switch v4.0.4, commit
// a29a4f3868e9c6ffc2212957cfa66a718642e344, src-tauri/src/live/floor.rs
// and live/project/{claude,codex}.rs. Shared settings in older provider rows
// are historical snapshots; they must not configure the selected child.
var claudeCompatibilityEnv = map[string]bool{
	"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS":               true,
	"CLAUDE_CODE_DISABLE_ARTIFACT":                         true,
	"ENABLE_TOOL_SEARCH":                                   true,
	"CLAUDE_CODE_DISABLE_THINKING":                         true,
	"DISABLE_INTERLEAVED_THINKING":                         true,
	"CLAUDE_CODE_ALWAYS_ENABLE_EFFORT":                     true,
	"CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING":       true,
	"CLAUDE_CODE_AUTO_MODE_SERVER":                         true,
	"CLAUDE_CODE_MAX_CONTEXT_TOKENS":                       true,
	"CLAUDE_CODE_AUTO_COMPACT_WINDOW":                      true,
	"CLAUDE_CODE_MAX_OUTPUT_TOKENS":                        true,
	"CLAUDE_CODE_DISABLE_1M_CONTEXT":                       true,
	"CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT": true,
	"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":           true,
}

func claudeModelEnvKey(key string) bool {
	return key == "ANTHROPIC_MODEL" || key == "ANTHROPIC_SMALL_FAST_MODEL" ||
		key == "CLAUDE_CODE_SUBAGENT_MODEL" || key == "CLAUDE_CODE_SUBAGENT_MODEL_FORCE" ||
		strings.HasPrefix(key, "ANTHROPIC_DEFAULT_") && strings.HasSuffix(key, "_MODEL")
}

func claudeDirectEnvKey(key string) bool {
	return key == "ANTHROPIC_AUTH_TOKEN" || key == "ANTHROPIC_API_KEY" || key == "ANTHROPIC_BASE_URL" ||
		claudeModelEnvKey(key) || claudeCompatibilityEnv[key] ||
		strings.HasPrefix(key, "ANTHROPIC_DEFAULT_") && strings.HasSuffix(key, "_MODEL_NAME")
}

// These unsupported values can contain credentials in arbitrary request JSON,
// header text, or shell commands. Their contents cannot be redacted reliably,
// so even a disabled row must withhold its author-supplied catalog metadata.
func claudeOpaqueSettings(settings map[string]any) bool {
	env, _ := settings["env"].(map[string]any)
	for _, key := range []string{"ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_EXTRA_BODY"} {
		if value, present := env[key]; present && value != nil && value != "" {
			return true
		}
	}
	for _, key := range []string{"apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh"} {
		if value, present := settings[key]; present && value != nil && value != "" {
			return true
		}
	}
	return false
}

func validateClaudeProfile(p profileRow, settings map[string]any, env map[string]string) error {
	rawEnv, _ := settings["env"].(map[string]any)
	for _, key := range []string{
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_GATEWAY", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
	} {
		if claudeTruthy(rawEnv[key]) {
			return profileError(p, ReasonProxyConversion, "Claude profile requires a cloud or gateway authentication mode that cannot be activated as a direct Anthropic profile.")
		}
	}
	for _, key := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN"} {
		if env[key] != "" {
			return profileError(p, ReasonManagedOAuth, "Claude OAuth credentials remain owned by the native session and cannot be materialized as an API-key profile.")
		}
	}
	for _, key := range []string{"apiKeyHelper", "apiKey", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh"} {
		if value, present := settings[key]; present && value != nil && value != "" {
			return profileError(p, ReasonInvalidConfig, "Claude profile declares an unsupported credential source.")
		}
	}
	if env["ANTHROPIC_CUSTOM_HEADERS"] != "" {
		return profileError(p, ReasonInvalidConfig, "Claude profiles with custom authentication headers cannot be materialized independently.")
	}
	if env["CLAUDE_CODE_EXTRA_BODY"] != "" {
		return profileError(p, ReasonInvalidConfig, "Claude profiles with an extra request body require native configuration.")
	}
	if endpoint := env["ANTHROPIC_BASE_URL"]; endpoint != "" {
		if err := validateEndpoint(endpoint); err != nil {
			return profileError(p, ReasonInvalidConfig, "Claude profile base URL cannot be materialized safely: "+err.Error())
		}
		lower := strings.ToLower(endpoint)
		if strings.Contains(lower, "githubcopilot.com") || strings.Contains(lower, "chatgpt.com/backend-api/codex") {
			return profileError(p, ReasonManagedOAuth, "Claude profile requires CC Switch managed authentication.")
		}
	}
	return nil
}

func claudeTruthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case float64:
		return typed != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func claudeCompatibilityArgs(env map[string]string) []string {
	settings := make(map[string]string)
	for key, value := range env {
		if claudeCompatibilityEnv[key] {
			settings[key] = value
		}
	}
	if len(settings) == 0 {
		return nil
	}
	// The host-routing switch protects provider/authentication fields. Other
	// compatibility flags need CLI settings precedence over user/project/local
	// settings.env. Managed policy still applies.
	// Only this explicit non-secret allowlist reaches argv; finishMaterialization
	// checks it against every credential in the provider row before returning it.
	encoded, _ := json.Marshal(map[string]any{"env": settings})
	return []string{"--settings", string(encoded)}
}

func isProxyCredential(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "PROXY_MANAGED")
}

// These are the credential fields the selected Runtime actually reads. Their
// malformed containers cannot be redacted as scalar secrets. Unrelated table
// names such as mcp_servers.secret do not declare this Provider's credentials;
// known credential values elsewhere still pass through global redaction.
func uninspectableTOMLCredentials(doc tomlDocument, runtime model.RuntimeKind) bool {
	if doc.Invalid || doc.UnsafeCredentials {
		return true
	}
	var paths []string
	switch runtime.Canonical() {
	case model.RuntimeCodex:
		prefix := "model_providers." + tomlKey(strings.TrimSpace(doc.Root["model_provider"]))
		paths = []string{"experimental_bearer_token", prefix + ".env_key", prefix + ".experimental_bearer_token"}
	case model.RuntimeGrok:
		prefix := "model." + tomlKey(strings.TrimSpace(doc.Sections["models"]["default"]))
		paths = []string{prefix + ".api_key", prefix + ".env_key"}
	}
	for _, path := range paths {
		if !supportedTOMLScalar(doc, path, tomlString) {
			return true
		}
	}
	return false
}

func codexProviderSection(p profileRow, doc tomlDocument) (map[string]string, error) {
	id := strings.TrimSpace(doc.Root["model_provider"])
	sectionName := "model_providers." + tomlKey(id)
	for _, field := range []struct {
		path string
		kind tomlScalarKind
	}{
		{"model_provider", tomlString}, {"openai_base_url", tomlString},
		{sectionName + ".name", tomlScalarText}, {sectionName + ".base_url", tomlString}, {sectionName + ".wire_api", tomlString},
	} {
		if !supportedTOMLScalar(doc, field.path, field.kind) {
			return nil, profileError(p, ReasonInvalidConfig, "Codex profile contains an invalid provider or credential declaration.")
		}
	}
	section := doc.Sections[sectionName]
	if unsupportedTOMLPrefix(doc, sectionName) {
		return nil, profileError(p, ReasonInvalidConfig, "Codex profile contains unsupported provider configuration syntax.")
	}
	if (id == "" || id == "openai") && doc.Root["openai_base_url"] != "" {
		// CC Switch 4 normalizes this older explicit API endpoint into a custom
		// provider. Merge its defaults regardless of whether selected options used
		// headings or dotted keys. Every explicit value, including empty values,
		// remains authoritative; native login is never a credential fallback.
		merged := map[string]string{"base_url": doc.Root["openai_base_url"], "name": "Custom", "wire_api": "responses"}
		for key, value := range section {
			merged[key] = value
		}
		section = merged
	}
	if len(section) == 0 {
		return nil, profileError(p, ReasonInvalidConfig, "Codex profile does not select a materializable custom model provider.")
	}
	// requires_openai_auth is deliberately ignored: the child projection fixes
	// it to false and uses only its own environment-key authentication.
	return section, nil
}

// Only explicitly owned scalar options are projected. In particular, no
// hooks, MCP definitions, permission settings, or arbitrary TOML are copied.
func codexProfileOptions(p profileRow, doc tomlDocument, id string) ([]string, error) {
	sectionName := "model_providers." + tomlKey(strings.TrimSpace(doc.Root["model_provider"]))
	var args []string
	for _, field := range []struct {
		path, target string
		kind         tomlScalarKind
	}{
		// Model and effort are defaults passed separately by AgentResolver.
		{"model", "", tomlString}, {"model_reasoning_effort", "", tomlString},
		{"review_model", "review_model", tomlScalarText},
		{"plan_mode_reasoning_effort", "plan_mode_reasoning_effort", tomlScalarText},
		{"web_search", "web_search", tomlScalarText},
		{"model_verbosity", "model_verbosity", tomlScalarText},
		{"disable_response_storage", "disable_response_storage", tomlBoolean},
		{"model_supports_reasoning_summaries", "model_supports_reasoning_summaries", tomlBoolean},
		{sectionName + ".supports_websockets", "model_providers." + id + ".supports_websockets", tomlBoolean},
		{"model_context_window", "model_context_window", tomlInteger},
		{"model_auto_compact_token_limit", "model_auto_compact_token_limit", tomlInteger},
		{"agents.default_subagent_model", "agents.default_subagent_model", tomlScalarText},
		{"agents.default_subagent_reasoning_effort", "agents.default_subagent_reasoning_effort", tomlScalarText},
		{"memories.extract_model", "memories.extract_model", tomlScalarText},
		{"memories.consolidation_model", "memories.consolidation_model", tomlScalarText},
	} {
		if !supportedTOMLScalar(doc, field.path, field.kind) {
			return nil, profileError(p, ReasonInvalidConfig, "Codex profile contains an invalid scalar model or provider option.")
		}
		if _, present := doc.ScalarKinds[field.path]; !present || field.target == "" {
			continue
		}
		value := tomlScalarValue(doc, field.path)
		switch field.kind {
		case tomlString, tomlScalarText:
			value = tomlQuote(value)
		case tomlInteger:
			// The scalar check validated both integer and quoted-integer input.
			number, _ := strconv.ParseInt(value, 10, 64)
			if number <= 0 {
				return nil, profileError(p, ReasonInvalidConfig, "Codex profile contains an invalid context-window option.")
			}
			value = strconv.FormatInt(number, 10)
		}
		args = append(args, "-c", field.target+"="+value)
	}
	return args, nil
}

func profileModels(runtime model.RuntimeKind, settings, meta map[string]any, doc tomlDocument) []string {
	var models []string
	switch runtime.Canonical() {
	case model.RuntimeClaude:
		models = append(models, stringValue(settings["model"]))
		for key, value := range stringMap(settings["env"]) {
			if claudeModelEnvKey(key) {
				models = append(models, value)
			}
		}
		if entries, ok := meta["stackModels"].([]any); ok {
			for _, entry := range entries {
				row, _ := entry.(map[string]any)
				name := stringValue(row["model"])
				if name != "" && boolValue(row["oneM"]) && !strings.HasSuffix(strings.ToLower(name), "[1m]") {
					name += "[1M]"
				}
				models = append(models, name)
			}
		}
	case model.RuntimeCodex:
		if doc.ScalarKinds["model"] == tomlString {
			models = append(models, doc.Root["model"])
		}
		catalog, _ := settings["modelCatalog"].(map[string]any)
		if entries, ok := catalog["models"].([]any); ok {
			for _, entry := range entries {
				row, _ := entry.(map[string]any)
				models = append(models, stringValue(row["model"]))
			}
		}
	case model.RuntimeGrok:
		if doc.ScalarKinds["models.default"] == tomlString {
			selected := strings.TrimSpace(doc.Sections["models"]["default"])
			models = append(models, selected)
			if doc.ScalarKinds["model."+tomlKey(selected)+".model"] == tomlString {
				models = append(models, grokModelSection(doc, selected)["model"])
			}
		}
	}
	return uniqueStrings(models)
}
