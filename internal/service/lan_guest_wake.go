package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

// Readiness maintenance is body-free and never claims an inbox. It can settle
// only original receipts whose local collectors already reported stdout.
// It continues after CLI exit and Service restart under the same room
// certificate, so no new invitation or owner approval is needed.
func (g *lanGuestManager) maintain() {
	defer g.wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	workers := make(chan struct{}, 8)
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
		}
		g.mu.Lock()
		guests := make([]*lanGuest, 0, len(g.guests))
		for _, guest := range g.guests {
			guests = append(guests, guest)
		}
		g.mu.Unlock()
		for _, guest := range guests {
			guest.mu.Lock()
			eligible := !guest.polling && (guest.record.Status == "pending" || guest.record.Status == "accepted")
			if eligible {
				select {
				case workers <- struct{}{}:
					guest.polling = true
				default:
					eligible = false
				}
			}
			guest.mu.Unlock()
			if !eligible {
				continue
			}
			g.wg.Add(1)
			go func() {
				defer g.wg.Done()
				defer func() {
					guest.mu.Lock()
					guest.polling = false
					guest.mu.Unlock()
					<-workers
				}()
				g.refresh(guest)
			}()
		}
	}
}

func (g *lanGuestManager) refresh(guest *lanGuest) {
	guest.mu.Lock()
	record := guest.record
	guest.mu.Unlock()
	ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
	defer cancel()
	var admission lanshare.JoinResponse
	if err := guest.call(ctx, "join-status", lanshare.JoinStatusRequest{RequestID: record.RequestID}, &admission); err != nil {
		return
	}
	if err := g.updateAdmission(guest, admission); err != nil || admission.Status != "accepted" {
		return
	}
	guest.reconcileDeliveryReceipts(ctx)
	guest.mu.Lock()
	waker := guest.waker
	guest.mu.Unlock()
	if waker == nil {
		adapter := &lanGuestWakeRelay{guest: guest, ctx: g.ctx}
		cfg := nativeWakerConfig{Relay: adapter}
		if resolver := g.server.agentResolver; resolver != nil {
			cfg.Mock = resolver.mock
			if command := strings.TrimSpace(resolver.runtimes.For(model.RuntimeCodex).Command); command != "" {
				cfg.CodexCommand = command
				cfg.Run = fixedNativeWakeCommand(command)
			}
		}
		if cfg.Mock {
			cfg.Run = unavailableNativeWakeCommand()
		} else {
			cfg.Claude = prepareNativeClaudeWake(record.Workspace, record.ID)
		}
		waker = newNativeWaker(cfg)
		guest.mu.Lock()
		guest.waker = waker
		guest.mu.Unlock()
	}
	waker.Reconcile(g.ctx)
}

// The existing Native waker applies capability, grace, collector, rate and
// recovery rules. This adapter supplies only local session identity and stores
// a durable spent record after a fresh HOST reservation and before the effect.
type lanGuestWakeRelay struct {
	guest *lanGuest
	ctx   context.Context
}

func (a *lanGuestWakeRelay) candidate(id string) (relay.WakeCandidate, bool) {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	var result struct {
		Candidate *relay.WakeCandidate `json:"candidate"`
	}
	if a.guest.call(ctx, "wake-candidate", map[string]string{"id": id}, &result) != nil || result.Candidate == nil {
		return relay.WakeCandidate{}, false
	}
	c := *result.Candidate
	a.guest.mu.Lock()
	defer a.guest.mu.Unlock()
	r := a.guest.record
	if r.Status != "accepted" || r.Room == nil || c.Target != r.Room.Slot || c.Runtime != r.Runtime || c.MessageID == "" || id != "" && c.MessageID != id {
		return relay.WakeCandidate{}, false
	}
	c.Remote = false // native wake stays in this guest machine
	c.BindID, c.Generation, c.SessionID = r.BindID, r.Room.Generation, r.SessionID
	for _, spent := range r.Spent {
		if spent.MessageID == c.MessageID {
			c.Reserved = true
			break
		}
	}
	return c, true
}

func (a *lanGuestWakeRelay) WakeCandidate(id string) (relay.WakeCandidate, bool) {
	return a.candidate(id)
}

func (a *lanGuestWakeRelay) WakeHeads() []relay.WakeCandidate {
	if c, ok := a.candidate(""); ok {
		return []relay.WakeCandidate{c}
	}
	return nil
}

func (a *lanGuestWakeRelay) ReserveWake(id string, target model.ActorID) error {
	a.guest.mu.Lock()
	r := a.guest.record
	if r.Status != "accepted" || r.Room == nil || r.Room.Slot != target {
		a.guest.mu.Unlock()
		return relay.ErrAuth
	}
	for _, spent := range r.Spent {
		if spent.MessageID == id {
			a.guest.mu.Unlock()
			return relay.ErrWakeReserved
		}
	}
	if len(r.Spent) >= 4096 {
		a.guest.mu.Unlock()
		return errNativeWakeAudit // bounded fail-closed journal; inbox remains usable
	}
	a.guest.mu.Unlock()
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	var result struct {
		Reserved bool `json:"reserved"`
	}
	if err := a.guest.call(ctx, "wake-reserve", map[string]string{"id": id}, &result); err != nil {
		var failure *lanshare.Error
		if errors.As(err, &failure) && failure.Code == "wake_reserved" {
			return relay.ErrWakeReserved
		}
		return err
	}
	if !result.Reserved {
		return relay.ErrWakeIneligible
	}
	a.guest.mu.Lock()
	defer a.guest.mu.Unlock()
	next := a.guest.record
	if next.Status != "accepted" || next.Room == nil || next.Room.Generation != r.Room.Generation || next.Room.Slot != target {
		return relay.ErrAuth
	}
	for _, spent := range next.Spent {
		if spent.MessageID == id {
			return relay.ErrWakeReserved
		}
	}
	next.Spent = append(append([]relay.WakeReservation{}, next.Spent...), relay.WakeReservation{MessageID: id, Target: target, At: time.Now().UTC()})
	if err := privatefile.WriteJSON(filepath.Join(a.guest.dir, "guest.json"), next); err != nil {
		return err
	}
	a.guest.record = next
	return nil
}

func (a *lanGuestWakeRelay) RecordWake(outcome, reason string, target model.ActorID) error {
	return a.record("", outcome, reason, target)
}

func (a *lanGuestWakeRelay) RecordWakeAttempt(id, outcome, reason string, target model.ActorID) error {
	return a.record(id, outcome, reason, target)
}

func (a *lanGuestWakeRelay) record(id, outcome, reason string, target model.ActorID) error {
	a.guest.mu.Lock()
	room := a.guest.record.Room
	a.guest.mu.Unlock()
	if room == nil || room.Slot != target {
		return relay.ErrAuth
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	return a.guest.call(ctx, "wake-record", map[string]string{"id": id, "outcome": outcome, "reason": reason}, nil)
}

func (a *lanGuestWakeRelay) WakeReservations() []relay.WakeReservation {
	a.guest.mu.Lock()
	defer a.guest.mu.Unlock()
	return append([]relay.WakeReservation{}, a.guest.record.Spent...)
}
