package lanclient

import (
	"context"
	"errors"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/relay"
)

func deliveryCapacity(r record) error {
	unresolved := 0
	for _, d := range r.Deliveries {
		if d.State != "acknowledged" {
			unresolved++
		}
	}
	if unresolved >= maxDeliveries {
		return errors.New("LAN delivery journal is full; inspect unresolved receipts before collecting")
	}
	return nil
}

func retainedDelivery(r record, id string) bool {
	for _, d := range r.Deliveries {
		if d.ID == id {
			return true
		}
	}
	return false
}

func (c *Client) retainDelivery(ctx context.Context, auth relay.Auth, claim *lanshare.Claim) error {
	_, err := c.withRecord(ctx, func(r *record) error {
		if err := authenticate(*r, auth, false); err != nil {
			return err
		}
		if err := c.store.identities.Check(ctx, reservation(*r)); err != nil {
			return err
		}
		if !lanshare.ValidID(claim.ID) || !lanshare.ValidID(claim.Receipt) {
			return relay.ErrAuth
		}
		next := make([]delivery, 0, len(r.Deliveries)+1)
		for _, d := range r.Deliveries {
			if d.ID == claim.ID {
				return errors.New("LAN original delivery is already retained; its envelope cannot be printed again")
			}
			if len(r.Deliveries) < maxDeliveries || d.State != "acknowledged" {
				next = append(next, d)
			}
		}
		if len(next) >= maxDeliveries {
			return errors.New("LAN delivery receipt limit exceeded")
		}
		r.Deliveries = append(next, delivery{ID: claim.ID, Receipt: claim.Receipt, Generation: auth.Generation, State: "claimed"})
		return nil
	})
	return err
}

// A claim response alone never implies stdout. Only the authenticated local
// collector can promote it to stdout, durably before the original remote ACK.
// Restart recovery is limited to that receipt; it cannot claim or print again.
func (c *Client) acknowledgeDelivery(ctx context.Context, auth relay.Auth, id, receipt string, generation uint64, stdout bool) error {
	r, err := c.withRecord(ctx, func(r *record) error {
		if r.Status != "accepted" || r.Room == nil || r.Room.Generation != generation {
			return relay.ErrAuth
		}
		if stdout {
			if err := authenticate(*r, auth, false); err != nil {
				return err
			}
		}
		if err := c.store.identities.Check(ctx, reservation(*r)); err != nil {
			return err
		}
		for i, d := range r.Deliveries {
			if d.ID != id || d.Receipt != receipt || d.Generation != generation {
				continue
			}
			if d.State == "unknown" || d.State == "claimed" && !stdout {
				return errors.New("LAN delivery has no recoverable stdout receipt")
			}
			if d.State == "claimed" {
				r.Deliveries[i].State = "stdout"
			}
			return nil
		}
		return relay.ErrAuth
	})
	if err != nil {
		return err
	}
	var response struct {
		HandedOff bool `json:"handed_off"`
	}
	err = c.call(ctx, r, "ack", map[string]string{"id": id, "receipt": receipt}, &response)
	state := "acknowledged"
	if err != nil || !response.HandedOff {
		var failure *lanshare.Error
		if err != nil && (!errors.As(err, &failure) || failure.Status >= 500 || failure.Status == 429) {
			return safeError(err) // retain stdout; only this original ACK is recoverable
		}
		state = "unknown" // explicit refusal is not automatically retried
		if err == nil {
			err = errors.New("LAN stdout acknowledgement was not confirmed")
		}
	}
	_, saveErr := c.withRecord(ctx, func(next *record) error {
		for i, d := range next.Deliveries {
			if d.ID == id && d.Receipt == receipt && d.Generation == generation && d.State != "acknowledged" {
				next.Deliveries[i].State = state
				break
			}
		}
		return nil
	})
	if saveErr != nil {
		return saveErr
	}
	return safeError(err)
}

func (c *Client) reconcileDeliveryReceipts(ctx context.Context) error {
	r, err := c.read(ctx)
	if err != nil {
		return err
	}
	count := 0
	for _, d := range r.Deliveries {
		if d.State != "stdout" {
			continue
		}
		if err := c.acknowledgeDelivery(ctx, relay.Auth{}, d.ID, d.Receipt, d.Generation, false); err != nil {
			return err
		}
		count++
		if count == 8 {
			break
		}
	}
	return nil
}
