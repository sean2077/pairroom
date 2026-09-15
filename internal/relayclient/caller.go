package relayclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

// IsolateNativeCaller clears inherited session metadata and process ancestry so
// tests can install an exact caller fixture. Production code never calls this.
func IsolateNativeCaller(t *testing.T) {
	t.Helper()
	for _, key := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "GROK_SESSION_ID", "CLAUDECODE"} {
		t.Setenv(key, "")
	}
	before := harnessAncestor
	harnessAncestor = func() (int, string, bool) { return 0, "", false }
	t.Cleanup(func() { harnessAncestor = before })
}

// nativeCaller is discovery metadata, never a replacement for approved-hook
// association or the Service's credential/generation/session checks. A desktop
// may serve several sessions from one process, so session metadata beats PID.
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
		{model.RuntimeCodex, "CODEX_THREAD_ID"},
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
		if len(value) > 256 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "/\\\r\n\x00") {
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
// It only selects existing state; it cannot associate a pending nonce. Explicit
// Room/slot filters remain supported, but cannot silently target another known
// session. No provider, model or credential configuration is harvested.
func applyCallerDefaults(root, action string, o *options) error {
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
	if caller.runtime == model.RuntimeGrok {
		return errors.New("Grok Build was detected, but PairRoom Native currently supports Claude Code and Codex only; do not bind Grok as another runtime (Embedded Grok remains available)")
	}
	if caller.session != "" && o.session != "" && o.session != caller.session {
		return errors.New("--session-id conflicts with the calling native session")
	}
	if action == "install" {
		if o.kind == "" {
			o.kind = string(caller.runtime)
		}
		return nil
	}
	if caller.session == "" {
		return nil // older harnesses retain the existing lineage/explicit path
	}
	if action != "bind" && (o.room == "") != (o.slot == "") {
		return errors.New("pass --room and --slot together, or omit both inside the associated native session")
	}
	paths, err := statePaths(root)
	if err != nil {
		return err
	}
	var matches, pending []State
	for _, path := range paths {
		var s State
		if err := readPrivate(path, &s); err != nil {
			return err
		}
		if s.Runtime != caller.runtime {
			continue
		}
		associated := s.SessionID == caller.session
		if !associated && s.SessionID != "" {
			continue
		}
		if associated && action == "bind" && o.create {
			return errors.New("this native session is already associated; use pairroom relay bind to resume it, not bind --create")
		}
		if !safePart(s.Room) || !s.Slot.ValidParticipant() || filepath.Base(filepath.Dir(path)) != string(s.Slot) || filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path)))) != s.Room {
			return errors.New("invalid local relay binding identity")
		}
		if (o.room != "" && o.room != s.Room) || (o.slot != "" && o.slot != string(s.Slot)) {
			continue
		}
		if associated {
			matches = append(matches, s)
			continue
		}
		// Pending bindings have no official session yet. Status may inspect a
		// unique match; send/wait/exchange still require nonce association.
		pending = append(pending, s)
	}
	if len(matches) > 1 {
		return errors.New("native session matches multiple relay bindings; inspect them and pass --room/--slot explicitly")
	}
	if len(matches) == 1 {
		s := matches[0]
		o.room, o.slot = s.Room, string(s.Slot)
		if action == "bind" {
			if o.endpoint == "" {
				o.endpoint = s.EndpointPath
			}
			o.cont, o.session = true, caller.session
		}
		return nil
	}
	if action == "status" && len(pending) == 1 {
		s := pending[0]
		o.room, o.slot = s.Room, string(s.Slot)
		return nil
	}
	if action == "status" && len(pending) > 1 {
		return errors.New("multiple pending native bindings; pass --room/--slot to inspect one (status does not associate or collect)")
	}
	if action == "bind" {
		// New/pending bindings still resolve against the Service's active Room
		// and pair selections, then require the official Stop-hook nonce.
		return nil
	}
	if len(pending) > 0 {
		return errors.New("this native session is awaiting approved Stop-hook nonce association; echo bind_nonce in this session's visible reply before send/wait/exchange")
	}
	return errors.New("this native session has no matching associated binding in this workspace; run pairroom relay bind here first (do not consume another session's inbox)")
}
