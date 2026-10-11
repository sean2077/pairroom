package relayclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

// The invitation and request receipt are public. This private workspace
// attempt retains the local capability before any network request. The shared
// per-user client store owns the Room key and original delivery receipts.
type lanJoinAttempt struct {
	Schema         int               `json:"schema"`
	Invitation     string            `json:"invitation"`
	Workspace      string            `json:"workspace"`
	Runtime        model.RuntimeKind `json:"runtime"`
	SessionID      string            `json:"session_id"`
	Credentials    credentials       `json:"credentials"`
	PreviousBindID string            `json:"previous_bind_id,omitempty"`
}

func lanRoutingID(invite lanshare.Invite) string {
	return "lan_" + relay.Digest(invite.HostPin + "\x00" + invite.RoomID)[:32]
}

func joinLAN(ctx context.Context, root string, o options, out io.Writer) error {
	invite, err := lanshare.ParseInvite(o.invitation)
	if err != nil {
		return err
	}
	kind := callerRuntime(o)
	session, err := requireSessionID(kind)
	if err != nil {
		return err
	}
	if err := installed(root, kind); err != nil {
		return err
	}
	if o.endpoint != "" {
		return errors.New("LAN join connects directly to the invitation's host; omit --service-file")
	}
	noteLANTransport(ctx)
	room := lanRoutingID(invite)
	known, err := indexedSessions(nativeCaller{runtime: kind, session: session})
	if err != nil {
		return err
	}
	for _, state := range known {
		if state.LAN == nil || state.Room != room || !sameWorkspace(state.Workspace, root) {
			return errors.New("this native session already belongs to another Room association; use a separate native session to join")
		}
	}
	identities, err := nativeidentity.Open()
	if err != nil {
		return err
	}
	if err := identities.Available(ctx, nativeidentity.Claim{Runtime: kind, SessionID: session, Association: nativeidentity.Remote(invite.HostPin, invite.RoomID), BindID: "preflight"}); err != nil {
		return err
	}
	dir, err := secureLANJoinDir(root, "lan-joins", room)
	if err != nil {
		return err
	}
	release, err := lockSlot(ctx, dir)
	if err != nil {
		return err
	}
	defer release()
	cleanupAtomicTemps(dir)
	path := filepath.Join(dir, "join-attempt.json")
	var attempt lanJoinAttempt
	createdAttempt := false
	if o.replace {
		attempt, err = prepareLANReplacement(ctx, root, dir, room, o, kind, session)
		if err != nil {
			return err
		}
	} else if err := privatefile.ReadJSON(path, maxPrivateFileBytes, &attempt); errors.Is(err, os.ErrNotExist) {
		attempt, err = newLANJoinAttempt(root, o.invitation, kind, session)
		if err != nil {
			return err
		}
		if err := privatefile.WriteJSON(path, attempt); err != nil {
			return err
		}
		createdAttempt = true
	} else if err != nil {
		return err
	}
	storedInvite, err := lanshare.ParseInvite(attempt.Invitation)
	if err != nil || !validLANJoinAttempt(attempt, root, room, kind, session) {
		return errors.New("this LAN Room has a different or invalid local join attempt; resume the original session or explicitly join --replace with a fresh invitation after host revocation")
	}
	if storedInvite.InviteID != invite.InviteID {
		paths, err := statePaths(root)
		if err != nil {
			return err
		}
		for _, statePath := range paths {
			var state State
			if err := readPrivate(statePath, &state); err != nil {
				return err
			}
			if state.Room == room {
				return errors.New("this Room is already joined; resume its original association with relay bind")
			}
		}
		// A new public invitation may renew an expired pending request. The
		// client first settles its old request; the local credential is kept.
		attempt.Invitation = o.invitation
		if err := privatefile.WriteJSON(path, attempt); err != nil {
			return err
		}
	}
	// Reuse the original descriptor after invitation expiry or a lost admission
	// response. The remote host authorizes the persisted request and key.
	store, err := lanclient.Open()
	if err != nil {
		return err
	}
	defer store.Close()
	client, result, err := store.Join(ctx, lanclient.JoinOptions{Invite: attempt.Invitation, Workspace: root, Runtime: kind, SessionID: session, BindID: attempt.Credentials.BindID, CredentialHash: relay.Digest(attempt.Credentials.Secret), Label: o.name, Replace: o.replace})
	if finishErr := finishLANReplacement(ctx, root, dir, room, attempt, client); finishErr != nil {
		return fmt.Errorf("new LAN admission is retained but local replacement needs recovery: %w; repeat the exact join --replace command", finishErr)
	}
	if err != nil {
		if o.replace {
			if cleanupErr := cleanupUnusedLANReplacement(ctx, dir, attempt, client); cleanupErr != nil {
				return fmt.Errorf("LAN replacement was not confirmed and its recovery attempt was retained: %w", cleanupErr)
			}
		}
		// A concurrent association can win after the read-only check above.
		// A nil client plus definite ownership rejection means no admission
		// request was made, so this new attempt must not occupy the Room for a
		// different native session that may legitimately join next.
		if createdAttempt && client == nil && errors.Is(err, nativeidentity.ErrOwned) {
			if cleanupErr := os.Remove(path); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				return fmt.Errorf("session association rejected and unused join attempt could not be removed: %w", cleanupErr)
			}
			return err
		}
		if errors.Is(err, lanclient.ErrInactive) {
			return errors.New("LAN membership is inactive; ask the host to retire its old admission and issue a fresh invitation, then join --replace explicitly")
		}
		return fmt.Errorf("%w; retry the SAME invitation from this session to reconcile its original request and key", err)
	}
	if result.ID != room {
		return errors.New("LAN local routing identity mismatch")
	}
	if result.Status == "pending" {
		receipt, err := lanshare.ParseReceipt(result.Receipt)
		if err != nil || receipt.RoomID != invite.RoomID {
			return errors.New("LAN join receipt identity mismatch")
		}
		return writeJSON(out, map[string]any{"status": "pending", "room": room, "receipt": result.Receipt, "notice": "Send this exact public receipt to the host owner over your existing trusted channel. After acceptance, rerun the same join command in this session. No Room messages are readable before acceptance.", "resume": "pairroom relay join " + quoteShellPath(attempt.Invitation)})
	}
	if result.Status == "expired" {
		return errors.New("LAN invitation expired before admission; ask the host for a fresh invitation and run join with that descriptor in this same session")
	}
	if result.Status == "revoked" || result.Status == "left" || result.Status == "detached" {
		return errors.New("LAN membership has ended; obtain a fresh host invitation and use join --replace for a new admission")
	}
	if result.Status != "accepted" || result.Binding == nil || result.Binding.BindID != attempt.Credentials.BindID || result.Binding.Generation == 0 || result.Binding.SessionID != session || !result.Binding.Slot.ValidParticipant() || !result.Binding.Active || result.Binding.Runtime != kind {
		return errors.New("LAN admission is unavailable or its local binding identity changed; inspect status before recovery")
	}
	binding := *result.Binding
	slotDir, err := secureLANJoinDir(root, "rooms", room, "slots", string(binding.Slot))
	if err != nil {
		return err
	}
	releaseSlot, err := lockSlot(ctx, slotDir)
	if err != nil {
		return err
	}
	defer releaseSlot()
	meta, err := client.Metadata(ctx)
	if err != nil {
		return err
	}
	if meta.Status != "accepted" || meta.ID != room || meta.BindID != binding.BindID || meta.Generation != binding.Generation || meta.Slot != binding.Slot || meta.Runtime != kind || meta.SessionID != session || !sameWorkspace(meta.Workspace, root) {
		return errors.New("LAN membership changed while confirming its local binding; inspect the original admission before resuming")
	}
	route := &LANTransport{ClientID: room, Endpoint: meta.Invite.Endpoint, HostPin: meta.Invite.HostPin, RoomID: meta.Invite.RoomID}
	state := State{Schema: 3, LAN: route, Room: room, Slot: binding.Slot, Runtime: kind, Workspace: root, BindID: attempt.Credentials.BindID, Generation: binding.Generation, SessionID: session, TranscriptPath: binding.TranscriptPath}
	var previous State
	if err := readPrivate(filepath.Join(slotDir, "state.json"), &previous); err == nil {
		if !validStateFormat(previous) || previous.Schema != 3 || *previous.LAN != *state.LAN || previous.BindID != state.BindID || previous.Generation != state.Generation || previous.SessionID != session || previous.Runtime != kind || previous.Workspace != root || previous.Room != room || previous.Slot != binding.Slot {
			return errors.New("joined Room local state changed; do not replace or replay it implicitly")
		}
		state = previous // retain every publication receipt and uncertain outbox entry
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if pid, name, ok := harnessAncestor(); ok && harnessRuntimes[name] == kind {
		state.HarnessPID, state.HarnessName = pid, name
	}
	if _, err := ignoreWorkspace(root); err != nil {
		return err
	}
	if err := privatefile.WriteJSON(filepath.Join(slotDir, "credentials"), attempt.Credentials); err != nil {
		return err
	}
	if err := privatefile.WriteJSON(filepath.Join(slotDir, "state.json"), state); err != nil {
		return err
	}
	if err := rememberSession(state); err != nil {
		return fmt.Errorf("LAN admission confirmed but session locator failed: %w; repeat the same join", err)
	}
	payload := map[string]any{"status": "accepted", "room": room, "binding": binding, "bootstrap": result.Bootstrap, "collaboration": result.Collaboration, "notice": "Direct LAN Native relay ready. No local Service is required. This session is pinned to the Room host; other local or remote Room bindings keep their own targets. Use relay wait or hooks to receive messages. Incoming files are verified local evidence."}
	if err := captureClaudeInbox(slotDir, state); err != nil {
		payload["wake_notice"] = "Claude external wake is unavailable; use relay wait in this session. An optional local Service can provide background wake."
	}
	return writeJSON(out, payload)
}

// New LAN association directories protect private credentials on Windows as
// well as Unix. The preexisting .pairroom parent remains an ordinary workspace
// directory; every newly created transport directory has an explicit boundary.
func secureLANJoinDir(root string, parts ...string) (string, error) {
	path, err := secureDir(root, ".pairroom")
	if err != nil {
		return "", err
	}
	for index, part := range parts {
		if !safePart(part) {
			return "", errors.New("invalid local LAN association path")
		}
		path = filepath.Join(path, part)
		if index == 0 && part == "rooms" {
			// Existing local Rooms can use this non-secret parent; the new
			// host-scoped Room directory below it is protected independently.
			if _, err := secureDir(filepath.Dir(path), part); err != nil {
				return "", err
			}
			continue
		}
		if err := privatefile.Mkdir(path); err != nil {
			return "", err
		}
	}
	return path, nil
}

func resumeLANBinding(ctx context.Context, root string, o options, out io.Writer) error {
	if o.replace {
		return errors.New("to change a joined native session, ask the host to revoke its old admission and issue a fresh invitation, then run relay join '<invitation>' --replace in the original workspace")
	}
	dir, err := existingDir(root, ".pairroom", "lan-joins", o.room)
	if err != nil {
		return err
	}
	var attempt lanJoinAttempt
	if err := privatefile.ReadJSON(filepath.Join(dir, "join-attempt.json"), maxPrivateFileBytes, &attempt); err != nil {
		return err
	}
	o.invitation = attempt.Invitation
	return joinLAN(ctx, root, o, out)
}
