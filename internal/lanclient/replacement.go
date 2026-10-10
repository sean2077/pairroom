package lanclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

const maxRetiredBindings = 32

// A cutover plan is private and durable before a replacement identity can be
// reserved. It is never an active binding or a second inbox. A crash after the
// reservation or archive write retries this exact new key and request.
type replacementPlan struct {
	Schema             int    `json:"schema"`
	PreviousBindID     string `json:"previous_bind_id"`
	PreviousRequestID  string `json:"previous_request_id"`
	PreviousGeneration uint64 `json:"previous_generation"`
	Candidate          record `json:"candidate"`
}

func sameMember(a, b *lanshare.RoomInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.RoomID == b.RoomID && a.BindID == b.BindID && a.Generation == b.Generation && a.Slot == b.Slot
}

func sameLocalAssociation(a, b record) bool {
	return a.ID == b.ID && a.RequestID == b.RequestID && a.BindID == b.BindID && a.Workspace == b.Workspace && a.Runtime == b.Runtime && a.SessionID == b.SessionID && a.CredentialHash == b.CredentialHash && a.Identity == b.Identity && a.Invite == b.Invite && sameMember(a.Room, b.Room)
}

func matchesOptions(r record, invite lanshare.Invite, options JoinOptions) bool {
	return r.Invite == invite && r.Workspace == options.Workspace && r.Runtime == options.Runtime && r.SessionID == options.SessionID && r.BindID == options.BindID && r.CredentialHash == options.CredentialHash
}

func (c *Client) replacementProof(ctx context.Context, r record) error {
	if r.Status == "left" && r.Room != nil {
		return nil
	} // confirmed leave already completed at the host
	var admission lanshare.JoinResponse
	if err := c.call(ctx, r, "join-status", lanshare.JoinStatusRequest{RequestID: r.RequestID}, &admission); err != nil {
		return safeError(err)
	}
	if admission.Status != "revoked" && admission.Status != "expired" {
		return errors.New("previous LAN membership is still active or pending; the host must retire it before replacement")
	}
	if r.Room != nil && admission.Status == "expired" {
		return errors.New("the host did not confirm retirement of the admitted membership")
	}
	receipt, err := lanshare.ParseReceipt(admission.Receipt)
	key, keyErr := r.Identity.Fingerprint()
	if err != nil || keyErr != nil || receipt.RoomID != r.Invite.RoomID || receipt.RequestID != r.RequestID || receipt.Fingerprint != key {
		return errors.New("LAN replacement proof does not identify the original request and key")
	}
	return nil
}

func (s *Store) prepareReplacement(ctx context.Context, invite lanshare.Invite, options JoinOptions) (*Client, error) {
	c, err := s.Get(ctx, ID(invite))
	if err != nil {
		return nil, err
	}
	original, err := c.read(ctx)
	if err != nil {
		return c, err
	}
	if matchesOptions(original, invite, options) && original.PreviousBindID != "" {
		if original.Status != "pending" && original.Status != "accepted" {
			return c, ErrInactive
		}
		return c, nil // exact replacement retry; Resume performs remaining recovery
	}
	if original.Workspace != options.Workspace {
		return c, errors.New("LAN replacement must keep the previous local workspace")
	}
	if original.Invite.Endpoint != invite.Endpoint || original.Invite.HostPin != invite.HostPin || original.Invite.RoomID != invite.RoomID {
		return c, errors.New("LAN replacement must keep the original pinned host endpoint and Room")
	}
	if original.Invite.InviteID == invite.InviteID || original.BindID == options.BindID {
		return c, errors.New("LAN replacement requires a fresh invitation and local binding identity")
	}
	if err := c.replacementProof(ctx, original); err != nil {
		return c, err
	}
	_, err = c.withRecord(ctx, func(current *record) error {
		if matchesOptions(*current, invite, options) && current.PreviousBindID == original.BindID {
			return c.finishReplacement(ctx, *current)
		}
		if !sameLocalAssociation(*current, original) {
			return errors.New("LAN association changed while replacement was being checked; retry the current binding")
		}
		archive, err := c.archiveDirectory(current.BindID)
		if err != nil {
			return err
		}
		planPath := filepath.Join(c.dir, "replacement.json")
		var plan replacementPlan
		err = privatefile.ReadJSON(planPath, 2<<20, &plan)
		if err == nil {
			if validateReplacementPlan(plan) != nil {
				return ErrInvalidState
			}
			if plan.PreviousBindID != current.BindID || plan.PreviousRequestID != current.RequestID || plan.PreviousGeneration != binding(*current).Generation {
				// An earlier completed replacement left this bounded recovery
				// file. Its installed candidate is now the current old binding.
				if plan.Candidate.BindID != current.BindID || plan.Candidate.RequestID != current.RequestID {
					return ErrInvalidState
				}
				err = os.ErrNotExist
			} else if !matchesOptions(plan.Candidate, invite, options) {
				// A rejected reservation has no remote effect and must not poison
				// a fresh replacement attempt. A successfully reserved candidate
				// can only be completed with its original private local identity.
				if checkErr := s.identities.Check(ctx, reservation(plan.Candidate)); checkErr == nil {
					return errors.New("a LAN replacement is already reserved; resume its original local binding")
				} else if !errors.Is(checkErr, nativeidentity.ErrUnowned) {
					return checkErr
				}
				err = os.ErrNotExist
			}
		}
		if errors.Is(err, os.ErrNotExist) {
			identity, err := lanshare.NewIdentity()
			if err != nil {
				return err
			}
			requestID, err := relay.RandomID()
			if err != nil {
				return err
			}
			candidate := record{Schema: 1, ID: c.id, Invite: invite, RequestID: requestID, Identity: identity, Workspace: options.Workspace, Runtime: options.Runtime, SessionID: options.SessionID, BindID: options.BindID, PreviousBindID: current.BindID, CredentialHash: options.CredentialHash, Status: "pending", ParkEnabled: true}
			plan = replacementPlan{Schema: 1, PreviousBindID: current.BindID, PreviousRequestID: current.RequestID, PreviousGeneration: binding(*current).Generation, Candidate: candidate}
			if err := privatefile.WriteJSON(planPath, plan); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		previous := reservation(*current)
		release := &previous
		if current.Status == "left" || current.Status == "detached" || current.Status == "expired" {
			if err := s.identities.Check(ctx, previous); errors.Is(err, nativeidentity.ErrUnowned) {
				release = nil
			} else if err != nil {
				return err
			}
		}
		old := *current
		err = s.identities.ReplacePending(ctx, release, reservation(plan.Candidate), func() error {
			// A failed prior cutover may have saved this same old association.
			// Refresh its final receipt snapshot while it is still current;
			// after installation no stale operation can alter this archive.
			if archived, err := readRecord(archive, c.id); err == nil {
				if !sameLocalAssociation(archived, old) {
					return errors.New("LAN retired binding archive belongs to a different private identity")
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := privatefile.WriteJSON(filepath.Join(archive, "client.json"), old); err != nil {
				return err
			}
			return privatefile.WriteJSON(filepath.Join(c.dir, "client.json"), plan.Candidate)
		})
		if err != nil {
			return err
		}
		*current = plan.Candidate
		return nil
	})
	return c, err
}

func validateReplacementPlan(plan replacementPlan) error {
	if plan.Schema != 1 || !lanshare.ValidID(plan.PreviousBindID) || !lanshare.ValidID(plan.PreviousRequestID) || plan.Candidate.PreviousBindID != plan.PreviousBindID || plan.Candidate.Status != "pending" || plan.Candidate.Room != nil || plan.Candidate.Receipt != "" || len(plan.Candidate.Spent) != 0 || len(plan.Candidate.Deliveries) != 0 {
		return ErrInvalidState
	}
	return validateRecord(plan.Candidate)
}

func (c *Client) archiveDirectory(bindID string) (string, error) {
	if !lanshare.ValidID(bindID) {
		return "", ErrInvalidState
	}
	dir := filepath.Join(c.dir, "retired")
	if err := privatefile.Mkdir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, bindID)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", err
		}
		if len(entries) >= maxRetiredBindings {
			return "", errors.New("LAN retired binding limit reached; preserve and inspect the private archives before replacing again")
		}
	}
	if err := privatefile.Mkdir(path); err != nil {
		return "", err
	}
	return path, nil
}

// Called under the current record lock. If cutover installed the candidate
// but the old session's release was interrupted, finish only that exact old
// reservation. A reused session owned elsewhere is never released.
func (c *Client) finishReplacement(ctx context.Context, current record) error {
	if current.PreviousBindID == "" {
		return nil
	}
	dir := filepath.Join(c.dir, "retired", current.PreviousBindID)
	if err := privatefile.CheckDirectory(dir); err != nil {
		return err
	}
	old, err := readRecord(dir, c.id)
	if err != nil {
		return err
	}
	if old.BindID != current.PreviousBindID || old.Workspace != current.Workspace || old.Invite.HostPin != current.Invite.HostPin || old.Invite.RoomID != current.Invite.RoomID {
		return ErrInvalidState
	}
	claim := reservation(old)
	if err := c.store.identities.Check(ctx, claim); errors.Is(err, nativeidentity.ErrUnowned) {
		return nil
	} else if err != nil {
		return err
	}
	return c.store.identities.Release(ctx, claim)
}
