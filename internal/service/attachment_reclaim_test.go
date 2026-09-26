package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Close waits on done before closing the Room store, so a returned loop is
// the guarantee that no pass can touch a closed store.
func TestAttachmentReclaimLoopRunsPassesAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	passes := make(chan struct{}, 8)
	go runAttachmentReclaim(ctx, done, "room", time.Millisecond, time.Millisecond, func() (int, error) {
		passes <- struct{}{}
		return 1, errors.New("partial")
	})
	for i := 0; i < 2; i++ {
		select {
		case <-passes:
		case <-time.After(5 * time.Second):
			t.Fatal("reclamation pass did not run")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reclamation loop outlived its runtime")
	}
}
