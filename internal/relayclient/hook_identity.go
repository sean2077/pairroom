package relayclient

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

var errHookSessionMismatch = errors.New("PairRoom: this bound harness reported a different session identity; no reply was published or collected. Run bind inside the intended session; use --replace only for an intentional session change")

func boundHookCandidates(paths []string, kind model.RuntimeKind, session string) ([]string, error) {
	pid, name, hasLineage := harnessAncestor()
	envSession := sessionIDFromEnv(kind)
	var candidates []string
	lineageMatches := 0
	for _, path := range paths {
		var s State
		// A binding removed between resolution and this read (a concurrent local
		// unbind, or a bind replaced under the same session) no longer exists:
		// that is inert, not a Stop-hook failure. A present file that cannot be
		// read still fails closed below.
		if err := readPrivate(path, &s); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		if s.Schema != 2 || s.Runtime != kind || s.Generation == 0 || s.SessionID == "" {
			continue
		}
		// These observations only identify an error for an already-bound caller.
		// They never authorize a request, associate a session or select a fallback.
		sameHarness := hasLineage && s.HarnessPID != 0 && s.HarnessPID == pid && strings.EqualFold(s.HarnessName, name)
		if s.SessionID != session && envSession != "" && s.SessionID == envSession {
			return nil, errHookSessionMismatch
		}
		if sameHarness && s.SessionID != session {
			lineageMatches++
		}
		if s.SessionID == session {
			candidates = append(candidates, filepath.Dir(path))
		}
	}
	// A shared host process can own several sessions. A valid exact identity
	// must not be rejected merely because another binding shares that PID.
	if len(candidates) == 0 && lineageMatches == 1 {
		return nil, errHookSessionMismatch
	}
	if len(candidates) == 0 && lineageMatches > 1 {
		return nil, errors.New("PairRoom: this harness has multiple bindings but none matches its reported session identity; no reply was published or collected. Inspect relay status and rebind the intended session")
	}
	return candidates, nil
}
