package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestNotifierDeduplicatesFoldsBurstsAndStaysBodyFree(t *testing.T) {
	var clock atomic.Int64
	clock.Store(1800000000)
	var mu sync.Mutex
	var runs [][]byte
	delivered := make(chan struct{}, 3)
	n := NewNotifier(NotifierConfig{
		Command: []string{"notify-fixture"},
		Now:     func() time.Time { return time.Unix(clock.Load(), 0).UTC() },
		Run: func(_ context.Context, argv []string, stdin []byte) error {
			if len(argv) != 1 || argv[0] != "notify-fixture" {
				t.Errorf("argv = %v", argv)
			}
			mu.Lock()
			runs = append(runs, stdin)
			mu.Unlock()
			delivered <- struct{}{}
			return nil
		},
	})
	room := Room{ID: "room-1", Name: "Review", HostMode: model.HostNative}
	if !n.Notify(room, "human_turn", model.ActorSlot1, "msg-1") {
		t.Fatal("first notification dropped")
	}
	if n.Notify(room, "human_turn", model.ActorSlot1, "msg-1") {
		t.Fatal("same fact notified twice")
	}
	if n.Notify(room, "human_turn", model.ActorSlot2, "msg-2") {
		t.Fatal("burst within the cooldown was not folded")
	}
	if !n.Notify(room, "wake_failed", model.ActorSlot2, "ev-1") {
		t.Fatal("different kind was folded into another burst")
	}
	if n.Notify(room, "not_a_kind", model.ActorSlot1, "x") {
		t.Fatal("unknown kind accepted")
	}
	clock.Add(int64(notificationCooldown / time.Second))
	if !n.Notify(room, "human_turn", model.ActorSlot2, "msg-3") {
		t.Fatal("cooldown never expired")
	}
	items, _ := n.Since(0)
	if len(items) != 3 || items[0].Seq != 1 || items[2].Seq != 3 {
		t.Fatalf("items = %+v", items)
	}
	if later, _ := n.Since(items[1].Seq); len(later) != 1 || later[0].Kind != "human_turn" {
		t.Fatalf("Since cursor = %+v", later)
	}
	for range 3 {
		select {
		case <-delivered:
		case <-time.After(5 * time.Second):
			t.Fatal("notification command did not run")
		}
	}
	n.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != 3 {
		t.Fatalf("command runs = %d, want 3", len(runs))
	}
	var got map[string]any
	if err := json.Unmarshal(runs[0], &got); err != nil {
		t.Fatal(err)
	}
	for key := range got {
		switch key {
		case "seq", "room_id", "room_name", "host_mode", "kind", "slot", "at":
		default:
			t.Fatalf("notification carries unexpected field %q: %s", key, runs[0])
		}
	}
}

// A Native Room's @user escalation, uncertain delivery and failed wake reach the
// notifier; an ordinary peer message does not.
func TestNativeRoomFactsRaiseNotifications(t *testing.T) {
	notifier := NewNotifier(NotifierConfig{})
	t.Cleanup(notifier.Close)
	f := nativeHTTPWithNotifier(t, notifier)
	sender := f.bind(t, model.ActorSlot1)
	f.bind(t, model.ActorSlot2)
	if _, err := f.native.engine.Send(sender, relay.SendRequest{ID: "peer", Text: "please review"}); err != nil {
		t.Fatal(err)
	}
	if items, _ := notifier.Since(0); len(items) != 0 {
		t.Fatalf("peer message raised a notification: %+v", items)
	}
	if _, err := f.native.engine.Send(sender, relay.SendRequest{ID: "human", Text: "need a decision", To: model.ActorUser}); err != nil {
		t.Fatal(err)
	}
	if err := f.native.engine.RecordWake("failed", "socket_failed", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	items, _ := notifier.Since(0)
	kinds := map[string]bool{}
	for _, item := range items {
		kinds[item.Kind] = true
		if item.RoomID != f.room.ID {
			t.Fatalf("notification for another Room: %+v", item)
		}
	}
	if !kinds["human_turn"] || !kinds["wake_failed"] || len(items) != 2 {
		t.Fatalf("notifications = %+v", items)
	}
}

func TestNotificationsRouteLongPollsWithoutSideEffects(t *testing.T) {
	notifier := NewNotifier(NotifierConfig{})
	t.Cleanup(notifier.Close)
	f := nativeHTTPWithNotifier(t, notifier)
	get := func(query string) []Notification {
		req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/v1/notifications"+query, nil)
		req.Header.Set("Authorization", "Bearer management-secret")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(res.Body)
			t.Fatalf("status %d: %s", res.StatusCode, body)
		}
		var payload struct {
			Notifications []Notification `json:"notifications"`
		}
		if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return payload.Notifications
	}
	if got := get(""); len(got) != 0 {
		t.Fatalf("initial list = %+v", got)
	}
	done := make(chan []Notification, 1)
	go func() { done <- get("?after=0&wait=10") }()
	time.Sleep(100 * time.Millisecond)
	notifier.Notify(f.room, "approval_requested", model.ActorSlot2, "approval-1")
	select {
	case got := <-done:
		if len(got) != 1 || got[0].Kind != "approval_requested" {
			t.Fatalf("long poll = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long poll did not return on a new notification")
	}
	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/v1/notifications?wait=31", nil)
	req.Header.Set("Authorization", "Bearer management-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("out-of-range wait accepted: %d", res.StatusCode)
	}
}
