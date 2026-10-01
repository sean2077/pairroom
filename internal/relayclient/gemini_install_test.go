package relayclient

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestGeminiInstallPreservesSettingsAndUsesMilliseconds(t *testing.T) {
	IsolateNativeCaller(t)
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", home)
	path := filepath.Join(root, ".gemini", "settings.json")
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := `{"theme":"unchanged","hooks":{"BeforeTool":[{"matcher":"write_file","hooks":[{"type":"command","command":"other-tool"}]}]},"hooksConfig":{"enabled":false}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := runInstall(root, []model.RuntimeKind{model.RuntimeGemini}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	config, err := readHooks(path)
	if err != nil {
		t.Fatal(err)
	}
	if config["theme"] != "unchanged" || config["hooksConfig"].(map[string]any)["enabled"] != false {
		t.Fatal("installation overwrote settings or granted consent")
	}
	hooks := config["hooks"].(map[string]any)
	if len(hooks["BeforeTool"].([]any)) != 2 || len(hooks["AfterAgent"].([]any)) != 1 || hooks["Stop"] != nil || hooks["StopFailure"] != nil {
		t.Fatalf("unexpected events/duplicates: %+v", hooks)
	}
	for _, event := range []string{"BeforeTool", "AfterAgent"} {
		groups := hooks[event].([]any)
		group := groups[len(groups)-1].(map[string]any)
		entry := group["hooks"].([]any)[0].(map[string]any)
		if entry["timeout"] != float64(45000) {
			t.Fatal("timeout must be milliseconds")
		}
		if event == "BeforeTool" && group["matcher"] != "^run_shell_command$" {
			t.Fatal("unscoped tool hook")
		}
	}
	if present, disabled, err := ownRelayStopHook(root, model.RuntimeGemini); err != nil || present || !disabled {
		t.Fatal("disabled hooks considered ready")
	}
	want := filepath.Join(home, ".gemini", "skills", "pairroom-relay")
	if got, err := skillHome(model.RuntimeGemini); err != nil || got != want {
		t.Fatalf("Gemini home: %s %v", got, err)
	}
	if content, err := os.ReadFile(filepath.Join(want, "SKILL.md")); err != nil || string(content) != skillContent {
		t.Fatal("missing Gemini skill")
	}
	if err := editHooks(root, model.RuntimeGemini, true); err != nil {
		t.Fatal(err)
	}
	config, err = readHooks(path)
	if err != nil {
		t.Fatal(err)
	}
	var baseline map[string]any
	if err := json.Unmarshal([]byte(original), &baseline); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config, baseline) {
		t.Fatalf("purge removed unrelated content: %+v", config)
	}
}

func TestGeminiSetupInfersShellIdentityWithoutSession(t *testing.T) {
	IsolateNativeCaller(t)
	t.Setenv("GEMINI_CLI", "1")
	kinds, err := selectInstallRuntimes("", strings.NewReader(""), io.Discard)
	if err != nil || !reflect.DeepEqual(kinds, []model.RuntimeKind{model.RuntimeGemini}) {
		t.Fatalf("inference: %v %v", kinds, err)
	}
	if _, err := requireSessionID(model.RuntimeGemini); err == nil {
		t.Fatal("shell identification alone bound a session")
	}
	for _, token := range []string{"gemini", "gemini-cli", "GEMINI_CLI"} {
		kind, ok := parseRuntimeToken(token)
		if !ok || kind != model.RuntimeGemini {
			t.Fatalf("alias rejected: %s", token)
		}
	}
}

func TestGeminiForegroundIdentityFailsClosed(t *testing.T) {
	IsolateNativeCaller(t)
	t.Setenv("GEMINI_CLI", "1")
	t.Setenv("CODEX_SESSION_ID", "outer-session")
	t.Setenv("GEMINI_SESSION_ID", "hook-only-not-shell-identity")
	caller, err := currentNativeCaller()
	if err != nil || caller.runtime != model.RuntimeGemini || caller.session != "" {
		t.Fatal("inherited identity selected")
	}
	t.Setenv(geminiSessionEnv, "official-session")
	caller, err = currentNativeCaller()
	if err != nil || caller.session != "official-session" || caller.runtime != model.RuntimeGemini {
		t.Fatalf("bridge identity: %+v %v", caller, err)
	}
	t.Setenv("GEMINI_CLI", "")
	if _, err := currentNativeCaller(); err == nil {
		t.Fatal("forwarded identity accepted outside Gemini")
	}
}

func TestGeminiCreationDefaultsToCreatorSlotAndSupportsDuplicatePair(t *testing.T) {
	IsolateNativeCaller(t)
	t.Setenv("GEMINI_CLI", "1")
	t.Setenv(geminiSessionEnv, "official-session")
	slot, err := inferCreateSlot(options{})
	if err != nil || slot != model.ActorSlot1 {
		t.Fatalf("creator slot: %s %v", slot, err)
	}
	for _, peer := range []string{"claude", "codex", "grok", "gemini"} {
		agents, err := createAgents(options{peer: peer}, slot)
		if err != nil || agents[slot].Runtime != model.RuntimeGemini || agents[model.ActorSlot2].Runtime != model.RuntimeKind(peer) {
			t.Fatalf("pair: %+v %v", agents, err)
		}
	}
}
