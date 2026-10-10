package relayclient

import (
	"context"
	"errors"
	"time"

	"github.com/sean2077/pairroom/internal/lanclient"
)

// Workspace files and session locators do not keep a retired direct client
// active. The same-user client record owns admission and local retirement,
// including leave/detach performed through the optional Service dashboard.
// Call only after matching the exact native caller; this reads local authority
// without opening a network connection, repairing state, or consuming receipts.
func currentDirectBinding(s State) (bool, error) {
	if s.Schema == 2 && s.LAN == nil {
		return true, nil
	}
	if !validStateFormat(s) || s.LAN == nil {
		return false, errors.New("invalid direct LAN binding format")
	}
	store, err := lanclient.Open()
	if err != nil {
		return false, err
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := store.Get(ctx, s.LAN.ClientID)
	if err != nil {
		return false, err
	}
	meta, err := client.Metadata(ctx)
	if err != nil {
		return false, err
	}
	if meta.Status != "accepted" || meta.BindID != s.BindID {
		return false, nil // retired or superseded; never select its old WAL
	}
	if meta.ID != s.Room || meta.Invite.Endpoint != s.LAN.Endpoint || meta.Invite.HostPin != s.LAN.HostPin || meta.Invite.RoomID != s.LAN.RoomID ||
		!sameWorkspace(meta.Workspace, s.Workspace) || meta.Runtime != s.Runtime || meta.SessionID != s.SessionID || meta.Generation != s.Generation || meta.Slot != s.Slot {
		return false, errors.New("direct LAN binding differs from its private client record; inspect the original association")
	}
	return true, nil
}
