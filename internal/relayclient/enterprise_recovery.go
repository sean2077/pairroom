package relayclient

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/enterpriseclient"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

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
