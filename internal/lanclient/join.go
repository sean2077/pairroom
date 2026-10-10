package lanclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/relay"
)

func validateJoin(options JoinOptions) (lanshare.Invite, error) {
	invite, err := lanshare.ParseInvite(options.Invite)
	if err != nil || !options.Runtime.Valid() || !validSession(options.SessionID) || !lanshare.ValidID(options.BindID) || !lanshare.ValidFingerprint(options.CredentialHash) || len(options.Label) > 128 || strings.ContainsFunc(options.Label, unicode.IsControl) {
		return invite, errors.New("invalid local LAN join identity")
	}
	workspace, err := filepath.EvalSymlinks(options.Workspace)
	if err != nil || !filepath.IsAbs(workspace) || workspace != options.Workspace || filepath.Clean(workspace) != workspace {
		return invite, errors.New("LAN join requires its canonical local workspace")
	}
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return invite, errors.New("LAN join workspace is unavailable")
	}
	return invite, nil
}

// Join persists a per-Room key and request ID before contacting the host. The
// public invitation cannot admit itself. Retrying uses the original private
// identity and exact request, including after an accepted response was lost.
func (s *Store) Join(ctx context.Context, options JoinOptions) (*Client, JoinResult, error) {
	invite, err := validateJoin(options)
	if err != nil {
		return nil, JoinResult{}, err
	}
	var c *Client
	if options.Replace {
		c, err = s.prepareReplacement(ctx, invite, options)
	} else {
		c, err = s.prepare(ctx, invite, options)
	}
	if err != nil {
		return c, JoinResult{}, err
	}
	result, err := c.resume(ctx, options.Label)
	if err != nil {
		return c, result, err
	}
	r, err := c.read(ctx)
	if err != nil {
		return c, result, err
	}
	if r.Status == "expired" && r.Room == nil && r.Invite.InviteID != invite.InviteID {
		// Only an authoritative expiry permits a fresh public request. Keep
		// the same key; an accepted membership never changes its endpoint.
		c, err = s.prepare(ctx, invite, options)
		if err != nil {
			return c, result, err
		}
		result, err = c.resume(ctx, options.Label)
	}
	return c, result, err
}

func (s *Store) prepare(ctx context.Context, invite lanshare.Invite, options JoinOptions) (*Client, error) {
	if err := os.MkdirAll(filepath.Dir(s.root), 0o700); err != nil {
		return nil, err
	}
	if err := privatefile.Mkdir(s.root); err != nil {
		return nil, err
	}
	unlockRoot, err := privatelock.Lock(ctx, s.root)
	if err != nil {
		return nil, err
	}
	defer unlockRoot()
	c, err := s.client(ID(invite))
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(c.dir); errors.Is(err, os.ErrNotExist) {
		entries, err := readClientDirectories(s.root)
		if err != nil {
			return nil, err
		}
		if len(entries) >= maxClients {
			return nil, errors.New("LAN joined Room limit exceeded")
		}
	}
	if err := privatefile.Mkdir(c.dir); err != nil {
		return nil, err
	}
	unlock, err := privatelock.Lock(ctx, c.dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := readRecord(c.dir, c.id)
	if errors.Is(err, os.ErrNotExist) {
		identity, keyErr := lanshare.NewIdentity()
		if keyErr != nil {
			return nil, keyErr
		}
		requestID, keyErr := relay.RandomID()
		if keyErr != nil {
			return nil, keyErr
		}
		r = record{Schema: 1, ID: c.id, Invite: invite, RequestID: requestID, Identity: identity, Workspace: options.Workspace, Runtime: options.Runtime, SessionID: options.SessionID, BindID: options.BindID, CredentialHash: options.CredentialHash, Status: "pending", ParkEnabled: true}
		// Reject an already-owned native session before installing a target
		// Room record. Otherwise that rejected attempt would prevent a fresh
		// session from joining a Room the host has never seen. The exact
		// pending reservation survives an ambiguous subsequent disk failure;
		// retrying the same local BindID can finish that interrupted write.
		if err := s.identities.Reserve(ctx, reservation(r)); err != nil {
			return nil, err
		}
		if err := privatefile.WriteJSON(filepath.Join(c.dir, "client.json"), r); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if r.Workspace != options.Workspace || r.Runtime != options.Runtime || r.SessionID != options.SessionID || r.BindID != options.BindID || r.CredentialHash != options.CredentialHash {
		return nil, errors.New("joined Room already belongs to a different local native session or credential")
	}
	if r.Status == "detached" || r.Status == "left" || r.Status == "revoked" {
		return nil, ErrInactive
	}
	if r.Status == "expired" && r.Room == nil && r.Invite.InviteID != invite.InviteID {
		requestID, err := relay.RandomID()
		if err != nil {
			return nil, err
		}
		r.Invite, r.RequestID, r.Status, r.Receipt = invite, requestID, "pending", ""
		if err := privatefile.WriteJSON(filepath.Join(c.dir, "client.json"), r); err != nil {
			return nil, err
		}
	}
	if r.Status == "pending" || r.Status == "accepted" {
		if err := s.identities.Reserve(ctx, reservation(r)); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func joinResult(r record) JoinResult {
	result := JoinResult{ID: r.ID, Status: r.Status, Receipt: r.Receipt, PreviousBindID: r.PreviousBindID}
	if r.Status == "accepted" && r.Room != nil {
		b := binding(r)
		result.Binding = &b
		result.Bootstrap = protocol.NativeBootstrap(b.Slot, b.Runtime, r.Room.Runtimes[model.OtherParticipant(b.Slot)]) + "\n" + protocol.SharedRoomBootstrapNotice
		result.Collaboration = protocol.CollaborationInstructions(b.Slot, r.Room.Collaboration)
	}
	return result
}

func (c *Client) Resume(ctx context.Context) (JoinResult, error) { return c.resume(ctx, "") }

func (c *Client) resume(ctx context.Context, label string) (JoinResult, error) {
	r, err := c.withRecord(ctx, func(r *record) error {
		if r.Status != "pending" && r.Status != "accepted" && r.Status != "expired" {
			return ErrInactive
		}
		if r.Status == "expired" {
			return nil
		}
		if err := c.finishReplacement(ctx, *r); err != nil {
			return err
		}
		return c.store.identities.Reserve(ctx, reservation(*r))
	})
	if err != nil {
		return JoinResult{}, err
	}
	if r.Status == "expired" {
		return joinResult(r), nil
	}
	action := "join-status"
	var payload any = lanshare.JoinStatusRequest{RequestID: r.RequestID}
	if r.Status == "pending" {
		action = "join"
		payload = lanshare.JoinRequest{InviteID: r.Invite.InviteID, RequestID: r.RequestID, Runtime: r.Runtime, Label: label}
	}
	var admission lanshare.JoinResponse
	if err := c.call(ctx, r, action, payload, &admission); err != nil {
		return joinResult(r), safeError(err)
	}
	r, err = c.updateAdmission(ctx, r, admission)
	if err != nil {
		return JoinResult{}, err
	}
	if r.Status == "accepted" {
		if err := c.reconcileDeliveryReceipts(ctx); err != nil {
			return joinResult(r), err
		}
	}
	return joinResult(r), nil
}

func (c *Client) updateAdmission(ctx context.Context, original record, admission lanshare.JoinResponse) (record, error) {
	return c.withRecord(ctx, func(next *record) error {
		before, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if next.RequestID != original.RequestID || next.BindID != original.BindID || next.Invite.InviteID != original.Invite.InviteID || original.Room != nil && !sameMember(original.Room, next.Room) {
			return ErrInactive
		}
		if next.Status == "left" || next.Status == "revoked" || next.Status == "detached" {
			return ErrInactive
		}
		if admission.Status != "pending" && admission.Status != "accepted" && admission.Status != "revoked" && admission.Status != "expired" {
			return errors.New("invalid LAN admission status")
		}
		if admission.Receipt != "" {
			receipt, err := lanshare.ParseReceipt(admission.Receipt)
			fingerprint, keyErr := next.Identity.Fingerprint()
			if err != nil || keyErr != nil || receipt.RequestID != next.RequestID || receipt.RoomID != next.Invite.RoomID || receipt.Fingerprint != fingerprint {
				return errors.New("LAN receipt does not identify this request and key")
			}
			next.Receipt = admission.Receipt
		}
		if next.Status == "accepted" && admission.Status == "pending" {
			return nil
		}
		if admission.Status == "expired" && next.Room != nil {
			return errors.New("accepted LAN membership cannot become a pending expiry")
		}
		if admission.Status == "accepted" {
			if admission.Room == nil || next.Room != nil && (next.Room.Generation != admission.Room.Generation || next.Room.BindID != admission.Room.BindID || next.Room.Slot != admission.Room.Slot) {
				return errors.New("LAN membership generation changed; old local receipts must not be replayed")
			}
			next.Room = admission.Room
		}
		next.Status = admission.Status
		if err := validateRecord(*next); err != nil {
			return err
		}
		// Persist promotion before upgrading the separate identity claim. Its
		// existing pending reservation excludes any other association during
		// this gap; Resume repairs an interrupted upgrade before use.
		if next.Status == "accepted" || next.Status == "expired" {
			after, err := json.Marshal(next)
			if err != nil {
				return err
			}
			if !bytes.Equal(before, after) {
				if err := privatefile.WriteJSON(filepath.Join(c.dir, "client.json"), *next); err != nil {
					return err
				}
			}
			if next.Status == "accepted" {
				return c.store.identities.Reserve(ctx, reservation(*next))
			}
			return c.store.identities.Release(ctx, reservation(*next))
		}
		return nil
	})
}

// Maintenance is an explicit bounded body-free pass for foreground callers or
// an optional local observer. It never claims, renders or replays an envelope.
func (c *Client) Maintenance(ctx context.Context) error {
	r, err := c.withRecord(ctx, func(r *record) error {
		if r.Status != "accepted" && r.Status != "pending" {
			return ErrInactive
		}
		if err := c.finishReplacement(ctx, *r); err != nil {
			return err
		}
		return c.store.identities.Reserve(ctx, reservation(*r))
	})
	if err != nil {
		return err
	}
	var admission lanshare.JoinResponse
	if err := c.call(ctx, r, "join-status", lanshare.JoinStatusRequest{RequestID: r.RequestID}, &admission); err != nil {
		return safeError(err)
	}
	r, err = c.updateAdmission(ctx, r, admission)
	if err != nil || r.Status != "accepted" {
		return err
	}
	return c.reconcileDeliveryReceipts(ctx)
}
