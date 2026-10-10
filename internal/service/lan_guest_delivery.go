package service

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

const maxLANGuestDeliveries = 4096

type lanGuestDelivery struct {
	ID         string `json:"id"`
	Receipt    string `json:"receipt"`
	Generation uint64 `json:"generation"`
	State      string `json:"state"` // claimed, stdout, acknowledged, unknown
}

func validateLANGuestDeliveries(r lanGuestRecord) error {
	if len(r.Deliveries) > maxLANGuestDeliveries {
		return errors.New("LAN delivery receipt limit exceeded")
	}
	seen := make(map[string]bool, len(r.Deliveries))
	for _, d := range r.Deliveries {
		if !lanshare.ValidID(d.ID) || !lanshare.ValidID(d.Receipt) || r.Room == nil || d.Generation != r.Room.Generation || seen[d.ID] {
			return errors.New("invalid LAN delivery receipt identity")
		}
		switch d.State {
		case "claimed", "stdout", "acknowledged", "unknown":
		default:
			return errors.New("invalid LAN delivery receipt state")
		}
		seen[d.ID] = true
	}
	return nil
}

// Check capacity before acquiring a remote lease. Completed receipt entries
// can be pruned; unresolved deliveries require explicit inspection instead.
func (guest *lanGuest) deliveryCapacity() error {
	guest.mu.Lock()
	defer guest.mu.Unlock()
	unresolved := 0
	for _, d := range guest.record.Deliveries {
		if d.State != "acknowledged" {
			unresolved++
		}
	}
	if unresolved >= maxLANGuestDeliveries {
		return errors.New("LAN delivery journal is full; inspect unresolved receipts before collecting")
	}
	return nil
}

func (guest *lanGuest) retainDelivery(claim *lanshare.Claim, generation uint64) error {
	guest.mu.Lock()
	defer guest.mu.Unlock()
	r := guest.record
	if r.Status != "accepted" || r.Room == nil || r.Room.Generation != generation || !lanshare.ValidID(claim.Receipt) {
		return relay.ErrAuth
	}
	next := r
	next.Deliveries = make([]lanGuestDelivery, 0, len(r.Deliveries)+1)
	for _, d := range r.Deliveries {
		if d.ID == claim.ID {
			if d.Receipt != claim.Receipt || d.Generation != generation || d.State != "claimed" {
				return errors.New("LAN original delivery receipt changed")
			}
			return nil
		}
		if len(r.Deliveries) < maxLANGuestDeliveries || d.State != "acknowledged" {
			next.Deliveries = append(next.Deliveries, d)
		}
	}
	if len(next.Deliveries) >= maxLANGuestDeliveries {
		return errors.New("LAN delivery receipt limit exceeded")
	}
	next.Deliveries = append(next.Deliveries, lanGuestDelivery{ID: claim.ID, Receipt: claim.Receipt, Generation: generation, State: "claimed"})
	if err := privatefile.WriteJSON(filepath.Join(guest.dir, "guest.json"), next); err != nil {
		return err
	}
	guest.record = next
	return nil
}

// Only the local authenticated collector may report stdout. Network receipt
// alone stays "claimed" and can never trigger an acknowledgement on restart.
func (guest *lanGuest) acknowledgeDelivery(ctx context.Context, id, receipt string, generation uint64, stdout bool) error {
	guest.mu.Lock()
	r := guest.record
	if r.Status != "accepted" || r.Room == nil || r.Room.Generation != generation {
		guest.mu.Unlock()
		return relay.ErrAuth
	}
	index := -1
	for i, d := range r.Deliveries {
		if d.ID == id && d.Receipt == receipt && d.Generation == generation {
			index = i
			break
		}
	}
	if index < 0 {
		guest.mu.Unlock()
		return relay.ErrAuth
	}
	delivery := r.Deliveries[index]
	if delivery.State == "unknown" || delivery.State == "claimed" && !stdout {
		guest.mu.Unlock()
		return errors.New("LAN delivery has no recoverable stdout receipt")
	}
	if delivery.State == "claimed" {
		next := r
		next.Deliveries = append([]lanGuestDelivery(nil), r.Deliveries...)
		next.Deliveries[index].State = "stdout"
		if err := privatefile.WriteJSON(filepath.Join(guest.dir, "guest.json"), next); err != nil {
			guest.mu.Unlock()
			return err
		}
		guest.record = next
	}
	guest.mu.Unlock()
	var response struct {
		HandedOff bool `json:"handed_off"`
	}
	err := guest.call(ctx, "ack", map[string]string{"id": id, "receipt": receipt}, &response)
	state := "acknowledged"
	if err != nil || !response.HandedOff {
		var failure *lanshare.Error
		if err != nil && (!errors.As(err, &failure) || failure.Status >= 500 || failure.Status == 429) {
			return err // retry only this original ACK after reconnection
		}
		state = "unknown" // an explicit refusal is not automatically retried
		if err == nil {
			err = errors.New("LAN stdout acknowledgement was not confirmed")
		}
	}
	guest.mu.Lock()
	defer guest.mu.Unlock()
	next := guest.record
	next.Deliveries = append([]lanGuestDelivery(nil), next.Deliveries...)
	for i, d := range next.Deliveries {
		if d.ID == id && d.Receipt == receipt && d.Generation == generation && d.State != "acknowledged" {
			next.Deliveries[i].State = state
			if saveErr := privatefile.WriteJSON(filepath.Join(guest.dir, "guest.json"), next); saveErr != nil {
				return saveErr
			}
			guest.record = next
			break
		}
	}
	return err
}

// Reconciliation sends no message body and never repeats a claim or stdout.
// A bounded pass settles only receipts whose local collector already wrote.
func (guest *lanGuest) reconcileDeliveryReceipts(ctx context.Context) {
	guest.mu.Lock()
	var pending []lanGuestDelivery
	for _, d := range guest.record.Deliveries {
		if d.State == "stdout" {
			pending = append(pending, d)
			if len(pending) == 8 {
				break
			}
		}
	}
	guest.mu.Unlock()
	for _, d := range pending {
		if ctx.Err() != nil {
			return
		}
		_ = guest.acknowledgeDelivery(ctx, d.ID, d.Receipt, d.Generation, false)
	}
}
