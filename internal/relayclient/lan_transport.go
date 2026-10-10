package relayclient

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/relay"
)

func (c *Client) localAuth() relay.Auth {
	return relay.Auth{Slot: c.State.Slot, BindID: c.State.BindID, Generation: c.State.Generation, SessionID: c.State.SessionID, Secret: c.Secret}
}

func (c *Client) close() {
	if c.LAN != nil {
		c.LAN.Close()
	}
	if c.HTTP != nil {
		c.HTTP.CloseIdleConnections()
	}
}

// An explicit remote Room can recover its workspace locator from the bounded
// client store. It never asks a local Service to resolve that host-scoped ID.
func directRoomWorkspace(ctx context.Context, id string, caller nativeCaller) (string, error) {
	noteLANTransport(ctx)
	store, err := lanclient.Open()
	if err != nil {
		return "", err
	}
	defer store.Close()
	client, err := store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	meta, err := client.Metadata(ctx)
	if err != nil {
		return "", err
	}
	if caller.session != "" && (caller.runtime != meta.Runtime || caller.session != meta.SessionID) {
		return "", relay.ErrAuth
	}
	return workspace(ctx, meta.Workspace)
}

// Recover a disposable session locator from the bounded per-user client
// catalog before consulting any local Service. This reads metadata only.
func directSessionMetadata(ctx context.Context, caller nativeCaller) (*lanclient.Metadata, error) {
	if caller.session == "" {
		return nil, nil
	}
	store, err := lanclient.Open()
	if err != nil {
		return nil, err
	}
	defer store.Close()
	clients, err := store.List(ctx)
	if err != nil {
		return nil, err
	}
	var found *lanclient.Metadata
	for _, entry := range clients {
		if entry.Status != "pending" && entry.Status != "accepted" {
			continue
		}
		client, err := store.Get(ctx, entry.ID)
		if err != nil {
			return nil, err
		}
		meta, err := client.Metadata(ctx)
		if err != nil {
			return nil, err
		}
		if meta.Runtime != caller.runtime || meta.SessionID != caller.session || meta.Status != "pending" && meta.Status != "accepted" {
			continue
		}
		if found != nil {
			return nil, errors.New("native session has multiple direct LAN associations; inspect its original client records before recovery")
		}
		found = &meta
	}
	return found, nil
}

// Recovered workspace bytes are never authority for a direct association. Match
// the complete immutable route and native identity against its protected record.
func matchesDirectMetadata(state State, meta lanclient.Metadata) bool {
	return validStateFormat(state) && state.LAN != nil && state.Room == meta.ID && state.Slot == meta.Slot &&
		state.BindID == meta.BindID && state.Generation == meta.Generation && state.Runtime == meta.Runtime &&
		state.SessionID == meta.SessionID && sameWorkspace(state.Workspace, meta.Workspace) &&
		state.LAN.Endpoint == meta.Invite.Endpoint && state.LAN.HostPin == meta.Invite.HostPin && state.LAN.RoomID == meta.Invite.RoomID
}

func directMetadataState(meta lanclient.Metadata) (*State, error) {
	var state State
	if meta.Generation == 0 || !meta.Slot.ValidParticipant() {
		return nil, errors.New("LAN admission is pending; run relay bind to reconcile the original request, or unbind --local-only to abandon it locally")
	}
	path := filepath.Join(meta.Workspace, ".pairroom", "rooms", meta.ID, "slots", string(meta.Slot), "state.json")
	if err := readPrivate(path, &state); errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("LAN admission needs local confirmation; run relay bind from the original native session")
	} else if err != nil {
		return nil, err
	}
	if !matchesDirectMetadata(state, meta) {
		return nil, errors.New("direct LAN workspace binding differs from its private client record; inspect the original association")
	}
	return &state, nil
}

func selectDirectWorkspace(ctx context.Context, meta lanclient.Metadata, action string, o *options) (string, error) {
	noteLANTransport(ctx)
	if action == "bind" && o.create {
		return "", errors.New("this native session already has a direct LAN association; use relay bind to resume it or a separate session to create a Room")
	}
	root, err := workspace(ctx, meta.Workspace)
	if err != nil || !sameWorkspace(root, meta.Workspace) {
		return "", errors.New("the direct LAN association's original workspace is unavailable")
	}
	if o.repoExplicit {
		explicit, err := workspace(ctx, o.repo)
		if err != nil || !sameWorkspace(explicit, root) {
			return "", errors.New("explicit workspace conflicts with this native session's direct LAN association")
		}
	}
	if o.endpoint != "" || o.room != "" && o.room != meta.ID || o.slot != "" && o.slot != string(meta.Slot) {
		return "", errors.New("explicit relay target conflicts with this native session's direct LAN association")
	}
	o.room, o.slot = meta.ID, string(meta.Slot)
	return root, nil
}

// Loading a direct binding only reads its private record. The route is checked
// against the workspace binding before the first request, independently of any
// running local Service or its default endpoint file.
func (c *Client) loadLAN() error {
	store, err := lanclient.Open()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := store.Get(ctx, c.State.LAN.ClientID)
	if err != nil {
		return fmt.Errorf("read direct LAN client: %w", err)
	}
	meta, err := client.Metadata(ctx)
	if err != nil {
		client.Close()
		return err
	}
	if !matchesDirectMetadata(c.State, meta) {
		client.Close()
		return errors.New("direct LAN client identity or route changed; inspect the original association before recovery")
	}
	c.LAN = client
	return nil
}

// An original ACK may be retried only when no HTTP response was received.
// Definite host rejections preserve their stable publication conflict codes.
func normalizeLANError(action string, err error) error {
	var localFailure *lanclient.Error
	if errors.As(err, &localFailure) {
		return &relayError{action: action, message: localFailure.Message, code: localFailure.Code}
	}
	if errors.Is(err, lanclient.ErrTransportUnavailable) {
		return fmt.Errorf("relay %s %w", action, errTransportUnavailable)
	}
	var failure *lanshare.Error
	if errors.As(err, &failure) {
		return &relayError{action: action, message: failure.Message, code: failure.Code}
	}
	var transport *url.Error
	if errors.As(err, &transport) {
		return fmt.Errorf("relay %s %w", action, errTransportUnavailable)
	}
	return err
}
