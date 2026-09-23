package relayclient

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/model"
)

var errHookSessionMismatch = errors.New("PairRoom: this bound harness reported a different session identity; no reply was published or collected. Run bind inside the intended session; use --replace only for an intentional session change")

func boundHookCandidates(paths []string, kind model.RuntimeKind, session string) ([]string, error) {
	envSession := sessionIDFromEnv(kind)
	var candidates []string
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
		// Only exact session metadata identifies an already-bound caller. A
		// shared (or reused) host PID cannot establish that this session opted in.
		if s.SessionID != session && envSession != "" && s.SessionID == envSession {
			return nil, errHookSessionMismatch
		}
		if s.SessionID == session {
			candidates = append(candidates, filepath.Dir(path))
		}
	}
	return candidates, nil
}
