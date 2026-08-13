package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/room"
	"github.com/sean2077/pairroom/internal/store"
)

func newTestServer(t *testing.T, token string) (*Server, *room.Engine) {
	t.Helper()
	repo := t.TempDir()
	eventStore, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engine, err := room.New(room.Config{
		Name: "test room", Repo: repo, Store: eventStore,
		Settings:      model.RoomSettings{RoutingMode: model.RoutingManual, MaxHops: 3},
		ClaudeFactory: agent.MockFactory, CodexFactory: agent.MockFactory,
		ClaudeConfig: agent.Config{MockDelay: 5 * time.Millisecond},
		CodexConfig:  agent.Config{MockDelay: 5 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	server, err := New(Config{Engine: engine, Repo: repo, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return server, engine
}

func localRequest(method, target string, body *bytes.Buffer) *http.Request {
	var reader any
	if body != nil {
		reader = body
	}
	request := httptest.NewRequest(method, "http://127.0.0.1"+target, nil)
	if reader != nil {
		request = httptest.NewRequest(method, "http://127.0.0.1"+target, body)
	}
	return request
}

func TestHealthSnapshotAndMessageAPI(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "")

	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, localRequest(http.MethodGet, "/api/v1/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d: %s", health.Code, health.Body.String())
	}

	body := bytes.NewBufferString(`{"text":"Review the design","to":["claude","codex"]}`)
	send := httptest.NewRecorder()
	request := localRequest(http.MethodPost, "/api/v1/messages", body)
	request.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(send, request)
	if send.Code != http.StatusAccepted {
		t.Fatalf("message status = %d: %s", send.Code, send.Body.String())
	}
	var created model.Message
	if err := json.Unmarshal(send.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Seq == 0 || created.From != model.ActorUser {
		t.Fatalf("unexpected message: %#v", created)
	}

	snapshot := httptest.NewRecorder()
	server.Handler().ServeHTTP(snapshot, localRequest(http.MethodGet, "/api/v1/snapshot", nil))
	if snapshot.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d: %s", snapshot.Code, snapshot.Body.String())
	}
	var state model.RoomSnapshot
	if err := json.Unmarshal(snapshot.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) == 0 || state.Messages[0].ID != created.ID {
		t.Fatalf("message was not projected into snapshot: %#v", state.Messages)
	}
}

func TestBearerTokenProtectsOnlyAPI(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "secret")

	asset := httptest.NewRecorder()
	server.Handler().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/", nil))
	if asset.Code != http.StatusOK {
		t.Fatalf("static asset should remain reachable: %d", asset.Code)
	}

	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", unauthorized.Code)
	}

	authorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	request.Header.Set("Authorization", "Bearer secret")
	server.Handler().ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK {
		t.Fatalf("valid token status = %d: %s", authorized.Code, authorized.Body.String())
	}
}

func TestQueryTokenIsRestrictedToSSE(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "secret")

	snapshot := httptest.NewRecorder()
	server.Handler().ServeHTTP(snapshot, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?token=secret", nil))
	if snapshot.Code != http.StatusUnauthorized {
		t.Fatalf("query token must not authorize snapshot API: %d", snapshot.Code)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events?token=secret", nil).WithContext(ctx)
	server.Handler().ServeHTTP(events, request)
	if events.Code != http.StatusOK {
		t.Fatalf("query token must authorize EventSource endpoint: %d: %s", events.Code, events.Body.String())
	}
	if got := events.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("SSE content type = %q", got)
	}
}

func TestCrossOriginRequestsAreRejected(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://pairroom.local/api/v1/snapshot", nil)
	request.Host = "pairroom.local"
	request.Header.Set("Origin", "https://attacker.example")
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", recorder.Code)
	}
}

func TestTokenlessServerRejectsNonLoopbackHost(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://attacker.example/api/v1/snapshot", nil)
	request.Host = "attacker.example"
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-loopback Host status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestLoopbackHostRecognition(t *testing.T) {
	for _, value := range []string{"localhost", "localhost:7332", "127.0.0.1", "127.0.0.1:7332", "[::1]", "[::1]:7332"} {
		if !isLoopbackRequestHost(value) {
			t.Errorf("expected loopback host %q", value)
		}
	}
	for _, value := range []string{"", "0.0.0.0:7332", "pairroom.local", "192.168.1.8:7332"} {
		if isLoopbackRequestHost(value) {
			t.Errorf("unexpected loopback host %q", value)
		}
	}
}

func TestSSECursorKeepsTransientEventsLive(t *testing.T) {
	last := uint64(12)
	write, next := advanceSSECursor(model.Event{Seq: 0}, last)
	if !write || next != last {
		t.Fatalf("transient event must be written without moving cursor: write=%v next=%d", write, next)
	}
	write, next = advanceSSECursor(model.Event{Seq: 12}, last)
	if write || next != last {
		t.Fatalf("durable duplicate must be filtered: write=%v next=%d", write, next)
	}
	write, next = advanceSSECursor(model.Event{Seq: 13}, last)
	if !write || next != 13 {
		t.Fatalf("new durable event must advance cursor: write=%v next=%d", write, next)
	}

	var output bytes.Buffer
	if err := writeSSE(&output, model.Event{Seq: 0, Kind: "runtime.event"}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("id:")) {
		t.Fatalf("transient event must not set an SSE replay id: %q", output.String())
	}
}

func TestRetryAndExportAPI(t *testing.T) {
	server, engine := newTestServer(t, "")

	send := httptest.NewRecorder()
	request := localRequest(http.MethodPost, "/api/v1/messages", bytes.NewBufferString(`{"text":"Inspect failure","to":["codex"]}`))
	request.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(send, request)
	if send.Code != http.StatusAccepted {
		t.Fatalf("message status = %d: %s", send.Code, send.Body.String())
	}
	var original model.Message
	if err := json.Unmarshal(send.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var accepted bool
		for _, message := range engine.Snapshot().Messages {
			if message.ID == original.ID && message.Delivery[model.ActorCodex] != model.DeliveryPending {
				accepted = true
				break
			}
		}
		if accepted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorCodex, Kind: model.RuntimeInputFailed,
		CorrelationID: original.ID, TurnID: "turn-test", Text: "synthetic failure",
		CreatedAt: time.Now().UTC(),
	})

	retry := httptest.NewRecorder()
	retryRequest := localRequest(http.MethodPost, "/api/v1/messages/"+original.ID+"/retry", bytes.NewBufferString(`{"to":["codex"]}`))
	retryRequest.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(retry, retryRequest)
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d: %s", retry.Code, retry.Body.String())
	}
	var retried model.Message
	if err := json.Unmarshal(retry.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.RetryOf != original.ID || retried.ID == original.ID {
		t.Fatalf("unexpected retry message: %#v", retried)
	}

	markdown := httptest.NewRecorder()
	server.Handler().ServeHTTP(markdown, localRequest(http.MethodGet, "/api/v1/export?format=markdown", nil))
	if markdown.Code != http.StatusOK || !strings.Contains(markdown.Body.String(), "# test room") || !strings.Contains(markdown.Body.String(), "Inspect failure") {
		t.Fatalf("unexpected markdown export: status=%d body=%q", markdown.Code, markdown.Body.String())
	}
	if disposition := markdown.Header().Get("Content-Disposition"); !strings.Contains(disposition, "test-room.md") {
		t.Fatalf("unexpected markdown filename: %q", disposition)
	}

	jsonExport := httptest.NewRecorder()
	server.Handler().ServeHTTP(jsonExport, localRequest(http.MethodGet, "/api/v1/export?format=json", nil))
	if jsonExport.Code != http.StatusOK {
		t.Fatalf("json export status = %d: %s", jsonExport.Code, jsonExport.Body.String())
	}
	var snapshot model.RoomSnapshot
	if err := json.Unmarshal(jsonExport.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) < 2 {
		t.Fatalf("json export omitted retry history: %#v", snapshot.Messages)
	}
	if len(snapshot.Events) != 0 {
		t.Fatalf("normal JSON export must omit Inspector event noise: %d events", len(snapshot.Events))
	}

	forensic := httptest.NewRecorder()
	server.Handler().ServeHTTP(forensic, localRequest(http.MethodGet, "/api/v1/export?format=json&include_events=1", nil))
	if forensic.Code != http.StatusOK {
		t.Fatalf("forensic export status = %d: %s", forensic.Code, forensic.Body.String())
	}
	var forensicSnapshot model.RoomSnapshot
	if err := json.Unmarshal(forensic.Body.Bytes(), &forensicSnapshot); err != nil {
		t.Fatal(err)
	}
	if len(forensicSnapshot.Events) == 0 {
		t.Fatal("forensic JSON export should retain the Inspector event tail")
	}
}
