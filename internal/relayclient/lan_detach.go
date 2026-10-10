package relayclient

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

// A pending or lost admission may have no slot state yet. Its original local
// attempt still authorizes an explicit offline detach from the exact native
// session. The private client record is retained for later host revocation.
func unbindLANLocalOnly(ctx context.Context, root string, o options, out io.Writer) error {
	caller, err := currentNativeCaller()
	if err != nil {
		return err
	}
	if caller.session == "" {
		return errors.New("detach a direct LAN association from its original native session")
	}
	store, err := lanclient.Open()
	if err != nil {
		return err
	}
	defer store.Close()
	client, err := store.Get(ctx, o.room)
	if err != nil {
		return err
	}
	meta, err := client.Metadata(ctx)
	if err != nil {
		return err
	}
	if meta.Runtime != caller.runtime || meta.SessionID != caller.session || !sameWorkspace(meta.Workspace, root) || o.endpoint != "" || o.slot != "" && o.slot != string(meta.Slot) {
		return relay.ErrAuth
	}
	dir, err := existingDir(root, ".pairroom", "lan-joins", o.room)
	if err != nil {
		return err
	}
	release, err := lockSlot(ctx, dir)
	if err != nil {
		return err
	}
	defer release()
	var attempt lanJoinAttempt
	readErr := privatefile.ReadJSON(filepath.Join(dir, "join-attempt.json"), maxPrivateFileBytes, &attempt)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	matches := func(a lanJoinAttempt) bool {
		invite, err := lanshare.ParseInvite(a.Invitation)
		return err == nil && validLANJoinAttempt(a, root, meta.ID, caller.runtime, caller.session) && a.Credentials.BindID == meta.BindID && a.PreviousBindID == meta.PreviousBindID && invite.Endpoint == meta.Invite.Endpoint && invite.HostPin == meta.Invite.HostPin && invite.RoomID == meta.Invite.RoomID
	}
	if readErr != nil || !matches(attempt) {
		// The client cutover can commit before workspace promotion. Its staged
		// capability is eligible only when the installed private record names
		// that exact new identity; no host call or new request is necessary.
		var staged lanJoinAttempt
		if err := privatefile.ReadJSON(filepath.Join(dir, lanReplacementAttemptFile), maxPrivateFileBytes, &staged); err != nil {
			return relay.ErrAuth
		}
		if staged.PreviousBindID == "" || !matches(staged) {
			return relay.ErrAuth
		}
		if err := finishLANReplacement(ctx, root, dir, meta.ID, staged, client); err != nil {
			return err
		}
		attempt = staged
	}
	meta, err = client.Metadata(ctx)
	if err != nil {
		return err
	}
	if !matches(attempt) {
		return relay.ErrAuth
	}
	if meta.Slot.ValidParticipant() {
		slotDir, err := existingDir(root, ".pairroom", "rooms", meta.ID, "slots", string(meta.Slot))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			// Existence only selects the workspace cleanup path. Its locked
			// reader validates all bytes, including recovered ones, against the
			// protected client identity before any retirement or file removal.
			if _, err := os.Lstat(filepath.Join(slotDir, "state.json")); err == nil {
				return unbindLocalOnlyWithMetadata(ctx, root, slotDir, o, &meta, out)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if err := client.Detach(ctx, relay.Auth{Slot: meta.Slot, BindID: meta.BindID, Generation: meta.Generation, SessionID: caller.session, Secret: attempt.Credentials.Secret}); err != nil {
		return err
	}
	result := map[string]any{"unbound": "local-only", "room": meta.ID, "notice": "Direct LAN admission detached locally without contacting the host. The original request and key are retained; ask the host owner to revoke any pending or accepted membership. This native session is free for another Room and will not reconnect automatically."}
	if o.purge {
		paths, err := statePaths(root)
		if err != nil {
			return err
		}
		for _, path := range paths {
			var state State
			if readPrivate(path, &state) == nil && state.Runtime == meta.Runtime {
				result["hooks_preserved"] = "another local binding uses this runtime"
				return writeJSON(out, result)
			}
		}
		if err := editHooks(root, meta.Runtime, true); err != nil {
			return err
		}
	}
	return writeJSON(out, result)
}
