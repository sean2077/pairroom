package relay

import (
	"encoding/json"
	"errors"
)

const (
	lanSnapshotMessageBudget = 4 << 20
	// Leave room below the LAN transport's 8 MiB response ceiling. This is a
	// final encoded-size guard, including audit, bindings and delivery data.
	lanSnapshotResponseBudget = 6 << 20
)

// AuthSnapshotTail is the bounded LAN status projection. Budget the encoded
// messages, not their raw text: JSON escaping, review anchors and attachment
// metadata can all expand a response. Message bodies and quotes stay whole;
// exact retained totals and the notice point to paginated history.
func (e *Engine) AuthSnapshotTail(a Auth) (TailSnapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return TailSnapshot{}, err
	}
	start, encodedBytes := len(e.order), 0
	for start > 0 && len(e.order)-start < SnapshotMessageLimit {
		encoded, err := json.Marshal(e.messages[e.order[start-1]])
		if err != nil {
			return TailSnapshot{}, err
		}
		if encodedBytes+len(encoded)+1 > lanSnapshotMessageBudget {
			if start == len(e.order) {
				return TailSnapshot{}, errors.New("newest message exceeds the LAN status budget; inspect its history entry")
			}
			break
		}
		encodedBytes += len(encoded) + 1
		start--
	}
	tail := TailSnapshot{Snapshot: e.snapshotRangeLocked(start, max(0, len(e.audit)-SnapshotAuditLimit)), TotalMessages: len(e.order), TotalAudit: len(e.audit)}
	for slot, binding := range tail.Bindings {
		binding.SessionID = ""
		binding.TranscriptPath = ""
		tail.Bindings[slot] = binding
	}
	tail.Notice += " Status includes a bounded recent window; total_messages and total_audit describe retained history. Use history pages or a message ID for older evidence; this view never claims or replays work."
	encoded, err := json.Marshal(tail)
	if err != nil {
		return TailSnapshot{}, err
	}
	if len(encoded)+1 > lanSnapshotResponseBudget {
		return TailSnapshot{}, errors.New("LAN status metadata exceeds its encoded response budget; use summary and history")
	}
	return tail, nil
}
