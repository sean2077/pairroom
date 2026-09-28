package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestNotifierConcurrentCloseAndPublish(t *testing.T) {
	for round := 0; round < 100; round++ {
		n := NewNotifier(NotifierConfig{Command: []string{"fixture"}, Run: func(context.Context, []string, []byte) error { return nil }})
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				n.Notify(Room{ID: fmt.Sprint(i)}, "human_turn", model.ActorSlot1, "one")
			}(i)
		}
		for range 2 {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; n.Close() }()
		}
		close(start)
		wg.Wait()
		if n.Notify(Room{ID: "late"}, "human_turn", model.ActorSlot1, "late") {
			t.Fatal("closed notifier accepted work")
		}
	}
}

func TestNotifierCloseCancelsCurrentCommandAndDiscardsBacklog(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int64
	n := NewNotifier(NotifierConfig{Command: []string{"fixture"}, Run: func(ctx context.Context, _ []string, _ []byte) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	n.Notify(Room{ID: "first"}, "human_turn", model.ActorSlot1, "one")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	for i := 0; i < 64; i++ {
		n.Notify(Room{ID: fmt.Sprint(i)}, "human_turn", model.ActorSlot1, "one")
	}
	done := make(chan struct{})
	go func() { n.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close waited for individual command timeouts")
	}
	if calls.Load() != 1 {
		t.Fatalf("started %d commands; queued commands must be dropped", calls.Load())
	}
}

func TestNotifierCursorResetsAcrossEpochsAndAheadSequences(t *testing.T) {
	old := NewNotifier(NotifierConfig{})
	defer old.Close()
	fresh := NewNotifier(NotifierConfig{})
	defer fresh.Close()
	oldPage, _ := old.page(0, "")
	fresh.Notify(Room{ID: "first"}, "human_turn", model.ActorSlot1, "one")
	for _, query := range []struct {
		after uint64
		epoch string
	}{{99, ""}, {0, oldPage.Epoch}, {1, oldPage.Epoch}} {
		page, _ := fresh.page(query.after, query.epoch)
		if !page.Reset || page.Epoch == oldPage.Epoch || page.LatestSeq != 1 || len(page.Notifications) != 1 {
			t.Fatalf("stale cursor lost fresh notification: %+v", page)
		}
	}
	page, _ := fresh.page(1, fresh.epoch)
	if page.Reset || len(page.Notifications) != 0 {
		t.Fatalf("current cursor replayed history: %+v", page)
	}
}

func TestNotificationsStaleEpochReturnsResetWithoutWaiting(t *testing.T) {
	n := NewNotifier(NotifierConfig{})
	t.Cleanup(n.Close)
	f := nativeHTTPWithNotifier(t, n)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/api/v1/notifications?after=99&epoch=previous&wait=25", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer management-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var page notificationPage
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || !page.Reset || page.Epoch == "" || page.Epoch == "previous" || page.LatestSeq != 0 || len(page.Notifications) != 0 {
		t.Fatalf("stale epoch was not reset: status=%d page=%+v", res.StatusCode, page)
	}
}
