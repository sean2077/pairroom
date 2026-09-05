package room

import (
	"encoding/json"
	"sort"

	"github.com/sean2077/pairroom/internal/model"
)

func (e *Engine) Snapshot() model.RoomSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneSnapshot(e.snapshot)
}

// WindowedSnapshot returns the newest messages while retaining full room and
// runtime state. The authoritative in-memory/event-sourced transcript remains
// complete; this is only a transport optimization for long-lived rooms.
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
	// Slice before cloning: transport cost must scale with the requested page,
	// not with messages that will immediately be discarded.
	snapshot := cloneSnapshot(view)
	window := &model.MessageWindow{Total: total, Loaded: len(snapshot.Messages), HasMore: total > len(snapshot.Messages)}
	if len(snapshot.Messages) > 0 {
		window.OldestSeq = snapshot.Messages[0].Seq
	}
	snapshot.MessageWindow = window
	return snapshot
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
