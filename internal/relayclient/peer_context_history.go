package relayclient

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/relay"
)

// This private, credential-free record only restricts automatic publication.
// It cannot discover, authenticate, or associate a native session. Retaining
// its original scope prevents unbind/rejoin (including another workspace or
// transport) from relabeling a late Stop as a response to a new peer.
type responseContextHistory struct {
	Schema         int               `json:"schema"`
	Runtime        model.RuntimeKind `json:"runtime"`
	SessionID      string            `json:"session_id"`
	Room           string            `json:"room"`
	Slot           model.ActorID     `json:"slot"`
	Workspace      string            `json:"workspace"`
	PeerGeneration uint64            `json:"peer_generation"`
	ScopeChanged   bool              `json:"scope_changed,omitempty"`
}

func responseHistoryDirectory(s State) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", err
	}
	base, err = secureDir(base, "pairroom")
	if err != nil {
		return "", err
	}
	for _, part := range []string{"relay-response-contexts", relay.Digest(string(s.Runtime) + "\x00" + s.SessionID)} {
		base = filepath.Join(base, part)
		if err := privatefile.Mkdir(base); err != nil {
			return "", err
		}
	}
	return base, nil
}

// Called after Service admission identifies the exact slot, before any
// bootstrap output. Its lock serializes another workspace's same-session
// bind; history is immutable except for a sticky cross-scope pause bit.
func retainResponseContext(ctx context.Context, s *State, generation uint64) error {
	if !s.DynamicBindings {
		// A v8 session can still finish an older response after joining v9.
		// Record that ambiguity without changing the v8 binding's behavior.
		legacy := *s
		legacy.DynamicBindings = true
		legacy.AutomaticScopeChanged = true
		s = &legacy
		generation = 0
	}
	if !validResponseScope(*s) {
		return errResponseContextMissing
	}
	if generation == 0 {
		s.AutomaticPaused, s.AutomaticScopeChanged = true, true
	}
	dir, err := responseHistoryDirectory(*s)
	if err != nil {
		return err
	}
	unlock, err := privatelock.Lock(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(dir, "context.json")
	var history responseContextHistory
	err = privatefile.ReadJSON(path, 16<<10, &history)
	if err == nil {
		if history.Schema != 1 || history.Runtime != s.Runtime || history.SessionID != s.SessionID || !safePart(history.Room) || !history.Slot.ValidParticipant() || !filepath.IsAbs(history.Workspace) || history.PeerGeneration == 0 && !history.ScopeChanged {
			return errors.New("invalid original Native response-context history; automatic binding output withheld; inspect the private history before recovery")
		}
		s.inheritResponseContext(State{DynamicBindings: true, Runtime: history.Runtime, SessionID: history.SessionID, Room: history.Room, Slot: history.Slot, Workspace: history.Workspace, AutomaticPeerGeneration: history.PeerGeneration, AutomaticScopeChanged: history.ScopeChanged})
	} else if errors.Is(err, os.ErrNotExist) {
		// A restored workspace may retain archives without the per-user record.
		// Never silently treat that same native session as a fresh response.
		if err := inheritRetiredResponseContexts(s); err != nil {
			return err
		}
	} else {
		return err
	}
	s.capturePeerContext(generation)
	if history.Schema == 0 {
		history = responseContextHistory{Schema: 1, Runtime: s.Runtime, SessionID: s.SessionID, Room: s.Room, Slot: s.Slot, Workspace: s.Workspace, PeerGeneration: s.AutomaticPeerGeneration}
	}
	history.ScopeChanged = history.ScopeChanged || s.AutomaticScopeChanged
	return privatefile.WriteJSON(path, history)
}

// Validate identity before comparing it with the current caller. A valid JSON
// object with omitted fields is damaged evidence, not an unrelated session.
func validResponseScope(s State) bool {
	return validResponseSession(s.Runtime, s.SessionID) && safePart(s.Room) && s.Slot.ValidParticipant() && filepath.IsAbs(s.Workspace)
}

func validResponseSession(kind model.RuntimeKind, session string) bool {
	return kind.Valid() && kind == kind.Canonical() && session != "" && len(session) <= 256 && strings.TrimSpace(session) == session && !strings.ContainsAny(session, "/\\") && !strings.ContainsFunc(session, unicode.IsControl)
}

func inheritRetiredResponseContexts(s *State) error {
	dir, err := existingDir(s.Workspace, ".pairroom", "retired")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	entries, err := file.ReadDir(1025)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > 1024 {
		return errors.New("retired binding history exceeds its bounded scan; inspect before rebinding this native session")
	}
	for _, entry := range entries {
		if !entry.IsDir() || !safePart(entry.Name()) {
			return errors.New("invalid retired binding history; inspect before rebinding")
		}
		var previous State
		err := readPrivate(filepath.Join(dir, entry.Name(), "state.json"), &previous)
		if errors.Is(err, os.ErrNotExist) {
			// A join-attempt can also be the surviving fragment of an admitted
			// binding's backup; it never proves a fresh response boundary.
			var attempt lanJoinAttempt
			if readPrivate(filepath.Join(dir, entry.Name(), "join-attempt.json"), &attempt) != nil {
				return errors.New("retired binding history is incomplete; restore its original private state before rebinding")
			}
			invite, parseErr := lanshare.ParseInvite(attempt.Invitation)
			if parseErr != nil || attempt.Credentials.BindID != entry.Name() || !validResponseSession(attempt.Runtime, attempt.SessionID) || !validLANJoinAttempt(attempt, s.Workspace, lanRoutingID(invite), attempt.Runtime, attempt.SessionID) {
				return errors.New("retired LAN attempt identity is corrupt; inspect before rebinding")
			}
			if attempt.Runtime == s.Runtime && attempt.SessionID == s.SessionID {
				s.AutomaticScopeChanged, s.AutomaticPaused = true, true
			}
			continue
		}
		if err != nil {
			return err
		}
		if !validStateFormat(previous) || !validResponseScope(previous) || previous.Generation == 0 || previous.BindID != entry.Name() || !sameWorkspace(previous.Workspace, s.Workspace) {
			return errors.New("retired binding identity is corrupt; inspect before rebinding")
		}
		s.inheritResponseContext(previous)
	}
	return nil
}
