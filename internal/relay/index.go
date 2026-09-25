package relay

import (
	"sort"

	"github.com/sean2077/pairroom/internal/model"
)

// These indexes are projections of message facts, never another durable queue.
// Updates/searches touch the current work set, not all terminal Room history.
func (e *Engine) initIndexes() {
	if e.positions == nil {
		e.positions = map[string]int{}
	}
	if e.queued == nil {
		e.queued = map[model.ActorID][]string{}
	}
	if e.counts == nil {
		e.counts = map[model.ActorID]InboxSummary{}
	}
	if e.lastWake == nil {
		e.lastWake = map[model.ActorID]WakeObservation{}
	}
	if e.lastOutbound == nil {
		e.lastOutbound = map[model.ActorID]string{}
	}
	if e.lastInbound == nil {
		e.lastInbound = map[model.ActorID]string{}
	}
}

// replyExpectedLocked reports whether a Stop park can plausibly collect a peer
// reply: this slot's newest message to its peer is recent, not cancelled, and
// the peer has not addressed this slot since. A human is usually in front of
// the harness, so an @user escalation does not hold it. Otherwise a park only
// delays the native harness; queued input is still claimed without it.
func (e *Engine) replyExpectedLocked(slot model.ActorID) bool {
	id := e.lastOutbound[slot]
	if id == "" {
		return false
	}
	out := e.messages[id]
	if out.State == "cancelled" || e.cfg.Now().Sub(out.CreatedAt) > ReplyParkWindow {
		return false
	}
	in := e.lastInbound[slot]
	return in == "" || e.positions[in] < e.positions[id]
}

func unresolvedState(state string) bool {
	return state == "queued" || state == "delivering" || state == "unknown"
}

func (e *Engine) indexEntry(ids []string, id string, add bool) []string {
	position := e.positions[id]
	at := sort.Search(len(ids), func(i int) bool { return e.positions[ids[i]] >= position })
	present := at < len(ids) && ids[at] == id
	if add && !present {
		ids = append(ids, "")
		copy(ids[at+1:], ids[at:])
		ids[at] = id
	}
	if !add && present {
		copy(ids[at:], ids[at+1:])
		ids[len(ids)-1] = ""
		ids = ids[:len(ids)-1]
	}
	return ids
}

func (e *Engine) countMessage(m Message, delta int) {
	if !m.To.ValidParticipant() {
		return
	}
	c := e.counts[m.To]
	switch m.State {
	case "queued":
		c.Queued += delta
	case "delivering":
		c.Delivering += delta
	case "unknown":
		c.Unknown += delta
	}
	e.counts[m.To] = c
}

func (e *Engine) putMessage(m Message) {
	e.initIndexes()
	old, exists := e.messages[m.ID]
	if !exists {
		e.positions[m.ID] = len(e.order)
		e.order = append(e.order, m.ID)
		// A user-initiated Retry keeps the original sender but is not a new
		// request or answer from that Agent.
		if m.Source != "retry" && m.From.ValidParticipant() && m.To == model.OtherParticipant(m.From) {
			e.lastOutbound[m.From] = m.ID
			e.lastInbound[m.To] = m.ID
		}
	}
	if exists {
		e.countMessage(old, -1)
		if old.State == "queued" {
			e.queued[old.To] = e.indexEntry(e.queued[old.To], m.ID, false)
		}
	}
	e.messages[m.ID] = m
	if m.To == model.ActorUser && (e.lastUserMessage == "" || e.positions[m.ID] > e.positions[e.lastUserMessage]) {
		e.lastUserMessage = m.ID
	}
	e.countMessage(m, 1)
	if m.State == "queued" {
		e.queued[m.To] = e.indexEntry(e.queued[m.To], m.ID, true)
	}
	e.unresolved = e.indexEntry(e.unresolved, m.ID, m.To.ValidParticipant() && unresolvedState(m.State))
	if m.State == "delivering" {
		e.inFlight[m.ID] = struct{}{}
	} else {
		delete(e.inFlight, m.ID)
	}
}
