package relayclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/sean2077/pairroom/internal/relay"
)

var errResponseContextMissing = errors.New("automatic publication has no original peer-generation context; reply retained locally; inspect relay peer, then explicitly send the intended text with --peer-generation N and a new --id")
var errResponseScopeChanged = errors.New("this native session changed Room or slot without a provable new response boundary; automatic reply retained locally as context_changed; inspect relay peer and explicitly send the intended text with --peer-generation N and a new --id")

func automaticPauseNotice(generation uint64) string {
	return fmt.Sprintf("Automatic publication is paused because this native hook cannot prove which response belongs to the current peer. Original Stop text and its target generation stay in the local publication journal. Inspect relay status and relay peer; use that peer_generation as N, then explicitly send the intended text with --peer-generation N and a new --id. The last observed generation is %d; it never retargets an old reply. Use reconcile --discard only after deciding what to retain. Foreground relay wait remains available. A fresh native session/binding can establish a new automatic context.", generation)
}

// capturePeerContext is called only when a bootstrap, claim, or explicit peer
// inspection provides a positive context generation. It never changes an
// established automatic target: late old Stops have no common native turn ID.
func (s *State) capturePeerContext(generation uint64) {
	if !s.DynamicBindings || generation == 0 {
		return
	}
	if s.AutomaticPeerGeneration == 0 {
		s.AutomaticPeerGeneration = generation
	} else if s.AutomaticPeerGeneration != generation {
		s.AutomaticPaused = true
	}
	if generation > s.PeerGeneration {
		s.PeerGeneration = generation
	}
}

// Rotating a local credential inside the same native session does not prove a
// new response boundary. Keep its original fence, including across an explicit
// replacement; only a different runtime/session can discard that ambiguity.
func (s *State) inheritResponseContext(previous State) {
	if s.DynamicBindings && s.Runtime == previous.Runtime && s.SessionID == previous.SessionID {
		if !previous.DynamicBindings || previous.AutomaticPeerGeneration == 0 {
			s.AutomaticScopeChanged = true
		} else if s.AutomaticPeerGeneration == 0 || previous.AutomaticPeerGeneration < s.AutomaticPeerGeneration {
			s.AutomaticPeerGeneration = previous.AutomaticPeerGeneration
		}
		if previous.PeerGeneration > s.PeerGeneration {
			s.PeerGeneration = previous.PeerGeneration
		}
		s.AutomaticScopeChanged = s.AutomaticScopeChanged || previous.AutomaticScopeChanged || previous.Room != s.Room || previous.Slot != s.Slot || !sameWorkspace(previous.Workspace, s.Workspace)
		s.AutomaticPaused = s.AutomaticPaused || previous.AutomaticPaused || s.AutomaticScopeChanged
	}
}

// recordPeerContext serializes with hook WAL writes and bind replacement. It
// runs before exposing a new envelope so a failed write never authorizes a
// reply against an unrecorded context.
func (c *Client) recordPeerContext(ctx context.Context, generation uint64, hook bool) error {
	if c.State.DynamicBindings && generation == 0 {
		return errors.New("dynamic Native delivery omitted peer_generation; acknowledgement withheld; update CLI and Service together")
	}
	release, err := lockSlot(ctx, c.Dir)
	if err != nil {
		return err
	}
	defer release()
	current, err := loadLocal(c.Dir)
	if err != nil {
		return err
	}
	defer current.close()
	if current.State.BindID != c.State.BindID || current.State.Generation != c.State.Generation || current.State.Runtime != c.State.Runtime {
		return relay.ErrAuth
	}
	next := current.State
	if hook {
		next.Blocks++
	}
	if next.DynamicBindings && next.AutomaticPeerGeneration == 0 {
		// A restored state that lost its original bootstrap must not grant
		// automatic permission to an already-running response from a new read.
		next.AutomaticPaused = true
		if generation > next.PeerGeneration {
			next.PeerGeneration = generation
		}
	} else {
		next.capturePeerContext(generation)
	}
	if err := current.persist(next); err != nil {
		return err
	}
	c.State = next
	return nil
}

func (c *Client) publicationPeerGeneration(explicit uint64) (uint64, error) {
	if !c.State.DynamicBindings {
		if explicit != 0 {
			return 0, errors.New("--peer-generation applies only to dynamic Native Rooms")
		}
		return 0, nil
	}
	if explicit != 0 {
		return explicit, nil
	}
	if c.State.AutomaticPaused {
		return 0, errors.New(automaticPauseNotice(c.State.PeerGeneration))
	}
	if c.State.AutomaticPeerGeneration == 0 {
		return 0, errResponseContextMissing
	}
	return c.State.AutomaticPeerGeneration, nil
}
