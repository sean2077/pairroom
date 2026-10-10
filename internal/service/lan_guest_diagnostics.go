package service

import (
	"context"
	"net/http"
	"os/exec"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func (guest *lanGuest) doctor(ctx context.Context, w http.ResponseWriter) error {
	var remote struct {
		Relay          relay.Summary `json:"relay"`
		Protocol       string        `json:"protocol"`
		ServiceVersion string        `json:"service_version"`
	}
	// The host's doctor completion route does not activate a suspended Room.
	if err := guest.call(ctx, "doctor", nil, &remote); err != nil {
		return err
	}
	guest.mu.Lock()
	r := guest.record
	waker := guest.waker
	connected, lastSeen := guest.connected, guest.lastSeen
	guest.mu.Unlock()
	if r.Room == nil {
		return relay.ErrAuth
	}
	remote.Relay.RoomID = r.ID
	slot := r.Room.Slot
	state := remote.Relay.Bindings[slot]
	d := nativeReachability{Runtime: r.Runtime, Capability: "unavailable", HookApproval: "unknown", ModelAcceptance: "unknown", CollectorActive: state.CollectorActive, Reason: "no_pending_input", NextAction: "none"}
	mock := waker != nil && waker.mock
	switch r.Runtime.NativeWakePolicy().Transport {
	case model.NativeWakeClaudeInbox:
		if !mock {
			prepare := prepareNativeClaudeWake(r.Workspace, r.ID)
			if send, err := prepare(relay.WakeCandidate{Target: slot, Runtime: r.Runtime, BindID: r.BindID, Generation: r.Room.Generation, SessionID: r.SessionID}); err == nil && send != nil {
				d.Capability = "claude_inbox_captured"
			}
		}
	case model.NativeWakeCodexQueue:
		command := "codex"
		if waker != nil && waker.codexCommand != "" {
			command = waker.codexCommand
		}
		if !mock {
			if _, err := exec.LookPath(command); err == nil {
				d.Capability = "codex_queue_unverified"
			}
		}
	case model.NativeWakeUnavailable:
		d.Capability = "tracked_wait_only"
	}
	inbox := remote.Relay.Inboxes[slot]
	switch {
	case inbox.Unknown > 0:
		d.Reason, d.NextAction = "uncertain_delivery", "inspect_pending_evidence"
	case state.CollectorActive:
		d.Reason, d.NextAction = "collector_registered", "wait_for_collector_result"
	case inbox.Delivering > 0:
		d.Reason, d.NextAction = "delivery_in_flight", "wait_for_receipt"
	case inbox.Queued > 0 && !remote.Relay.WakeEnabled:
		d.Reason, d.NextAction = "wake_disabled", "collect_in_native_session"
	case inbox.Queued > 0:
		d.Reason, d.NextAction = "queued_input", "collect_or_check_local_wake"
	}
	if waker != nil {
		if reason, due := waker.RateWindow(slot); reason != "" {
			d.NextEligibleAt = &due
		}
	}
	writeManagementJSON(w, http.StatusOK, map[string]any{"schema": 1, "scope": "native_lan_joined", "service_version": remote.ServiceVersion, "protocol": remote.Protocol, "generated_at": time.Now().UTC(), "relay": remote.Relay, "participants": map[model.ActorID]nativeReachability{slot: d}, "connection": map[string]any{"connected": connected, "last_seen": lastSeen}, "notice": "Connected is transport reachability. This guest alone checks its native capabilities; hook approval and model acceptance are unknown. No claim, acknowledgement or native wake is performed by doctor."})
	return nil
}
