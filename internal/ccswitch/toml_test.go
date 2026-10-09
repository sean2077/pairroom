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

func TestTOMLUnselectedCredentialNamesDoNotDisableProvider(t *testing.T) {
	for _, suffix := range []string{
		"[model_providers.other.env_key]\nname = 'UNRELATED_ENV'\n",
		"[mcp_servers.secret]\ncommand = 'unrelated-tool'\n",
	} {
		t.Run(strings.SplitN(suffix, "\n", 2)[0], func(t *testing.T) {
			reader := tomlCodexReader(t, "Selected provider", tomlDirectCodexConfig+suffix)
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported || resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != tomlDirectKey {
				t.Fatal("unrelated table changed selected Provider support or credentials")
			}
			if strings.Contains(strings.Join(resolved.Args, "\x00"), "unrelated") {
				t.Fatal("unrelated table entered the selected Provider arguments")
			}
			v4AssertPublicSecretFree(t, catalog, resolved, tomlDirectKey)
		})
	}
}

func TestTOMLUnselectedSecretsStillProtectPublicMetadata(t *testing.T) {
	const (
		apiKey = "toml-unselected-api-fixture-key"
		token  = "toml-unselected-token-fixture"
		envKey = "toml-unselected-env-fixture-key"
	)
	t.Setenv("PAIRROOM_TOML_UNSELECTED_KEY", envKey)
	for _, layout := range []string{"table", "dotted"} {
		for _, collision := range []string{"none", "name", "id", "model"} {
			t.Run(layout+"/"+collision, func(t *testing.T) {
				config := tomlDirectCodexConfig + "[model_providers.other.env_key]\nname = 'UNRELATED_ENV'\n[mcp_servers.secret]\ncommand = 'unrelated-tool'\n"
				fields := []string{"api_key = '" + apiKey + "'", "token = '" + token + "'", "env_key = 'PAIRROOM_TOML_UNSELECTED_KEY'"}
				if layout == "table" {
					config += "[model_providers.spare]\n" + strings.Join(fields, "\n") + "\n"
				} else {
					config = "model_providers.spare." + strings.Join(fields, "\nmodel_providers.spare.") + "\n" + config
				}
				id, name := "unselected-secret", "Selected provider"
				switch collision {
				case "name":
					name += " " + apiKey
				case "id":
					id += "-" + envKey
				case "model":
					config = strings.Replace(config, `model = "codex-safe"`, `model = "`+token+`"`, 1)
				}
				reader := v4FixtureReader(t, fixtureProfile{
					id: id, appType: "codex", name: name,
					settings: v4JSON(t, map[string]any{"auth": map[string]string{"OPENAI_API_KEY": tomlDirectKey}, "config": config}), meta: `{}`,
				})
				catalog, err := reader.Catalog(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: id}, model.RuntimeCodex)
				if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported != (collision == "none") {
					t.Fatal("unselected credential values changed support without a public-field collision, or a collision remained selectable")
				}
				if collision == "none" {
					if err != nil || resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != tomlDirectKey {
						t.Fatal("unselected credentials replaced or disabled the selected key")
					}
				} else {
					v4AssertUnsupported(t, err, ReasonInvalidConfig, apiKey, token, envKey, tomlDirectKey)
					if len(resolved.Env) != 0 || len(resolved.Args) != 0 {
						t.Fatal("public credential collision produced a partial materialization")
					}
				}
				v4AssertPublicSecretFree(t, catalog, resolved, apiKey, token, envKey, tomlDirectKey)
			})
		}
	}
}

func TestTOMLSelectedOpaqueCredentialsWithholdMetadataAndDirectErrors(t *testing.T) {
	const hiddenKey = "toml-selected-opaque-fixture-key"
	for _, test := range []struct {
		name, app, config string
		runtime           model.RuntimeKind
	}{
		{"codex-env-heading", "codex", tomlDirectCodexConfig + "[model_providers.custom.env_key]\nvalue = '" + hiddenKey + "'\n", model.RuntimeCodex},
		{"codex-env-dotted", "codex", tomlDirectCodexConfig + "env_key.value = '" + hiddenKey + "'\n", model.RuntimeCodex},
		{"codex-bearer-heading", "codex", tomlDirectCodexConfig + "[model_providers.custom.experimental_bearer_token]\nvalue = '" + hiddenKey + "'\n", model.RuntimeCodex},
		{"codex-root-bearer", "codex", tomlDirectCodexConfig + "[experimental_bearer_token]\nvalue = '" + hiddenKey + "'\n", model.RuntimeCodex},
		{"grok-api-heading", "grokbuild", strings.Replace(tomlDirectGrokConfig, "api_key = 'grok-private-fixture-key'\n", "", 1) + "[model.direct.api_key]\nvalue = '" + hiddenKey + "'\n", model.RuntimeGrok},
		{"grok-env-dotted", "grokbuild", tomlDirectGrokConfig + "env_key.value = '" + hiddenKey + "'\n", model.RuntimeGrok},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := profileRow{
				ID: "opaque-" + hiddenKey, AppType: test.app, Name: "Provider " + hiddenKey,
				Settings: v4JSON(t, map[string]any{"auth": map[string]string{"OPENAI_API_KEY": tomlDirectKey}, "config": test.config}), Meta: `{}`,
			}
			reader := v4FixtureReader(t, fixtureProfile{id: p.ID, appType: p.AppType, name: p.Name, settings: p.Settings, meta: p.Meta})
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported || catalog.Profiles[0].ProviderRef.ProfileID != "" || len(catalog.Profiles[0].Models) != 0 {
				t.Fatal("opaque selected credentials left row metadata or a selectable reference")
			}
			resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: p.AppType, ProfileID: p.ID}, test.runtime)
			v4AssertUnsupported(t, err, ReasonInvalidConfig, hiddenKey, tomlDirectKey)
			v4AssertPublicSecretFree(t, catalog, resolved, hiddenKey, tomlDirectKey)
			direct, err := materialize(p, test.runtime)
			v4AssertUnsupported(t, err, ReasonInvalidConfig, hiddenKey, tomlDirectKey)
			if len(direct.Env) != 0 || len(direct.Args) != 0 || direct.Grok != nil {
				t.Fatal("direct mapper bypassed selected credential inspection")
			}
		})
	}
}

func TestTOMLQuotedScalarOptionsRemainCompatible(t *testing.T) {
	for _, test := range []struct{ name, prefix, suffix, want string }{
		{"storage", "disable_response_storage = 'false'\n", "", "disable_response_storage=false"},
		{"summaries", "model_supports_reasoning_summaries = 'true'\n", "", "model_supports_reasoning_summaries=true"},
		{"context", "model_context_window = '262144'\n", "", "model_context_window=262144"},
		{"compaction", "model_auto_compact_token_limit = '200000'\n", "", "model_auto_compact_token_limit=200000"},
		{"websockets", "", "supports_websockets = 'false'\n", ".supports_websockets=false"},
		{"ignored-auth", "", "requires_openai_auth = 'ignored'\n", ".requires_openai_auth=false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := tomlCodexReader(t, "Scalar compatibility", test.prefix+tomlDirectCodexConfig+test.suffix)
			resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			found := slices.Contains(resolved.Args, test.want)
			if strings.HasPrefix(test.want, ".") {
				found = slices.ContainsFunc(resolved.Args, func(arg string) bool { return strings.HasSuffix(arg, test.want) })
			}
			if !found {
				t.Fatalf("missing canonical option %q", test.want)
			}
		})
	}
}

func TestTOMLInvalidQuotedScalarOptionsRemainRejected(t *testing.T) {
	for _, option := range []string{
		"disable_response_storage = 'FALSE'\n",
		"model_context_window = '262144.0'\n",
		"model_context_window = '9223372036854775808'\n",
		"model_context_window = '0'\n",
		"model_auto_compact_token_limit = '-1'\n",
	} {
		t.Run(strings.TrimSpace(option), func(t *testing.T) {
			tomlAssertRejected(t, tomlCodexReader(t, "Invalid scalar option", option+tomlDirectCodexConfig))
		})
	}
}

func TestTOMLGrokDottedIdentityAndCredentialsRequireStrings(t *testing.T) {
	for _, test := range []struct{ name, selector, alias, field, value string }{
		{"numeric-selector", "123", "123", "", ""},
		{"boolean-selector", "true", "true", "", ""},
		{"numeric-model", "'direct'", "direct", "model", "123"},
		{"boolean-name", "'direct'", "direct", "name", "true"},
		{"boolean-credential", "'direct'", "direct", "api_key", "true"},
		{"numeric-credential", "'direct'", "direct", "api_key", "123"},
		{"boolean-env-key", "'direct'", "direct", "env_key", "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields := map[string]string{
				"model": "'grok-upstream'", "name": "'Grok direct'", "base_url": "'https://grok.invalid/v1'",
				"api_key": "'grok-private-fixture-key'", "env_key": "''", "context_window": "262144",
			}
			if test.field != "" {
				fields[test.field] = test.value
			}
			prefix := "model." + test.alias + "."
			config := "models.default = " + test.selector + "\n"
			for _, key := range []string{"model", "name", "base_url", "api_key", "env_key", "context_window"} {
				config += prefix + key + " = " + fields[key] + "\n"
			}
			reader := v4FixtureReader(t, fixtureProfile{
				id: "grok-types", appType: "grokbuild", name: "Grok scalar types",
				settings: v4JSON(t, map[string]any{"config": config}), meta: `{}`,
			})
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "grokbuild", ProfileID: "grok-types"}, model.RuntimeGrok)
			v4AssertUnsupported(t, err, ReasonInvalidConfig, "grok-private-fixture-key")
			if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported || len(resolved.Env) != 0 || resolved.Grok != nil {
				t.Fatal("non-string identity or credential reached Grok materialization")
			}
			if test.field == "" && len(catalog.Profiles[0].Models) != 0 {
				t.Fatal("non-string selector became a catalog model suggestion")
			}
			v4AssertPublicSecretFree(t, catalog, resolved, "grok-private-fixture-key")
		})
	}
}

func TestTOMLGrokDottedAndTableProfilesPreserveValues(t *testing.T) {
	const envKey = "grok-environment-fixture-key"
	t.Setenv("PAIRROOM_TOML_GROK_KEY", envKey)
	for _, layout := range []string{"table", "dotted"} {
		for _, source := range []string{"api_key", "env_key"} {
			t.Run(layout+"/"+source, func(t *testing.T) {
				key, wantKey := "grok-private-fixture-key", "grok-private-fixture-key"
				if source == "env_key" {
					key, wantKey = "", envKey
				}
				fields := []string{
					"model = 'grok-upstream'", "name = 'Grok direct'", "base_url = 'https://grok.invalid/v1'",
					"api_key = '" + key + "'", "env_key = 'PAIRROOM_TOML_GROK_KEY'", "context_window = ' 262144 '",
				}
				// A numeric-looking alias remains valid when the selector is a
				// string. Grok also historically trims a quoted context window.
				config := "[models]\ndefault = '123'\n[model.123]\n" + strings.Join(fields, "\n") + "\n"
				if layout == "dotted" {
					config = "models.default = '123'\nmodel.123." + strings.Join(fields, "\nmodel.123.") + "\n"
				}
				reader := v4FixtureReader(t, fixtureProfile{
					id: "grok-valid", appType: "grokbuild", name: "Grok scalar compatibility",
					settings: v4JSON(t, map[string]any{"config": config}), meta: `{}`,
				})
				catalog, err := reader.Catalog(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "grokbuild", ProfileID: "grok-valid"}, model.RuntimeGrok)
				if err != nil {
					t.Fatal(err)
				}
				if len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported || resolved.DefaultModel != "123" || resolved.Grok == nil || resolved.Grok.ContextWindow != 262144 || resolved.Grok.UpstreamModel != "grok-upstream" || resolved.Env["PAIRROOM_CC_SWITCH_GROK_API_KEY"] != wantKey {
					t.Fatal("equivalent Grok declarations changed the model, context window, or credential priority")
				}
				v4AssertPublicSecretFree(t, catalog, resolved, "grok-private-fixture-key", envKey)
			})
		}
	}
}

const tomlDirectKey = "toml-direct-api-fixture-key"

const tomlDirectGrokConfig = `[models]
default = 'direct'
[model.direct]
model = 'grok-upstream'
name = 'Grok direct'
base_url = 'https://grok.invalid/v1'
api_key = 'grok-private-fixture-key'
context_window = 262144
`

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
