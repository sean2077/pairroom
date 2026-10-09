package ccswitch

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

// Both payloads are valid TOML: the apparent table and URL are string contents
// inside shared hook settings, not an alternate provider configuration.
func TestTOMLSharedMultilineCannotRetargetProvider(t *testing.T) {
	for _, test := range []struct{ name, payload string }{
		{"escaped-basic-delimiter", `[hooks.fixture]
command = """
An escaped delimiter: \"""
[model_providers.custom]
base_url = "https://shared-payload.invalid/v1"
"""
`},
		{"multiline-string-inside-array", `[hooks.fixture]
commands = [
"""
]
[model_providers.custom]
base_url = "https://shared-payload.invalid/v1"
"""
]
`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := tomlCodexReader(t, "Shared hook snapshot", tomlDirectCodexConfig+test.payload)
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Join(resolved.Args, "\x00")
			if strings.Contains(args, "shared-payload.invalid") || !strings.Contains(args, "https://selected-toml.invalid/v1") {
				t.Fatal("shared multiline text replaced the selected provider endpoint")
			}
			if len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported || resolved.DefaultModel != "codex-safe" {
				t.Fatal("shared multiline text changed the selected provider or its model")
			}
			v4AssertPublicSecretFree(t, catalog, resolved, tomlDirectKey)
		})
	}
}

func TestTOMLQuotedProviderIDDoesNotResolveNestedTable(t *testing.T) {
	cases := []struct {
		app, config string
		runtime     model.RuntimeKind
	}{
		{"codex", `model_provider = "vendor.route"
model = "codex-safe"
[model_providers."vendor.route"]
wire_api = "responses"
base_url = "https://selected-toml.invalid/v1"
[model_providers.vendor.route]
wire_api = "responses"
base_url = "https://nested-table.invalid/v1"
`, model.RuntimeCodex},
		{"grokbuild", `[models]
default = "vendor.route"
[model."vendor.route"]
model = "grok-selected"
name = "Selected literal ID"
api_key = "toml-selected-grok-fixture-key"
api_backend = "responses"
context_window = 262144
base_url = "https://selected-toml.invalid/v1"
[model.vendor.route]
model = "grok-nested"
name = "Nested table"
api_key = "toml-nested-grok-fixture-key"
api_backend = "responses"
context_window = 262144
base_url = "https://nested-table.invalid/v1"
`, model.RuntimeGrok},
	}
	for _, test := range cases {
		t.Run(test.app, func(t *testing.T) {
			settings := map[string]any{"config": test.config}
			if test.runtime == model.RuntimeCodex {
				settings["auth"] = map[string]string{"OPENAI_API_KEY": tomlDirectKey}
			}
			reader := v4FixtureReader(t, fixtureProfile{
				id: "quoted-id", appType: test.app, name: "Quoted provider ID",
				settings: v4JSON(t, settings), meta: `{}`,
			})
			resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: test.app, ProfileID: "quoted-id"}, test.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if test.runtime == model.RuntimeGrok {
				if resolved.Grok == nil || resolved.Grok.BaseURL != "https://selected-toml.invalid/v1" || resolved.Grok.UpstreamModel != "grok-selected" || resolved.Env["PAIRROOM_CC_SWITCH_GROK_API_KEY"] != "toml-selected-grok-fixture-key" {
					t.Fatal("Grok selected a nested table instead of the exact quoted provider ID")
				}
			} else if args := strings.Join(resolved.Args, "\x00"); strings.Contains(args, "nested-table.invalid") || !strings.Contains(args, "https://selected-toml.invalid/v1") {
				t.Fatal("Codex selected a nested table instead of the exact quoted provider ID")
			}
		})
	}
}

func TestTOMLUnsupportedAuthDeclarationsFailClosed(t *testing.T) {
	for _, test := range []struct{ name, declaration string }{
		{"dotted-auth", "auth.type = 'bearer'\nauth.token = 'toml-other-fixture-token'\n"},
		{"dotted-http-header", "http_headers.Authorization = 'Bearer toml-header-fixture-token'\n"},
		{"dotted-env-header", "env_http_headers.Authorization = 'PAIRROOM_TOML_HEADER_FIXTURE'\n"},
		{"nested-auth-table", "[model_providers.custom.auth.aws]\nregion = 'us-west-2'\n"},
		{"empty-auth-table", "[model_providers.custom.auth]\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := tomlCodexReader(t, "Unsupported provider auth", tomlDirectCodexConfig+test.declaration)
			tomlAssertRejected(t, reader)
		})
	}
}

func TestTOMLUnsupportedCredentialSyntaxCannotLeakPublicFields(t *testing.T) {
	const hiddenKey = "toml-hidden-credential-fixture"
	for _, test := range []struct{ name, declaration string }{
		{"inline-auth", "auth = { token = '" + hiddenKey + "' }\n"},
		{"dotted-auth", "auth.token = '" + hiddenKey + "'\n"},
		{"multiline-bearer", "experimental_bearer_token = \"\"\"" + hiddenKey + "\"\"\"\n"},
		{"array-table-credential", "[[hooks.fixture]]\napi_key = '" + hiddenKey + "'\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := tomlCodexReader(t, "Provider "+hiddenKey, tomlDirectCodexConfig+test.declaration)
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, resolveErr := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
			v4AssertPublicSecretFree(t, catalog, resolved, hiddenKey, tomlDirectKey)
			v4AssertUnsupported(t, resolveErr, ReasonInvalidConfig, hiddenKey, tomlDirectKey)
			if len(resolved.Env) != 0 || len(resolved.Args) != 0 {
				t.Fatal("unsupported credential syntax produced a partial materialization")
			}
		})
	}
}

func TestTOMLCompositeProviderOptionsFailClosed(t *testing.T) {
	for _, test := range []struct{ name, config string }{
		{"model-array", strings.Replace(tomlDirectCodexConfig, `model = "codex-safe"`, `model = ["codex-unsupported"]`, 1)},
		{"review-model-inline-table", "review_model = { name = 'codex-unsupported' }\n" + tomlDirectCodexConfig},
		{"env-key-inline-table", tomlDirectCodexConfig + "env_key = { name = 'PAIRROOM_TOML_KEY_FIXTURE' }\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := tomlCodexReader(t, "Unsupported provider scalar", test.config)
			tomlAssertRejected(t, reader)
		})
	}
}

func TestTOMLInvalidProviderDocumentDoesNotMaterializeValidPrefix(t *testing.T) {
	for _, test := range []struct{ name, suffix string }{
		{"unterminated-shared-string", "[hooks.fixture]\ncommand = \"\"\"unfinished\n"},
		{"duplicate-provider-endpoint", "base_url = 'https://duplicate-endpoint.invalid/v1'\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tomlAssertRejected(t, tomlCodexReader(t, "Invalid provider document", tomlDirectCodexConfig+test.suffix))
		})
	}
}

func TestTOMLDottedProviderOptionsPreserveTheirMeaning(t *testing.T) {
	for _, declarations := range []string{
		`agents.default_subagent_model = "codex-subagent"
agents.default_subagent_reasoning_effort = "low"
memories.extract_model = "codex-extract"
memories.consolidation_model = "codex-consolidate"
`,
		`[agents]
default_subagent_model = "codex-subagent"
default_subagent_reasoning_effort = "low"
[memories]
extract_model = "codex-extract"
consolidation_model = "codex-consolidate"
`,
	} {
		t.Run(strings.SplitN(declarations, "\n", 2)[0], func(t *testing.T) {
			// Keep top-level dotted assignments before the first table header.
			config := "model_provider = \"custom\"\nmodel = \"codex-safe\"\n" + declarations +
				"[model_providers.custom]\nwire_api = \"responses\"\nbase_url = \"https://selected-toml.invalid/v1\"\n"
			reader := tomlCodexReader(t, "Provider-owned nested options", config)
			resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				`agents.default_subagent_model="codex-subagent"`,
				`agents.default_subagent_reasoning_effort="low"`,
				`memories.extract_model="codex-extract"`,
				`memories.consolidation_model="codex-consolidate"`,
			} {
				if !slices.Contains(resolved.Args, want) {
					t.Fatalf("provider-owned option %q was silently discarded", want)
				}
			}
		})
	}
}

func TestTOMLTableCredentialDeclarationCannotFallBackToStoredKey(t *testing.T) {
	for _, declaration := range []string{
		"[model_providers.custom.env_key]\nname = 'PAIRROOM_TOML_MISSING_KEY'\n",
		"env_key.name = 'PAIRROOM_TOML_MISSING_KEY'\n",
	} {
		t.Run(strings.SplitN(declaration, "\n", 2)[0], func(t *testing.T) {
			tomlAssertRejected(t, tomlCodexReader(t, "Invalid credential declaration", tomlDirectCodexConfig+declaration))
		})
	}
}

func TestTOMLDottedProviderRouteKeepsCredentialsPrivate(t *testing.T) {
	const credential = "toml-dotted-environment-fixture-key"
	t.Setenv("PAIRROOM_TOML_DOTTED_KEY", credential)
	for _, test := range []struct{ name, config string }{
		{"root", `model_provider = "vendor.route"
model_providers."vendor.route".name = "Dotted provider"
model_providers."vendor.route".wire_api = "responses"
model_providers."vendor.route".base_url = "https://dotted-toml.invalid/v1"
model_providers."vendor.route".env_key = "PAIRROOM_TOML_DOTTED_KEY"
`},
		{"parent-table", `model_provider = "vendor.route"
[model_providers]
"vendor.route".name = "Dotted provider"
"vendor.route".wire_api = "responses"
"vendor.route".base_url = "https://dotted-toml.invalid/v1"
"vendor.route".env_key = "PAIRROOM_TOML_DOTTED_KEY"
`},
		{"implicit-provider-table", `model_provider = "vendor.route"
[model_providers."vendor.route".shared]
ignored = true
[model_providers]
"vendor.route".name = "Dotted provider"
"vendor.route".wire_api = "responses"
"vendor.route".base_url = "https://dotted-toml.invalid/v1"
"vendor.route".env_key = "PAIRROOM_TOML_DOTTED_KEY"
`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := tomlCodexReader(t, "Dotted provider", test.config)
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != credential ||
				!strings.Contains(strings.Join(resolved.Args, "\x00"), `base_url="https://dotted-toml.invalid/v1"`) {
				t.Fatal("dotted route did not preserve its endpoint and authoritative credential source")
			}
			v4AssertPublicSecretFree(t, catalog, resolved, credential, tomlDirectKey)
			redacted, err := tomlCodexReader(t, "Provider "+credential, test.config).Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			v4AssertPublicSecretFree(t, redacted, Materialization{}, credential, tomlDirectKey)
		})
	}
}

func TestTOMLDeclaredScalarShapesCannotBecomeNativeDefaults(t *testing.T) {
	for _, test := range []struct{ name, prefix, suffix string }{
		{"inline-agents", "agents = { default_subagent_model = 'codex-subagent' }\n", ""},
		{"inline-memories", "memories = { extract_model = 'codex-extract' }\n", ""},
		{"scalar-agents", "agents = false\n", ""},
		{"scalar-memories", "memories = 123\n", ""},
		{"table-model", "", "[model]\nname = 'codex-unsupported'\n"},
		{"table-effort", "", "[model_reasoning_effort]\nlevel = 'low'\n"},
		{"table-subagent", "", "[agents.default_subagent_model]\nname = 'codex-subagent'\n"},
		{"table-bearer", "", "[model_providers.custom.experimental_bearer_token]\nvalue = 'nested-secret'\n"},
		{"table-websockets", "", "[model_providers.custom.supports_websockets]\nenabled = true\n"},
		{"numeric-model", "model = 123\n", ""},
		{"boolean-effort", "model_reasoning_effort = false\n", ""},
		{"string-context", "model_context_window = '262144'\n", ""},
		{"string-boolean", "disable_response_storage = 'false'\n", ""},
		{"numeric-env-key", "", "env_key = 123\n"},
		{"boolean-bearer", "", "experimental_bearer_token = false\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := strings.Replace(tomlDirectCodexConfig, "model = \"codex-safe\"\n", "", 1)
			tomlAssertRejected(t, tomlCodexReader(t, "Invalid scalar declaration", test.prefix+base+test.suffix))
		})
	}
}

func TestTOMLDottedTableCannotBeRedefined(t *testing.T) {
	for _, test := range []struct{ name, config string }{
		{"dotted-then-heading", `model_provider = "custom"
model_providers.custom.name = "Dotted provider"
[model_providers.custom]
wire_api = "responses"
base_url = "https://selected-toml.invalid/v1"
`},
		{"heading-then-dotted", tomlDirectCodexConfig + "[model_providers]\ncustom.name = 'Redefined provider'\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tomlAssertRejected(t, tomlCodexReader(t, "Redefined dotted table", test.config))
		})
	}
}

const tomlDirectKey = "toml-direct-api-fixture-key"

const tomlDirectCodexConfig = `model_provider = "custom"
model = "codex-safe"
[model_providers.custom]
wire_api = "responses"
base_url = "https://selected-toml.invalid/v1"
`

func tomlCodexReader(t *testing.T, name, config string) *Reader {
	t.Helper()
	return v4FixtureReader(t, fixtureProfile{
		id: "toml-fixture", appType: "codex", name: name,
		settings: v4JSON(t, map[string]any{
			"auth": map[string]string{"OPENAI_API_KEY": tomlDirectKey}, "config": config,
		}),
		meta: `{}`,
	})
}

func tomlCodexRef() model.ProviderRef {
	return model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "toml-fixture"}
}

func tomlAssertRejected(t *testing.T, reader *Reader) {
	t.Helper()
	catalog, err := reader.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported {
		t.Fatal("unsupported or invalid TOML was exposed as a selectable provider")
	}
	resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
	v4AssertUnsupported(t, err, "", tomlDirectKey)
	if len(resolved.Env) != 0 || len(resolved.Args) != 0 {
		t.Fatal("unsupported or invalid TOML returned a partial materialization")
	}
	v4AssertPublicSecretFree(t, catalog, resolved, tomlDirectKey)
}
