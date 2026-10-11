package service

import (
	"net/http"
	"time"

	"github.com/sean2077/pairroom/internal/relay"
)

const (
	lanTransferLimit     = 8
	lanRoomTransferLimit = 2
)

// Bound disk staging, buffers and open transfer streams independently of the
// lightweight relay request budget. Acquire before reading any upload body.
func (h *lanHostServer) acquireTransfer(roomID string) (func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.transfers >= lanTransferLimit || h.roomTransfers[roomID] >= lanRoomTransferLimit {
		return nil, false
	}
	if h.roomTransfers == nil {
		h.roomTransfers = make(map[string]int)
	}
	h.transfers++
	h.roomTransfers[roomID]++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.transfers--
		h.roomTransfers[roomID]--
		if h.roomTransfers[roomID] == 0 {
			delete(h.roomTransfers, roomID)
		}
	}, true
}

// Interrupt deadlines unblock the actual network read/write before Close
// waits for the request body's internal read lock. The Engine joins this
// transfer before publishing revocation, without holding its Room lock.
func beginLANHTTPTransfer(w http.ResponseWriter, r *http.Request, n *nativeHostRuntime, a relay.Auth) (*relay.LANTransfer, func(), error) {
	rc := http.NewResponseController(w)
	body := r.Body
	t, err := n.engine.BeginLANTransfer(r.Context(), a, func() {
		_ = rc.SetReadDeadline(time.Now())
		_ = rc.SetWriteDeadline(time.Now())
		_ = body.Close()
	})
	reset := func() {
		_ = rc.SetReadDeadline(time.Time{})
		_ = rc.SetWriteDeadline(time.Time{})
	}
	return t, reset, err
}
