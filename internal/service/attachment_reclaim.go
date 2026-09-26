package service

import (
	"context"
	"log/slog"
	"time"
)

// Reclamation passes of an active Room: the first runs shortly after
// activation, off its hot path, then one per interval. Each pass is bounded
// by attachment.ReclaimBatch, so a backlog drains over later passes.
const (
	attachmentReclaimDelay    = 10 * time.Second
	attachmentReclaimInterval = time.Hour
)

// reclaimRoomAttachments runs one reclamation pass and logs only the count.
// Errors may carry host attachment paths, so they are summarized rather
// than logged; the next pass retries whatever remains.
func reclaimRoomAttachments(roomID string, reclaim func() (int, error)) {
	removed, err := reclaim()
	if removed > 0 {
		slog.Info("reclaimed unreferenced Room attachments", "room", roomID, "count", removed)
	}
	if err != nil {
		slog.Warn("Room attachment reclamation incomplete; retrying on the next pass", "room", roomID)
	}
}

// runAttachmentReclaim runs a pass after delay and then every interval until
// ctx ends. It closes done on return so an owner can wait for it before
// closing the Room store.
func runAttachmentReclaim(ctx context.Context, done chan<- struct{}, roomID string, delay, interval time.Duration, reclaim func() (int, error)) {
	defer close(done)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			reclaimRoomAttachments(roomID, reclaim)
			timer.Reset(interval)
		}
	}
}
