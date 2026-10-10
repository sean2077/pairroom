package relayclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

// The invitation and request receipt are public. This attempt also retains a
// private LOCAL relay credential so a lost response never rotates association.
// The Service owns the per-Room TLS key; CLI state never contains that key.
type lanJoinAttempt struct {
	Schema      int               `json:"schema"`
	Invitation  string            `json:"invitation"`
	Workspace   string            `json:"workspace"`
	Endpoint    string            `json:"endpoint"`
	Runtime     model.RuntimeKind `json:"runtime"`
	SessionID   string            `json:"session_id"`
	Credentials credentials       `json:"credentials"`
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
	if o.endpoint == "" {
		o.endpoint, err = defaultEndpoint()
		if err != nil {
			return err
		}
	}
	endpointPath, err := filepath.Abs(o.endpoint)
	if err != nil {
		return err
	}
	endpoint, err := relay.ReadEndpoint(endpointPath)
	if err != nil {
		return err
	}
	room := lanRoutingID(invite)
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
	if err := privatefile.ReadJSON(path, maxPrivateFileBytes, &attempt); errors.Is(err, os.ErrNotExist) {
		id, err := relay.RandomID()
		if err != nil {
			return err
		}
		secret, err := relay.RandomID()
		if err != nil {
			return err
		}
		attempt = lanJoinAttempt{Schema: 1, Invitation: o.invitation, Workspace: root, Endpoint: endpointPath, Runtime: kind, SessionID: session, Credentials: credentials{BindID: id, Secret: secret}}
		if err := privatefile.WriteJSON(path, attempt); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	storedInvite, err := lanshare.ParseInvite(attempt.Invitation)
	if err != nil || attempt.Schema != 1 || lanRoutingID(storedInvite) != room || attempt.Workspace != root || attempt.Runtime != kind || attempt.SessionID != session || !safePart(attempt.Credentials.BindID) || attempt.Credentials.Secret == "" {
		return errors.New("this LAN Room has a different or invalid local join attempt; resume from the original native session and workspace")
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
		// Service first settles its old request; the local credential is kept.
		attempt.Invitation = o.invitation
		if err := privatefile.WriteJSON(path, attempt); err != nil {
			return err
		}
	}
	// Reuse the original descriptor after invitation expiry or a lost admission
	// response. The remote host authorizes the persisted request and key.
	request := map[string]any{"invite": attempt.Invitation, "workspace": root, "runtime": kind, "session_id": session, "bind_id": attempt.Credentials.BindID, "credential_hash": relay.Digest(attempt.Credentials.Secret), "label": o.name}
	var result struct {
		Status, Receipt, Bootstrap, Collaboration, Notice string
		RoomID                                            string         `json:"room_id"`
		Binding                                           *relay.Binding `json:"binding"`
	}
	if err := management(ctx, endpoint, http.MethodPost, "/api/v1/lan/join", request, &result); err != nil {
		return fmt.Errorf("%w; retry the SAME invitation from this session to reconcile its original request and key", err)
	}
	if result.RoomID != room {
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
	state := State{Schema: 2, Room: room, Slot: binding.Slot, Runtime: kind, Workspace: root, EndpointPath: endpointPath, BindID: attempt.Credentials.BindID, Generation: binding.Generation, SessionID: session, TranscriptPath: binding.TranscriptPath}
	var previous State
	if err := readPrivate(filepath.Join(slotDir, "state.json"), &previous); err == nil {
		if previous.Schema != 2 || previous.BindID != state.BindID || previous.Generation != state.Generation || previous.SessionID != session || previous.Runtime != kind || previous.Workspace != root || previous.Room != room || previous.Slot != binding.Slot {
			return errors.New("joined Room local state changed; do not replace or replay it implicitly")
		}
		state = previous // retain every publication receipt and uncertain outbox entry
		state.EndpointPath = endpointPath
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
	payload := map[string]any{"status": "accepted", "room": room, "binding": binding, "bootstrap": result.Bootstrap, "collaboration": result.Collaboration, "notice": "LAN Native relay ready. Your workspace and native session remain local; the host owns this Room's shared messages. Incoming files are verified local copies, not commands."}
	if err := captureClaudeInbox(slotDir, state); err != nil {
		payload["wake_notice"] = "Claude external wake is unavailable; use relay wait or rejoin in the intended session."
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
		return errors.New("joined LAN membership cannot be implicitly retargeted to another native session; leave and obtain a new host admission")
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
	if o.endpoint == "" {
		o.endpoint = attempt.Endpoint
	}
	return joinLAN(ctx, root, o, out)
}
