package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const defaultCCConnectConfig = "~/.cc-connect/config.toml"

func (c *File) resolveProviderProfiles(configPath string) error {
	if err := validateProviderNames(c.Providers); err != nil {
		return err
	}
	providers := append([]Provider(nil), c.Providers...)
	if c.CCConnect != nil {
		source, err := resolveImportPath(c.CCConnect.Path, configPath)
		if err != nil {
			return err
		}
		imported, err := loadCCConnectProviders(source)
		if err != nil {
			return fmt.Errorf("import cc-connect providers: %w", err)
		}
		imported = selectImportedProviders(imported, c.CCConnect.Providers)
		existing := make(map[string]struct{}, len(providers))
		for _, provider := range providers {
			existing[strings.ToLower(strings.TrimSpace(provider.Name))] = struct{}{}
		}
		for _, provider := range imported {
			provider.Name = strings.TrimSpace(c.CCConnect.Prefix) + provider.Name
			key := strings.ToLower(provider.Name)
			if _, found := existing[key]; found {
				continue // an explicit PairRoom profile intentionally wins
			}
			provider.ImportedFrom = source
			providers = append(providers, provider)
			existing[key] = struct{}{}
		}
	}
	if err := validateProviderNames(providers); err != nil {
		return err
	}
	c.Providers = providers

	byName := make(map[string]Provider, len(providers))
	for _, provider := range providers {
		byName[strings.ToLower(provider.Name)] = provider
	}
	if err := resolveAgentProvider("claudecode", &c.Claude, byName); err != nil {
		return fmt.Errorf("claude provider: %w", err)
	}
	if err := resolveAgentProvider("codex", &c.Codex, byName); err != nil {
		return fmt.Errorf("codex provider: %w", err)
	}
	return nil
}

func validateProviderNames(providers []Provider) error {
	seen := make(map[string]struct{}, len(providers))
	for i, provider := range providers {
		name := strings.TrimSpace(provider.Name)
		if name == "" {
			return fmt.Errorf("providers[%d].name is required", i)
		}
		key := strings.ToLower(name)
		if _, found := seen[key]; found {
			return fmt.Errorf("duplicate provider name %q", name)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func resolveAgentProvider(agentType string, agent *Agent, providers map[string]Provider) error {
	selected := strings.TrimSpace(agent.Provider)
	if selected == "" {
		agent.RuntimeEnv = copyStringMap(agent.RuntimeEnv)
		return nil
	}
	provider, found := providers[strings.ToLower(selected)]
	if !found {
		return fmt.Errorf("unknown provider %q", selected)
	}
	if !supportsAgent(provider.AgentTypes, agentType) {
		return fmt.Errorf("provider %q does not support %s", provider.Name, agentType)
	}
	agent.Provider = provider.Name
	agent.RuntimeEnv = mergeStringMaps(provider.Env, agent.RuntimeEnv)

	endpoint := strings.TrimSpace(provider.BaseURL)
	if value := lookupAgentValue(provider.Endpoints, agentType); value != "" {
		endpoint = value
	}
	if agent.Model == "" {
		agent.Model = lookupAgentValue(provider.AgentModels, agentType)
		if agent.Model == "" {
			agent.Model = strings.TrimSpace(provider.Model)
		}
	}
	secret, err := resolveProviderSecret(provider.APIKey)
	if err != nil {
		return fmt.Errorf("provider %q: %w", provider.Name, err)
	}

	switch agentType {
	case "claudecode":
		if endpoint != "" {
			agent.RuntimeEnv["ANTHROPIC_BASE_URL"] = endpoint
		}
		if secret != "" {
			agent.RuntimeEnv["ANTHROPIC_AUTH_TOKEN"] = secret
		}
	case "codex":
		id := "pairroom_" + providerIdentifier(provider.Name)
		envKey := "PAIRROOM_PROVIDER_" + strings.ToUpper(providerIdentifier(provider.Name)) + "_API_KEY"
		wireAPI := "responses"
		headers := map[string]string(nil)
		if provider.Codex != nil {
			if value := strings.TrimSpace(provider.Codex.EnvKey); value != "" {
				envKey = value
			}
			if value := strings.TrimSpace(provider.Codex.WireAPI); value != "" {
				wireAPI = value
			}
			headers = provider.Codex.HTTPHeaders
		}
		if secret != "" {
			agent.RuntimeEnv[envKey] = secret
		}
		args := []string{
			"-c", "model_provider=" + tomlString(id),
			"-c", "model_providers." + id + ".name=" + tomlString(provider.Name),
			"-c", "model_providers." + id + ".wire_api=" + tomlString(wireAPI),
			"-c", "model_providers." + id + ".env_key=" + tomlString(envKey),
		}
		if endpoint != "" {
			args = append(args, "-c", "model_providers."+id+".base_url="+tomlString(endpoint))
		}
		if len(headers) > 0 {
			envHeaders := make(map[string]string, len(headers))
			keys := make([]string, 0, len(headers))
			for header := range headers {
				keys = append(keys, header)
			}
			sort.Strings(keys)
			for index, header := range keys {
				headerEnv := fmt.Sprintf("PAIRROOM_PROVIDER_%s_HTTP_HEADER_%d", strings.ToUpper(providerIdentifier(provider.Name)), index+1)
				agent.RuntimeEnv[headerEnv] = headers[header]
				envHeaders[header] = headerEnv
			}
			args = append(args, "-c", "model_providers."+id+".env_http_headers="+tomlInlineTable(envHeaders))
		}
		agent.Args = append(append([]string(nil), agent.Args...), args...)
	default:
		return fmt.Errorf("unsupported provider agent type %q", agentType)
	}
	return nil
}

func resolveProviderSecret(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	name := ""
	switch {
	case strings.HasPrefix(value, "env:"):
		name = strings.TrimSpace(strings.TrimPrefix(value, "env:"))
	case strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}"):
		name = strings.TrimSpace(value[2 : len(value)-1])
	}
	if name == "" {
		return value, nil
	}
	if !validEnvName(name) {
		return "", fmt.Errorf("invalid API-key environment variable %q", name)
	}
	secret := os.Getenv(name)
	if secret == "" {
		return "", fmt.Errorf("environment variable %s is not set", name)
	}
	return secret, nil
}

func validEnvName(value string) bool {
	for i, r := range value {
		if r == '_' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r)) {
			continue
		}
		return false
	}
	return value != ""
}

func supportsAgent(values []string, target string) bool {
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		normalized := normalizeAgentType(value)
		if normalized == target {
			return true
		}
	}
	return false
}

func normalizeAgentType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "claude", "claude-code", "claude_code", "claudecode":
		return "claudecode"
	case "codex":
		return "codex"
	default:
		return value
	}
}

func lookupAgentValue(values map[string]string, target string) string {
	for key, value := range values {
		if normalizeAgentType(key) == target {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func providerIdentifier(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	result := strings.Trim(b.String(), "_")
	if result == "" {
		return "provider"
	}
	return result
}

func tomlString(value string) string { return strconv.Quote(value) }

func tomlInlineTable(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]string, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, tomlString(key)+" = "+tomlString(values[key]))
	}
	return "{ " + strings.Join(entries, ", ") + " }"
}

func copyStringMap(source map[string]string) map[string]string {
	output := make(map[string]string, len(source))
	for key, value := range source {
		output[key] = value
	}
	return output
}

func mergeStringMaps(groups ...map[string]string) map[string]string {
	output := make(map[string]string)
	for _, group := range groups {
		for key, value := range group {
			output[key] = value
		}
	}
	return output
}

func resolveImportPath(value, pairroomConfig string) (string, error) {
	if strings.TrimSpace(value) == "" {
		value = defaultCCConnectConfig
	}
	expanded, err := expandHome(value)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) && pairroomConfig != "" {
		expanded = filepath.Join(filepath.Dir(pairroomConfig), expanded)
	}
	absolute, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("resolve cc-connect config path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func expandHome(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value != "~" && !strings.HasPrefix(value, "~/") && !strings.HasPrefix(value, `~\`) {
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if value == "~" {
		return home, nil
	}
	return filepath.Join(home, value[2:]), nil
}

func selectImportedProviders(providers []Provider, selected []string) []Provider {
	if len(selected) == 0 {
		return providers
	}
	wanted := make(map[string]struct{}, len(selected))
	for _, name := range selected {
		wanted[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}
	output := make([]Provider, 0, len(providers))
	for _, provider := range providers {
		if _, found := wanted[strings.ToLower(provider.Name)]; found {
			output = append(output, provider)
		}
	}
	return output
}

// loadCCConnectProviders parses only cc-connect's provider tables. It is a
// deliberately bounded TOML reader, not a general TOML implementation: all
// unrelated project/platform settings are ignored, while unsupported
// provider syntax fails with a precise line number instead of being guessed.
func loadCCConnectProviders(path string) ([]Provider, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var providers []Provider
	current := -1
	section := ""
	modelIndex := -1
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if line == "" {
			continue
		}
		switch {
		case line == "[[providers]]":
			providers = append(providers, Provider{})
			current = len(providers) - 1
			section = ""
			modelIndex = -1
			continue
		case line == "[[providers.models]]":
			if current < 0 {
				return nil, fmt.Errorf("line %d: providers.models appears before [[providers]]", lineNumber)
			}
			providers[current].Models = append(providers[current].Models, ProviderModel{})
			modelIndex = len(providers[current].Models) - 1
			section = "models"
			continue
		case strings.HasPrefix(line, "[providers.") && strings.HasSuffix(line, "]"):
			if current < 0 {
				return nil, fmt.Errorf("line %d: provider section appears before [[providers]]", lineNumber)
			}
			section = strings.TrimSuffix(strings.TrimPrefix(line, "[providers."), "]")
			modelIndex = -1
			continue
		case strings.HasPrefix(line, "["):
			section = "ignore"
			current = -1
			modelIndex = -1
			continue
		}
		if current < 0 || section == "ignore" {
			continue
		}
		key, raw, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("line %d: expected key = value", lineNumber)
		}
		key = strings.TrimSpace(key)
		raw = strings.TrimSpace(raw)
		if err := assignProviderTOML(&providers[current], section, modelIndex, key, raw); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := validateProviderNames(providers); err != nil {
		return nil, err
	}
	return providers, nil
}

func assignProviderTOML(provider *Provider, section string, modelIndex int, key, raw string) error {
	switch section {
	case "":
		switch key {
		case "name":
			provider.Name = parseTOMLString(raw)
		case "api_key":
			provider.APIKey = parseTOMLString(raw)
		case "base_url":
			provider.BaseURL = parseTOMLString(raw)
		case "model":
			provider.Model = parseTOMLString(raw)
		case "thinking":
			provider.Thinking = parseTOMLString(raw)
		case "agent_types":
			values, err := parseTOMLStringArray(raw)
			if err != nil {
				return err
			}
			provider.AgentTypes = values
		}
	case "models":
		if modelIndex < 0 || modelIndex >= len(provider.Models) {
			return errors.New("invalid providers.models state")
		}
		switch key {
		case "model":
			provider.Models[modelIndex].Model = parseTOMLString(raw)
		case "alias":
			provider.Models[modelIndex].Alias = parseTOMLString(raw)
		}
	case "env":
		if provider.Env == nil {
			provider.Env = make(map[string]string)
		}
		provider.Env[parseTOMLKey(key)] = parseTOMLString(raw)
	case "endpoints":
		if provider.Endpoints == nil {
			provider.Endpoints = make(map[string]string)
		}
		provider.Endpoints[parseTOMLKey(key)] = parseTOMLString(raw)
	case "agent_models":
		if provider.AgentModels == nil {
			provider.AgentModels = make(map[string]string)
		}
		provider.AgentModels[parseTOMLKey(key)] = parseTOMLString(raw)
	case "codex":
		if provider.Codex == nil {
			provider.Codex = &CodexProvider{}
		}
		switch key {
		case "env_key":
			provider.Codex.EnvKey = parseTOMLString(raw)
		case "wire_api":
			provider.Codex.WireAPI = parseTOMLString(raw)
		}
	case "codex.http_headers":
		if provider.Codex == nil {
			provider.Codex = &CodexProvider{}
		}
		if provider.Codex.HTTPHeaders == nil {
			provider.Codex.HTTPHeaders = make(map[string]string)
		}
		provider.Codex.HTTPHeaders[parseTOMLKey(key)] = parseTOMLString(raw)
	}
	return nil
}

func stripTOMLComment(value string) string {
	quote := rune(0)
	escaped := false
	for index, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if quote == '"' && r == '\\' {
			escaped = true
			continue
		}
		if r == '\'' || r == '"' {
			if quote == 0 {
				quote = r
			} else if quote == r {
				quote = 0
			}
			continue
		}
		if r == '#' && quote == 0 {
			return value[:index]
		}
	}
	return value
}

func parseTOMLKey(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
		return parseTOMLString(value)
	}
	return value
}

func parseTOMLString(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		if decoded, err := strconv.Unquote(value); err == nil {
			return decoded
		}
	}
	return value
}

func parseTOMLStringArray(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, errors.New("expected an array of strings")
	}
	body := strings.TrimSpace(value[1 : len(value)-1])
	if body == "" {
		return nil, nil
	}
	var values []string
	var current strings.Builder
	quote := rune(0)
	escaped := false
	flush := func() {
		item := strings.TrimSpace(current.String())
		if item != "" {
			values = append(values, parseTOMLString(item))
		}
		current.Reset()
	}
	for _, r := range body {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if quote == '"' && r == '\\' {
			current.WriteRune(r)
			escaped = true
			continue
		}
		if r == '\'' || r == '"' {
			if quote == 0 {
				quote = r
			} else if quote == r {
				quote = 0
			}
			current.WriteRune(r)
			continue
		}
		if r == ',' && quote == 0 {
			flush()
			continue
		}
		current.WriteRune(r)
	}
	if quote != 0 {
		return nil, errors.New("unterminated string in array")
	}
	flush()
	return values, nil
}
