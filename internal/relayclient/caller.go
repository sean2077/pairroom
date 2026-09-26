package relayclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sean2077/pairroom/internal/model"
)

// Keep the cross-package fixture seam without importing the testing runtime
// (and its flag registrations) into the shipped CLI.
type nativeCallerTest interface {
	Helper()
	Setenv(string, string)
	TempDir() string
	Cleanup(func())
}

// IsolateNativeCaller clears inherited session/config metadata and process ancestry so
// tests can install an exact caller fixture. Production code never calls this.
func IsolateNativeCaller(t nativeCallerTest) {
	t.Helper()
	for _, key := range []string{"CLAUDE_PROJECT_DIR", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "GROK_SESSION_ID", "CLAUDECODE", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		t.Setenv(key, "")
	}
	// os.UserConfigDir uses these platform-specific anchors. Isolate locators
	// without ever reading or writing a developer's real session index.
	home := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(key, home)
	}
	before := harnessAncestor
	harnessAncestor = func() (int, string, bool) { return 0, "", false }
	t.Cleanup(func() { harnessAncestor = before })
}

// nativeCaller supplies bind-time identity and discovery metadata. It never
// replaces the Service's credential/generation/session checks or hook approval.
// A desktop may serve several sessions from one process, so session metadata
// beats PID.
type nativeCaller struct {
	runtime model.RuntimeKind
	session string
}

func currentNativeCaller() (nativeCaller, error) {
	vars := []struct {
		kind model.RuntimeKind
		name string
	}{
		{model.RuntimeClaude, "CLAUDE_CODE_SESSION_ID"},
		{model.RuntimeCodex, "CODEX_SESSION_ID"},
		{model.RuntimeGrok, "GROK_SESSION_ID"},
	}
	// Prefer the nearest recognized harness over inherited outer-harness
	// variables. Never choose arbitrarily between multiple unscoped hints.
	var nearest model.RuntimeKind
	if _, name, ok := harnessAncestor(); ok {
		nearest = harnessRuntimes[name]
	}
	caller := nativeCaller{runtime: nearest}
	for _, v := range vars {
		value := os.Getenv(v.name)
		if value == "" || (nearest != "" && nearest != v.kind) {
			continue
		}
		if len(value) > 256 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "/\\") || strings.ContainsFunc(value, unicode.IsControl) {
			return nativeCaller{}, fmt.Errorf("invalid %s session metadata; run the command in the intended native session", v.name)
		}
		if caller.runtime != "" && caller.runtime != v.kind {
			return nativeCaller{}, errors.New("conflicting native session metadata; run the command in the intended native session without inherited outer-session variables")
		}
		caller = nativeCaller{runtime: v.kind, session: value}
	}
	if caller.runtime == "" && os.Getenv("CLAUDECODE") == "1" {
		caller.runtime = model.RuntimeClaude
	}
	return caller, nil
}

// applyCallerDefaults runs before create/bind or any foreground side effect.
// It only selects existing state; association happens at bind from the harness
// session id. Explicit Room/slot filters remain supported, but cannot silently
// target another known session. No provider, model or credential configuration
// is harvested.
func applyCallerDefaults(root, action string, o *options) error {
	o.slot = normalizeSlot(o.slot)
	// Preparing another harness's hooks is an explicit setup operation, not
	// an attempt to bind this session as that runtime. Keep that path usable.
	if action == "install" && o.kind != "" {
		return nil
	}
	caller, err := currentNativeCaller()
	if err != nil {
		return err
	}
	if o.kind != "" && caller.runtime != "" && model.RuntimeKind(o.kind) != caller.runtime {
		return errors.New("--runtime conflicts with the calling native harness")
	}
	if action == "install" {
		if o.kind == "" {
			o.kind = string(caller.runtime)
		}
		return nil
	}
	if caller.session == "" {
		return nil // a plain terminal may select an existing binding for diagnostics
	}
	paths, err := statePaths(root)
	if err != nil {
		return err
	}
	var matches []State
	for _, path := range paths {
		var s State
		if err := readPrivate(path, &s); err != nil {
			return err
		}
		if s.Schema != 2 || s.Generation == 0 || s.SessionID != caller.session || s.Runtime != caller.runtime {
			continue
		}
		if action == "bind" && o.create {
			return errors.New("this native session is already associated; use pairroom relay bind to resume it, not bind --create")
		}
		if !safePart(s.Room) || !s.Slot.ValidParticipant() || filepath.Base(filepath.Dir(path)) != string(s.Slot) || filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path)))) != s.Room {
			return errors.New("invalid local relay binding identity")
		}
		if (o.room != "" && o.room != s.Room) || (o.slot != "" && o.slot != string(s.Slot)) {
			continue
		}
		matches = append(matches, s)
	}
	if len(matches) > 1 {
		return errors.New("native session matches multiple relay bindings; inspect them and pass --room/--slot explicitly")
	}
	if len(matches) == 1 {
		s := matches[0]
		o.room, o.slot = s.Room, string(s.Slot)
		if action == "bind" && o.endpoint == "" {
			o.endpoint = s.EndpointPath
		}
		return nil
	}
	if action == "bind" {
		// New bindings still resolve against the Service's active Room and pair
		// selections, then associate from the harness session id at bind.
		return nil
	}
	return errNoAssociatedBinding
}
