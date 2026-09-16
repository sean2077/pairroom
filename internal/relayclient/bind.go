package relayclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// A private, bounded write-ahead record. It is not an active binding and is
// deliberately not named state.json, so hook/foreground discovery ignores it.
// Keep it through ambiguous HTTP results and interrupted local promotion.
const bindAttemptFile = "bind-attempt.json"

type bindAttempt struct {
	State       State       `json:"state"`
	Credentials credentials `json:"credentials"`
	Replace     bool        `json:"replace"`
}

func bind(ctx context.Context, root string, o options, out io.Writer) (resultErr error) {
	slot := model.ActorID(o.slot)
	if o.slot != "" && !slot.ValidParticipant() {
		return errors.New("bind requires --slot 1|2")
	}
	if o.create && o.room != "" {
		return errors.New("choose --create or --room, not both")
	}
	if !o.create && o.room != "" && !safePart(o.room) {
		return errors.New("invalid --room value")
	}
	if o.create {
		// An explicit slot is not evidence of an in-session caller. Reject before
		// even discovering Service defaults, and certainly before provisioning.
		if _, err := requireSessionID(callerRuntime(o)); err != nil {
			return err
		}
	}
	if o.endpoint == "" {
		var err error
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
	var snapshot serviceSnapshot
	if err := management(ctx, endpoint, http.MethodGet, "/api/v1/service", nil, &snapshot); err != nil {
		return err
	}
	created := false
	defer func() {
		if created && resultErr != nil {
			resultErr = fmt.Errorf("Room %s was created but binding failed: %w; retry setup with %s (use --replace only to revoke another binding). Do not repeat --create", o.room, resultErr, bindCommand(root, endpointPath, o.room, slot))
		}
	}()
	if o.create {
		o, slot, err = prepareNativeCreation(ctx, endpoint, root, o, slot)
		if err != nil {
			return err
		}
		o.room, err = createNativeRoom(ctx, endpoint, root, o, slot)
		if err != nil {
			return err
		}
		created = true
		if err := management(ctx, endpoint, http.MethodGet, "/api/v1/service", nil, &snapshot); err != nil {
			return err
		}
	} else if o.room == "" {
		o.room, err = resolveNativeRoom(snapshot, root)
		if err != nil {
			return err
		}
	}
	target, ok := snapshot.findRoom(o.room)
	if !ok || target.HostMode != model.HostNative {
		return errors.New("bind requires a native-hosted Room")
	}
	if slot == "" {
		slot, err = resolveSlotForRoom(target, callerRuntime(o))
		if err != nil {
			return err
		}
	}
	o.slot = string(slot)
	kind := target.Agents[slot].Runtime
	if own := callerRuntime(o); own != "" && own != kind {
		return errors.New("this session's runtime does not match the selected Room slot")
	}
	project := ""
	for _, p := range snapshot.Projects {
		if p.ID == target.ProjectID {
			project = p.Root
		}
	}
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil || canonical != root {
		return errors.New("bind must run in the Room's canonical project workspace")
	}
	session, err := requireSessionID(kind)
	if err != nil {
		return err
	}
	if err := installed(root, kind); err != nil {
		return err
	}
	dir, err := secureDir(root, ".pairroom", "rooms", o.room, "slots", o.slot)
	if err != nil {
		return err
	}
	release, err := lockSlot(ctx, dir)
	if err != nil {
		return err
	}
	defer release()
	cleanupAtomicTemps(dir)

	attempt, staged, err := prepareBindAttempt(dir, root, endpointPath, o, slot, kind, session)
	if err != nil {
		return err
	}
	if pid, name, ok := harnessAncestor(); ok && harnessRuntimes[name] == kind {
		attempt.State.HarnessPID, attempt.State.HarnessName = pid, name
	}
	attempt.State.EndpointPath = endpointPath
	if err := ignoreWorkspace(root); err != nil {
		return err
	}
	if staged {
		if err := relay.AtomicJSON(filepath.Join(dir, bindAttemptFile), attempt); err != nil {
			return err
		}
	}
	var result struct {
		Binding                          relay.Binding `json:"binding"`
		Bootstrap, Collaboration, Notice string
	}
	request := relay.BindRequest{BindID: attempt.State.BindID, CredentialHash: relay.Digest(attempt.Credentials.Secret), SessionID: session, Replace: attempt.Replace}
	if err := management(ctx, endpoint, http.MethodPost, "/api/v1/rooms/"+o.room+"/native-bindings/"+o.slot, request, &result); err != nil {
		// Never infer rollback from an HTTP failure: a store append may have
		// succeeded. The committed local files remain untouched; retry the same
		// attempt explicitly to reconcile its original ID and replacement intent.
		return fmt.Errorf("%w; binding not confirmed locally; retry bind for this Room and slot without --create or --replace before using relay", err)
	}
	if result.Binding.BindID != attempt.State.BindID || result.Binding.Generation == 0 || result.Binding.SessionID != session || result.Binding.Slot != slot || !result.Binding.Active {
		return errors.New("binding response identity mismatch; retry the same bind to reconcile")
	}
	attempt.State.Generation = result.Binding.Generation
	if staged {
		// Journal removal is last. A crash between either promotion write can
		// be repaired by replaying the same attempt without rotating generation.
		if err := relay.AtomicJSON(filepath.Join(dir, "credentials"), attempt.Credentials); err != nil {
			return err
		}
	}
	if err := relay.AtomicJSON(filepath.Join(dir, "state.json"), attempt.State); err != nil {
		return err
	}
	if staged {
		if err := os.Remove(filepath.Join(dir, bindAttemptFile)); err != nil {
			return err
		}
	}
	payload := map[string]any{"binding": result.Binding, "bootstrap": result.Bootstrap, "collaboration": result.Collaboration, "notice": result.Notice + " Added .pairroom/ to .gitignore. This session is ready to relay."}
	if created {
		payload["peer_join"] = bindCommand(root, endpointPath, o.room, peerSlot(slot))
	}
	return writeJSON(out, payload)
}

// Called under the slot lock. An attempt for another session never replaces a
// committed binding implicitly. Retrying a matching attempt retains its key,
// including its original replacement decision. A new explicit --replace always
// supersedes the attempt rather than endlessly retrying a revoked bind ID.
func prepareBindAttempt(dir, root, endpoint string, o options, slot model.ActorID, kind model.RuntimeKind, session string) (bindAttempt, bool, error) {
	var pending bindAttempt
	pendingErr := readPrivate(filepath.Join(dir, bindAttemptFile), &pending)
	matches := pendingErr == nil && pending.State.SessionID == session
	validPending := pending.State.Schema == 1 && pending.State.Room == o.room && pending.State.Slot == slot && pending.State.Runtime == kind && pending.State.Workspace == root && safePart(pending.State.BindID) && pending.Credentials.BindID == pending.State.BindID && pending.Credentials.Secret != ""
	if matches && !validPending && !o.replace {
		return bindAttempt{}, false, errors.New("invalid pending bind identity; inspect before explicitly replacing it")
	}
	if matches && validPending && !o.replace {
		// Promotion may have finished before a crash, and a subsequent hook may
		// already have advanced the publication WAL. Never restore stale counters.
		var promoted State
		var cred credentials
		if readPrivate(filepath.Join(dir, "state.json"), &promoted) == nil &&
			readPrivate(filepath.Join(dir, "credentials"), &cred) == nil &&
			promoted.Schema == 1 && promoted.BindID == pending.State.BindID &&
			promoted.Generation > 0 && promoted.Room == o.room && promoted.Slot == slot &&
			promoted.Runtime == kind && promoted.Workspace == root && promoted.SessionID == session &&
			cred.BindID == pending.Credentials.BindID && cred.Secret == pending.Credentials.Secret {
			pending.State = promoted
		}
		return pending, true, nil
	}
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) && !o.replace {
		return bindAttempt{}, false, pendingErr
	}
	var current State
	prior := readPrivate(filepath.Join(dir, "state.json"), &current)
	if prior == nil && !o.replace {
		if current.SessionID == "" {
			return bindAttempt{}, false, errors.New("incomplete binding; run bind --replace inside the intended session")
		}
		if current.SessionID != session {
			return bindAttempt{}, false, relay.ErrOccupied
		}
		if current.Schema != 1 || current.Room != o.room || current.Slot != slot || current.Runtime != kind || current.Workspace != root {
			return bindAttempt{}, false, errors.New("local binding identity mismatch")
		}
		var cred credentials
		if err := readPrivate(filepath.Join(dir, "credentials"), &cred); err != nil {
			return bindAttempt{}, false, err
		}
		if cred.BindID != current.BindID || cred.Secret == "" {
			return bindAttempt{}, false, errors.New("state/credential mismatch: inspect and --replace explicitly")
		}
		// Do not overwrite an unresolved attempt belonging to another session.
		return bindAttempt{State: current, Credentials: cred}, false, nil
	}
	if prior != nil && !errors.Is(prior, os.ErrNotExist) && !o.replace {
		return bindAttempt{}, false, prior
	}
	if pendingErr == nil && !o.replace {
		return bindAttempt{}, false, relay.ErrOccupied
	}
	id, err := relay.RandomID()
	if err != nil {
		return bindAttempt{}, false, err
	}
	secret, err := relay.RandomID()
	if err != nil {
		return bindAttempt{}, false, err
	}
	return bindAttempt{State: State{Schema: 1, Room: o.room, Slot: slot, Runtime: kind, Workspace: root, EndpointPath: endpoint, BindID: id, SessionID: session}, Credentials: credentials{BindID: id, Secret: secret}, Replace: o.replace}, true, nil
}
