package ccswitch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestClaudeCompatibilitySettingsContainOnlyNonSecretProviderOptions(t *testing.T) {
	reader := v4FixtureReader(t, fixtureProfile{
		id: "compatible", appType: "claude", name: "Compatible direct",
		settings: `{"env":{"ANTHROPIC_API_KEY":"fixture-private-key","ANTHROPIC_BASE_URL":"https://direct.invalid","CLAUDE_CODE_AUTO_MODE_SERVER":"0","ENABLE_TOOL_SEARCH":"false","NODE_OPTIONS":"--inspect"},"permissions":{"defaultMode":"bypassPermissions"}}`,
		meta:     `{}`,
	})
	resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "claude", ProfileID: "compatible"}, model.RuntimeClaude)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Args) != 2 || resolved.Args[0] != "--settings" {
		t.Fatal("provider compatibility flags lack CLI settings precedence")
	}
	var settings map[string]map[string]string
	if err := json.Unmarshal([]byte(resolved.Args[1]), &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings) != 1 || len(settings["env"]) != 2 || settings["env"]["CLAUDE_CODE_AUTO_MODE_SERVER"] != "0" || settings["env"]["ENABLE_TOOL_SEARCH"] != "false" {
		t.Fatal("compatibility settings included credentials or shared native settings")
	}
	if resolved.Env["ANTHROPIC_API_KEY"] != "fixture-private-key" {
		t.Fatal("selected credential did not remain in the child environment")
	}
}

func TestClaudeProfileCannotHideUnsupportedAuthenticationBehindTruthySelectors(t *testing.T) {
	for _, value := range []any{"yes", "on", "1", true, 2} {
		t.Run(v4JSON(t, value), func(t *testing.T) {
			reader := v4FixtureReader(t, fixtureProfile{
				id: "cloud", appType: "claude", name: "Cloud provider",
				settings: v4JSON(t, map[string]any{"env": map[string]any{"ANTHROPIC_API_KEY": "fixture-private-key", "CLAUDE_CODE_USE_VERTEX": value}}),
				meta:     `{}`,
			})
			_, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "claude", ProfileID: "cloud"}, model.RuntimeClaude)
			v4AssertUnsupported(t, err, ReasonProxyConversion, "fixture-private-key")
		})
	}
}

func TestClaudeCompatibilityCannotPublishCredentialsOrOpaqueBodies(t *testing.T) {
	for _, key := range []string{"ENABLE_TOOL_SEARCH", "CLAUDE_CODE_EXTRA_BODY"} {
		for _, secret := range []string{"fixture-private-key", "fixture<secret>&key"} {
			t.Run(key+"/"+secret, func(t *testing.T) {
				reader := v4FixtureReader(t, fixtureProfile{
					id: "unsafe", appType: "claude", name: "Unsafe provider",
					settings: v4JSON(t, map[string]any{"env": map[string]string{"ANTHROPIC_API_KEY": secret, key: secret}}),
					meta:     `{}`,
				})
				_, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "claude", ProfileID: "unsafe"}, model.RuntimeClaude)
				v4AssertUnsupported(t, err, ReasonInvalidConfig, secret)
			})
		}
	}
}

func TestClaudeOpaqueSettingsCannotPublishCatalogMetadata(t *testing.T) {
	const hiddenKey = "opaque-claude-fixture-credential"
	for _, test := range []struct {
		name     string
		settings map[string]any
	}{
		{"custom-headers", map[string]any{"env": map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer " + hiddenKey}}},
		{"extra-body", map[string]any{"env": map[string]any{"CLAUDE_CODE_EXTRA_BODY": `{"private":"` + hiddenKey + `"}`}}},
		{"structured-headers", map[string]any{"env": map[string]any{"ANTHROPIC_CUSTOM_HEADERS": map[string]string{"Authorization": hiddenKey}}}},
		{"array-body", map[string]any{"env": map[string]any{"CLAUDE_CODE_EXTRA_BODY": []string{hiddenKey}}}},
		{"credential-helper", map[string]any{"apiKeyHelper": "echo " + hiddenKey}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := v4FixtureReader(t, fixtureProfile{
				id: hiddenKey, appType: "claude", name: "Provider " + hiddenKey,
				settings: v4JSON(t, test.settings), meta: `{}`,
			})
			catalog, err := reader.Catalog(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "claude", ProfileID: hiddenKey}, model.RuntimeClaude)
			v4AssertUnsupported(t, err, ReasonInvalidConfig, hiddenKey)
			v4AssertPublicSecretFree(t, catalog, resolved, hiddenKey)
			if len(catalog.Profiles) != 1 || catalog.Profiles[0].Supported || catalog.Profiles[0].ProviderRef.ProfileID != "" {
				t.Fatal("opaque credential settings exposed row-authored metadata")
			}
		})
	}
}
