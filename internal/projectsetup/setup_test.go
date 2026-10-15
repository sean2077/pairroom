package projectsetup

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/relayclient/skill"
)

func setupItem(t *testing.T, root, kind, component string) Item {
	t.Helper()
	status, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, runtime := range status.Runtimes {
		if runtime.Runtime == kind {
			if component == "skill" {
				return runtime.Skill
			}
			return runtime.Hook
		}
	}
	t.Fatal("missing runtime")
	return Item{}
}

func setupInstall(t *testing.T, root, kind, component string, confirm bool) error {
	t.Helper()
	item := setupItem(t, root, kind, component)
	_, err := Install(root, Request{Runtime: kind, Component: component, Revision: item.Revision, ConfirmReplace: confirm})
	return err
}

func put(t *testing.T, root, path, text string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(text), 0o640); err != nil {
		t.Fatal(err)
	}
}

func contents(t *testing.T, root, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestInspectIsReadOnlyAndAllRuntimesInstallIndependently(t *testing.T) {
	t.Setenv("GROK_CLAUDE_HOOKS_ENABLED", "false")
	for _, kind := range []string{"claude", "codex", "grok", "gemini"} {
		for _, component := range []string{"skill", "hook"} {
			t.Run(kind+"/"+component, func(t *testing.T) {
				root := t.TempDir()
				item := setupItem(t, root, kind, component)
				if item.State != "missing" || !item.CanInstall || item.NeedsConfirmation {
					t.Fatalf("%+v", item)
				}
				files, _ := os.ReadDir(root)
				if len(files) != 0 {
					t.Fatal("inspection wrote files")
				}
				if err := setupInstall(t, root, kind, component, false); err != nil {
					t.Fatal(err)
				}
				now := setupItem(t, root, kind, component)
				if now.State != "current" || now.CanInstall {
					t.Fatalf("installed: %+v", now)
				}
				other := "skill"
				if component == "skill" {
					other = "hook"
				}
				if got := setupItem(t, root, kind, other).State; got != "missing" {
					t.Fatalf("unselected %s changed: %s", other, got)
				}
				before, _ := os.Stat(filepath.Join(root, item.Path))
				if err := setupInstall(t, root, kind, component, false); err != nil {
					t.Fatal(err)
				}
				after, _ := os.Stat(filepath.Join(root, item.Path))
				if !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
					t.Fatal("idempotent install wrote file")
				}
				if component == "skill" && string(contents(t, root, item.Path)) != skill.Content {
					t.Fatal("wrong skill payload")
				}
			})
		}
	}
}

func TestSkillConfirmationPreservesOtherSkillsAndRejectsStalePreview(t *testing.T) {
	root := t.TempDir()
	path := ".agents/skills/pairroom-relay/SKILL.md"
	put(t, root, path, "user's existing skill\n")
	put(t, root, ".agents/skills/pairroom-relay/notes.txt", "keep notes")
	put(t, root, ".agents/skills/other/SKILL.md", "keep other skill")
	item := setupItem(t, root, "codex", "skill")
	if item.State != "conflict" || !item.NeedsConfirmation {
		t.Fatalf("%+v", item)
	}
	if err := setupInstall(t, root, "codex", "skill", false); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("%v", err)
	}
	if string(contents(t, root, path)) != "user's existing skill\n" {
		t.Fatal("unconfirmed overwrite")
	}
	put(t, root, path, "new edits")
	if _, err := Install(root, Request{Runtime: "codex", Component: "skill", Revision: item.Revision, ConfirmReplace: true}); !errors.Is(err, ErrChanged) {
		t.Fatalf("%v", err)
	}
	if string(contents(t, root, path)) != "new edits" {
		t.Fatal("stale confirmation overwrote edits")
	}
	if err := setupInstall(t, root, "codex", "skill", true); err != nil {
		t.Fatal(err)
	}
	if string(contents(t, root, path)) != skill.Content {
		t.Fatal("confirmed update missing")
	}
	for path, expected := range map[string]string{".agents/skills/pairroom-relay/notes.txt": "keep notes", ".agents/skills/other/SKILL.md": "keep other skill"} {
		if string(contents(t, root, path)) != expected {
			t.Fatal("unrelated file changed")
		}
	}
	// Even an existing directory with no manifest belongs to the user.
	other := t.TempDir()
	put(t, other, ".agents/skills/pairroom-relay/notes.txt", "keep")
	if err := setupInstall(t, other, "codex", "skill", false); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("existing folder: %v", err)
	}
}

func TestProjectSetupNeverWritesGlobalsOrFollowsManagedLinks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		t.Setenv(name, "")
	}
	t.Setenv("GEMINI_CLI_HOME", "")
	put(t, home, ".agents/skills/pairroom-relay/SKILL.md", "global sentinel")
	if _, err := Inspect(home); err == nil {
		t.Fatal("home root accepted")
	}
	root := t.TempDir()
	if err := setupInstall(t, root, "codex", "skill", false); err != nil {
		t.Fatal(err)
	}
	if string(contents(t, home, ".agents/skills/pairroom-relay/SKILL.md")) != "global sentinel" {
		t.Fatal("global modified")
	}
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	if item := setupItem(t, root, "codex", "hook"); item.State != "blocked" || item.CanInstall {
		t.Fatalf("global override: %+v", item)
	}
	for _, linkPath := range []string{".agents", ".agents/skills", ".agents/skills/pairroom-relay", ".agents/skills/pairroom-relay/SKILL.md"} {
		t.Run(linkPath, func(t *testing.T) {
			root := t.TempDir()
			link := filepath.Join(root, filepath.FromSlash(linkPath))
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			target := home
			if strings.HasSuffix(link, "SKILL.md") {
				target = filepath.Join(home, ".agents/skills/pairroom-relay/SKILL.md")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			item := setupItem(t, root, "codex", "skill")
			if item.State != "blocked" || item.CanInstall {
				t.Fatalf("symlink: %+v", item)
			}
			if _, err := Install(root, Request{Runtime: "codex", Component: "skill", Revision: item.Revision, ConfirmReplace: true}); err == nil {
				t.Fatal("wrote through link")
			}
		})
	}
}

func TestHookMergePreservesUnrelatedDataAndRequiresConfirmation(t *testing.T) {
	root := t.TempDir()
	old := `{"large":9007199254740993,"permissions":{"allow":["Read"]},"hooks":{"Stop":[{"matcher":"*","extra":"keep","hooks":[{"type":"command","command":"echo keep","timeout":1},{"type":"command","command":"pairroom relay hook --runtime codex","timeout":2}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"echo other"}]}]}}`
	put(t, root, ".codex/hooks.json", old)
	item := setupItem(t, root, "codex", "hook")
	if item.State != "update_required" || !item.NeedsConfirmation {
		t.Fatalf("%+v", item)
	}
	if err := setupInstall(t, root, "codex", "hook", false); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("%v", err)
	}
	if string(contents(t, root, ".codex/hooks.json")) != old {
		t.Fatal("unconfirmed change")
	}
	if err := setupInstall(t, root, "codex", "hook", true); err != nil {
		t.Fatal(err)
	}
	data := contents(t, root, ".codex/hooks.json")
	config, err := decodeHooks(data)
	if err != nil {
		t.Fatal(err)
	}
	if config["large"] != json.Number("9007199254740993") {
		t.Fatal("numeric user setting changed")
	}
	if !reflect.DeepEqual(config["permissions"], map[string]any{"allow": []any{"Read"}}) {
		t.Fatal("permissions changed")
	}
	if !bytes.Contains(data, []byte(`"extra": "keep"`)) || !bytes.Contains(data, []byte("echo keep")) || !bytes.Contains(data, []byte("echo other")) {
		t.Fatal("user hooks changed")
	}
	if bytes.Count(data, []byte(command("codex"))) != 1 {
		t.Fatal("duplicate hook")
	}
}

func TestHookInvalidDisabledAndCustomConfigurationsFailClosed(t *testing.T) {
	for _, text := range []string{``, `null`, `[]`, `{broken`, `{"hooks":null}`, `{"hooks":{"AfterAgent":{}}}`, `{"hooks":{"AfterAgent":[{}]}}`, `{"hooks":{"AfterAgent":[{"hooks":[5]}]}}`, `{"hooks":{},"hooks":{}}`, `{"disableAllHooks":true}`, `{"hooks":{"enabled":false}}`, `{"tools":{"enableHooks":false}}`, `{"hooks":{"AfterAgent":[{"hooks":[{"command":"wrapper pairroom relay hook --runtime gemini"}]}]}}`} {
		t.Run(text, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, ".gemini/settings.json", text)
			item := setupItem(t, root, "gemini", "hook")
			if item.CanInstall || (item.State != "blocked" && item.State != "disabled") {
				t.Fatalf("%+v", item)
			}
			_, err := Install(root, Request{Runtime: "gemini", Component: "hook", Revision: item.Revision, ConfirmReplace: true})
			if err == nil {
				t.Fatal("invalid/disabled configuration overwritten")
			}
			if string(contents(t, root, ".gemini/settings.json")) != text {
				t.Fatal("configuration changed")
			}
		})
	}
}

func TestGeminiHookUnitsAndAliasPrecedence(t *testing.T) {
	root := t.TempDir()
	if err := setupInstall(t, root, "gemini", "hook", false); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct {
				Type, Command string
				Timeout       int
			}
		}
	}
	if err := json.Unmarshal(contents(t, root, ".gemini/settings.json"), &config); err != nil {
		t.Fatal(err)
	}
	if config.Hooks["BeforeTool"][0].Matcher != "run_shell_command" {
		t.Fatal("missing bind identity matcher")
	}
	for _, event := range []string{"BeforeTool", "AfterAgent"} {
		hook := config.Hooks[event][0].Hooks[0]
		if hook.Timeout != 45000 || hook.Type != "command" || hook.Command != "pairroom relay hook --runtime gemini" {
			t.Fatalf("%+v", hook)
		}
	}
	put(t, root, ".agents/skills/pairroom-relay/SKILL.md", "user shared skill")
	item := setupItem(t, root, "gemini", "skill")
	if item.Path != ".agents/skills/pairroom-relay/SKILL.md" || !item.NeedsConfirmation {
		t.Fatalf("hidden alias: %+v", item)
	}
}

// The Gemini caller identity is captured from the run_shell_command BeforeTool
// observation, and `relay bind` re-checks exactly that matcher. A hand-written
// wildcard or absent matcher must not be reported as configured, or the panel
// would present a ready project that bind refuses.
func TestGeminiBeforeToolMatcherMustBeTheBindIdentityCommand(t *testing.T) {
	for _, group := range []string{
		`{"matcher":"*","hooks":[{"type":"command","command":"pairroom relay hook --runtime gemini","timeout":45000}]}`,
		`{"hooks":[{"type":"command","command":"pairroom relay hook --runtime gemini","timeout":45000}]}`,
	} {
		t.Run(group, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, ".gemini/settings.json", `{"hooks":{"AfterAgent":[{"hooks":[{"type":"command","command":"pairroom relay hook --runtime gemini","timeout":45000}]}],"BeforeTool":[`+group+`]}}`)
			item := setupItem(t, root, "gemini", "hook")
			if item.State != "update_required" || !item.CanInstall || !item.NeedsConfirmation {
				t.Fatalf("%+v", item)
			}
			if err := setupInstall(t, root, "gemini", "hook", true); err != nil {
				t.Fatal(err)
			}
			if now := setupItem(t, root, "gemini", "hook"); now.State != "current" {
				t.Fatalf("matcher not repaired: %+v", now)
			}
			data := contents(t, root, ".gemini/settings.json")
			if bytes.Count(data, []byte(command("gemini"))) != 2 || bytes.Count(data, []byte(`"matcher": "run_shell_command"`)) != 1 {
				t.Fatalf("expected one enabled shell-command hook per event: %s", data)
			}
		})
	}
}

// Grok has no hook of its own when its file is absent, so an incomplete Claude
// hook is an action on the Claude item, not a Grok conflict to resolve. The
// bind preflight already reuses that hook, and the panel must not invent an
// overlap that does not exist while also asking for a confirmation it refuses.
func TestGrokSharedHookPointsAtAnIncompleteClaudeHook(t *testing.T) {
	t.Setenv("GROK_CLAUDE_HOOKS_ENABLED", "true")
	root := t.TempDir()
	put(t, root, ".claude/settings.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"pairroom relay hook --runtime claude","timeout":45}]}]}}`)
	item := setupItem(t, root, "grok", "hook")
	if item.State != "blocked" || item.Reason != "claude_hook_update" || item.CanInstall || item.NeedsConfirmation {
		t.Fatalf("%+v", item)
	}
	if _, err := os.Stat(filepath.Join(root, ".grok")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Grok hook file written for a blocked item")
	}
	claude := setupItem(t, root, "claude", "hook")
	if claude.State != "update_required" || !claude.CanInstall {
		t.Fatalf("Claude item is not the actionable one: %+v", claude)
	}
	if err := setupInstall(t, root, "claude", "hook", true); err != nil {
		t.Fatal(err)
	}
	if now := setupItem(t, root, "grok", "hook"); now.State != "shared" || now.Reason != "claude_hook" {
		t.Fatalf("overlap not resolved into the shared Claude hook: %+v", now)
	}
	// A real overlap keeps its own reason and never asks for confirmation.
	put(t, root, ".grok/hooks/pairroom.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"pairroom relay hook --runtime grok","timeout":10}]}]}}`)
	conflict := setupItem(t, root, "grok", "hook")
	if conflict.State != "blocked" || conflict.Reason != "shared_hook_conflict" || conflict.CanInstall || conflict.NeedsConfirmation {
		t.Fatalf("existing Grok hook: %+v", conflict)
	}
}

func TestGrokSharedHookDoesNotInstallDuplicateOrRemoveExisting(t *testing.T) {
	t.Setenv("GROK_CLAUDE_HOOKS_ENABLED", "true")
	root := t.TempDir()
	if err := setupInstall(t, root, "claude", "hook", false); err != nil {
		t.Fatal(err)
	}
	item := setupItem(t, root, "grok", "hook")
	if item.State != "shared" || item.CanInstall {
		t.Fatalf("%+v", item)
	}
	if err := setupInstall(t, root, "grok", "hook", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".grok")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("duplicate Grok hook created")
	}
	put(t, root, ".grok/hooks/pairroom.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"pairroom relay hook --runtime grok","timeout":45}]}]}}`)
	for _, kind := range []string{"grok", "claude"} {
		item := setupItem(t, root, kind, "hook")
		if item.State != "blocked" || item.CanInstall {
			t.Fatalf("duplicate not surfaced: %+v", item)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".grok/hooks/pairroom.json")); err != nil {
		t.Fatal("existing hook removed")
	}
}

func TestMissingRevisionAndConcurrentRequestsCannotOverwrite(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(root, Request{Runtime: "codex", Component: "skill"}); !errors.Is(err, ErrChanged) {
		t.Fatalf("%v", err)
	}
	item := setupItem(t, root, "codex", "skill")
	request := Request{Runtime: "codex", Component: "skill", Revision: item.Revision}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := Install(root, request); results <- err }()
	}
	wg.Wait()
	close(results)
	success, rejected := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrChanged) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatalf("success=%d rejected=%d", success, rejected)
	}
}

func TestReplacingHardlinkedManifestDoesNotChangeOtherFile(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "global-skill")
	if err := os.WriteFile(external, []byte("global"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".agents/skills/pairroom-relay")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(external, filepath.Join(dir, "SKILL.md")); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	if err := setupInstall(t, root, "codex", "skill", true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(external)
	if string(data) != "global" {
		t.Fatal("global hardlink target changed")
	}
}

func TestSharedHookGuardHandlesEscapedAndWrappedCommands(t *testing.T) {
	t.Setenv("GROK_CLAUDE_HOOKS_ENABLED", "true")
	for _, command := range []string{`pairroom relay hook --runtime \u0067rok`, `wrapper pairroom relay hook --runtime grok`} {
		root := t.TempDir()
		put(t, root, ".grok/hooks/pairroom.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"`+command+`","timeout":45}]}]}}`)
		item := setupItem(t, root, "claude", "hook")
		if item.State != "blocked" || item.CanInstall {
			t.Fatalf("duplicate bypass: %+v", item)
		}
	}
}

func TestHookMergeRetainsEmptyUserGroups(t *testing.T) {
	root := t.TempDir()
	put(t, root, ".codex/hooks.json", `{"hooks":{"Stop":[{"matcher":"user-matcher","extra":"retain","hooks":[]}]}}`)
	if err := setupInstall(t, root, "codex", "hook", true); err != nil {
		t.Fatal(err)
	}
	data := contents(t, root, ".codex/hooks.json")
	if !bytes.Contains(data, []byte("user-matcher")) || !bytes.Contains(data, []byte("retain")) {
		t.Fatal("unrelated empty group was discarded")
	}
}

func TestPreviewRejectsFileThatAppearsBeforeFirstInstall(t *testing.T) {
	root := t.TempDir()
	item := setupItem(t, root, "codex", "hook")
	put(t, root, ".codex/hooks.json", `{"user":"new configuration"}`)
	_, err := Install(root, Request{Runtime: "codex", Component: "hook", Revision: item.Revision})
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("%v", err)
	}
	if string(contents(t, root, ".codex/hooks.json")) != `{"user":"new configuration"}` {
		t.Fatal("new file overwritten")
	}
}

func TestGrokSharedHookDoesNotHideDisabledProjectHooks(t *testing.T) {
	t.Setenv("GROK_CLAUDE_HOOKS_ENABLED", "true")
	root := t.TempDir()
	if err := setupInstall(t, root, "claude", "hook", false); err != nil {
		t.Fatal(err)
	}
	const disabled = `{"disableAllHooks":true}`
	put(t, root, ".grok/hooks/pairroom.json", disabled)
	item := setupItem(t, root, "grok", "hook")
	if item.State != "disabled" || item.CanInstall {
		t.Fatalf("disabled hook reported ready: %+v", item)
	}
	if err := setupInstall(t, root, "grok", "hook", true); !errors.Is(err, ErrBlocked) {
		t.Fatalf("disabled install: %v", err)
	}
	if string(contents(t, root, ".grok/hooks/pairroom.json")) != disabled {
		t.Fatal("disabled setting changed")
	}
}
