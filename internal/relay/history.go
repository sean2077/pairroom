package relay

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const HistoryPageLimit = 100
const historyScanLimit = 5000

// HistoryQuery reads evidence only. Cursors are stable publication ordinals,
// not offsets in a shrinking unresolved list. Pages never claim or replay work.
type HistoryQuery struct {
	ID      string    `json:"id,omitempty"`
	Cursor  string    `json:"cursor,omitempty"`
	Limit   int       `json:"limit,omitempty"`
	Pending bool      `json:"pending,omitempty"`
	Since   time.Time `json:"since,omitempty"`
}

type HistoryPage struct {
	Messages   []Message `json:"messages"`
	NextCursor string    `json:"next_cursor,omitempty"`
	HasMore    bool      `json:"has_more"`
	Sequence   uint64    `json:"sequence"`
	Total      int       `json:"total"`
	Notice     string    `json:"notice"`
}

func (e *Engine) History(q HistoryQuery) (HistoryPage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.available(); err != nil {
		return HistoryPage{}, err
	}
	return e.historyLocked(q)
}

func (e *Engine) AuthHistory(a Auth, q HistoryQuery) (HistoryPage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.authenticate(a, false); err != nil {
		return HistoryPage{}, err
	}
	return e.historyLocked(q)
}

func (e *Engine) historyLocked(q HistoryQuery) (HistoryPage, error) {
	result := HistoryPage{Messages: []Message{}, Sequence: e.sequence, Total: len(e.order), Notice: "Historical evidence only; reading does not claim, acknowledge, retry or authorize repeating work."}
	if q.Limit < 0 || q.Limit > HistoryPageLimit {
		return result, errors.New("history limit must be 1–100")
	}
	if q.Limit == 0 {
		q.Limit = 50
	}
	if q.ID != "" {
		if !validID(q.ID) || q.Cursor != "" || q.Pending || !q.Since.IsZero() {
			return result, errors.New("choose a message ID or a history page")
		}
		if m, ok := e.messages[q.ID]; ok {
			result.Messages = append(result.Messages, cloneMessage(m))
		}
		return result, nil
	}
	prefix := "history:"
	ids := e.order
	at, step := len(ids)-1, -1
	if q.Pending {
		prefix = "pending:"
		ids = e.unresolved
		at = 0
		step = 1
		result.Total = len(ids)
	}
	if q.Cursor != "" {
		if !strings.HasPrefix(q.Cursor, prefix) {
			return result, errors.New("cursor does not match history view")
		}
		position, err := strconv.Atoi(strings.TrimPrefix(q.Cursor, prefix))
		if err != nil || position < 0 || position >= len(e.order) {
			return result, errors.New("invalid history cursor")
		}
		if q.Pending {
			at = sort.Search(len(ids), func(i int) bool { return e.positions[ids[i]] > position })
		} else {
			at = position - 1
		}
	}
	bytes, scanned, last := 0, 0, -1
	for at >= 0 && at < len(ids) {
		if len(result.Messages) >= q.Limit || scanned >= historyScanLimit {
			break
		}
		id := ids[at]
		m := e.messages[id]
		size := len(m.Text)
		if m.Quote != nil {
			size += len(m.Quote.Text)
		}
		if !q.Since.IsZero() && m.CreatedAt.Before(q.Since) {
			last = e.positions[id]
			scanned++
			at += step
			continue
		}
		if len(result.Messages) > 0 && bytes+size > SnapshotTextBudget {
			break
		}
		bytes += size
		result.Messages = append(result.Messages, cloneMessage(m))
		last = e.positions[id]
		scanned++
		at += step
	}
	result.HasMore = at >= 0 && at < len(ids)
	if result.HasMore && last >= 0 {
		result.NextCursor = fmt.Sprintf("%s%d", prefix, last)
	}
	return result, nil
}

type SendReceipt struct {
	Found   bool     `json:"found"`
	Message *Message `json:"message,omitempty"`
}

// UserSendReceipt resolves only the authenticated Room user's original client ID.
// It does not accept a replacement payload or manufacture another publication.
func (e *Engine) UserSendReceipt(clientID string) (SendReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.available(); err != nil {
		return SendReceipt{}, err
	}
	if !validID(clientID) {
		return SendReceipt{}, errors.New("invalid client message ID")
	}
	if m, ok := e.messages[e.sends["user/"+clientID]]; ok {
		m = cloneMessage(m)
		return SendReceipt{Found: true, Message: &m}, nil
	}
	return SendReceipt{}, nil
}
