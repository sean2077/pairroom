package service

import (
	"errors"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/review"
	"github.com/sean2077/pairroom/internal/version"
)

func nativeHistoryQuery(r *http.Request, pending bool) (relay.HistoryQuery, error) {
	q := relay.HistoryQuery{Pending: pending, ID: r.URL.Query().Get("id"), Cursor: r.URL.Query().Get("cursor")}
	for key, values := range r.URL.Query() {
		if len(values) != 1 || (key != "id" && key != "cursor" && key != "limit" && key != "since") {
			return q, errors.New("invalid history query")
		}
	}
	if value := r.URL.Query().Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > relay.HistoryPageLimit {
			return q, errors.New("history limit must be 1–100")
		}
		q.Limit = n
	}
	if value := r.URL.Query().Get("since"); value != "" {
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return q, errors.New("since must be RFC3339")
		}
		q.Since = t
	}
	return q, nil
}

func (n *nativeHostRuntime) serveInspection(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if r.Method != http.MethodGet {
		return false
	}
	switch p {
	case "/api/v1/review":
		if len(r.URL.Query()) == 0 {
			anchor, err := review.Capture(r.Context(), n.project.Root, "")
			nativeResult(w, anchor, err)
		} else {
			if len(r.URL.Query()) != 1 || len(r.URL.Query()["id"]) != 1 || r.URL.Query().Get("id") == "" {
				nativeResult(w, nil, errors.New("review accepts only message id"))
				return true
			}
			page, err := n.engine.History(relay.HistoryQuery{ID: r.URL.Query().Get("id")})
			if err != nil || len(page.Messages) != 1 || page.Messages[0].Review == nil {
				nativeResult(w, nil, errors.New("review evidence unavailable"))
				return true
			}
			// The incoming anchor's path is display data, never a filesystem authority.
			nativeResult(w, map[string]string{"status": review.Check(r.Context(), n.project.Root, *page.Messages[0].Review)}, nil)
		}
		return true
	case "/api/v1/history", "/api/v1/pending":
		q, err := nativeHistoryQuery(r, p == "/api/v1/pending")
		if err != nil {
			nativeResult(w, nil, err)
			return true
		}
		result, err := n.engine.History(q)
		nativeResult(w, result, err)
		return true
	case "/api/v1/diagnostics":
		nativeResult(w, n.nativeDiagnostics(), nil)
		return true
	}
	if strings.HasPrefix(p, "/api/v1/sends/") {
		result, err := n.engine.UserSendReceipt(strings.TrimPrefix(p, "/api/v1/sends/"))
		nativeResult(w, result, err)
		return true
	}
	return false
}

type nativeReachability struct {
	Runtime         model.RuntimeKind `json:"runtime"`
	Capability      string            `json:"capability"`
	HookApproval    string            `json:"hook_approval"`
	ModelAcceptance string            `json:"model_acceptance"`
	CollectorActive bool              `json:"collector_active"`
	HeadID          string            `json:"head_id,omitempty"`
	WakeReserved    bool              `json:"wake_reserved"`
	NextEligibleAt  *time.Time        `json:"next_eligible_at,omitempty"`
	Reason          string            `json:"reason"`
	NextAction      string            `json:"next_action"`
}

// A local read-only report: no socket write, vendor invocation, transcript read,
// guessed liveness or automatic permission change occurs here.
func (n *nativeHostRuntime) nativeDiagnostics() map[string]any {
	summary, bindings, candidates := n.engine.InspectTransport()
	heads := map[model.ActorID]relay.WakeCandidate{}
	for _, head := range candidates {
		heads[head.Target] = head
	}
	slots := map[model.ActorID]nativeReachability{}
	for _, slot := range model.SlotActors() {
		binding := bindings[slot]
		state := summary.Bindings[slot]
		kind := n.room.Agents[slot].Runtime
		head := heads[slot]
		d := nativeReachability{Runtime: kind, Capability: "unavailable", HookApproval: "unknown", ModelAcceptance: "unknown", CollectorActive: state.CollectorActive, HeadID: head.MessageID, WakeReserved: head.Reserved, Reason: "no_pending_input", NextAction: "none"}
		switch kind.Canonical() {
		case model.RuntimeCodex:
			command := n.waker.codexCommand
			if command == "" {
				command = "codex"
			}
			if !n.waker.mock {
				if _, err := exec.LookPath(command); err == nil {
					d.Capability = "codex_queue_unverified"
				}
			}
		case model.RuntimeClaude:
			c := relay.WakeCandidate{Target: slot, Runtime: kind, BindID: binding.BindID, Generation: binding.Generation, SessionID: binding.SessionID}
			if n.waker.claude != nil {
				if send, err := n.waker.claude(c); err == nil && send != nil {
					d.Capability = "claude_inbox_captured"
				}
			}
		case model.RuntimeGrok:
			d.Capability = "tracked_wait_only"
		}
		if reason, due := n.waker.RateWindow(slot); reason != "" {
			d.NextEligibleAt = &due
		}
		switch {
		case !state.Active || !state.Associated:
			d.Reason = "unbound"
			d.NextAction = "bind_in_intended_session"
		case summary.Inboxes[slot].Unknown > 0:
			d.Reason = "uncertain_delivery"
			d.NextAction = "inspect_pending_evidence"
		case state.CollectorActive:
			d.Reason = "collector_registered"
			d.NextAction = "wait_for_collector_result"
		case summary.Inboxes[slot].Delivering > 0:
			d.Reason = "delivery_in_flight"
			d.NextAction = "wait_for_receipt"
		case head.Reserved:
			d.Reason = "wake_already_attempted"
			d.NextAction = "check_native_inbound_policy_or_collect"
		case head.MessageID != "" && !summary.WakeEnabled:
			d.Reason = "wake_disabled"
			d.NextAction = "collect_in_native_session"
		case head.MessageID != "" && (d.Capability == "unavailable" || d.Capability == "tracked_wait_only"):
			d.Reason = "capability_unavailable"
			d.NextAction = "rebind_or_collect_in_native_session"
		case head.MessageID != "" && d.NextEligibleAt != nil:
			d.Reason = "wake_deferred"
			d.NextAction = "wait_for_eligible_time"
		case head.MessageID != "":
			d.Reason = "wake_eligible"
			d.NextAction = "wait_for_service_wake"
		}
		slots[slot] = d
	}
	return map[string]any{"schema": 1, "scope": "native_room", "service_version": version.Current, "protocol": protocol.NativeVersion, "generated_at": time.Now().UTC(), "relay": summary, "participants": slots, "notice": "Transport observations only. Captured capability/available executable is not proof of native policy, live presence or model acceptance. In-session relay doctor checks local hook installation; approvals remain unknown."}
}
