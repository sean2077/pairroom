package relayclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseclient"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

type enterpriseBindAttempt struct {
	Version            int                 `json:"version"`
	Route              EnterpriseTransport `json:"route"`
	Workspace          string              `json:"workspace"`
	Runtime            model.RuntimeKind   `json:"runtime"`
	SessionID          string              `json:"session_id"`
	Slot               model.ActorID       `json:"slot"`
	ObservedGeneration uint64              `json:"observed_generation"`
	Replace            bool                `json:"replace"`
	Credentials        credentials         `json:"credentials"`
	PreviousBindID     string              `json:"previous_bind_id,omitempty"`
	DetachedAt         time.Time           `json:"detached_at,omitzero"`
}

func enterpriseClaim(state State) nativeidentity.Claim {
	return nativeidentity.Claim{Runtime: state.Runtime, SessionID: state.SessionID, Association: nativeidentity.Enterprise(state.Enterprise.Origin, state.Enterprise.DeploymentID, state.Enterprise.RoomID), BindID: state.BindID, Generation: state.Generation}
}

func bindEnterprise(ctx context.Context, root string, o options, out io.Writer) error {
	if o.create || o.endpoint != "" || o.share != "" || o.room == "" {
		return errors.New("enterprise bind requires an existing --room and no local Service/create selectors")
	}
	kind := callerRuntime(o)
	session, err := requireSessionID(kind)
	if err != nil {
		return err
	}
	if err := installed(root, kind); err != nil {
		return err
	}
	if o.slot != "" && !model.ActorID(o.slot).ValidParticipant() {
		return errors.New("enterprise bind requires --slot 1 or 2")
	}
	if o.id != "" && (!safePart(o.id) || len(o.id) > 128) {
		return errors.New("enterprise bind --id must be a bounded stable attempt ID")
	}
	serverRoom := o.room
	var previous State
	if strings.HasPrefix(o.room, "ent_") && o.slot != "" {
		if err := readPrivate(filepath.Join(root, ".pairroom", "rooms", o.room, "slots", o.slot, "state.json"), &previous); err != nil {
			return err
		}
		if !validEnterpriseRoute(previous) || !matchesEnterpriseSelection(previous, o) {
			return errors.New("enterprise binding route changed; inspect its original private record")
		}
		inheritEnterpriseSelection(previous, &o)
		serverRoom = previous.Enterprise.RoomID
	}
	if o.enterpriseOrigin == "" || o.enterpriseDeployment == "" {
		return errors.New("new enterprise binding requires --enterprise-service and --deployment")
	}
	if o.enterpriseCA != "" {
		o.enterpriseCA, err = filepath.Abs(o.enterpriseCA)
		if err != nil {
			return err
		}
	}
	parent, err := enterpriseclient.New(enterpriseclient.Config{Origin: o.enterpriseOrigin, DeploymentID: o.enterpriseDeployment, Directory: o.enterpriseDirectory, CAFile: o.enterpriseCA})
	if err != nil {
		return err
	}
	defer parent.Close()
	routing := enterpriseRoutingID(o.enterpriseOrigin, o.enterpriseDeployment, serverRoom)
	if previous.BindID == "" && o.slot != "" {
		err := readPrivate(filepath.Join(root, ".pairroom", "rooms", routing, "slots", o.slot, "state.json"), &previous)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (!validEnterpriseRoute(previous) || previous.Enterprise.Origin != o.enterpriseOrigin || previous.Enterprise.DeploymentID != o.enterpriseDeployment || previous.Enterprise.RoomID != serverRoom) {
			return errors.New("selected enterprise slot has another local route")
		}
	}
	if previous.BindID != "" && !o.replace {
		if previous.Runtime != kind || previous.SessionID != session {
			return errors.New("slot is associated with another native session; replacement requires --replace and Owner approval")
		}
		if !matchesEnterpriseSelection(previous, o) {
			return errors.New("enterprise selection differs from the original binding")
		}
		return resumeEnterpriseState(ctx, root, previous, o.renewCredential, out)
	}
	identities, err := nativeidentity.Open()
	if err != nil {
		return err
	}
	probe := nativeidentity.Claim{Runtime: kind, SessionID: session, Association: nativeidentity.Enterprise(o.enterpriseOrigin, o.enterpriseDeployment, serverRoom), BindID: "preflight"}
	if err := identities.Available(ctx, probe); err != nil {
		return err
	}
	attemptDir, err := secureLANJoinDir(root, "enterprise-joins", routing, relay.Digest(string(kind) + "\x00" + session)[:32])
	if err != nil {
		return err
	}
	unlock, err := lockSlot(ctx, attemptDir)
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(attemptDir, enterpriseAttemptFilename(o))
	var attempt enterpriseBindAttempt
	if err := privatefile.ReadJSON(path, maxPrivateFileBytes, &attempt); errors.Is(err, os.ErrNotExist) {
		if o.renewCredential {
			return errors.New("credential renewal requires this session's original confirmed Binding record")
		}
		view, err := parent.ReadBindingContext(ctx, serverRoom)
		if err != nil {
			return err
		}
		slot := model.ActorID(o.slot)
		if slot == "" {
			if o.replace {
				return errors.New("replacement requires an explicit --slot")
			}
			for _, candidate := range model.SlotActors() {
				if view.Slots[candidate].Vacant {
					slot = candidate
					break
				}
			}
		}
		if !slot.ValidParticipant() {
			return errors.New("no vacant enterprise slot; inspect both slots before explicit replacement")
		}
		selected := view.Slots[slot]
		if selected.Vacant == o.replace {
			return errors.New("selected slot occupancy changed; use a vacant slot or explicit --replace for an occupied slot")
		}
		if previous.BindID != "" && previous.Slot != slot {
			return errors.New("replacement cannot silently change the original slot")
		}
		id, err := relay.RandomID()
		if err != nil {
			return err
		}
		localSecret, err := relay.RandomID()
		if err != nil {
			return err
		}
		status, err := parent.Status()
		if err != nil {
			return err
		}
		attempt = enterpriseBindAttempt{Version: 1, Route: EnterpriseTransport{Origin: o.enterpriseOrigin, DeploymentID: o.enterpriseDeployment, UserID: status.UserID, DeviceID: status.DeviceID, RoomID: serverRoom, RecordID: "binding-" + id, Directory: parent.Directory(), CAFile: o.enterpriseCA}, Workspace: root, Runtime: kind, SessionID: session, Slot: slot, ObservedGeneration: selected.HeadGeneration, Replace: o.replace, Credentials: credentials{BindID: "binding-" + id, Secret: localSecret}, PreviousBindID: previous.BindID}
		pending := probe
		pending.BindID = attempt.Credentials.BindID
		effect := func() error {
			if err := privatefile.WriteJSON(path, attempt); err != nil {
				return err
			}
			_, err := parent.PrepareBinding(ctx, enterpriseclient.BindingOptions{RecordID: attempt.Route.RecordID, RoomID: serverRoom, Workspace: root, Auth: relay.Auth{Slot: slot, Runtime: kind, BindID: attempt.Credentials.BindID, SessionID: session, Secret: localSecret}, ObservedGeneration: attempt.ObservedGeneration, Replace: o.replace, ParkEnabled: true})
			return err
		}
		if previous.BindID != "" && previous.SessionID == session && previous.Runtime == kind {
			old := enterpriseClaim(previous)
			err = identities.ReplacePending(ctx, &old, pending, effect)
		} else {
			err = identities.Commit(ctx, nil, &pending, effect)
		}
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if !attempt.DetachedAt.IsZero() {
		return errors.New("this enterprise attempt was detached locally; use a new explicit --id for another admission")
	}
	if attempt.Version != 1 || attempt.Route.Origin != o.enterpriseOrigin || attempt.Route.DeploymentID != o.enterpriseDeployment || attempt.Route.RoomID != serverRoom || !sameWorkspace(attempt.Route.Directory, parent.Directory()) || attempt.Route.CAFile != o.enterpriseCA || attempt.Workspace != root || attempt.Runtime != kind || attempt.SessionID != session || !attempt.Slot.ValidParticipant() || o.slot != "" && string(attempt.Slot) != o.slot || attempt.Replace != o.replace || attempt.Credentials.BindID != attempt.Route.RecordID || attempt.Credentials.Secret == "" || attempt.ObservedGeneration == 0 {
		return errors.New("enterprise bind attempt differs from its original session, route or intent")
	}
	// A crash may have saved the attempt before the separate protected client
	// record. Recreate only that exact local candidate, never allocate a new key.
	binding, err := parent.PrepareBinding(ctx, enterpriseclient.BindingOptions{RecordID: attempt.Route.RecordID, RoomID: serverRoom, Workspace: root, Auth: relay.Auth{Slot: attempt.Slot, Runtime: kind, BindID: attempt.Credentials.BindID, SessionID: session, Secret: attempt.Credentials.Secret}, ObservedGeneration: attempt.ObservedGeneration, Replace: attempt.Replace, ParkEnabled: true})
	if err != nil {
		return err
	}
	if o.renewCredential {
		meta, err := binding.LocalMetadata(ctx)
		if err != nil {
			return err
		}
		if meta.Status != "active" || meta.Generation == 0 {
			return errors.New("resolve the original binding completion before explicit credential renewal")
		}
	}
	meta, err := binding.ResumeBinding(ctx)
	if err != nil {
		return err
	}
	if meta.Status != "active" {
		approval, err := binding.ApprovalURL(ctx)
		if err != nil {
			return err
		}
		return writeJSON(out, map[string]any{"status": meta.Status, "room": serverRoom, "slot": meta.Slot, "request_id": meta.RequestID, "approval_url": approval, "notice": "The Room Owner must explicitly approve this exact request. Then repeat the same bind command in this native session; this endpoint alone can complete it. Once completed, this Binding can read the Room's full retained history, including messages from before admission, referenced attachments/images and same-Room quoted evidence."})
	}
	if meta.BindID != attempt.Credentials.BindID || meta.UserID != attempt.Route.UserID || meta.DeviceID != attempt.Route.DeviceID || meta.Slot != attempt.Slot || meta.Runtime != kind || meta.Generation == 0 {
		return errors.New("enterprise completion changed the original endpoint identity")
	}
	slotDir, err := secureLANJoinDir(root, "rooms", routing, "slots", string(meta.Slot))
	if err != nil {
		return err
	}
	releaseSlot, err := lockSlot(ctx, slotDir)
	if err != nil {
		return err
	}
	defer releaseSlot()
	var current State
	if err := readPrivate(filepath.Join(slotDir, "state.json"), &current); err == nil {
		if current.BindID == meta.BindID {
			if !validEnterpriseRoute(current) || current.Generation != meta.Generation || current.SessionID != session || current.Runtime != kind {
				return errors.New("confirmed enterprise workspace identity changed")
			}
			previous = current
		} else if current.BindID != attempt.PreviousBindID {
			return errors.New("another Binding replaced this local slot; inspect before recovery")
		} else {
			previous = current
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	claim := probe
	claim.BindID, claim.Generation = meta.BindID, meta.Generation
	if err := identities.Reserve(ctx, claim); err != nil {
		return err
	}
	auth := relay.Auth{Slot: meta.Slot, Runtime: kind, BindID: meta.BindID, Generation: meta.Generation, SessionID: session, Secret: attempt.Credentials.Secret}
	if o.renewCredential {
		if _, err := binding.RenewCredential(ctx, auth); err != nil {
			return err
		}
	}
	view, err := binding.Context(ctx, auth)
	if err != nil {
		return err
	}
	route := attempt.Route
	route.AgentID = meta.AgentID
	state := State{Schema: 4, Enterprise: &route, Room: routing, Slot: meta.Slot, Runtime: kind, Workspace: root, BindID: meta.BindID, Generation: meta.Generation, SessionID: session, DynamicBindings: true}
	if previous.BindID == state.BindID {
		state = previous
	} else if previous.BindID != "" {
		if err := archiveEnterpriseState(root, previous); err != nil {
			return err
		}
		state.inheritResponseContext(previous)
	}
	if err := retainResponseContext(ctx, &state, view.PeerGeneration); err != nil {
		return err
	}
	if pid, name, ok := harnessAncestor(); ok && harnessRuntimes[name] == kind {
		state.HarnessPID, state.HarnessName = pid, name
	}
	if _, err := ignoreWorkspace(root); err != nil {
		return err
	}
	if err := identities.Commit(ctx, nil, &claim, func() error {
		if err := privatefile.WriteJSON(filepath.Join(slotDir, "credentials"), attempt.Credentials); err != nil {
			return err
		}
		return privatefile.WriteJSON(filepath.Join(slotDir, "state.json"), state)
	}); err != nil {
		return err
	}
	if previous.BindID != "" && previous.BindID != state.BindID && (previous.Runtime != state.Runtime || previous.SessionID != state.SessionID) {
		if err := identities.Release(ctx, enterpriseClaim(previous)); err != nil && !errors.Is(err, nativeidentity.ErrOwned) {
			return err
		}
	}
	if err := rememberSession(state); err != nil {
		return fmt.Errorf("enterprise admission confirmed but session locator needs repair: %w; repeat the same bind command", err)
	}
	return writeEnterpriseReady(out, state, view)
}

func enterpriseAttemptFilename(o options) string {
	name := "bind-attempt"
	if o.replace {
		name = "replace-attempt"
	}
	if o.id != "" {
		name += "-" + relay.Digest(o.id)[:24]
	}
	return name + ".json"
}

func archiveEnterpriseState(root string, state State) error {
	if !validEnterpriseRoute(state) || !safePart(state.BindID) || !sameWorkspace(root, state.Workspace) {
		return errors.New("invalid retired enterprise workspace identity")
	}
	dir, err := secureLANJoinDir(root, "retired", state.BindID)
	if err != nil {
		return err
	}
	if err := privatefile.WriteJSON(filepath.Join(dir, "state.json"), state); err != nil {
		return err
	}
	var archived credentials
	if err := privatefile.ReadJSON(filepath.Join(dir, "credentials"), maxPrivateFileBytes, &archived); err == nil {
		if archived.BindID != state.BindID {
			return errors.New("retired enterprise credential belongs to another binding")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var original credentials
	if _, err := readBindingFile(filepath.Join(root, ".pairroom", "rooms", state.Room, "slots", string(state.Slot), "credentials"), &original, readPrivate, true); err != nil {
		return err
	}
	if original.BindID != state.BindID {
		return errors.New("retired enterprise credential identity changed")
	}
	return privatefile.WriteJSON(filepath.Join(dir, "credentials"), original)
}
func resumeEnterpriseState(ctx context.Context, root string, state State, renew bool, out io.Writer) error {
	dir := filepath.Join(root, ".pairroom", "rooms", state.Room, "slots", string(state.Slot))
	unlock, err := lockSlot(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	client, err := load(dir)
	if err != nil {
		return err
	}
	defer client.close()
	if client.State.BindID != state.BindID || client.Enterprise == nil {
		return relay.ErrAuth
	}
	if renew {
		if _, err := client.Enterprise.RenewCredential(ctx, client.localAuth()); err != nil {
			return err
		}
	}
	view, err := client.Enterprise.Context(ctx, client.localAuth())
	if err != nil {
		return err
	}
	next := client.State
	if err := retainResponseContext(ctx, &next, view.PeerGeneration); err != nil {
		return err
	}
	if err := client.persist(next); err != nil {
		return err
	}
	return writeEnterpriseReady(out, client.State, view)
}
func writeEnterpriseReady(out io.Writer, state State, view enterpriseclient.RelayContext) error {
	result := map[string]any{"status": "accepted", "room": state.Enterprise.RoomID, "service": state.Enterprise.Origin, "deployment": state.Enterprise.DeploymentID, "binding": view.Binding, "bootstrap": view.Bootstrap, "collaboration": view.Collaboration, "peer_generation": view.PeerGeneration, "notice": "Enterprise Native relay is bound to this original native session. Use wait or the approved hooks to receive; credential renewal is explicit foreground work. This Binding can read the Room's full retained history, including messages from before admission, referenced attachments/images and same-Room quoted evidence."}
	if state.AutomaticPaused {
		result["automatic_publication_notice"] = automaticPauseNotice(view.PeerGeneration)
	}
	return writeJSON(out, result)
}
