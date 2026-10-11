package lanclient

import (
	"context"
	"errors"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// WakeCandidate is body-free. Only an explicitly running local observer uses
// these methods; the client package never starts an observer or vendor work.
// The host cannot supply local session identity or native command arguments.
func (c *Client) WakeCandidate(ctx context.Context, id string) (relay.WakeCandidate, bool, error) {
	r, err := c.ownerRecord(ctx, false)
	if err != nil {
		return relay.WakeCandidate{}, false, err
	}
	if id != "" && !lanshare.ValidID(id) {
		return relay.WakeCandidate{}, false, errors.New("invalid wake message ID")
	}
	var response struct {
		Candidate *relay.WakeCandidate `json:"candidate"`
	}
	if err := c.call(ctx, r, "wake-candidate", map[string]string{"id": id}, &response); err != nil {
		return relay.WakeCandidate{}, false, safeError(err)
	}
	if response.Candidate == nil {
		return relay.WakeCandidate{}, false, nil
	}
	r, err = c.ownerRecord(ctx, false)
	if err != nil {
		return relay.WakeCandidate{}, false, err
	}
	candidate := *response.Candidate
	if r.Room == nil || candidate.Target != r.Room.Slot || candidate.Runtime != r.Runtime || !lanshare.ValidID(candidate.MessageID) || id != "" && candidate.MessageID != id {
		return relay.WakeCandidate{}, false, errors.New("LAN wake identity mismatch")
	}
	candidate.Remote = false
	candidate.BindID, candidate.Generation, candidate.SessionID = r.BindID, r.Room.Generation, r.SessionID
	for _, spent := range r.Spent {
		if spent.MessageID == candidate.MessageID {
			candidate.Reserved = true
			break
		}
	}
	return candidate, true, nil
}

// ReserveWake first obtains a fresh durable host reservation, then commits a
// durable local spent record before returning permission for a fixed effect.
// A lost response or any intervening detach forbids that effect. The same
// spent reservation is shared by every local observer process.
func (c *Client) ReserveWake(ctx context.Context, id string, target model.ActorID) error {
	r, err := c.ownerRecord(ctx, false)
	if err != nil {
		return err
	}
	if r.Room == nil || r.Room.Slot != target || !lanshare.ValidID(id) {
		return relay.ErrAuth
	}
	for _, spent := range r.Spent {
		if spent.MessageID == id {
			return relay.ErrWakeReserved
		}
	}
	if len(r.Spent) >= maxWakeReservations {
		return errors.New("LAN wake journal is full; foreground collection remains available")
	}
	var response struct {
		Reserved bool `json:"reserved"`
	}
	if err := c.call(ctx, r, "wake-reserve", map[string]string{"id": id}, &response); err != nil {
		return safeError(err)
	}
	if !response.Reserved {
		return relay.ErrWakeIneligible
	}
	_, err = c.withRecord(ctx, func(next *record) error {
		if next.Status != "accepted" || next.Room == nil || next.Room.Generation != r.Room.Generation || next.Room.Slot != target || next.BindID != r.BindID {
			return relay.ErrAuth
		}
		if err := c.store.identities.Check(ctx, reservation(*next)); err != nil {
			return err
		}
		for _, spent := range next.Spent {
			if spent.MessageID == id {
				return relay.ErrWakeReserved
			}
		}
		if len(next.Spent) >= maxWakeReservations {
			return errors.New("LAN wake journal is full")
		}
		next.Spent = append(next.Spent, relay.WakeReservation{MessageID: id, Target: target, At: time.Now().UTC()})
		return nil
	})
	return err
}

func (c *Client) RecordWake(ctx context.Context, id, outcome, reason string, target model.ActorID) error {
	r, err := c.ownerRecord(ctx, false)
	if err != nil {
		return err
	}
	if r.Room == nil || r.Room.Slot != target {
		return relay.ErrAuth
	}
	return safeError(c.call(ctx, r, "wake-record", map[string]string{"id": id, "outcome": outcome, "reason": reason}, nil))
}

func (c *Client) WakeReservations(ctx context.Context) ([]relay.WakeReservation, error) {
	r, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	return append([]relay.WakeReservation{}, r.Spent...), nil
}
