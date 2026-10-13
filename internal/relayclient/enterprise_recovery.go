package relayclient

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseclient"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

// This is an explicit offline retirement, not remote cancellation. The exact
// original attempt and its uncertain phase remain in owner-private storage.
func abandonEnterprisePending(ctx context.Context, root string, o options, out io.Writer) error {
	if !safePart(o.room) || o.id != "" && (!safePart(o.id) || len(o.id) > 128) || o.slot != "" && !model.ActorID(o.slot).ValidParticipant() {
		return errors.New("invalid original enterprise attempt selectors")
	}
	caller, err := currentNativeCaller()
	if err != nil {
		return err
	}
	if caller.session == "" || o.kind != "" && string(caller.runtime) != o.kind {
		return errors.New("abandon the pending attempt from its original native session")
	}
	routing := enterpriseRoutingID(o.enterpriseOrigin, o.enterpriseDeployment, o.room)
	dir, err := existingDir(root, ".pairroom", "enterprise-joins", routing, relay.Digest(string(caller.runtime) + "\x00" + caller.session)[:32])
	if err != nil {
		return err
	}
	unlock, err := lockSlot(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(dir, enterpriseAttemptFilename(o))
	var attempt enterpriseBindAttempt
	if err := privatefile.ReadJSON(path, maxPrivateFileBytes, &attempt); err != nil {
		return err
	}
	if attempt.Version != 1 || attempt.Workspace != root || attempt.Runtime != caller.runtime || attempt.SessionID != caller.session || attempt.Route.Origin != o.enterpriseOrigin || attempt.Route.DeploymentID != o.enterpriseDeployment || attempt.Route.RoomID != o.room || attempt.Replace != o.replace || !attempt.Slot.ValidParticipant() || o.slot != "" && o.slot != string(attempt.Slot) || !safePart(attempt.Route.RecordID) || attempt.Credentials.BindID != attempt.Route.RecordID || attempt.Credentials.Secret == "" || attempt.ObservedGeneration == 0 {
		return errors.New("pending attempt differs from its original session, route or intent")
	}
	if o.enterpriseDirectory != "" && !sameWorkspace(o.enterpriseDirectory, attempt.Route.Directory) {
		return errors.New("pending attempt belongs to another private Device directory")
	}
	if o.enterpriseCA != "" && !sameWorkspace(o.enterpriseCA, attempt.Route.CAFile) {
		return errors.New("pending attempt has another original trust selector")
	}
	var active State
	statePath := filepath.Join(root, ".pairroom", "rooms", routing, "slots", string(attempt.Slot), "state.json")
	if err := readPrivate(statePath, &active); err == nil && active.BindID == attempt.Credentials.BindID {
		return errors.New("this attempt has active workspace state; use unbind --local-only without --abandon-pending")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfg := enterpriseConfig(&attempt.Route)
	cfg.CAFile = "" // offline: a removed original CA cannot prevent local retirement
	parent, err := enterpriseclient.New(cfg)
	if err != nil {
		return err
	}
	defer parent.Close()
	auth := relay.Auth{Runtime: attempt.Runtime, SessionID: attempt.SessionID, Slot: attempt.Slot, BindID: attempt.Credentials.BindID, Secret: attempt.Credentials.Secret}
	remotePhase := "local_candidate_only"
	var binding *enterpriseclient.BindingClient
	recordPath := filepath.Join(parent.Directory(), "bindings", attempt.Route.RecordID, "binding.json")
	if _, err := os.Lstat(recordPath); err == nil {
		binding, err = parent.OpenBinding(attempt.Route.RecordID)
		if err != nil {
			return err
		}
		meta, err := binding.LocalMetadata(ctx)
		if err != nil {
			return err
		}
		if meta.UserID != attempt.Route.UserID || meta.DeviceID != attempt.Route.DeviceID || meta.RoomID != o.room || meta.Workspace != root || meta.Slot != attempt.Slot || meta.Runtime != attempt.Runtime || meta.BindID != auth.BindID || meta.SessionHash != relay.Digest(string(auth.Runtime)+"\x00"+auth.SessionID) || meta.CredentialHash != relay.Digest(auth.Secret) || meta.ObservedGeneration != attempt.ObservedGeneration || meta.Replace != attempt.Replace {
			return errors.New("pending attempt differs from its protected Binding record")
		}
		auth.Generation, remotePhase = meta.Generation, meta.RemotePhase
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if attempt.DetachedAt.IsZero() {
		attempt.DetachedAt = time.Now().UTC()
		if err := privatefile.WriteJSON(path, attempt); err != nil {
			return err
		}
	}
	if binding != nil {
		if err := binding.DetachLocal(ctx, auth); err != nil {
			return err
		}
	} else {
		identities, err := nativeidentity.Open()
		if err != nil {
			return err
		}
		if _, err := identities.ReleaseIfHeld(ctx, nativeidentity.Claim{Runtime: auth.Runtime, SessionID: auth.SessionID, Association: nativeidentity.Enterprise(attempt.Route.Origin, attempt.Route.DeploymentID, attempt.Route.RoomID), BindID: auth.BindID}); err != nil {
			return err
		}
	}
	return writeJSON(out, map[string]any{"status": "locally_detached", "room": o.room, "remote_phase": remotePhase, "notice": "Only the local attempt was detached. No server request was sent; original operation identities and private journals were retained. Remote admission or completion may still exist. Use a new explicit --id for another admission."})
}

func unbindEnterpriseLocalOnly(ctx context.Context, root, dir string, out io.Writer) error {
	unlock, err := lockSlotRecovery(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	var state State
	if _, err := readBindingFile(filepath.Join(dir, "state.json"), &state, readPrivate, true); err != nil {
		return err
	}
	if !validEnterpriseRoute(state) || !sameWorkspace(root, state.Workspace) || dir != filepath.Join(root, ".pairroom", "rooms", state.Room, "slots", string(state.Slot)) {
		return errors.New("invalid original enterprise retirement path")
	}
	if err := privatefile.CheckDirectory(state.Enterprise.Directory); err != nil {
		return err
	}
	// No network is possible on this path; a missing original CA file cannot
	// block explicit local retirement. Stored route bytes still match exactly.
	cfg := enterpriseConfig(state.Enterprise)
	cfg.CAFile = ""
	parent, err := enterpriseclient.New(cfg)
	if err != nil {
		return err
	}
	defer parent.Close()
	client, err := parent.OpenBinding(state.Enterprise.RecordID)
	if err != nil {
		return err
	}
	meta, err := client.LocalMetadata(ctx)
	if err != nil {
		return err
	}
	if !matchesEnterpriseIdentity(state, meta) {
		return errors.New("enterprise retirement identity differs from its protected client record")
	}
	var cred credentials
	if _, err := readBindingFile(filepath.Join(dir, "credentials"), &cred, readPrivate, true); err != nil {
		return err
	}
	if cred.BindID != state.BindID || meta.CredentialHash != relay.Digest(cred.Secret) {
		return relay.ErrAuth
	}
	if err := archiveEnterpriseState(root, state); err != nil {
		return err
	}
	if err := client.DetachLocal(ctx, relay.Auth{Runtime: state.Runtime, Slot: state.Slot, BindID: state.BindID, Generation: state.Generation, SessionID: state.SessionID, Secret: cred.Secret}); err != nil {
		return err
	}
	if err := forgetSession(state); err != nil {
		return err
	}
	for _, name := range []string{"state.json", "credentials", "bootstrap"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return writeJSON(out, map[string]any{"status": "detached", "room": state.Enterprise.RoomID, "notice": "Detached locally without contacting the enterprise service. Server Binding/grants remain until explicitly revoked; private journals were retained and will not reconnect automatically."})
}

func reconcileEnterpriseTransport(ctx context.Context, c *Client, ack bool) ([]enterpriseclient.RecoveryOperation, error) {
	if c.Enterprise == nil {
		return nil, nil
	}
	// The same collector gate protects the stdout interval. A concurrent
	// reconciliation cannot retire its receive journal while stdout is active.
	unlock, err := acquireCollector(ctx, c.Dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ops, err := c.Enterprise.ReconcileOperations(ctx, c.localAuth())
	if err != nil {
		return ops, normalizeEnterpriseError(err)
	}
	delivery, err := c.Enterprise.ReconcileDelivery(ctx, c.localAuth(), ack)
	return append(ops, delivery...), normalizeEnterpriseError(err)
}
