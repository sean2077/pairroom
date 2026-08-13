package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestHealthSnapshotAndMessageAPI(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "")

	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d: %s", health.Code, health.Body.String())
	}

	body := bytes.NewBufferString(`{"text":"Review the design","to":["claude","codex"]}`)
	send := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/messages", body)
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
	server.Handler().ServeHTTP(snapshot, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil))
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
