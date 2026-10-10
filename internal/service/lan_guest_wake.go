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
		snapshots, err := g.store.List(g.ctx)
		if err != nil {
			continue
		}
		for _, snapshot := range snapshots {
			if snapshot.Status != "pending" && snapshot.Status != "accepted" {
				continue
			}
			guest := g.get(snapshot.ID)
			if guest == nil {
				continue
			}
			guest.mu.Lock()
			eligible := !guest.polling
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
				g.refresh(guest)
			}()
		}
	}
}

func (g *lanGuestManager) refresh(guest *lanGuest) {
	ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
	defer cancel()
	if err := guest.client.Maintenance(ctx); err != nil {
		return
	}
	metadata, err := guest.client.Metadata(ctx)
	if err != nil || metadata.Status != "accepted" {
		return
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
