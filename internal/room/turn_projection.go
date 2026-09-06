package room

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

// Turn projection keeps bounded inspector details separate from native ownership
// and message routing. Text remains in the complete Message/Event Log authority.
func (e *Engine) projectTurnSummary(runtimeEvent model.RuntimeEvent) {
	// These events do not change the summary. In particular, text.delta must
	// not scan history or clone all tool details for every generated token.
	switch runtimeEvent.Kind {
	case model.RuntimeTextDelta, model.RuntimeState, model.RuntimeSession, model.RuntimeInfoUpdated:
		return
	}
	if !runtimeEvent.Agent.ValidParticipant() || strings.TrimSpace(runtimeEvent.TurnID) == "" {
		return
	}
	id := string(runtimeEvent.Agent) + ":" + runtimeEvent.TurnID
	e.mu.RLock()
	var summary model.TurnSummary
	for _, existing := range e.snapshot.Turns {
		if existing.ID == id {
			summary = cloneTurnSummary(existing)
			break
		}
	}
	e.mu.RUnlock()
	if summary.ID == "" {
		summary = model.TurnSummary{
			ID: id, Agent: runtimeEvent.Agent, TurnID: runtimeEvent.TurnID,
			Status: "working", StartedAt: runtimeEvent.CreatedAt,
		}
	}
	if summary.StartedAt.IsZero() {
		summary.StartedAt = runtimeEvent.CreatedAt
	}
	summary.UpdatedAt = runtimeEvent.CreatedAt
	if runtimeEvent.SessionID != "" {
		summary.SessionID = runtimeEvent.SessionID
	}
	if runtimeEvent.CorrelationID != "" && !containsString(summary.MessageIDs, runtimeEvent.CorrelationID) {
		summary.MessageIDs = append(summary.MessageIDs, runtimeEvent.CorrelationID)
	}

	persist := true
	switch runtimeEvent.Kind {
	case model.RuntimeTurnStarted:
		summary.Status = "working"
	case model.RuntimeToolStarted:
		upsertTurnItem(&summary, runtimeEvent, "tool", "working")
	case model.RuntimeToolCompleted:
		upsertTurnItem(&summary, runtimeEvent, "tool", "completed")
	case model.RuntimeCommandOutput:
		item := findOrCreateTurnItem(&summary, runtimeEvent.ItemID, "command")
		item.Status = "working"
		item.Detail = boundedTail(item.Detail+runtimeEvent.Text, 12<<10)
		persist = false
	case model.RuntimePlanUpdated:
		if runtimeEvent.Text != "" {
			summary.Plan = boundedTail(summary.Plan+runtimeEvent.Text, 24<<10)
		} else if len(runtimeEvent.Data) > 0 {
			summary.Plan = boundedTail(string(runtimeEvent.Data), 24<<10)
		}
	case model.RuntimeDiffUpdated:
		if runtimeEvent.Text != "" {
			summary.Diff = boundedTail(runtimeEvent.Text, 48<<10)
		} else if len(runtimeEvent.Data) > 0 {
			summary.Diff = boundedTail(string(runtimeEvent.Data), 48<<10)
		}
	case model.RuntimeUsageUpdated:
		summary.Usage = boundedRaw(runtimeEvent.Data, 16<<10)
	case model.RuntimeFinal:
		summary.FinalText = boundedTail(runtimeEvent.Text, 48<<10)
	case model.RuntimeInputCancelled:
		summary.Status = "cancelled"
		summary.Error = boundedTail(runtimeEvent.Text, 8<<10)
	case model.RuntimeInputFailed:
		summary.Status = "failed"
		summary.Error = boundedTail(runtimeEvent.Text, 8<<10)
	case model.RuntimeError:
		// Keep diagnostic errors visible without declaring the native Turn
		// terminal. A later input.failed or turn.completed owns final status.
		summary.Error = boundedTail(runtimeEvent.Text, 8<<10)
	case model.RuntimeTurnCompleted:
		for i := range summary.Items {
			if summary.Items[i].CompletedAt == nil && summary.Items[i].Status == "working" {
				summary.Items[i].Status = "completed"
				completed := runtimeEvent.CreatedAt
				summary.Items[i].CompletedAt = &completed
			}
		}
		status := strings.TrimSpace(runtimeEvent.Name)
		if status == "" || status == "completed" || status == "success" {
			status = "completed"
		}
		if summary.Status != "failed" && summary.Status != "cancelled" {
			summary.Status = status
		}
		completed := runtimeEvent.CreatedAt
		summary.CompletedAt = &completed
		summary.DurationMillis = max(int64(0), completed.Sub(summary.StartedAt).Milliseconds())
	default:
		// Retain a summary heartbeat for durable turn-scoped events, but avoid
		// creating extra events for unrelated adapter status notifications.
		if runtimeEvent.Kind == model.RuntimeLog || runtimeEvent.Kind == model.RuntimeApprovalRequested || runtimeEvent.Kind == model.RuntimeApprovalResolved {
			persist = false
		}
	}
	if len(summary.Items) > 128 {
		summary.Items = append([]model.TurnWorkItem(nil), summary.Items[len(summary.Items)-128:]...)
	}
	if persist {
		_, _ = e.record(EventTurnSummaryUpdated, runtimeEvent.Agent, summary)
		return
	}
	e.mu.Lock()
	replaceTurnSummaryLocked(&e.snapshot, summary)
	e.mu.Unlock()
}

func upsertTurnItem(summary *model.TurnSummary, event model.RuntimeEvent, kind, status string) {
	item := findOrCreateTurnItem(summary, event.ItemID, kind)
	if event.Name != "" {
		item.Name = event.Name
	}
	item.Status = status
	if item.StartedAt.IsZero() {
		item.StartedAt = event.CreatedAt
	}
	if event.Text != "" {
		item.Detail = boundedTail(event.Text, 12<<10)
	}
	if len(event.Data) > 0 {
		item.Data = boundedRaw(event.Data, 16<<10)
	}
	if status == "completed" || status == "failed" || status == "cancelled" {
		completed := event.CreatedAt
		item.CompletedAt = &completed
	}
}

func findOrCreateTurnItem(summary *model.TurnSummary, itemID, kind string) *model.TurnWorkItem {
	if itemID == "" {
		itemID = kind + "-" + fmt.Sprint(len(summary.Items)+1)
	}
	for i := range summary.Items {
		if summary.Items[i].ID == itemID {
			if summary.Items[i].Kind == "" {
				summary.Items[i].Kind = kind
			}
			return &summary.Items[i]
		}
	}
	summary.Items = append(summary.Items, model.TurnWorkItem{ID: itemID, Kind: kind, Status: "working"})
	return &summary.Items[len(summary.Items)-1]
}

func boundedTail(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	marker := "…"
	if limit < len(marker) {
		marker = ""
	}
	start := len(value) - (limit - len(marker))
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return marker + value[start:]
}

func boundedRaw(value json.RawMessage, limit int) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	if len(value) <= limit {
		return append(json.RawMessage(nil), value...)
	}
	wrapped, _ := json.Marshal(map[string]any{
		"truncated": true,
		"tail":      boundedTail(string(value), limit-64),
	})
	return wrapped
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func replaceTurnSummaryLocked(snapshot *model.RoomSnapshot, summary model.TurnSummary) {
	for i := range snapshot.Turns {
		if snapshot.Turns[i].ID == summary.ID {
			snapshot.Turns[i] = summary
			return
		}
	}
	snapshot.Turns = append(snapshot.Turns, summary)
}
