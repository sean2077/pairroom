package relayclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/sean2077/pairroom/internal/claudewake"
	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

const lanReplacementAttemptFile = "join-replacement.json"

func newLANJoinAttempt(root, invitation string, kind model.RuntimeKind, session string) (lanJoinAttempt, error) {
	id, err := relay.RandomID()
	if err != nil {
		return lanJoinAttempt{}, err
	}
	secret, err := relay.RandomID()
	if err != nil {
		return lanJoinAttempt{}, err
	}
	return lanJoinAttempt{Schema: 2, Invitation: invitation, Workspace: root, Runtime: kind, SessionID: session, Credentials: credentials{BindID: id, Secret: secret}}, nil
}

func validLANJoinAttempt(attempt lanJoinAttempt, root, room string, kind model.RuntimeKind, session string) bool {
	invite, err := lanshare.ParseInvite(attempt.Invitation)
	return err == nil && attempt.Schema == 2 && lanRoutingID(invite) == room && attempt.Workspace == root && attempt.Runtime == kind && attempt.SessionID == session && safePart(attempt.Credentials.BindID) && attempt.Credentials.Secret != "" && (attempt.PreviousBindID == "" || safePart(attempt.PreviousBindID) && attempt.PreviousBindID != attempt.Credentials.BindID)
}

// Stage a replacement local credential separately. The active attempt and WAL
// stay intact until the client store proves retirement at the original host
// and durably installs the fresh admission request with this exact credential.
func prepareLANReplacement(ctx context.Context, root, dir, room string, o options, kind model.RuntimeKind, session string) (lanJoinAttempt, error) {
	var attempt lanJoinAttempt
	path := filepath.Join(dir, lanReplacementAttemptFile)
	if err := privatefile.ReadJSON(path, maxPrivateFileBytes, &attempt); err == nil {
		if !validLANJoinAttempt(attempt, root, room, kind, session) || attempt.PreviousBindID == "" || attempt.Invitation != o.invitation {
			return attempt, errors.New("a different explicit LAN replacement is pending; resume its original invitation and native session")
		}
		return attempt, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return attempt, err
	}
	if err := privatefile.ReadJSON(filepath.Join(dir, "join-attempt.json"), maxPrivateFileBytes, &attempt); err == nil {
		if validLANJoinAttempt(attempt, root, room, kind, session) && attempt.PreviousBindID != "" && attempt.Invitation == o.invitation {
			return attempt, nil // original replacement already promoted
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return attempt, err
	}
	store, err := lanclient.Open()
	if err != nil {
		return attempt, err
	}
	defer store.Close()
	client, err := store.Get(ctx, room)
	if err != nil {
		return attempt, fmt.Errorf("explicit LAN replacement requires an existing private client record: %w", err)
	}
	meta, err := client.Metadata(ctx)
	if err != nil {
		return attempt, err
	}
	invite, err := lanshare.ParseInvite(o.invitation)
	if err != nil || !sameWorkspace(meta.Workspace, root) || meta.Invite.HostPin != invite.HostPin || meta.Invite.RoomID != invite.RoomID || meta.Invite.Endpoint != invite.Endpoint || meta.Invite.InviteID == invite.InviteID {
		return attempt, errors.New("explicit LAN replacement requires a fresh invitation for the original host, Room, and local workspace")
	}
	attempt, err = newLANJoinAttempt(root, o.invitation, kind, session)
	if err != nil {
		return attempt, err
	}
	attempt.PreviousBindID = meta.BindID
	if err := privatefile.WriteJSON(path, attempt); err != nil {
		return attempt, err
	}
	return attempt, nil
}

func archiveLANAttempt(root string, attempt lanJoinAttempt) error {
	if !safePart(attempt.Credentials.BindID) {
		return errors.New("invalid retired LAN attempt identity")
	}
	dir, err := secureLANJoinDir(root, "retired", attempt.Credentials.BindID)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "join-attempt.json")
	var previous lanJoinAttempt
	if err := privatefile.ReadJSON(path, maxPrivateFileBytes, &previous); err == nil {
		if !reflect.DeepEqual(previous, attempt) {
			return errors.New("retired LAN attempt differs from its original private archive")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return privatefile.WriteJSON(path, attempt)
}

// Called with the original slot lock held, after membership is retired. The
// complete publication WAL is retained as private evidence, never replayed.
func archiveLANWorkspaceState(root, slotDir string, state State) error {
	if !validStateFormat(state) || state.LAN == nil || !safePart(state.BindID) || !sameWorkspace(state.Workspace, root) {
		return errors.New("invalid retired direct LAN workspace identity")
	}
	dir, err := secureLANJoinDir(root, "retired", state.BindID)
	if err != nil {
		return err
	}
	var archived State
	if err := privatefile.ReadJSON(filepath.Join(dir, "state.json"), maxPrivateFileBytes, &archived); err == nil {
		if archived.BindID != state.BindID || archived.Generation != state.Generation || archived.Room != state.Room || archived.SessionID != state.SessionID {
			return errors.New("retired LAN workspace archive belongs to a different binding")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := privatefile.WriteJSON(filepath.Join(dir, "state.json"), state); err != nil {
		return err
	}
	var cred credentials
	if err := privatefile.ReadJSON(filepath.Join(slotDir, "credentials"), maxPrivateFileBytes, &cred); err == nil {
		if cred.BindID != state.BindID {
			return errors.New("retired LAN credential differs from its workspace binding")
		}
		if err := privatefile.WriteJSON(filepath.Join(dir, "credentials"), cred); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removeLANSlotFiles(dir string) error {
	for _, name := range []string{"state.json", "credentials", "bootstrap", bindAttemptFile, claudewake.FileName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func retireReplacedLANWorkspace(ctx context.Context, root, room string, attempt lanJoinAttempt) error {
	paths, err := statePaths(root)
	if err != nil {
		return err
	}
	for _, path := range paths {
		var state State
		if err := readPrivate(path, &state); err != nil {
			return err
		}
		if state.Room != room || state.BindID == attempt.Credentials.BindID {
			continue
		}
		if state.LAN == nil || state.BindID != attempt.PreviousBindID {
			return errors.New("a newer LAN workspace binding exists; explicit replacement cannot remove it")
		}
		dir := filepath.Dir(path)
		err := func() error {
			release, err := lockSlot(ctx, dir)
			if err != nil {
				return err
			}
			defer release()
			var current State
			if err := readPrivate(path, &current); errors.Is(err, os.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			if current.BindID != state.BindID || current.Generation != state.Generation || current.Room != room {
				return errors.New("LAN workspace changed during explicit replacement")
			}
			if err := archiveLANWorkspaceState(root, dir, current); err != nil {
				return err
			}
			if err := forgetSession(current); err != nil {
				return err
			}
			return removeLANSlotFiles(dir)
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// Finish after the shared client store has installed the candidate, even if
// the first network reply was lost. Repeating this step is safe at each write.
func finishLANReplacement(ctx context.Context, root, dir, room string, attempt lanJoinAttempt, client *lanclient.Client) error {
	if attempt.PreviousBindID == "" || client == nil {
		return nil
	}
	meta, err := client.Metadata(ctx)
	if err != nil {
		return err
	}
	if meta.BindID != attempt.Credentials.BindID || meta.Runtime != attempt.Runtime || meta.SessionID != attempt.SessionID || !sameWorkspace(meta.Workspace, root) {
		return nil // old membership was not replaced
	}
	path := filepath.Join(dir, "join-attempt.json")
	var previous lanJoinAttempt
	if err := privatefile.ReadJSON(path, maxPrivateFileBytes, &previous); err == nil {
		if previous.Credentials.BindID != attempt.Credentials.BindID {
			if previous.Credentials.BindID != attempt.PreviousBindID {
				return errors.New("LAN attempt changed during explicit replacement")
			}
			if err := archiveLANAttempt(root, previous); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := retireReplacedLANWorkspace(ctx, root, room, attempt); err != nil {
		return err
	}
	if err := privatefile.WriteJSON(path, attempt); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, lanReplacementAttemptFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// A refused proof/ownership check may leave only an unused workspace
// credential. Remove it only after proving that neither the shared record nor
// its reservation installed that candidate. Reserved candidates keep their
// exact retry identity even if the following disk write was interrupted.
func cleanupUnusedLANReplacement(ctx context.Context, dir string, attempt lanJoinAttempt, client *lanclient.Client) error {
	if attempt.PreviousBindID == "" || client == nil {
		return nil
	}
	meta, err := client.Metadata(ctx)
	if err != nil {
		return err
	}
	if meta.BindID == attempt.Credentials.BindID {
		return nil
	}
	if meta.BindID != attempt.PreviousBindID {
		return errors.New("LAN replacement identity changed; keep its original recovery attempt")
	}
	identities, err := nativeidentity.Open()
	if err != nil {
		return err
	}
	claim := nativeidentity.Claim{Runtime: attempt.Runtime, SessionID: attempt.SessionID, Association: nativeidentity.Remote(meta.Invite.HostPin, meta.Invite.RoomID), BindID: attempt.Credentials.BindID}
	if err := identities.Check(ctx, claim); err == nil {
		return nil
	} else if !errors.Is(err, nativeidentity.ErrUnowned) {
		return err
	}
	path := filepath.Join(dir, lanReplacementAttemptFile)
	var current lanJoinAttempt
	if err := privatefile.ReadJSON(path, maxPrivateFileBytes, &current); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, attempt) {
		return errors.New("LAN replacement attempt changed before unused-credential cleanup")
	}
	return os.Remove(path)
}
