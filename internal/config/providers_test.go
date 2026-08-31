package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderProfilesResolveIndependently(t *testing.T) {
	cfg := Defaults()
	cfg.Providers = []Provider{
		{
			Name: "shared", APIKey: "top-secret", BaseURL: "https://default.invalid/v1",
			AgentTypes:  []string{"claudecode", "codex"},
			Endpoints:   map[string]string{"claudecode": "https://claude.invalid", "codex": "https://codex.invalid/v1"},
			AgentModels: map[string]string{"claudecode": "claude-test", "codex": "gpt-test"},
			Env:         map[string]string{"PAIRROOM_TEST": "yes"},
			Codex:       &CodexProvider{EnvKey: "PAIRROOM_TEST_KEY", WireAPI: "responses"},
		},
	}
	cfg.Claude.Provider = "shared"
	cfg.Codex.Provider = "shared"
	if err := cfg.resolveProviderProfiles(""); err != nil {
		t.Fatal(err)
	}
	if cfg.Claude.Model != "claude-test" || cfg.Claude.RuntimeEnv["ANTHROPIC_BASE_URL"] != "https://claude.invalid" {
		t.Fatalf("unexpected Claude provider projection: %#v", cfg.Claude)
	}
	if cfg.Codex.Model != "gpt-test" || cfg.Codex.RuntimeEnv["PAIRROOM_TEST_KEY"] != "top-secret" {
		t.Fatalf("unexpected Codex provider projection: %#v", cfg.Codex)
	}
	joined := strings.Join(cfg.Codex.Args, " ")
	if !strings.Contains(joined, `model_provider="pairroom_shared"`) || strings.Contains(joined, "top-secret") {
		t.Fatalf("Codex arguments should select the provider without exposing its key: %s", joined)
	}
}

func TestProviderProfileEnvironmentReference(t *testing.T) {
	t.Setenv("PAIRROOM_PROVIDER_TEST", "from-env")
	cfg := Defaults()
	cfg.Providers = []Provider{{Name: "env", APIKey: "env:PAIRROOM_PROVIDER_TEST"}}
	cfg.Claude.Provider = "env"
	if err := cfg.resolveProviderProfiles(""); err != nil {
		t.Fatal(err)
	}
	if cfg.Claude.RuntimeEnv["ANTHROPIC_AUTH_TOKEN"] != "from-env" {
		t.Fatal("environment-backed provider secret was not resolved")
	}
}

func TestCCConnectProviderReferenceImport(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "cc-connect.toml")
	data := `
[[providers]]
name = "proxy"
api_key = "secret-from-source"
base_url = "https://proxy.invalid/v1"
agent_types = ["claudecode", "codex"]

[providers.endpoints]
claudecode = "https://claude.proxy.invalid"
codex = "https://codex.proxy.invalid/v1"

[providers.agent_models]
claudecode = "claude-imported"
codex = "gpt-imported"

[providers.codex]
env_key = "PAIRROOM_IMPORTED_KEY"
wire_api = "responses"

[providers.codex.http_headers]
X-Provider = "header-secret"
`
	if err := os.WriteFile(source, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "pairroom.json")
	configJSON := `{
  "listen": "127.0.0.1:7332",
  "routing_mode": "mentions",
  "max_agent_hops": 6,
  "stall_warning_seconds": 300,
  "auto_start": false,
  "cc_connect": {"path": "cc-connect.toml", "providers": ["proxy"], "prefix": "cc-"},
  "claude": {"command": "claude", "provider": "cc-proxy"},
  "codex": {"command": "codex", "provider": "cc-proxy"}
}`
	if err := os.WriteFile(cfgPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].ImportedFrom != source {
		t.Fatalf("unexpected imported providers: %#v", cfg.Providers)
	}
	if cfg.Claude.Model != "claude-imported" || cfg.Codex.Model != "gpt-imported" {
		t.Fatalf("agent-specific imported models were not selected: %#v %#v", cfg.Claude, cfg.Codex)
	}
	if cfg.Codex.RuntimeEnv["PAIRROOM_IMPORTED_KEY"] != "secret-from-source" {
		t.Fatal("imported Codex key was not projected through the environment")
	}
	joinedArgs := strings.Join(cfg.Codex.Args, " ")
	if strings.Contains(joinedArgs, "secret-from-source") || strings.Contains(joinedArgs, "header-secret") {
		t.Fatal("imported secret leaked into command arguments")
	}
	if !strings.Contains(joinedArgs, "env_http_headers") {
		t.Fatalf("Codex header environment projection is missing: %s", joinedArgs)
	}
	foundHeader := false
	for _, value := range cfg.Codex.RuntimeEnv {
		if value == "header-secret" {
			foundHeader = true
		}
	}
	if !foundHeader {
		t.Fatal("imported Codex header was not projected through the environment")
	}
	summaries := cfg.ProviderSummaries()
	if len(summaries) != 1 || summaries[0].Name != "cc-proxy" || summaries[0].ImportedFrom != source {
		t.Fatalf("unexpected safe summary: %#v", summaries)
	}
}

func TestProviderSummaryRedactsURLCredentials(t *testing.T) {
	cfg := Defaults()
	cfg.Providers = []Provider{{Name: "private", BaseURL: "https://user:password@example.invalid/v1?token=query-secret#fragment-secret"}}
	summary := cfg.ProviderSummaries()
	if len(summary) != 1 {
		t.Fatalf("summaries = %#v", summary)
	}
	got := summary[0].BaseURL
	for _, secret := range []string{"user", "password", "query-secret", "fragment-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted URL %q contains %q", got, secret)
		}
	}
}

func TestProviderValidationRejectsDuplicateAndUnsupportedAgent(t *testing.T) {
	cfg := Defaults()
	cfg.Providers = []Provider{{Name: "Same"}, {Name: "same"}}
	if err := cfg.resolveProviderProfiles(""); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate provider error, got %v", err)
	}

	cfg = Defaults()
	cfg.Providers = []Provider{{Name: "claude-only", AgentTypes: []string{"claudecode"}}}
	cfg.Codex.Provider = "claude-only"
	if err := cfg.resolveProviderProfiles(""); err == nil || !strings.Contains(err.Error(), "does not support codex") {
		t.Fatalf("expected unsupported-agent error, got %v", err)
	}
}
