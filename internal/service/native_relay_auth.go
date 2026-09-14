package service

import (
	"path/filepath"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Admission is not a cache or an execution authority. The selected Engine
// reauthenticates at every effect, closing replacement/revocation races. The
// loader is explicit so regressions can prove that active polls do no log I/O.
func authenticateNativeRelay(active RoomRuntime, dataDir string, auth relay.Auth, load func(string) ([]model.Event, error)) error {
	if active != nil {
		native, ok := active.(*nativeHostRuntime)
		if !ok {
			return relay.ErrAuth
		}
		return native.engine.CheckAuth(auth)
	}
	events, err := load(filepath.Join(dataDir, "events.jsonl"))
	if err != nil {
		return relay.ErrAuth
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != relay.EventBinding {
			continue
		}
		b, err := relay.BindingFromEvent(events[i])
		if err != nil {
			return relay.ErrAuth
		}
		if b.Slot == auth.Slot {
			return relay.AuthenticateBindingEvent(events[i], auth)
		}
	}
	return relay.ErrAuth
}
