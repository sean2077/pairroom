package service

import (
	"context"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Only an optional running Service performs background readiness observation.
// Direct client operation itself starts no worker or listener.
//
// The observer shares the host's per-source request budget with the user's own
// foreground commands, so an accepted or pending Room is polled on a schedule
// that grows with the number of joined Rooms and backs off after a failed
// pass instead of spending the whole budget on a fixed two-second ticker.
const (
	// observerPollInterval is the per-Room polling period with one joined Room.
	observerPollInterval = 2 * time.Second
	// observerMaxBackoff caps failure backoff unless the healthy per-Room
	// period is already longer; failures must never accelerate that schedule.
	observerMaxBackoff = 60 * time.Second
)

func (g *lanGuestManager) maintain() {
	defer g.wg.Done()
	ticker := time.NewTicker(observerPollInterval)
	defer ticker.Stop()
	workers := make(chan struct{}, 8)
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
		}
		snapshots, err := g.list(g.ctx)
		if err != nil {
			continue
		}
		interval := observerInterval(snapshots)
		now := time.Now()
		for _, snapshot := range snapshots {
			if snapshot.Status != "pending" && snapshot.Status != "accepted" {
				continue
			}
			guest := g.get(snapshot.ID)
			if guest == nil {
				continue
			}
			guest.mu.Lock()
			eligible := !guest.polling && !now.Before(guest.nextPoll)
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
				defer func() { guest.mu.Lock(); guest.polling = false; guest.mu.Unlock(); <-workers }()
				if err := g.refresh(guest); err != nil {
					guest.backoff(interval)
					return
				}
				guest.schedule(interval)
			}()
		}
	}
}

// observerInterval spreads the observer's aggregate request rate across the
// pending/accepted Rooms: one Room polls at the base period, and every additional
// live Room lengthens the period so the total stays near that of a single Room.
// Terminal records are retained for recovery, but generate no observer traffic.
func observerInterval(rooms []lanGuestSummary) time.Duration {
	live := 0
	for _, room := range rooms {
		if room.Status == "pending" || room.Status == "accepted" {
			live++
		}
	}
	return observerPollInterval * time.Duration(max(1, live))
}

// schedule records the next poll after a successful pass and clears any backoff.
func (guest *lanGuest) schedule(delay time.Duration) {
	guest.mu.Lock()
	defer guest.mu.Unlock()
	guest.pollDelay = 0
	guest.nextPoll = time.Now().Add(delay)
}

// backoff lengthens the next poll after a failed pass, bounded by
// the larger of observerMaxBackoff and the healthy base period, so a failed
// host cannot be polled faster than a healthy one as the Room count grows.
func (guest *lanGuest) backoff(base time.Duration) {
	guest.mu.Lock()
	defer guest.mu.Unlock()
	if guest.pollDelay < base {
		guest.pollDelay = base
	} else {
		guest.pollDelay = min(2*guest.pollDelay, max(base, observerMaxBackoff))
	}
	guest.nextPoll = time.Now().Add(guest.pollDelay)
}

func (g *lanGuestManager) refresh(guest *lanGuest) error {
	ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
	defer cancel()
	if err := guest.client.Maintenance(ctx); err != nil {
		return err
	}
	metadata, err := guest.client.Metadata(ctx)
	if err != nil {
		return err
	}
	if metadata.Status != "accepted" {
		// A pending membership is not a failure; it is rechecked on schedule.
		return nil
	}
	guest.mu.Lock()
	waker := guest.waker
	guest.mu.Unlock()
	if waker == nil {
		cfg := nativeWakerConfig{Relay: &lanGuestWakeRelay{guest: guest, ctx: g.ctx}}
		if resolver := g.server.agentResolver; resolver != nil {
			cfg.Mock = resolver.mock
			if command := strings.TrimSpace(resolver.runtimes.For(model.RuntimeCodex).Command); command != "" {
				cfg.CodexCommand, cfg.Run = command, fixedNativeWakeCommand(command)
			}
		}
		if cfg.Mock {
			cfg.Run = unavailableNativeWakeCommand()
		} else {
			cfg.Claude = prepareNativeClaudeWake(metadata.Workspace, metadata.ID)
		}
		waker = newNativeWaker(cfg)
		guest.mu.Lock()
		guest.waker = waker
		guest.mu.Unlock()
	}
	waker.Reconcile(g.ctx)
	return nil
}

// Client owns both the original host reservation and the durable local spent
// journal. The adapter adds no cached authority and never knows private keys.
type lanGuestWakeRelay struct {
	guest *lanGuest
	ctx   context.Context
}

func (a *lanGuestWakeRelay) WakeCandidate(id string) (relay.WakeCandidate, bool) {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	candidate, ok, err := a.guest.client.WakeCandidate(ctx, id)
	return candidate, ok && err == nil
}
func (a *lanGuestWakeRelay) WakeHeads() []relay.WakeCandidate {
	if c, ok := a.WakeCandidate(""); ok {
		return []relay.WakeCandidate{c}
	}
	return nil
}
func (a *lanGuestWakeRelay) ReserveWake(id string, target model.ActorID) error {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	return a.guest.client.ReserveWake(ctx, id, target)
}
func (a *lanGuestWakeRelay) RecordWake(outcome, reason string, target model.ActorID) error {
	return a.record("", outcome, reason, target)
}
func (a *lanGuestWakeRelay) RecordWakeAttempt(id, outcome, reason string, target model.ActorID) error {
	return a.record(id, outcome, reason, target)
}
func (a *lanGuestWakeRelay) record(id, outcome, reason string, target model.ActorID) error {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	return a.guest.client.RecordWake(ctx, id, outcome, reason, target)
}
func (a *lanGuestWakeRelay) WakeReservations() []relay.WakeReservation {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	reservations, err := a.guest.client.WakeReservations(ctx)
	if err != nil {
		return nil
	}
	return reservations
}
