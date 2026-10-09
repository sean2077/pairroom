package ccswitch

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestCredentialSuffixContainerDoesNotTurnSharedCommandsIntoSecrets(t *testing.T) {
	for _, command := range []string{"api", "codex", "responses"} {
		for _, layout := range []string{
			"root-dotted", "heading", "array-heading", "json", "json-array-map",
			"mcp-env-heading", "provider-env-heading", "provider-auth-heading",
			"json-mcp-env", "json-provider-auth", "json-flat-mcp-env", "json-flat-provider-auth",
		} {
			t.Run(layout+"/"+command, func(t *testing.T) {
				settings := map[string]any{
					"config": tomlDirectCodexConfig,
					"auth":   map[string]string{"OPENAI_API_KEY": tomlDirectKey},
				}
				switch layout {
				case "root-dotted":
					settings["config"] = "mcp_servers.alpha.my_token.command = '" + command + "'\n" + tomlDirectCodexConfig
				case "heading":
					settings["config"] = tomlDirectCodexConfig + "[mcp_servers.alpha.my_token]\ncommand = '" + command + "'\n"
				case "array-heading":
					settings["config"] = tomlDirectCodexConfig + "[[mcp_servers.alpha.my_token]]\ncommand = '" + command + "'\n"
				case "json":
					settings["mcpServers"] = map[string]any{"alpha": map[string]any{"my_token": map[string]string{"command": command}}}
				case "json-array-map":
					settings["mcpServers"] = map[string]any{"alpha": map[string]any{"my_token": []any{map[string]string{"command": command}}}}
				case "mcp-env-heading":
					settings["config"] = tomlDirectCodexConfig + "[mcp_servers.env.my_token]\ncommand = '" + command + "'\n"
				case "provider-env-heading":
					settings["config"] = tomlDirectCodexConfig + "[model_providers.env.my_token]\ncommand = '" + command + "'\n"
				case "provider-auth-heading":
					settings["config"] = tomlDirectCodexConfig + "[model_providers.auth.my_token]\ncommand = '" + command + "'\n"
				case "json-mcp-env":
					settings["mcpServers"] = map[string]any{"env": map[string]any{"my_token": map[string]string{"command": command}}}
				case "json-provider-auth":
					settings["model_providers"] = map[string]any{"auth": map[string]any{"my_token": map[string]string{"command": command}}}
				case "json-flat-mcp-env":
					settings["mcpServers.env.my_token"] = map[string]string{"command": command}
				case "json-flat-provider-auth":
					settings["model_providers.auth.my_token"] = map[string]string{"command": command}
				}
				reader := v4FixtureReader(t, fixtureProfile{
					id: "shared-command", appType: "codex", name: "Shared command",
					settings: v4JSON(t, settings), meta: `{}`,
				})
				catalog, err := reader.Catalog(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "shared-command"}, model.RuntimeCodex)
				if err != nil {
					t.Fatal(err)
				}
				if len(catalog.Profiles) != 1 || !catalog.Profiles[0].Supported || catalog.Profiles[0].Name != "Shared command" || resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != tomlDirectKey ||
					!slices.ContainsFunc(resolved.Args, func(arg string) bool { return strings.HasSuffix(arg, `.base_url="https://selected-toml.invalid/v1"`) }) {
					t.Fatal("a shared command beneath a suffix-shaped data key changed the selected Provider")
				}
				v4AssertPublicSecretFree(t, catalog, resolved, tomlDirectKey)
			})
		}
	}
}

func TestRealShortCredentialsRemainPrivateInSharedEnvironmentAndContainers(t *testing.T) {
	const secret = "api"
	for _, test := range []struct {
		name, prefix, suffix string
		settings, meta       map[string]any
	}{
		{"direct-suffix-scalar", "mcp_servers.alpha.my_token = 'api'\n", "", nil, nil},
		{"toml-unrecognized-collection-suffix", "mcpservers.my_token = 'api'\n", "", nil, nil},
		{"direct-suffix-opaque", "mcp_servers.alpha.my_token = ['api']\n", "", nil, nil},
		{"direct-suffix-multiline", "mcp_servers.alpha.my_token = '''api'''\n", "", nil, nil},
		{"direct-suffix-inline-table", "mcp_servers.alpha.my_token = { api_key = 'api' }\n", "", nil, nil},
		{"suffix-scalar-in-array-table", "", "[[hooks.namespace]]\nmy_token = 'api'\n", nil, nil},
		{"mcp-environment-scalar", "mcp_servers.alpha.env.MY_TOKEN = 'api'\n", "", nil, nil},
		{"mcp-environment-table", "", "[mcp_servers.alpha.env.MY_TOKEN]\nvalue = 'api'\n", nil, nil},
		{"mcp-env-identity-environment-table", "", "[mcp_servers.env.env.MY_TOKEN]\nvalue = 'api'\n", nil, nil},
		{"provider-auth-identity-environment-table", "", "[model_providers.auth.env.MY_TOKEN]\nvalue = 'api'\n", nil, nil},
		{"json-direct-suffix", "", "", map[string]any{"my_token": secret}, nil},
		{"json-direct-suffix-array", "", "", map[string]any{"my_token": []any{secret}}, nil},
		{"mcp-json-environment-scalar", "", "", map[string]any{"mcpServers": map[string]any{"alpha": map[string]any{"env": map[string]string{"MY_TOKEN": secret}}}}, nil},
		{"mcp-json-environment-map", "", "", map[string]any{"mcpServers": map[string]any{"alpha": map[string]any{"env": map[string]any{"MY_TOKEN": map[string]string{"value": secret}}}}}, nil},
		{"mcp-json-env-identity-environment-map", "", "", map[string]any{"mcpServers": map[string]any{"env": map[string]any{"env": map[string]any{"MY_TOKEN": map[string]string{"value": secret}}}}}, nil},
		{"json-flattened-environment-map", "", "", map[string]any{"mcp_servers.alpha.env.MY_TOKEN": map[string]string{"value": secret}}, nil},
		{"json-flattened-env-identity-environment-map", "", "", map[string]any{"mcpServers.env.env.MY_TOKEN": map[string]string{"value": secret}}, nil},
		{"json-flattened-auth-map", "", "", nil, map[string]any{"auth.OPENAI_API_KEY": map[string]string{"value": secret}}},
		{"json-auth-map", "", "", nil, map[string]any{"auth": map[string]any{"OPENAI_API_KEY": map[string]string{"value": secret}}}},
		{"json-provider-auth-identity-auth-map", "", "", nil, map[string]any{"model_providers": map[string]any{"auth": map[string]any{"auth": map[string]any{"MY_TOKEN": map[string]string{"value": secret}}}}}},
		{"json-flattened-auth-identity-auth-map", "", "", nil, map[string]any{"model_providers.auth.auth.MY_TOKEN": map[string]string{"value": secret}}},
		{"root-key-container", "", "[api_key]\nvalue = 'api'\n", nil, nil},
		{"unselected-provider-key", "", "[model_providers.spare.api_key]\nvalue = 'api'\n", nil, nil},
		{"unselected-provider-bearer", "", "[model_providers.spare.experimental_bearer_token]\nvalue = 'api'\n", nil, nil},
		{"nested-opaque-key", "", "", nil, map[string]any{"snapshot": map[string]string{"config": "[api_key]\nvalue = ['api']\n"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := map[string]any{
				"config": test.prefix + tomlDirectCodexConfig + test.suffix,
				"auth":   map[string]string{"OPENAI_API_KEY": tomlDirectKey},
			}
			for key, value := range test.settings {
				settings[key] = value
			}
			meta := test.meta
			if meta == nil {
				meta = map[string]any{}
			}
			reader := v4FixtureReader(t, fixtureProfile{
				id: "short-private", appType: "codex", name: "Provider " + secret,
				settings: v4JSON(t, settings), meta: v4JSON(t, meta),
			})
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "short-private"}, model.RuntimeCodex)
			v4AssertUnsupported(t, err, ReasonInvalidConfig, secret, tomlDirectKey)
			v4AssertPublicSecretFree(t, catalog, resolved, secret, tomlDirectKey)
			if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported || len(resolved.Args) != 0 || len(resolved.Env) != 0 {
				t.Fatal("a real short credential leaked or produced a partial materialization")
			}
		})
	}
}

func TestMCPDataNamesPreserveRealEnvironmentCredentialContainers(t *testing.T) {
	for _, server := range []string{"model", "model_providers", "mcp_servers", "mcpServers"} {
		for _, layout := range []string{"nested", "flattened"} {
			for _, declaration := range []string{"namespace", "credential"} {
				t.Run(server+"/"+layout+"/"+declaration, func(t *testing.T) {
					private := declaration == "credential"
					settings := map[string]any{
						"config": tomlDirectCodexConfig,
						"auth":   map[string]string{"OPENAI_API_KEY": tomlDirectKey},
					}
					if layout == "flattened" {
						if private {
							settings["mcpServers."+server+".env.MY_TOKEN"] = map[string]string{"value": "api"}
						} else {
							settings["mcpServers."+server+".my_token"] = map[string]string{"command": "api"}
						}
					} else {
						fields := map[string]any{"my_token": map[string]string{"command": "api"}}
						if private {
							fields = map[string]any{"env": map[string]any{"MY_TOKEN": map[string]string{"value": "api"}}}
						}
						settings["mcpServers"] = map[string]any{server: fields}
					}
					reader := v4FixtureReader(t, fixtureProfile{
						id: "mcp-data-name", appType: "codex", name: "MCP data name",
						settings: v4JSON(t, settings), meta: `{}`,
					})
					catalog, err := reader.Catalog(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "mcp-data-name"}, model.RuntimeCodex)
					if private {
						v4AssertUnsupported(t, err, ReasonInvalidConfig, "api", tomlDirectKey)
						v4AssertPublicSecretFree(t, catalog, resolved, "api", tomlDirectKey)
						if len(resolved.Args) != 0 || len(resolved.Env) != 0 {
							t.Fatal("a credential beneath a named MCP server produced a partial materialization")
						}
					} else if err != nil || resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != tomlDirectKey {
						t.Fatal("an MCP server name turned a command namespace into a credential container")
					}
					if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported == private {
						t.Fatal("an MCP server name changed the role of its environment map")
					}
					v4AssertPublicSecretFree(t, catalog, resolved, tomlDirectKey)
				})
			}
		}
	}
}
