package bus

import (
	"sync"

	"github.com/sean2077/pairroom/internal/model"
)

// Hub is an in-process fan-out bus for room events. Slow subscribers are
// disconnected rather than blocking the agent/runtime hot path.
type Hub struct {
	mu          sync.RWMutex
	subscribers map[uint64]chan model.Event
	nextID      uint64
	buffer      int
}

func New(buffer int) *Hub {
	if buffer < 1 {
		buffer = 128
	}
	return &Hub{subscribers: make(map[uint64]chan model.Event), buffer: buffer}
}

func (h *Hub) Subscribe() (<-chan model.Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	ch := make(chan model.Event, h.buffer)
	h.subscribers[id] = ch
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			if existing, ok := h.subscribers[id]; ok {
				delete(h.subscribers, id)
				close(existing)
			}
			h.mu.Unlock()
		})
	}
	return ch, cancel
}

func (h *Hub) Publish(event model.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.subscribers {
		select {
		case ch <- event:
		default:
			delete(h.subscribers, id)
			close(ch)
		}
	}
}
