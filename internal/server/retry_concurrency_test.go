package server

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/sean2077/pairroom/internal/agent"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/room"
	"github.com/sean2077/pairroom/internal/store"
)

// A projection/API fixture must not generate agent replies between reads. Leave
// the adapters unstarted; live HTTP/SSE behavior is exercised in the browser smoke.
func newDormantTestServer(t *testing.T) (*Server, *room.Engine) {
	t.Helper()
	repo := t.TempDir()
	events, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engine, err := room.New(room.Config{Name: "fixture", Repo: repo, Store: events, Settings: model.RoomSettings{StallWarningSeconds: 300}})
	if err != nil {
		_ = events.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	server, err := New(Config{Engine: engine, Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	return server, engine
}

func TestConcurrentHTTPRetryReturnsOneAcceptedOneConflict(t *testing.T) {
	// Seed a terminal original through the store, then open a live Mock engine
	// whose reply cannot finish during these requests. No sleep-based race gate.
	dir, repo := t.TempDir(), t.TempDir()
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	config := room.Config{Name: "retry", Repo: repo, Store: log,
		ClaudeFactory: agent.MockFactory, CodexFactory: agent.MockFactory,
		CodexConfig: agent.Config{MockDelay: time.Hour}}
	initial, err := room.New(config)
	if err != nil {
		t.Fatal(err)
	}
	roomID := initial.Snapshot().Meta.ID
	if err = initial.Close(); err != nil {
		t.Fatal(err)
	}
	log, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	original := model.Message{ID: "failed-input", From: model.ActorUser, To: []model.ActorID{model.ActorCodex}, Text: "execute once", ThreadID: "thread",
		Delivery:   map[model.ActorID]model.DeliveryState{model.ActorCodex: model.DeliveryFailed},
		Processing: map[model.ActorID]model.ProcessingState{model.ActorCodex: model.ProcessingFailed}}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if err = log.Append(&model.Event{RoomID: roomID, Kind: room.EventMessageCreated, Actor: model.ActorUser, Data: payload}); err != nil {
		t.Fatal(err)
	}
	config.Store = log
	e, err := room.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err = e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Engine: e, Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			response := httptest.NewRecorder()
			request := localRequest(http.MethodPost, "/api/v1/messages/"+original.ID+"/retry", bytes.NewBufferString(`{"to":["codex"]}`))
			request.Header.Set("Content-Type", "application/json")
			s.Handler().ServeHTTP(response, request)
			results <- response.Code
		}()
	}
	close(gate)
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[http.StatusAccepted] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("retry status counts: %v", counts)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, localRequest(http.MethodGet, "/api/v1/snapshot", nil))
	var snapshot model.RoomSnapshot
	if err = json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	children := 0
	for _, message := range snapshot.Messages {
		if message.RetryOf == original.ID {
			children++
		}
	}
	if children != 1 {
		t.Fatalf("persisted %d retry children", children)
	}
}
