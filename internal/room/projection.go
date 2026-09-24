package room

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func (e *Engine) Snapshot() model.RoomSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneSnapshot(e.snapshot)
}

// snapshotTurnWindow is the number of newest Turns a windowed snapshot
// carries in addition to unfinished and message-correlated Turns. It matches
// the Work inspector's visible limit.
const snapshotTurnWindow = 40

// WindowedSnapshot returns the newest messages while retaining current room and
// runtime state. The authoritative in-memory/event-sourced transcript remains
// complete; this is only a transport optimization for long-lived rooms. Turns
// are limited to the newest snapshotTurnWindow, each participant's current
// Turn, and Turns correlated with the returned messages; turn.summary.updated
// payloads are omitted from the event tail because `turns` already carries them.
func (e *Engine) WindowedSnapshot(limit int) model.RoomSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	view := e.snapshot
	total := len(view.Messages)
	if limit <= 0 || limit > 1000 {
		limit = 250
	}
	if total > limit {
		view.Messages = view.Messages[total-limit:]
	}
	view.Turns = selectTurnsLocked(e.snapshot.Turns, view.Messages, currentTurnIDsLocked(e.snapshot.Participants), snapshotTurnWindow)
	view.Events = SnapshotEventTail(view.Events)
	// Slice before cloning: transport cost must scale with the requested page,
	// not with messages or Turns that will immediately be discarded.
	snapshot := cloneSnapshot(view)
	window := &model.MessageWindow{Total: total, Loaded: len(snapshot.Messages), HasMore: total > len(snapshot.Messages)}
	if len(snapshot.Messages) > 0 {
		window.OldestSeq = snapshot.Messages[0].Seq
	}
	snapshot.MessageWindow = window
	snapshot.TurnWindow = &model.TurnWindow{Total: len(e.snapshot.Turns), Loaded: len(snapshot.Turns)}
	return snapshot
}

// snapshotEventTextLimit bounds each runtime event's text and payload in a
// snapshot's event tail. The inspector shows at most the first 1800
// characters of either; full records come from item evidence or the forensic
// export.
const snapshotEventTextLimit = 4 << 10

// SnapshotEventTail projects the recent event tail for a snapshot: it omits
// summary checkpoints (a snapshot's `turns` is their current projection) and
// bounds each runtime event's text and payload. The tail is a presentation
// aid, not the cursor authority (latest_seq is), and the engine's own events
// are never modified. Forensic exports use the unprojected tail.
func SnapshotEventTail(events []model.Event) []model.Event {
	out := make([]model.Event, 0, len(events))
	for _, event := range events {
		if event.Kind == EventTurnSummaryUpdated {
			continue
		}
		if event.Kind == EventRuntime && len(event.Data) > 2*snapshotEventTextLimit {
			var runtimeEvent model.RuntimeEvent
			if json.Unmarshal(event.Data, &runtimeEvent) == nil {
				runtimeEvent.Text = boundedHead(runtimeEvent.Text, snapshotEventTextLimit)
				if len(runtimeEvent.Data) > snapshotEventTextLimit {
					runtimeEvent.Data, _ = json.Marshal(map[string]any{
						"truncated": true,
						"head":      boundedHead(string(runtimeEvent.Data), snapshotEventTextLimit),
					})
				}
				if data, err := json.Marshal(runtimeEvent); err == nil {
					event.Data = data
				}
			}
		}
		out = append(out, event)
	}
	return out
}

// currentTurnIDsLocked returns the summary IDs of Turns a participant owns now.
// Ownership, not a missing completion time, marks a live Turn: a summary whose
// adapter died before turn.completed stays incomplete forever, while
// CurrentTurn is cleared on stop, permission replacement and restart.
func currentTurnIDsLocked(participants map[model.ActorID]model.ParticipantSnapshot) map[string]struct{} {
	current := make(map[string]struct{}, len(participants))
	for actor, participant := range participants {
		if participant.CurrentTurn != "" {
			current[string(actor)+":"+participant.CurrentTurn] = struct{}{}
		}
	}
	return current
}

// selectTurnsLocked returns, in projection order, the union of the newest
// `newest` Turns, currently owned Turns, and Turns correlated with messages.
// The result shares elements with turns; callers clone before exposing it.
func selectTurnsLocked(turns []model.TurnSummary, messages []model.Message, current map[string]struct{}, newest int) []model.TurnSummary {
	if len(turns) <= newest {
		return turns
	}
	keep := make([]bool, len(turns))
	order := make([]int, len(turns))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return turnRecency(turns[order[a]]).After(turnRecency(turns[order[b]]))
	})
	for _, index := range order[:newest] {
		keep[index] = true
	}
	related := make(map[string]struct{}, len(messages))
	for _, message := range messages {
		related[message.ID] = struct{}{}
	}
	for i, turn := range turns {
		if _, live := current[turn.ID]; live {
			keep[i] = true
			continue
		}
		for _, id := range turn.MessageIDs {
			if _, ok := related[id]; ok {
				keep[i] = true
				break
			}
		}
	}
	out := make([]model.TurnSummary, 0, newest)
	for i, turn := range turns {
		if keep[i] {
			out = append(out, turn)
		}
	}
	return out
}

func turnRecency(turn model.TurnSummary) time.Time {
	if !turn.UpdatedAt.IsZero() {
		return turn.UpdatedAt
	}
	return turn.StartedAt
}

// MessagesPage returns messages immediately before beforeSeq. A zero cursor
// addresses the newest page. Results remain chronological for direct merging.
func (e *Engine) MessagesPage(beforeSeq uint64, limit int) model.MessagePage {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	end := len(e.snapshot.Messages)
	if beforeSeq > 0 {
		end = sort.Search(end, func(i int) bool {
			return e.snapshot.Messages[i].Seq >= beforeSeq
		})
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	messages := make([]model.Message, end-start)
	for i := start; i < end; i++ {
		messages[i-start] = cloneMessage(e.snapshot.Messages[i])
	}
	page := model.MessagePage{Messages: messages, Total: len(e.snapshot.Messages), HasMore: start > 0}
	if len(messages) > 0 {
		page.OldestSeq = messages[0].Seq
	}
	related := make(map[string]struct{}, len(messages))
	for _, message := range messages {
		related[message.ID] = struct{}{}
	}
	for _, turn := range e.snapshot.Turns {
		for _, id := range turn.MessageIDs {
			if _, ok := related[id]; ok {
				page.Turns = append(page.Turns, cloneTurnSummary(turn))
				break
			}
		}
	}
	return page
}

// ReplayEvents copies only the bounded durable event tail, never the transcript.
// The caller must subscribe before reading it and deduplicate by sequence.
func (e *Engine) ReplayEvents() ([]model.Event, uint64) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	events := make([]model.Event, len(e.snapshot.Events))
	for i, event := range e.snapshot.Events {
		events[i] = event
		events[i].Data = append(json.RawMessage(nil), event.Data...)
	}
	return events, e.snapshot.LatestSeq
}

// Busy is the control-plane activity query. Read under the projection lock
// without cloning history for each management poll or runtime drain check.
// Include queued processing even when neither native participant is active.
func (e *Engine) Busy() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, participant := range e.snapshot.Participants {
		if participant.CurrentTurn != "" {
			return true
		}
		switch participant.State {
		case model.StateStarting, model.StateWorking, model.StateWaiting:
			return true
		}
	}
	for _, approval := range e.snapshot.Approvals {
		if approval.Status == "pending" {
			return true
		}
	}
	for _, message := range e.snapshot.Messages {
		for _, state := range message.Processing {
			if state == model.ProcessingWaiting || state == model.ProcessingWorking {
				return true
			}
		}
	}
	return false
}

func cloneSnapshot(in model.RoomSnapshot) model.RoomSnapshot {
	out := in
	out.Meta.Collaboration = model.CloneCollaboration(in.Meta.Collaboration)
	out.Messages = make([]model.Message, len(in.Messages))
	for i, message := range in.Messages {
		out.Messages[i] = cloneMessage(message)
	}
	if in.MessageWindow != nil {
		window := *in.MessageWindow
		out.MessageWindow = &window
	}
	out.Approvals = make([]model.Approval, len(in.Approvals))
	for i, approval := range in.Approvals {
		out.Approvals[i] = approval
		out.Approvals[i].Detail = append(json.RawMessage(nil), approval.Detail...)
		out.Approvals[i].ResolvedAt = cloneTime(approval.ResolvedAt)
	}
	out.Turns = make([]model.TurnSummary, len(in.Turns))
	for i, summary := range in.Turns {
		out.Turns[i] = cloneTurnSummary(summary)
	}
	out.Participants = make(map[model.ActorID]model.ParticipantSnapshot, len(in.Participants))
	for key, value := range in.Participants {
		value.Runtime = cloneRuntimeInfo(value.Runtime)
		out.Participants[key] = value
	}
	out.Events = make([]model.Event, len(in.Events))
	for i, event := range in.Events {
		out.Events[i] = event
		out.Events[i].Data = append(json.RawMessage(nil), event.Data...)
	}
	return out
}

func cloneMessage(message model.Message) model.Message {
	out := message
	out.To = append([]model.ActorID(nil), message.To...)
	out.Attachments = append([]model.Attachment(nil), message.Attachments...)
	out.Delivery = cloneDelivery(message.Delivery)
	out.DeliveryDetail = cloneDetails(message.DeliveryDetail)
	out.Processing = cloneProcessing(message.Processing)
	out.ProcessingDetail = cloneDetails(message.ProcessingDetail)
	out.ProcessingTurn = cloneDetails(message.ProcessingTurn)
	out.ProcessingLastUpdatedAt = cloneTimes(message.ProcessingLastUpdatedAt)
	return out
}

func cloneTurnSummary(in model.TurnSummary) model.TurnSummary {
	out := in
	out.CompletedAt = cloneTime(in.CompletedAt)
	out.MessageIDs = append([]string(nil), in.MessageIDs...)
	out.Usage = append(json.RawMessage(nil), in.Usage...)
	out.Items = make([]model.TurnWorkItem, len(in.Items))
	for i, item := range in.Items {
		out.Items[i] = item
		out.Items[i].Data = append(json.RawMessage(nil), item.Data...)
		out.Items[i].CompletedAt = cloneTime(item.CompletedAt)
		out.Items[i].SourceSeqs = append([]uint64(nil), item.SourceSeqs...)
	}
	return out
}

func cloneRuntimeInfo(in model.RuntimeInfo) model.RuntimeInfo {
	out := in
	out.Capabilities = append([]string(nil), in.Capabilities...)
	out.Warnings = append([]string(nil), in.Warnings...)
	out.Data = append(json.RawMessage(nil), in.Data...)
	return out
}

func cloneProcessing(in map[model.ActorID]model.ProcessingState) map[model.ActorID]model.ProcessingState {
	if in == nil {
		return nil
	}
	out := make(map[model.ActorID]model.ProcessingState, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneTimes(in map[model.ActorID]time.Time) map[model.ActorID]time.Time {
	if in == nil {
		return nil
	}
	out := make(map[model.ActorID]time.Time, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneDelivery(in map[model.ActorID]model.DeliveryState) map[model.ActorID]model.DeliveryState {
	if in == nil {
		return nil
	}
	out := make(map[model.ActorID]model.DeliveryState, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneDetails(in map[model.ActorID]string) map[model.ActorID]string {
	if in == nil {
		return nil
	}
	out := make(map[model.ActorID]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// cloneTime detaches optional timestamps along with the rest of a snapshot.
func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
