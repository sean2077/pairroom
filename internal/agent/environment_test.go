package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestRuntimeEnvironmentRespectsPlatformIdentity(t *testing.T) {
	for _, test := range []struct {
		name      string
		base      []string
		overrides map[string]string
		want      []string
	}{
		{"windows", []string{"anthropic_api_key=inherited", "Path=old", `=C:=C:\work`, `=D:=D:\work`}, map[string]string{"ANTHROPIC_API_KEY": "selected", "PATH": "new"}, []string{`=C:=C:\work`, `=D:=D:\work`, "ANTHROPIC_API_KEY=selected", "PATH=new"}},
		{"linux", []string{"token=lower", "TOKEN=old"}, map[string]string{"TOKEN": "selected"}, []string{"TOKEN=selected", "token=lower"}},
		{"darwin", []string{"TOKEN=old", "TOKEN=last", "EMPTY=old"}, map[string]string{"TOKEN": "selected", "EMPTY": ""}, []string{"EMPTY=", "TOKEN=selected"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := append([]string(nil), test.base...)
			got := mergeRuntimeEnvForOS(test.base, test.overrides, test.name)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("environment = %q; want %q", got, test.want)
			}
			if !reflect.DeepEqual(original, test.base) {
				t.Fatal("mutated inherited environment")
			}
		})
	}
	base := []string{"A=value"}
	if got := mergeRuntimeEnv(base, nil); !reflect.DeepEqual(got, base) {
		t.Fatalf("native inheritance changed: %v", got)
	}
}

func TestClaudeCCSwitchEnvironmentReplacesProviderStateOnly(t *testing.T) {
	base := []string{
		"PATH=/native/tools", "CLAUDE_CONFIG_DIR=/native/claude",
		"ANTHROPIC_AUTH_TOKEN=parent-token", "ANTHROPIC_API_KEY=parent-key",
		"ANTHROPIC_BASE_URL=https://parent.invalid", "ANTHROPIC_DEFAULT_HAIKU_MODEL=parent-model",
		"ANTHROPIC_CUSTOM_HEADERS=Authorization: parent-header",
		"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_GATEWAY=1",
		"CLAUDE_CODE_SKIP_VERTEX_AUTH=1", "CLAUDE_CODE_OAUTH_TOKEN=parent-oauth",
		"CLAUDE_CODE_SUBAGENT_MODEL=parent-subagent", "AWS_BEARER_TOKEN_BEDROCK=parent-bedrock",
		"CLOUD_ML_REGION=parent-region", "VERTEX_REGION_CLAUDE_4_5_SONNET=parent-region",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=0",
		"CLAUDE_CODE_USE_POWERSHELL_TOOL=1", "CLAUDE_CODE_SKIP_PROMPT_HISTORY=1",
		"AWS_ACCESS_KEY_ID=tool-account", "GOOGLE_APPLICATION_CREDENTIALS=/native/google.json",
		"HTTPS_PROXY=http://network-proxy.invalid", "CLAUDE_CODE_EFFORT_LEVEL=high",
	}
	cfg := Config{Provider: "cc-switch:claude/selected", Env: map[string]string{
		"ANTHROPIC_API_KEY": "selected-key", "ANTHROPIC_BASE_URL": "https://selected.invalid",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":        "selected-haiku",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": "0",
	}}
	originalBase := append([]string(nil), base...)
	originalOverrides := copyEnvForTest(cfg.Env)
	got := claudeRuntimeEnvForOS(base, cfg, "linux")
	want := []string{
		"ANTHROPIC_API_KEY=selected-key", "ANTHROPIC_BASE_URL=https://selected.invalid",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=selected-haiku", "AWS_ACCESS_KEY_ID=tool-account",
		"CLAUDE_CODE_EFFORT_LEVEL=high", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1",
		"CLAUDE_CODE_SKIP_PROMPT_HISTORY=1", "CLAUDE_CODE_USE_POWERSHELL_TOOL=1",
		"CLAUDE_CONFIG_DIR=/native/claude", "GOOGLE_APPLICATION_CREDENTIALS=/native/google.json",
		"HTTPS_PROXY=http://network-proxy.invalid", "PATH=/native/tools",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected Provider environment = %q; want %q", got, want)
	}
	if !reflect.DeepEqual(base, originalBase) || !reflect.DeepEqual(cfg.Env, originalOverrides) {
		t.Fatal("selected Provider mutated the Service environment or shared configuration")
	}
	for _, provider := range []string{"", "native"} {
		if got := claudeRuntimeEnvForOS(base, Config{Provider: provider}, "linux"); !reflect.DeepEqual(got, base) {
			t.Fatalf("native inheritance changed for provider %q", provider)
		}
	}
}

func TestClaudeCCSwitchEnvironmentRespectsWindowsNames(t *testing.T) {
	base := []string{
		`=C:=C:\work`, "Path=native-tools", "anthropic_auth_token=parent-token",
		"claude_code_use_vertex=1", "claude_code_provider_managed_by_host=0",
		"claude_code_use_native_file_search=1",
	}
	cfg := Config{Provider: "cc-switch:claude/selected", Env: map[string]string{
		"ANTHROPIC_API_KEY": "selected-key", "claude_code_provider_managed_by_host": "0",
	}}
	got := claudeRuntimeEnvForOS(base, cfg, "windows")
	want := []string{
		`=C:=C:\work`, "ANTHROPIC_API_KEY=selected-key", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1",
		"claude_code_use_native_file_search=1", "Path=native-tools",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Windows selected Provider environment = %q; want %q", got, want)
	}
	unix := claudeRuntimeEnvForOS([]string{"anthropic_auth_token=tool-value"}, Config{Provider: cfg.Provider}, "linux")
	if !containsString(unix, "anthropic_auth_token=tool-value") {
		t.Fatal("Unix case-sensitive unrelated variable was removed")
	}
}

func TestClaudeCCSwitchProviderVersionBoundary(t *testing.T) {
	for _, test := range []struct {
		version string
		allowed bool
	}{
		{"2.1.221", false}, {"2.1.222-rc.1", false}, {"", false},
		{"2.1.222", true}, {"2.1.283", true}, {"3.0.0", true},
	} {
		cfg := Config{Provider: "cc-switch:claude/selected"}
		if err := validateClaudeProviderVersion(cfg, test.version); (err == nil) != test.allowed {
			t.Fatalf("version %q allowed=%v: %v", test.version, test.allowed, err)
		}
		if err := validateClaudeProviderVersion(Config{Provider: "native"}, test.version); err != nil {
			t.Fatalf("native version %q was restricted: %v", test.version, err)
		}
	}
}

// The real adapter launches the existing protocol fixture; this checks the
// process environment wiring without authenticating to an external Provider.
func TestClaudeCCSwitchStartupKeepsTwoSlotsIsolated(t *testing.T) {
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT", "default")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "parent-token")
	t.Setenv("ANTHROPIC_API_KEY", "parent-key")
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
	t.Setenv("CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "0")
	for _, test := range []struct {
		actor model.ActorID
		key   string
	}{
		{model.ActorSlot1, "selected-key-a"}, {model.ActorSlot2, "selected-key-b"},
	} {
		cfg := Config{
			Actor: test.actor, Command: os.Args[0], Repo: t.TempDir(), DataDir: t.TempDir(),
			Provider: "cc-switch:claude/" + string(test.actor),
			Env:      map[string]string{"ANTHROPIC_API_KEY": test.key, "ANTHROPIC_BASE_URL": "https://selected.invalid"},
		}
		adapter := NewClaude(cfg, func(model.RuntimeEvent) {})
		t.Cleanup(func() { stopWithin(adapter, 10*time.Second) })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := adapter.Start(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		adapter.mu.Lock()
		childEnv := append([]string(nil), adapter.cmd.Env...)
		childArgs := append([]string(nil), adapter.cmd.Args...)
		adapter.mu.Unlock()
		if !containsString(childEnv, "ANTHROPIC_API_KEY="+test.key) ||
			!containsString(childEnv, "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1") {
			t.Fatal("Claude child did not receive the selected credential and host-routing flag")
		}
		for _, entry := range childEnv {
			if strings.HasPrefix(entry, "ANTHROPIC_AUTH_TOKEN=") || strings.HasPrefix(entry, "CLAUDE_CODE_USE_VERTEX=") {
				t.Fatal("Claude child retained conflicting inherited authentication or routing")
			}
		}
		if strings.Contains(strings.Join(childArgs, " "), test.key) {
			t.Fatal("selected credential entered argv")
		}
		if len(cfg.Env) != 2 {
			t.Fatal("Claude startup mutated the shared Provider configuration")
		}
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "parent-key" || os.Getenv("CLAUDE_CODE_USE_VERTEX") != "1" ||
		os.Getenv("CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST") != "0" {
		t.Fatal("Claude startup changed the Service environment")
	}
}

func TestClaudeCCSwitchRejectsOldCLIWithoutStartingSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture; environment and version cases above cover Windows")
	}
	marker := filepath.Join(t.TempDir(), "started")
	t.Setenv("PAIRROOM_CLAUDE_ENV_MARKER", marker)
	command := writeProbeFixture(t, `#!/bin/sh
case "$1" in
  --version) echo "2.1.221 (Claude Code)" ;;
  --help) echo "--input-format --output-format --session-id --resume --verbose" ;;
  *) touch "$PAIRROOM_CLAUDE_ENV_MARKER"; exit 91 ;;
esac
`)
	adapter := NewClaude(Config{
		Command: command, Repo: t.TempDir(), DataDir: t.TempDir(),
		Provider: "cc-switch:claude/selected", Env: map[string]string{"ANTHROPIC_API_KEY": "selected-key"},
	}, func(model.RuntimeEvent) {})
	if err := adapter.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "2.1.222") {
		t.Fatalf("old Claude startup error = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("old Claude session started before compatibility rejection: %v", err)
	}
	if adapter.State() != model.StateError {
		t.Fatalf("old Claude adapter state = %s", adapter.State())
	}
}

func copyEnvForTest(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
