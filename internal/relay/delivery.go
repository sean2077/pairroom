package relay

import (
	"sort"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// MessageDelivery is a read-only projection, never an Event Log payload.
// Reservations have message IDs; terminal results only have a slot, so their
// association is explicitly inferred from the single-worker event order.
type MessageDelivery struct {
	QueueWaitMS          *int64            `json:"queue_wait_ms,omitempty"`
	ReservedAt           *time.Time        `json:"reserved_at,omitempty"`
	InferredOutcome      *WakeObservation  `json:"inferred_outcome,omitempty"`
	SlotObservations     []WakeObservation `json:"slot_observations,omitempty"`
	SlotObservationCount int               `json:"slot_observation_count,omitempty"`
}
type wakeTimelineEntry struct {
	sequence    uint64
	observation WakeObservation
}
type deliveryProjection struct {
	created  map[string]uint64
	queueEnd map[string]uint64
	reserved map[string]time.Time
	inferred map[string]WakeObservation
	pending  map[model.ActorID]string
	slots    map[model.ActorID][]wakeTimelineEntry
}

func (e *Engine) deliveryIndex() *deliveryProjection {
	if e.delivery == nil {
		e.delivery = &deliveryProjection{created: map[string]uint64{}, queueEnd: map[string]uint64{}, reserved: map[string]time.Time{}, inferred: map[string]WakeObservation{}, pending: map[model.ActorID]string{}, slots: map[model.ActorID][]wakeTimelineEntry{}}
	}
	return e.delivery
}
func (e *Engine) indexDeliveryMessage(m Message, exists bool) {
	d := e.deliveryIndex()
	if !exists {
		d.created[m.ID] = e.sequence
	}
	if m.State == "delivering" || m.State == "cancelled" {
		d.queueEnd[m.ID] = e.sequence
	}
}
func (e *Engine) indexWakeReservation(r WakeReservation) {
	d := e.deliveryIndex()
	d.reserved[r.MessageID] = r.At
	d.pending[r.Target] = r.MessageID
}
func (e *Engine) indexWakeObservation(slot model.ActorID, observation WakeObservation) {
	d := e.deliveryIndex()
	d.slots[slot] = append(d.slots[slot], wakeTimelineEntry{sequence: e.sequence, observation: observation})
	if observation.Outcome != "suppressed" {
		if id := d.pending[slot]; id != "" {
			d.inferred[id] = observation
			delete(d.pending, slot)
		}
	}
}

const maxMessageWakeObservations = 3

func (e *Engine) messageDeliveryLocked(m Message) MessageDelivery {
	var result MessageDelivery
	d := e.delivery
	if d == nil {
		return result
	}
	if (m.State == "handed_off" || m.State == "unknown") && !m.ClaimedAt.IsZero() && !m.CreatedAt.IsZero() && !m.ClaimedAt.Before(m.CreatedAt) {
		ms := m.ClaimedAt.Sub(m.CreatedAt).Milliseconds()
		result.QueueWaitMS = &ms
	}
	if at, ok := d.reserved[m.ID]; ok {
		result.ReservedAt = &at
	}
	if observation, ok := d.inferred[m.ID]; ok {
		result.InferredOutcome = &observation
	}
	// Slot observations occurred while this message awaited collection. They
	// are not claimed to be attempts on this message. Sequence, not wall clock,
	// selects the interval, so clock corrections cannot misattribute evidence.
	entries := d.slots[m.To]
	first := sort.Search(len(entries), func(i int) bool { return entries[i].sequence > d.created[m.ID] })
	end := len(entries)
	if claim := d.queueEnd[m.ID]; claim != 0 {
		end = sort.Search(len(entries), func(i int) bool { return entries[i].sequence >= claim })
	}
	if end > first {
		result.SlotObservationCount = end - first
		for _, entry := range entries[max(first, end-maxMessageWakeObservations):end] {
			result.SlotObservations = append(result.SlotObservations, entry.observation)
		}
	}
	return result
}
func (e *Engine) deliveryForMessagesLocked(messages []Message) map[string]MessageDelivery {
	var result map[string]MessageDelivery
	for _, m := range messages {
		if !m.From.ValidParticipant() || m.To != model.OtherParticipant(m.From) {
			continue
		}
		value := e.messageDeliveryLocked(m)
		if value.QueueWaitMS == nil && value.ReservedAt == nil && value.InferredOutcome == nil && len(value.SlotObservations) == 0 {
			continue
		}
		if result == nil {
			result = map[string]MessageDelivery{}
		}
		result[m.ID] = value
	}
	return result
}
