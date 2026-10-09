package ccswitch

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestCodexLegacyEndpointWithDottedProviderOptions(t *testing.T) {
	for _, declaration := range []string{
		"model_providers.openai.name = 'Legacy'\n",
		"[model_providers]\nopenai.name = 'Legacy'\n",
	} {
		t.Run(strings.SplitN(declaration, "\n", 2)[0], func(t *testing.T) {
			reader := tomlCodexReader(t, "Legacy endpoint", "model_provider = 'openai'\nopenai_base_url = 'https://legacy.invalid/v1'\n"+declaration)
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported || resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != tomlDirectKey ||
				!slices.ContainsFunc(resolved.Args, func(arg string) bool { return strings.HasSuffix(arg, `.base_url="https://legacy.invalid/v1"`) }) {
				t.Fatal("a dotted provider option disabled or replaced the legacy endpoint")
			}
			v4AssertPublicSecretFree(t, catalog, resolved, tomlDirectKey)
		})
	}
}

func TestCodexStringOptionsRetainScalarTextCompatibility(t *testing.T) {
	for _, option := range []struct{ declaration, want string }{
		{"web_search = true\n", `web_search="true"`},
		{"model_verbosity = true\n", `model_verbosity="true"`},
		{"review_model = 123\n", `review_model="123"`},
		{"plan_mode_reasoning_effort = true\n", `plan_mode_reasoning_effort="true"`},
		{"[agents]\ndefault_subagent_model = 123\n", `agents.default_subagent_model="123"`},
		{"[memories]\nextract_model = 123\n", `memories.extract_model="123"`},
	} {
		t.Run(strings.TrimSpace(option.want), func(t *testing.T) {
			config := option.declaration + tomlDirectCodexConfig
			if strings.HasPrefix(option.declaration, "[") {
				config = tomlDirectCodexConfig + option.declaration
			}
			reader := tomlCodexReader(t, "Legacy scalar options", config)
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported || !slices.Contains(resolved.Args, option.want) {
				t.Fatalf("string option did not retain its quoted scalar text: want %q", option.want)
			}
			v4AssertPublicSecretFree(t, catalog, resolved, tomlDirectKey)
		})
	}
}

func TestCodexProviderDisplayNameRetainsScalarTextCompatibility(t *testing.T) {
	for _, scalar := range []string{"123", "true"} {
		t.Run(scalar, func(t *testing.T) {
			reader := tomlCodexReader(t, "Display name compatibility", tomlDirectCodexConfig+"name = "+scalar+"\n")
			resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(resolved.Args, func(arg string) bool { return strings.HasSuffix(arg, `.name="`+scalar+`"`) }) {
				t.Fatal("a scalar display name did not retain its quoted text")
			}
		})
	}
}

func TestCredentialTableDescendantsCannotEscapePublicRedaction(t *testing.T) {
	const hidden = "credential-table-private-fixture"
	for _, declaration := range []string{
		"[model_providers.spare.api_key]\nvalue = '" + hidden + "'\n",
		"[model_providers.spare]\napi_key.value = '" + hidden + "'\n",
	} {
		t.Run(strings.SplitN(declaration, "\n", 2)[0], func(t *testing.T) {
			reader := tomlCodexReader(t, "Provider "+hidden, tomlDirectCodexConfig+declaration)
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
			v4AssertPublicSecretFree(t, catalog, resolved, hidden, tomlDirectKey)
			v4AssertUnsupported(t, err, ReasonInvalidConfig, hidden, tomlDirectKey)
			if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported || len(resolved.Env) != 0 || len(resolved.Args) != 0 {
				t.Fatal("a credential table value escaped inspection or produced a partial materialization")
			}
		})
	}
}

func TestCodexLegacyDefaultsPreserveExplicitProviderValues(t *testing.T) {
	for _, layout := range []string{"heading", "root-dotted", "parent-dotted"} {
		for _, test := range []struct {
			name, selector, fields, endpoint, credential, reason string
		}{
			{"name-only", "openai", "name = 'Legacy name'\n", "https://legacy.invalid/v1", tomlDirectKey, ""},
			{"explicit-route", "openai", "name = 'Explicit name'\nbase_url = 'https://explicit.invalid/v1'\nwire_api = 'responses'\n", "https://explicit.invalid/v1", tomlDirectKey, ""},
			{"explicit-env", "openai", "env_key = 'PAIRROOM_LEGACY_ENV'\n", "https://legacy.invalid/v1", "legacy-environment-fixture-key", ""},
			{"empty-endpoint", "openai", "base_url = ''\n", "", "", ReasonInvalidConfig},
			{"invalid-endpoint", "openai", "base_url = 'relative/path'\n", "", "", ReasonInvalidConfig},
			{"typed-endpoint", "openai", "base_url = false\n", "", "", ReasonInvalidConfig},
			{"empty-wire", "openai", "wire_api = ''\n", "", "", ReasonUnsupportedWireAPI},
			{"explicit-wire", "openai", "wire_api = 'chat_completions'\n", "", "", ReasonUnsupportedWireAPI},
			{"composite-wire", "openai", "wire_api = { value = 'responses' }\n", "", "", ReasonInvalidConfig},
			{"missing-env", "openai", "env_key = 'PAIRROOM_LEGACY_MISSING_ENV'\n", "", "", ReasonMissingCredential},
			{"custom-no-fallback", "custom", "name = 'Custom name'\nwire_api = 'responses'\n", "", "", ReasonInvalidConfig},
		} {
			t.Run(layout+"/"+test.name, func(t *testing.T) {
				t.Setenv("PAIRROOM_LEGACY_ENV", "legacy-environment-fixture-key")
				t.Setenv("PAIRROOM_LEGACY_MISSING_ENV", "")
				section := "[model_providers." + test.selector + "]\n" + test.fields
				if layout != "heading" {
					prefix := "model_providers." + test.selector + "."
					section = ""
					if layout == "parent-dotted" {
						section, prefix = "[model_providers]\n", test.selector+"."
					}
					for _, line := range strings.Split(strings.TrimSpace(test.fields), "\n") {
						section += prefix + line + "\n"
					}
				}
				reader := tomlCodexReader(t, "Legacy defaults", "model_provider = '"+test.selector+"'\nopenai_base_url = 'https://legacy.invalid/v1'\n"+section)
				catalog, err := reader.Catalog(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
				if test.reason != "" {
					v4AssertUnsupported(t, err, test.reason, tomlDirectKey, "legacy-environment-fixture-key")
					if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported || len(resolved.Env) != 0 || len(resolved.Args) != 0 {
						t.Fatal("legacy defaults concealed an explicit invalid declaration")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported || resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != test.credential ||
						!slices.ContainsFunc(resolved.Args, func(arg string) bool { return strings.HasSuffix(arg, `.base_url="`+test.endpoint+`"`) }) {
						t.Fatal("equivalent provider declarations changed the effective route or credential")
					}
				}
				v4AssertPublicSecretFree(t, catalog, resolved, tomlDirectKey, "legacy-environment-fixture-key")
			})
		}
	}
}

func TestCodexEmptyWireRetainsMetadataFallback(t *testing.T) {
	reader := v4FixtureReader(t, fixtureProfile{
		id: "wire-metadata", appType: "codex", name: "Wire metadata fallback",
		settings: v4JSON(t, map[string]any{
			"config": "model_provider = 'openai'\nopenai_base_url = 'https://legacy.invalid/v1'\n[model_providers.openai]\nbase_url = 'https://explicit.invalid/v1'\nwire_api = ''\n",
			"auth":   map[string]string{"OPENAI_API_KEY": tomlDirectKey},
		}),
		meta: `{"apiFormat":"responses"}`,
	})
	resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "wire-metadata"}, model.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(resolved.Args, func(arg string) bool { return strings.HasSuffix(arg, `.base_url="https://explicit.invalid/v1"`) }) {
		t.Fatal("legacy defaults replaced the explicit route while resolving wire metadata")
	}
}

func TestCredentialContainerValuesArePrivateWithoutDisablingUnrelatedProviders(t *testing.T) {
	const hidden = "nested-credential-private-fixture"
	for _, declaration := range []struct {
		name, prefix, suffix string
		settings, meta       map[string]any
	}{
		{"unselected-heading", "", "[model_providers.spare.api_key]\nvalue = '" + hidden + "'\n", nil, nil},
		{"unselected-dotted", "", "[model_providers.spare]\napi_key.value = '" + hidden + "'\n", nil, nil},
		{"selected-unused-key", "", "[model_providers.custom.api_key]\nvalue = '" + hidden + "'\n", nil, nil},
		{"root-heading", "", "[api_key]\nvalue = '" + hidden + "'\n", nil, nil},
		{"root-dotted", "api_key.nested.value = '" + hidden + "'\n", "", nil, nil},
		{"quoted-nested", "", "[model_providers.'spare.route'.'api_key'.nested]\nvalue = '" + hidden + "'\n", nil, nil},
		{"environment-table", "", "[model_providers.spare.env_key]\nname = 'PAIRROOM_CONTAINER_ENV'\n", nil, nil},
		{"environment-in-key-table", "", "[model_providers.spare.api_key]\nenv_key = 'PAIRROOM_CONTAINER_ENV'\n", nil, nil},
		{"settings-json-map", "", "", map[string]any{"custom": map[string]any{"api_key": map[string]any{"nested": map[string]string{"value": hidden}}}}, nil},
		{"settings-json-env-in-key", "", "", map[string]any{"custom": map[string]any{"api_key": map[string]string{"env_key": "PAIRROOM_CONTAINER_ENV"}}}, nil},
		{"meta-json-array-map", "", "", nil, map[string]any{"token": []any{map[string]string{"value": hidden}}}},
		{"nested-config-snapshot", "", "", nil, map[string]any{"snapshot": map[string]any{"config": "[token]\nvalue = '" + hidden + "'\n"}}},
	} {
		for _, collision := range []string{"none", "name", "id", "model", "model-catalog", "argument"} {
			t.Run(declaration.name+"/"+collision, func(t *testing.T) {
				t.Setenv("PAIRROOM_CONTAINER_ENV", hidden)
				id, name := "container-fixture", "Credential container"
				config := tomlDirectCodexConfig
				settings := map[string]any{"auth": map[string]string{"OPENAI_API_KEY": tomlDirectKey}}
				for key, value := range declaration.settings {
					settings[key] = value
				}
				switch collision {
				case "name":
					name += " " + hidden
				case "id":
					id += "-" + hidden
				case "model":
					config = strings.Replace(config, `model = "codex-safe"`, `model = "`+hidden+`"`, 1)
				case "model-catalog":
					settings["modelCatalog"] = map[string]any{"models": []any{map[string]string{"model": hidden}}}
				case "argument":
					config = "review_model = '" + hidden + "'\n" + config
				}
				settings["config"] = declaration.prefix + config + declaration.suffix
				meta := declaration.meta
				if meta == nil {
					meta = map[string]any{}
				}
				reader := v4FixtureReader(t, fixtureProfile{id: id, appType: "codex", name: name, settings: v4JSON(t, settings), meta: v4JSON(t, meta)})
				catalog, err := reader.Catalog(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: id}, model.RuntimeCodex)
				if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported != (collision == "none") {
					t.Fatal("credential ancestry was ignored or disabled an unrelated provider without a public collision")
				}
				if collision == "none" {
					if err != nil || len(resolved.Env) != 1 || resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != tomlDirectKey {
						t.Fatal("an unrelated credential container changed the selected child credential")
					}
				} else {
					v4AssertUnsupported(t, err, ReasonInvalidConfig, hidden, tomlDirectKey)
					if len(resolved.Env) != 0 || len(resolved.Args) != 0 || resolved.Grok != nil {
						t.Fatal("a credential collision produced a partial materialization")
					}
				}
				v4AssertPublicSecretFree(t, catalog, resolved, hidden, tomlDirectKey)
			})
		}
	}
}

func TestOpaqueCredentialTableDescendantsWithholdAllMetadata(t *testing.T) {
	const hidden = "opaque-container-private-fixture"
	for _, suffix := range []string{
		"[model_providers.spare.api_key]\nvalue = ['" + hidden + "']\n",
		"[model_providers.spare]\napi_key.value = { nested = '" + hidden + "' }\n",
		"[[model_providers.spare.api_key]]\nvalue = '" + hidden + "'\n",
		"[api_key]\nvalue = '''" + hidden + "'''\n",
	} {
		t.Run(strings.SplitN(suffix, "\n", 2)[0], func(t *testing.T) {
			reader := v4FixtureReader(t, fixtureProfile{
				id: hidden, appType: "codex", name: "Provider " + hidden,
				settings: v4JSON(t, map[string]any{"config": tomlDirectCodexConfig + suffix, "auth": map[string]string{"OPENAI_API_KEY": tomlDirectKey}}), meta: `{}`,
			})
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: hidden}, model.RuntimeCodex)
			v4AssertUnsupported(t, err, ReasonInvalidConfig, hidden, tomlDirectKey)
			v4AssertPublicSecretFree(t, catalog, resolved, hidden, tomlDirectKey)
			if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported || catalog.Profiles[0].ProviderRef.ProfileID != "" || len(catalog.Profiles[0].Models) != 0 || len(resolved.Env) != 0 {
				t.Fatal("opaque credential descendants left authored metadata or a partial materialization")
			}
		})
	}
}

func TestGrokUnselectedCredentialTablesRemainPrivate(t *testing.T) {
	const hidden = "grok-container-private-fixture"
	for _, suffix := range []string{
		"[model.spare.api_key]\nvalue = '" + hidden + "'\n",
		"[model.spare]\napi_key.value = '" + hidden + "'\n",
	} {
		for _, name := range []string{"Grok credential table", "Grok " + hidden} {
			t.Run(strings.SplitN(suffix, "\n", 2)[0]+"/"+name, func(t *testing.T) {
				reader := v4FixtureReader(t, fixtureProfile{
					id: "grok-container", appType: "grokbuild", name: name,
					settings: v4JSON(t, map[string]any{"config": tomlDirectGrokConfig + suffix}), meta: `{}`,
				})
				catalog, err := reader.Catalog(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "grokbuild", ProfileID: "grok-container"}, model.RuntimeGrok)
				if strings.Contains(name, hidden) {
					v4AssertUnsupported(t, err, ReasonInvalidConfig, hidden, "grok-private-fixture-key")
					if len(resolved.Env) != 0 || resolved.Grok != nil {
						t.Fatal("a Grok credential-table collision produced a partial materialization")
					}
				} else if err != nil || resolved.Grok == nil || len(resolved.Env) != 1 || resolved.Env["PAIRROOM_CC_SWITCH_GROK_API_KEY"] != "grok-private-fixture-key" {
					t.Fatal("an unselected credential table changed Grok's selected credential")
				}
				v4AssertPublicSecretFree(t, catalog, resolved, hidden, "grok-private-fixture-key")
			})
		}
	}
}

func TestQuotedDataKeysDoNotBecomeCredentialAncestorsOrOwnedOptions(t *testing.T) {
	for _, test := range []struct{ name, prefix, suffix string }{
		{"quoted-credential-suffix", "", "[model_providers.'spare.api_key']\nname = 'Ordinary provider'\n"},
		{"quoted-literal-table", "", "['spare.api_key']\nvalue = 'Ordinary provider'\n"},
		{"quoted-literal-key", "'spare.api_key' = 'Ordinary provider'\n", ""},
		{"mcp-server-id", "", "[mcp_servers.secret]\ncommand = 'Ordinary provider'\n"},
		{"provider-id", "", "[model_providers.api_key]\nname = 'Ordinary provider'\n"},
		{"quoted-option-table", "", "[agents.'default_subagent_model.extra']\nvalues = ['Ordinary provider']\n"},
		{"quoted-option-key", "agents.'default_subagent_model.extra' = ['Ordinary provider']\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := tomlCodexReader(t, "Ordinary provider", test.prefix+tomlDirectCodexConfig+test.suffix)
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported || catalog.Profiles[0].Name != "Ordinary provider" || resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != tomlDirectKey {
				t.Fatal("a data key was reinterpreted as a credential or selected option")
			}
		})
	}
}

func TestReaderRevalidatesDatabaseAndEnvironmentAfterCatalog(t *testing.T) {
	const envName = "PAIRROOM_INSPECTION_FRESH_KEY"
	config := tomlDirectCodexConfig + "env_key = '" + envName + "'\n"
	t.Setenv(envName, "first-inspection-fixture-key")
	// This fixture intentionally models an external CC Switch write between
	// reads; ordinary fixtures separately assert that Reader never writes it.
	path := writeFixture(t, 20, fixtureProfile{
		id: tomlCodexRef().ProfileID, appType: "codex", name: "Fresh snapshot",
		settings: v4JSON(t, map[string]any{"config": config, "auth": map[string]string{"OPENAI_API_KEY": tomlDirectKey}}), meta: `{}`,
	})
	reader, err := NewReader(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := reader.Catalog(context.Background())
	if err != nil || len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported {
		t.Fatal("valid initial profile was not catalogued")
	}
	t.Setenv(envName, "")
	resolved, err := reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
	v4AssertUnsupported(t, err, ReasonMissingCredential, "first-inspection-fixture-key", tomlDirectKey)
	if len(resolved.Env) != 0 {
		t.Fatal("Resolve reused a credential captured by an earlier Catalog call")
	}
	db, err := sql.Open("sqlite", reader.database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	config = strings.Replace(config, "https://selected-toml.invalid/v1", "https://reloaded.invalid/v1", 1)
	settings := v4JSON(t, map[string]any{"config": config})
	if _, err := db.Exec("UPDATE providers SET settings_config = ? WHERE id = ?", settings, tomlCodexRef().ProfileID); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envName, "second-inspection-fixture-key")
	resolved, err = reader.Resolve(context.Background(), tomlCodexRef(), model.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != "second-inspection-fixture-key" ||
		!slices.ContainsFunc(resolved.Args, func(arg string) bool { return strings.HasSuffix(arg, `.base_url="https://reloaded.invalid/v1"`) }) {
		t.Fatal("Resolve reused stale configuration or credentials across public operations")
	}
}

func TestNestedConfigOpaqueCredentialsWithholdMetadata(t *testing.T) {
	const hidden = "nested-opaque-private-fixture"
	for _, config := range []string{
		"[api_key]\nvalue = ['" + hidden + "']\n",
		"token.value = { nested = '" + hidden + "' }\n",
		"[[api_key]]\nvalue = '" + hidden + "'\n",
		"[api_key]\nvalue = '''" + hidden + "'''\n",
	} {
		t.Run(strings.SplitN(config, "\n", 2)[0], func(t *testing.T) {
			reader := v4FixtureReader(t, fixtureProfile{
				id: hidden, appType: "codex", name: "Provider " + hidden,
				settings: v4JSON(t, map[string]any{"config": tomlDirectCodexConfig, "auth": map[string]string{"OPENAI_API_KEY": tomlDirectKey}}),
				meta:     v4JSON(t, map[string]any{"snapshot": map[string]string{"config": config}}),
			})
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: hidden}, model.RuntimeCodex)
			v4AssertUnsupported(t, err, ReasonInvalidConfig, hidden, tomlDirectKey)
			v4AssertPublicSecretFree(t, catalog, resolved, hidden, tomlDirectKey)
			if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported || catalog.Profiles[0].ProviderRef.ProfileID != "" || len(catalog.Profiles[0].Models) != 0 || len(resolved.Env) != 0 || len(resolved.Args) != 0 {
				t.Fatal("a nested opaque credential snapshot leaked metadata or a partial materialization")
			}
		})
	}
}

func TestUnrelatedNestedConfigMetadataDoesNotDisableProvider(t *testing.T) {
	for _, config := range []string{
		"ordinary free-form metadata, not a TOML document",
		"[notes]\nvalues = [1, 2]\n",
		"[notes]\ntext = 'unfinished metadata",
	} {
		t.Run(strings.SplitN(config, "\n", 2)[0], func(t *testing.T) {
			reader := v4FixtureReader(t, fixtureProfile{
				id: "nested-metadata", appType: "codex", name: "Ordinary metadata",
				settings: v4JSON(t, map[string]any{"config": tomlDirectCodexConfig, "auth": map[string]string{"OPENAI_API_KEY": tomlDirectKey}}),
				meta:     v4JSON(t, map[string]any{"snapshot": map[string]string{"config": config}}),
			})
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "nested-metadata"}, model.RuntimeCodex)
			if err != nil || len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported || resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != tomlDirectKey {
				t.Fatal("unused ordinary metadata was treated as an invalid active provider configuration")
			}
		})
	}
}
