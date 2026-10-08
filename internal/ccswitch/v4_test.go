package ccswitch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

// These fixtures follow CC Switch v4.0.4 (a29a4f3868e9c6ffc2212957cfa66a718642e344):
// src-tauri/src/live/project/codex.rs owns credential precedence,
// src-tauri/src/live/floor.rs owns provider fields, and provider.rs plus
// codex_config.rs own the stackModels / modelCatalog object arrays.
// Expectations are literals from those contracts, exercised through the Reader.

func TestV4VerifiedSchemaAndDirectFailoverMembership(t *testing.T) {
	const key = "v4-direct-queue-fixture-key"
	reader := v4FixtureReader(t, fixtureProfile{
		id: "direct-queue", appType: "claude", name: "Direct queue member",
		settings: `{"env":{"ANTHROPIC_AUTH_TOKEN":"` + key + `","ANTHROPIC_BASE_URL":"https://claude-v4.invalid","ANTHROPIC_MODEL":"claude-v4"}}`,
		meta:     `{}`, current: 1, failover: 1,
	})
	catalog, err := reader.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Schema != 20 || !catalog.SchemaVerified || catalog.CCSwitchVersion != "v4.0.4" {
		t.Fatalf("v4 catalog schema = %d, verified = %t, version = %q", catalog.Schema, catalog.SchemaVerified, catalog.CCSwitchVersion)
	}
	if len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported {
		t.Fatalf("direct API-key provider was disabled merely for queue membership: %#v", catalog.Profiles)
	}
	resolved, err := reader.Resolve(context.Background(), catalog.Profiles[0].ProviderRef, model.RuntimeClaude)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Env["ANTHROPIC_AUTH_TOKEN"] != key || resolved.DefaultModel != "claude-v4" {
		t.Fatal("queue member did not resolve its own API key and model")
	}
	v4AssertPublicSecretFree(t, catalog, resolved, key)
}

func TestV4ManagedAndRoutedProfilesRemainDisabled(t *testing.T) {
	const staleKey = "v4-stale-managed-fixture-key"
	claude := `{"env":{"ANTHROPIC_AUTH_TOKEN":"` + staleKey + `","ANTHROPIC_BASE_URL":"https://claude-v4.invalid","ANTHROPIC_MODEL":"claude-v4"}}`
	codex := v4JSON(t, map[string]any{
		"auth":   map[string]string{"OPENAI_API_KEY": staleKey},
		"config": "model_provider = \"custom\"\nmodel = \"codex-v4\"\n[model_providers.custom]\nwire_api = \"responses\"\nbase_url = \"https://codex-v4.invalid/v1\"\n",
	})
	grok := v4JSON(t, map[string]any{
		"config": "[models]\ndefault = \"direct\"\n[model.direct]\nmodel = \"grok-v4\"\nname = \"Direct\"\nbase_url = \"https://grok-v4.invalid/v1\"\napi_backend = \"responses\"\napi_key = \"" + staleKey + "\"\ncontext_window = 262144\n",
	})
	cases := []struct {
		name, app, settings, meta, reason string
		runtime                           model.RuntimeKind
	}{
		{"claude-managed-binding", "claude", claude, `{"authBinding":{"source":"managed_account","authProvider":"github_copilot","accountId":"fixture-account"}}`, ReasonManagedOAuth, model.RuntimeClaude},
		{"codex-managed-binding", "codex", codex, `{"authBinding":{"source":"managed_account","authProvider":"codex_oauth"}}`, ReasonManagedOAuth, model.RuntimeCodex},
		{"copilot-provider-type", "claude", claude, `{"providerType":"github_copilot"}`, ReasonManagedOAuth, model.RuntimeClaude},
		{"claude-full-url", "claude", claude, `{"isFullUrl":true}`, ReasonProxyConversion, model.RuntimeClaude},
		{"codex-full-url", "codex", codex, `{"isFullUrl":true}`, ReasonProxyConversion, model.RuntimeCodex},
		{"grok-full-url", "grokbuild", grok, `{"isFullUrl":true}`, ReasonProxyConversion, model.RuntimeGrok},
		{"claude-proxy-placeholder", "claude", strings.ReplaceAll(claude, staleKey, "PROXY_MANAGED"), `{}`, "", model.RuntimeClaude},
		{"codex-proxy-placeholder", "codex", strings.ReplaceAll(codex, staleKey, "PROXY_MANAGED"), `{}`, "", model.RuntimeCodex},
		{"grok-proxy-placeholder", "grokbuild", strings.ReplaceAll(grok, staleKey, "PROXY_MANAGED"), `{}`, "", model.RuntimeGrok},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			reader := v4FixtureReader(t, fixtureProfile{id: test.name, appType: test.app, name: test.name, settings: test.settings, meta: test.meta})
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported {
				t.Fatal("CC Switch-owned routing or credentials were exposed as a direct provider")
			}
			if test.reason != "" && catalog.Profiles[0].ReasonCode != test.reason {
				t.Fatalf("disabled reason = %q, want %q", catalog.Profiles[0].ReasonCode, test.reason)
			}
			resolved, err := reader.Resolve(context.Background(), catalog.Profiles[0].ProviderRef, test.runtime)
			v4AssertUnsupported(t, err, test.reason, staleKey)
			if len(resolved.Env) != 0 || len(resolved.Args) != 0 {
				t.Fatal("unsupported provider returned a partial materialization")
			}
			v4AssertPublicSecretFree(t, catalog, resolved, staleKey)
		})
	}
}

func TestV4CodexCredentialPrecedenceAndQuotedTOML(t *testing.T) {
	const (
		authKey    = "v4-auth-fixture-key"
		tableKey   = "v4-table-fixture-key"
		rootKey    = "v4-root-fixture-key"
		envKey     = "v4-env-fixture-key"
		literalKey = "v4-literal-fixture#key"
	)
	t.Setenv("PAIRROOM_V4_FIXTURE_AVAILABLE_KEY", envKey)
	t.Setenv("PAIRROOM_V4_FIXTURE_MISSING_KEY", "")
	cases := []struct {
		name, auth, rootToken, tableToken, envName, wantKey string
		config                                              string
	}{
		{name: "auth-before-inline-tokens", auth: authKey, rootToken: rootKey, tableToken: tableKey, wantKey: authKey},
		{name: "selected-table-before-root", rootToken: rootKey, tableToken: tableKey, wantKey: tableKey},
		{name: "root-token-fallback", rootToken: rootKey, wantKey: rootKey},
		{name: "declared-env-before-stale-auth", auth: authKey, rootToken: rootKey, tableToken: tableKey, envName: "PAIRROOM_V4_FIXTURE_AVAILABLE_KEY", wantKey: envKey},
		{name: "missing-declared-env-cannot-use-stale-auth", auth: authKey, rootToken: rootKey, tableToken: tableKey, envName: "PAIRROOM_V4_FIXTURE_MISSING_KEY"},
		{
			name: "quoted-table-and-literal-values", wantKey: literalKey,
			config: `model_provider = 'vendor.route'
model = 'codex-v4'
model_reasoning_effort = 'high'
[model_providers."vendor.route"]
name = 'Gateway # literal name'
wire_api = 'responses'
base_url = 'https://codex-v4.invalid/v1'
experimental_bearer_token = 'v4-literal-fixture#key' # a comment after a literal
`,
		},
		{
			name: "literal-table-key", wantKey: tableKey,
			config: `model_provider = 'vendor.route'
model = 'codex-v4'
model_reasoning_effort = 'high'
[model_providers.'vendor.route']
name = 'Literal table'
wire_api = 'responses'
base_url = 'https://codex-v4.invalid/v1'
experimental_bearer_token = 'v4-table-fixture-key'
`,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			configText := test.config
			if configText == "" {
				configText = "model_provider = \"custom\"\nmodel = \"codex-v4\"\nmodel_reasoning_effort = \"high\"\n"
				if test.rootToken != "" {
					configText += "experimental_bearer_token = \"" + test.rootToken + "\"\n"
				}
				configText += "[model_providers.custom]\nname = \"Fixture\"\nwire_api = \"responses\"\nbase_url = \"https://codex-v4.invalid/v1\"\n"
				if test.tableToken != "" {
					configText += "experimental_bearer_token = \"" + test.tableToken + "\"\n"
				}
				if test.envName != "" {
					configText += "env_key = \"" + test.envName + "\"\n"
				}
			}
			auth := map[string]string{}
			if test.auth != "" {
				auth["OPENAI_API_KEY"] = test.auth
			}
			reader := v4FixtureReader(t, fixtureProfile{
				id: test.name, appType: "codex", name: "Codex fixture",
				settings: v4JSON(t, map[string]any{"auth": auth, "config": configText}),
				meta:     `{"authBinding":{"source":"provider_config"}}`,
			})
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.Profiles) != 1 {
				t.Fatalf("catalog has %d profiles, want one", len(catalog.Profiles))
			}
			resolved, err := reader.Resolve(context.Background(), catalog.Profiles[0].ProviderRef, model.RuntimeCodex)
			if test.wantKey == "" {
				if catalog.Profiles[0].Supported || catalog.Profiles[0].ReasonCode != ReasonMissingCredential {
					t.Fatal("missing declared environment key was replaced with another credential source")
				}
				v4AssertUnsupported(t, err, ReasonMissingCredential, authKey, tableKey, rootKey)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !catalog.Profiles[0].Supported || resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != test.wantKey {
					t.Fatal("Codex credential source did not follow the declared v4 precedence")
				}
				if resolved.DefaultModel != "codex-v4" || resolved.DefaultEffort != "high" {
					t.Fatalf("model / effort = %q / %q, want codex-v4 / high", resolved.DefaultModel, resolved.DefaultEffort)
				}
				if !strings.Contains(strings.Join(resolved.Args, "\x00"), "https://codex-v4.invalid/v1") {
					t.Fatal("selected provider endpoint was not projected")
				}
			}
			v4AssertPublicSecretFree(t, catalog, resolved, authKey, tableKey, rootKey, envKey, literalKey)
		})
	}
}

func TestV4ClaudeProviderFieldsAndStackModels(t *testing.T) {
	const key = "v4-claude-fields-fixture-key"
	reader := v4FixtureReader(t, fixtureProfile{
		id: "claude-fields", appType: "claude", name: "Claude provider fields",
		settings: `{
  "model":"claude-primary-v4",
  "env":{
    "ANTHROPIC_AUTH_TOKEN":"` + key + `",
    "ANTHROPIC_BASE_URL":"https://claude-v4.invalid",
    "ANTHROPIC_DEFAULT_OPUS_MODEL":"claude-opus-v4",
    "ANTHROPIC_DEFAULT_SONNET_MODEL":"claude-sonnet-v4",
    "CLAUDE_CODE_AUTO_MODE_SERVER":"0",
    "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS":"1",
    "ENABLE_TOOL_SEARCH":"false",
    "CLAUDE_CODE_MAX_CONTEXT_TOKENS":"262144",
    "UNRELATED_SHARED_ENV":"shared-env-must-not-copy"
  },
  "hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"shared-hook-must-not-run"}]}]},
  "permissions":{"allow":["shared-permission-must-not-copy"]},
  "mcpServers":{"search":{"command":"shared-mcp-must-not-copy","model":"mcp-model-must-not-appear"}}
}`,
		meta: `{"stackModels":[{"model":"stack-v4","displayName":"stack-display-must-not-be-a-model"},{"model":"stack-large-v4","oneM":true}]}`,
	})
	catalog, err := reader.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported {
		t.Fatal("Claude v4 provider was not supported")
	}
	resolved, err := reader.Resolve(context.Background(), catalog.Profiles[0].ProviderRef, model.RuntimeClaude)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.DefaultModel != "claude-primary-v4" {
		t.Fatalf("default model = %q, want the provider's top-level model", resolved.DefaultModel)
	}
	for field, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN":                   key,
		"CLAUDE_CODE_AUTO_MODE_SERVER":           "0",
		"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1",
		"ENABLE_TOOL_SEARCH":                     "false",
		"CLAUDE_CODE_MAX_CONTEXT_TOKENS":         "262144",
	} {
		if resolved.Env[field] != want {
			t.Fatalf("provider-owned field %s was not preserved", field)
		}
	}
	if _, ok := resolved.Env["UNRELATED_SHARED_ENV"]; ok {
		t.Fatal("shared environment snapshot was imported as a provider override")
	}
	public, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	for _, shared := range []string{"shared-hook-must-not-run", "shared-permission-must-not-copy", "shared-mcp-must-not-copy", "mcp-model-must-not-appear", "stack-display-must-not-be-a-model"} {
		if strings.Contains(strings.Join(resolved.Args, "\x00"), shared) || strings.Contains(string(public), shared) {
			t.Fatal("shared snapshot or display text was projected into the provider")
		}
		for _, value := range resolved.Env {
			if strings.Contains(value, shared) {
				t.Fatal("shared snapshot was copied into the child environment")
			}
		}
	}
	wantModels := []string{"claude-primary-v4", "claude-opus-v4", "claude-sonnet-v4", "stack-v4", "stack-large-v4[1M]"}
	v4AssertModels(t, catalog.Profiles[0].Models, wantModels)
	v4AssertModels(t, resolved.Models, wantModels)
	v4AssertPublicSecretFree(t, catalog, resolved, key)
}

func TestV4CodexModelCatalogExcludesUnrelatedTables(t *testing.T) {
	const key = "v4-catalog-fixture-key"
	reader := v4FixtureReader(t, fixtureProfile{
		id: "codex-catalog", appType: "codex", name: "Codex model catalog",
		settings: v4JSON(t, map[string]any{
			"auth": map[string]string{"OPENAI_API_KEY": key},
			"config": `model_provider = "custom"
model = "codex-default-v4"
model_reasoning_effort = "high"
[model_providers.custom]
name = "Selected"
wire_api = "responses"
base_url = "https://codex-v4.invalid/v1"
[model_providers.unselected]
model = "unselected-provider-model"
[mcp_servers.search]
model = "unrelated-mcp-model"
[profiles.leftover]
model = "unrelated-profile-model"
`,
			"modelCatalog": map[string]any{"models": []map[string]any{
				{"model": "catalog-first-v4", "displayName": "display-is-not-a-model", "contextWindow": 262144},
				{"model": "catalog-second-v4", "baseInstructions": "instructions-are-not-a-model", "reasoningLevels": []string{"low", "high"}},
			}},
		}),
		meta: `{}`,
	})
	catalog, err := reader.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported {
		t.Fatal("Codex model catalog provider was not supported")
	}
	resolved, err := reader.Resolve(context.Background(), catalog.Profiles[0].ProviderRef, model.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	wantModels := []string{"codex-default-v4", "catalog-first-v4", "catalog-second-v4"}
	v4AssertModels(t, catalog.Profiles[0].Models, wantModels)
	v4AssertModels(t, resolved.Models, wantModels)
	if resolved.DefaultModel != "codex-default-v4" || resolved.DefaultEffort != "high" {
		t.Fatal("catalog entries displaced the provider's explicit default model or effort")
	}
	v4AssertPublicSecretFree(t, catalog, resolved, key)
}

func TestV4ModelMetadataCannotPublishCredentials(t *testing.T) {
	const key = "v4-model-metadata-fixture-key"
	cases := []struct {
		app, settings, meta string
		runtime             model.RuntimeKind
	}{
		{
			"claude",
			`{"env":{"ANTHROPIC_AUTH_TOKEN":"` + key + `","ANTHROPIC_BASE_URL":"https://claude-v4.invalid","ANTHROPIC_MODEL":"claude-safe"}}`,
			`{"stackModels":[{"model":"` + key + `"}]}`,
			model.RuntimeClaude,
		},
		{
			"codex",
			v4JSON(t, map[string]any{
				"auth":         map[string]string{},
				"config":       "model_provider = \"custom\"\nmodel = \"codex-safe\"\n[model_providers.custom]\nwire_api = \"responses\"\nbase_url = \"https://codex-v4.invalid/v1\"\nexperimental_bearer_token = \"" + key + "\"\n",
				"modelCatalog": map[string]any{"models": []map[string]string{{"model": key}}},
			}),
			`{}`,
			model.RuntimeCodex,
		},
	}
	for _, test := range cases {
		t.Run(test.app, func(t *testing.T) {
			reader := v4FixtureReader(t, fixtureProfile{id: "leaky-model", appType: test.app, name: "Metadata boundary", settings: test.settings, meta: test.meta})
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported || catalog.Profiles[0].ReasonCode != ReasonInvalidConfig {
				t.Fatal("credential-bearing model metadata was accepted")
			}
			resolved, err := reader.Resolve(context.Background(), catalog.Profiles[0].ProviderRef, test.runtime)
			v4AssertUnsupported(t, err, ReasonInvalidConfig, key)
			v4AssertPublicSecretFree(t, catalog, resolved, key)
		})
	}
}

func v4FixtureReader(t *testing.T, profiles ...fixtureProfile) *Reader {
	t.Helper()
	path := writeFixture(t, 20, profiles...)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
			return
		}
		if sha256.Sum256(before) != sha256.Sum256(after) {
			t.Error("reading or resolving v4 providers changed the CC Switch database")
		}
	})
	reader, err := NewReader(path)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func v4JSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func v4AssertModels(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("models = %q, want only %q", got, want)
	}
	for _, modelName := range want {
		if !slices.Contains(got, modelName) {
			t.Fatalf("models = %q, missing %q", got, modelName)
		}
	}
}

func v4AssertUnsupported(t *testing.T, err error, reason string, secrets ...string) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != CodeProfileUnsupported {
		t.Fatal("unsupported provider did not return a typed fail-closed error")
	}
	if reason != "" && typed.Params["reason"] != reason {
		t.Fatalf("error reason = %q, want %q", typed.Params["reason"], reason)
	}
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("profile rejection disclosed a fixture credential")
		}
	}
}

func v4AssertPublicSecretFree(t *testing.T, catalog Catalog, resolved Materialization, secrets ...string) {
	t.Helper()
	public := v4JSON(t, catalog) + v4JSON(t, resolved) + strings.Join(resolved.Args, "\x00")
	for _, secret := range secrets {
		if strings.Contains(public, secret) {
			t.Fatal("a fixture credential escaped into catalog, materialization JSON, or argv")
		}
	}
}
