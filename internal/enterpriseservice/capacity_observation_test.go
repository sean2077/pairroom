package enterpriseservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type capacityPendingSend struct {
	actor  capacityActor
	room   capacityRoom
	key    string
	intent sqlstore.HumanSendIntent
	cancel *sqlstore.RelayCommand
}

type capacityObservationKey struct{}
type capacityObservation struct {
	mu                sync.Mutex
	status, admission map[string]int
}

func (o *capacityObservation) record(status int, body []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if status >= 100 && status <= 599 {
		o.status[fmt.Sprintf("%d", status)]++
	} else {
		o.status["other"]++
	}
	if status != 429 {
		return
	}
	var response struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &response)
	code := response.Error
	switch code {
	case "request_capacity", "replay_capacity", "object_capacity":
	default:
		code = "other"
	}
	o.admission[code]++
}

// Only this closed metadata escapes to a report. Original scope, operation ID,
// text and known Message ID stay in the private in-memory verification journal.
type capacityFailure struct {
	Step     string
	Code     string
	Status   int
	Semantic bool
	Pending  *capacityPendingSend
}

func (e *capacityFailure) Error() string {
	return fmt.Sprintf("%s/%s/status-%d", e.Step, e.Code, e.Status)
}

func capacityFailureAt(step string, status int, body []byte, err error, pending *capacityPendingSend) error {
	var existing *capacityFailure
	if errors.As(err, &existing) {
		copy := *existing
		copy.Step = step
		copy.Pending = pending
		return &copy
	}
	code := "unexpected_response"
	if err != nil {
		var timeout net.Error
		if errors.Is(err, context.Canceled) {
			code = "canceled"
		} else if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
			code = "transport_timeout"
		} else {
			code = "transport_unknown"
		}
	} else {
		var reply struct {
			Error   string `json:"error"`
			Class   string `json:"class"`
			Outcome string `json:"outcome"`
		}
		_ = json.Unmarshal(body, &reply)
		value := reply.Class
		if value == "" {
			value = reply.Error
		}
		switch value {
		case "request_capacity", "replay_capacity", "object_capacity", "resource_limit", "authorization", "invalid_request", "revision_conflict", "payload_conflict", "lease_lost", "restore_pending", "operation_budget", "session_unavailable", "binding_invalid", "object_upload_active", "attachment_content_mismatch":
			code = value
		default:
			if reply.Outcome == "unknown" {
				code = "outcome_unknown"
			}
		}
	}
	return &capacityFailure{Step: step, Code: code, Status: status, Pending: pending}
}

func capacitySemanticFailure(step, code string) error {
	return &capacityFailure{Step: step, Code: code, Status: 200, Semantic: true}
}

func capacityCheckPublicMessages(messages []relay.Message, actor capacityActor) error {
	seen := make(map[string]bool, len(messages))
	for _, message := range messages {
		if message.ID == "" || seen[message.ID] || message.From != model.ActorUser || message.Author != relay.EnterpriseHumanAuthor(actor.auth.UserID) || message.Receipt != "" {
			return capacitySemanticFailure("response", "message_scope_or_identity")
		}
		seen[message.ID] = true
	}
	return nil
}

func capacityCheckBody(step string, body []byte, room capacityRoom, actor capacityActor, limit int) error {
	for _, key := range []string{`"event_encoding"`, `"token_hash"`, `"proof_hash"`, `"access_hash"`, `"refresh_hash"`, `"origin_user"`, `"receipt"`} {
		if bytes.Contains(body, []byte(key)) {
			return capacitySemanticFailure(step, "private_field_disclosure")
		}
	}
	if step == "tail" {
		var tail struct {
			Revision uint64 `json:"revision"`
			relay.TailSnapshot
		}
		if json.Unmarshal(body, &tail) != nil || tail.RoomID != room.id || tail.HostMode != model.HostNative || tail.Revision == 0 || tail.Sequence == 0 || len(tail.Messages) > relay.SnapshotMessageLimit || len(tail.Audit) > relay.SnapshotAuditLimit || tail.TotalMessages < len(tail.Messages) {
			return capacitySemanticFailure(step, "malformed_response")
		}
		return capacityCheckPublicMessages(tail.Messages, actor)
	}
	var page relay.HistoryPage
	if json.Unmarshal(body, &page) != nil || page.Sequence == 0 || len(page.Messages) > limit || page.Total < len(page.Messages) {
		return capacitySemanticFailure(step, "malformed_response")
	}
	return capacityCheckPublicMessages(page.Messages, actor)
}

func capacityVerifyPending(ctx context.Context, f *httpPGFixture, pending []capacityPendingSend) (map[string]int, int) {
	counts := map[string]int{}
	semantic := 0
	for _, intent := range pending {
		result, err := f.store.ResolveHumanSend(ctx, intent.actor.auth, intent.room.id, intent.key, intent.intent)
		if err != nil {
			counts["unconfirmed"]++
			continue
		}
		if result.Outcome == sqlstore.OutcomeNotCommitted {
			counts["not_committed"]++
			continue
		}
		if result.Outcome != sqlstore.OutcomeReplayed {
			counts["unconfirmed"]++
			continue
		}
		message, err := relay.EnterpriseHumanSendMessage(result.Fact, intent.actor.auth.UserID, intent.key, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: model.ActorID(intent.intent.To), Text: intent.intent.Text, AttachmentIDs: intent.intent.AttachmentIDs})
		if err != nil {
			semantic++
			counts["invalid_receipt"]++
			continue
		}
		if intent.cancel != nil {
			if _, err := f.store.ResolveRelayOperation(ctx, *intent.cancel); err != nil {
				counts["cancel_unconfirmed"]++
			}
		}
		snapshot, err := f.store.ReadonlySnapshot(ctx, intent.actor.auth, intent.room.id)
		if err != nil {
			counts["unconfirmed"]++
			continue
		}
		page, err := relay.ProjectEnterpriseHistory(relay.EnterpriseRoomSnapshot{RoomID: snapshot.RoomID, DeploymentID: snapshot.DeploymentID, OrgID: snapshot.OrgID, CreationUserID: snapshot.CreationUserID, HostMode: snapshot.HostMode, Events: snapshot.Events, Seq: snapshot.Seq, Revision: snapshot.Revision}, relay.HistoryQuery{ID: message.ID})
		if err != nil || len(page.Messages) != 1 {
			semantic++
			counts["invalid_history"]++
			continue
		}
		switch page.Messages[0].State {
		case "queued":
			counts["accepted_queued"]++
		case "cancelled":
			counts["accepted_cancelled"]++
		case "handed_off", "peer_changed":
			counts["accepted_terminal"]++
		default:
			counts["accepted_unsettled"]++
		}
	}
	return counts, semantic
}
