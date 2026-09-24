package room

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

const (
	// turnSummaryCheckpointInterval bounds how often an in-progress summary is
	// rewritten into the Event Log. Every checkpoint is a complete summary, so
	// persisting on each tool event made log growth quadratic in tool calls.
	turnSummaryCheckpointInterval = 30 * time.Second
	// turnSummaryTransientInterval throttles live-only summary publication for
	// high-frequency diff/usage telemetry so bursts cannot overflow SSE buffers.
	turnSummaryTransientInterval = time.Second
	// turnItemDataLimit bounds the inline payload copy kept only when an item
	// has no durable source record (the source append failed).
	turnItemDataLimit = 4 << 10
	// turnItemPreviewLimit bounds the inline text preview of an item whose full
	// evidence is loaded on demand from its source records.
	turnItemPreviewLimit = 512
	// turnItemSourceLimit bounds source references per item; native tools
	// report a start and a completion, occasionally a few progress updates.
	turnItemSourceLimit = 16
	// turnItemEvidenceBudget bounds one on-demand evidence response.
	turnItemEvidenceBudget = 4 << 20
	// turnSummaryByteBudget bounds one serialized summary regardless of item
	// count, so a single checkpoint record cannot reach hundreds of KiB.
	turnSummaryByteBudget = 256 << 10
)

// turnSummaryTracking is guarded by Engine.summaryMu. It is live bookkeeping
// only: after restart the next event on a known Turn simply checkpoints again.
type turnSummaryTracking struct {
	persistedAt map[string]time.Time
	publishedAt map[string]time.Time
	dirty       map[string]bool
}

// Turn projection keeps bounded inspector details separate from native ownership
// and message routing. Text remains in the complete Message/Event Log authority.
//
// The summary is a checkpointed projection, not an auditable transition: the
// raw runtime.event facts are recorded before projection. Creation, turn start,
// final text, errors and terminal states persist immediately; other updates stay
// in memory, are published as transient (sequence-zero) events, and persist at
// most once per checkpoint interval or when the adapter stops or the Room closes.
//
// sourceSeq is the durable sequence of runtimeEvent's own record, or zero when
// it was not recorded (transient telemetry or a failed append).
func (e *Engine) projectTurnSummary(runtimeEvent model.RuntimeEvent, sourceSeq uint64) {
	// These events do not change the summary. In particular, text.delta must
	// not scan history or clone all tool details for every generated token.
	switch runtimeEvent.Kind {
	case model.RuntimeTextDelta, model.RuntimeState, model.RuntimeSession, model.RuntimeInfoUpdated:
		return
	}
	if !runtimeEvent.Agent.ValidParticipant() || strings.TrimSpace(runtimeEvent.TurnID) == "" {
		return
	}
	// Serialize read-modify-persist across adapters and the checkpoint flusher,
	// so an older dirty copy can never be recorded after a newer terminal one.
	// Lock order: summaryMu before mu (record takes mu).
	e.summaryMu.Lock()
	defer e.summaryMu.Unlock()
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
	created := summary.ID == ""
	if created {
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

	// immediate marks milestones that must be durable without waiting for a
	// checkpoint. live selects which in-memory updates reach the UI as a
	// transient event: every event kind that used to publish a durable summary
	// still publishes, and diff/usage are additionally throttled.
	immediate := created
	live, throttled := true, false
	switch runtimeEvent.Kind {
	case model.RuntimeTurnStarted:
		summary.Status = "working"
		immediate = true
	case model.RuntimeToolStarted:
		upsertTurnItem(&summary, runtimeEvent, sourceSeq, "tool", "working")
	case model.RuntimeToolCompleted:
		upsertTurnItem(&summary, runtimeEvent, sourceSeq, "tool", "completed")
	case model.RuntimeCommandOutput:
		item := findOrCreateTurnItem(&summary, runtimeEvent.ItemID, "command")
		item.Status = "working"
		item.Detail = boundedTail(item.Detail+runtimeEvent.Text, 12<<10)
		// Command chunks already stream through their own transient runtime events.
		live = false
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
		throttled = true
	case model.RuntimeUsageUpdated:
		summary.Usage = boundedRaw(runtimeEvent.Data, 16<<10)
		throttled = true
	case model.RuntimeFinal:
		summary.FinalText = boundedTail(runtimeEvent.Text, 48<<10)
		immediate = true
	case model.RuntimeInputCancelled:
		summary.Status = "cancelled"
		summary.Error = boundedTail(runtimeEvent.Text, 8<<10)
		immediate = true
	case model.RuntimeInputFailed:
		summary.Status = "failed"
		summary.Error = boundedTail(runtimeEvent.Text, 8<<10)
		immediate = true
	case model.RuntimeError:
		// Keep diagnostic errors visible without declaring the native Turn
		// terminal. A later input.failed or turn.completed owns final status.
		summary.Error = boundedTail(runtimeEvent.Text, 8<<10)
		immediate = true
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
		immediate = true
	default:
		// Unrelated adapter status notifications only refresh the heartbeat.
		if runtimeEvent.Kind == model.RuntimeLog || runtimeEvent.Kind == model.RuntimeApprovalRequested || runtimeEvent.Kind == model.RuntimeApprovalResolved {
			live = false
		}
	}
	if len(summary.Items) > 128 {
		summary.Items = append([]model.TurnWorkItem(nil), summary.Items[len(summary.Items)-128:]...)
	}
	enforceTurnSummaryBudget(&summary)

	tracking := e.turnSummaryTrackingLocked()
	now := runtimeEvent.CreatedAt
	persistedAt, persisted := tracking.persistedAt[id]
	if immediate || !persisted || now.Sub(persistedAt) >= turnSummaryCheckpointInterval {
		_ = e.persistTurnSummaryLocked(summary, now)
		return
	}
	e.mu.Lock()
	replaceTurnSummaryLocked(&e.snapshot, summary)
	e.mu.Unlock()
	tracking.dirty[id] = true
	if !live {
		return
	}
	if throttled {
		if last, ok := tracking.publishedAt[id]; ok && now.Sub(last) < turnSummaryTransientInterval {
			return
		}
	}
	tracking.publishedAt[id] = now
	e.publishTransientTurnSummary(summary)
}

func (e *Engine) turnSummaryTrackingLocked() *turnSummaryTracking {
	if e.turnSummaries.dirty == nil {
		e.turnSummaries = turnSummaryTracking{
			persistedAt: make(map[string]time.Time),
			publishedAt: make(map[string]time.Time),
			dirty:       make(map[string]bool),
		}
	}
	return &e.turnSummaries
}

// persistTurnSummaryLocked records one complete checkpoint. Callers hold
// summaryMu. A store failure keeps the newer projection visible in memory and
// dirty; record has already marked the Room store fatal and surfaced it.
func (e *Engine) persistTurnSummaryLocked(summary model.TurnSummary, at time.Time) error {
	tracking := e.turnSummaryTrackingLocked()
	if _, err := e.record(EventTurnSummaryUpdated, summary.Agent, summary); err != nil {
		e.mu.Lock()
		replaceTurnSummaryLocked(&e.snapshot, summary)
		e.mu.Unlock()
		tracking.dirty[summary.ID] = true
		return err
	}
	delete(tracking.dirty, summary.ID)
	if summary.CompletedAt != nil {
		// A completed Turn receives no further projection work.
		delete(tracking.persistedAt, summary.ID)
		delete(tracking.publishedAt, summary.ID)
		return nil
	}
	tracking.persistedAt[summary.ID] = at
	tracking.publishedAt[summary.ID] = at
	return nil
}

// flushTurnSummaries persists dirty in-progress summaries for actor (both
// participants when actor is empty). A non-zero now persists only summaries
// whose checkpoint is due; a zero now forces every dirty summary, which Room
// close and adapter stops use before the native process can no longer report.
func (e *Engine) flushTurnSummaries(actor model.ActorID, now time.Time) error {
	e.summaryMu.Lock()
	defer e.summaryMu.Unlock()
	tracking := e.turnSummaryTrackingLocked()
	if len(tracking.dirty) == 0 {
		return nil
	}
	e.mu.RLock()
	var due []model.TurnSummary
	for _, summary := range e.snapshot.Turns {
		if !tracking.dirty[summary.ID] || (actor != "" && summary.Agent != actor) {
			continue
		}
		if !now.IsZero() && now.Sub(tracking.persistedAt[summary.ID]) < turnSummaryCheckpointInterval {
			continue
		}
		due = append(due, cloneTurnSummary(summary))
	}
	e.mu.RUnlock()
	var result error
	for _, summary := range due {
		at := now
		if at.IsZero() {
			at = time.Now().UTC()
		}
		if err := e.persistTurnSummaryLocked(summary, at); err != nil {
			result = errors.Join(result, fmt.Errorf("checkpoint turn summary %s: %w", summary.ID, err))
		}
	}
	return result
}

// publishTransientTurnSummary delivers a live-only summary update. Sequence
// zero never advances the durable SSE cursor and is not replayed.
func (e *Engine) publishTransientTurnSummary(summary model.TurnSummary) {
	if e.cfg.Hub == nil {
		return
	}
	e.mu.RLock()
	roomID := e.snapshot.Meta.ID
	e.mu.RUnlock()
	event, err := model.NewEvent(roomID, EventTurnSummaryUpdated, summary.Agent, summary)
	if err != nil {
		return
	}
	event.Seq = 0
	e.cfg.Hub.Publish(event)
}

// enforceTurnSummaryBudget keeps one serialized summary within
// turnSummaryByteBudget. Plan, diff, final text and error keep their own
// limits. Evidence is shed oldest-first — an item's payload, then its detail —
// so the newest work stays fully inspectable; item metadata is kept, and only
// if metadata alone still exceeds the budget are the oldest items dropped.
func enforceTurnSummaryBudget(summary *model.TurnSummary) {
	for i := range summary.Items {
		if turnSummaryApproxBytes(*summary) <= turnSummaryByteBudget {
			break
		}
		summary.Items[i].Data = nil
		if turnSummaryApproxBytes(*summary) <= turnSummaryByteBudget {
			break
		}
		summary.Items[i].Detail = ""
	}
	for len(summary.Items) > 0 && turnSummaryApproxBytes(*summary) > turnSummaryByteBudget {
		summary.Items = append([]model.TurnWorkItem(nil), summary.Items[1:]...)
	}
	// The estimate ignores JSON escaping; confirm with the real encoding.
	for len(summary.Items) > 0 {
		encoded, err := json.Marshal(summary)
		if err == nil && len(encoded) <= turnSummaryByteBudget {
			return
		}
		summary.Items = append([]model.TurnWorkItem(nil), summary.Items[1:]...)
	}
}

func turnSummaryApproxBytes(summary model.TurnSummary) int {
	const overhead = 512
	size := overhead + len(summary.Plan) + len(summary.Diff) + len(summary.FinalText) + len(summary.Error) + len(summary.Usage)
	for _, id := range summary.MessageIDs {
		size += len(id) + 4
	}
	for _, item := range summary.Items {
		size += 160 + len(item.ID) + len(item.Kind) + len(item.Name) + len(item.Status) + len(item.Detail) + len(item.Data)
	}
	return size
}

// upsertTurnItem records a tool event. When the event has a durable record,
// the item keeps only a short preview plus a reference to that record, so a
// summary checkpoint no longer copies every tool payload; the inspector loads
// the full evidence on demand. Without a source record the bounded evidence
// stays inline, as before.
func upsertTurnItem(summary *model.TurnSummary, event model.RuntimeEvent, sourceSeq uint64, kind, status string) {
	item := findOrCreateTurnItem(summary, event.ItemID, kind)
	if event.Name != "" {
		item.Name = event.Name
	}
	item.Status = status
	if item.StartedAt.IsZero() {
		item.StartedAt = event.CreatedAt
	}
	hasEvidence := event.Text != "" || len(event.Data) > 0
	if sourceSeq > 0 && hasEvidence {
		if event.Text != "" {
			item.Detail = boundedHead(event.Text, turnItemPreviewLimit)
		}
		item.Data = nil
		if !containsSeq(item.SourceSeqs, sourceSeq) {
			item.SourceSeqs = append(item.SourceSeqs, sourceSeq)
			if len(item.SourceSeqs) > turnItemSourceLimit {
				// Keep the first (the call) and the most recent records.
				item.SourceSeqs = append(item.SourceSeqs[:1:1], item.SourceSeqs[len(item.SourceSeqs)-turnItemSourceLimit+1:]...)
			}
		}
	} else {
		if event.Text != "" {
			item.Detail = boundedTail(event.Text, 12<<10)
		}
		if len(event.Data) > 0 {
			item.Data = boundedRaw(event.Data, turnItemDataLimit)
		}
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

func containsSeq(values []uint64, target uint64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// boundedHead keeps the beginning of a preview: a tool's call summary is at
// its start, unlike streamed output whose newest tail matters most.
func boundedHead(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit < len("…") {
		return ""
	}
	end := limit - len("…")
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "…"
}

// ErrTurnItemNotFound reports an unknown Turn summary or work item.
var ErrTurnItemNotFound = errors.New("turn item not found")

// TurnItemEvidence loads the durable runtime events behind one work item. It
// verifies each record's kind, participant, Turn and item before returning it,
// and bounds the response; an item without source records returns none (its
// evidence is inline in the summary).
func (e *Engine) TurnItemEvidence(summaryID, itemID string) ([]model.TurnItemEvidence, error) {
	e.mu.RLock()
	var summary *model.TurnSummary
	for i := range e.snapshot.Turns {
		if e.snapshot.Turns[i].ID == summaryID {
			summary = &e.snapshot.Turns[i]
			break
		}
	}
	var seqs []uint64
	var agent model.ActorID
	var turnID string
	found := false
	if summary != nil {
		agent, turnID = summary.Agent, summary.TurnID
		for _, item := range summary.Items {
			if item.ID == itemID {
				seqs = append([]uint64(nil), item.SourceSeqs...)
				found = true
				break
			}
		}
	}
	e.mu.RUnlock()
	if !found {
		return nil, ErrTurnItemNotFound
	}
	out := make([]model.TurnItemEvidence, 0, len(seqs))
	budget := turnItemEvidenceBudget
	for _, seq := range seqs {
		event, err := e.cfg.Store.ReadEvent(seq)
		if err != nil {
			return nil, err
		}
		if event.Kind != EventRuntime || event.Actor != agent {
			return nil, fmt.Errorf("event %d is not a runtime record of %s", seq, agent)
		}
		var runtimeEvent model.RuntimeEvent
		if err := json.Unmarshal(event.Data, &runtimeEvent); err != nil {
			return nil, fmt.Errorf("decode runtime event %d: %w", seq, err)
		}
		if runtimeEvent.Agent != agent || runtimeEvent.TurnID != turnID || runtimeEvent.ItemID != itemID {
			return nil, fmt.Errorf("event %d does not belong to item %s", seq, itemID)
		}
		evidence := model.TurnItemEvidence{
			Seq: seq, Kind: runtimeEvent.Kind, Name: runtimeEvent.Name,
			Text: runtimeEvent.Text, Data: runtimeEvent.Data, CreatedAt: runtimeEvent.CreatedAt,
		}
		if size := len(evidence.Text) + len(evidence.Data); size > budget {
			evidence.Text = boundedHead(evidence.Text, max(0, budget))
			evidence.Data = nil
			evidence.Truncated = true
		}
		budget = max(0, budget-len(evidence.Text)-len(evidence.Data))
		out = append(out, evidence)
	}
	return out, nil
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
