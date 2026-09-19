package relay

import "github.com/sean2077/pairroom/internal/model"

const (
	SnapshotMessageLimit = 300
	SnapshotAuditLimit   = 80
	SnapshotTextBudget   = 1 << 20
)

// TailSnapshot is a bounded browser projection, not an inbox or an export.
// Totals describe retained history; omitted messages have not been deleted.
type TailSnapshot struct {
	Snapshot
	TotalMessages int `json:"total_messages"`
	TotalAudit    int `json:"total_audit"`
}

// SnapshotTail copies only the recent window, with complete message/quote text.
// Budget the text as well as the count: 300 maximum-sized replies are not a
// small response. Always retain the newest message; never clip its content.
func (e *Engine) SnapshotTail() TailSnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	start, textBytes := len(e.order), 0
	for start > 0 && len(e.order)-start < SnapshotMessageLimit {
		m := e.messages[e.order[start-1]]
		size := len(m.Text)
		if m.Quote != nil {
			size += len(m.Quote.Text)
		}
		if start < len(e.order) && textBytes+size > SnapshotTextBudget {
			break
		}
		start--
		textBytes += size
	}
	return TailSnapshot{
		Snapshot:      e.snapshotRangeLocked(start, max(0, len(e.audit)-SnapshotAuditLimit)),
		TotalMessages: len(e.order),
		TotalAudit:    len(e.audit),
	}
}

func (e *Engine) snapshotRangeLocked(messageStart, auditStart int) Snapshot {
	s := Snapshot{HostMode: model.HostNative, RoomID: e.cfg.RoomID, Bindings: map[model.ActorID]Binding{}, Messages: make([]Message, 0, len(e.order)-messageStart), Audit: append([]Audit(nil), e.audit[auditStart:]...), Sequence: e.sequence, Notice: "handed_off means CLI stdout was written, not native acceptance. Interrupted replies and crashes before atomic publication may be undetectably lost. Park is bounded; queue and nudge/wait outside its window. Native work remains user-owned."}
	for slot, b := range e.bindings {
		s.Bindings[slot] = b.Binding
	}
	for _, id := range e.order[messageStart:] {
		s.Messages = append(s.Messages, cloneMessage(e.messages[id]))
	}
	return s
}
