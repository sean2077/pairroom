package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestCreateNativeRoomRegistersMissingProject(t *testing.T) {
	var registered map[string]any
	var roomRequest map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/service", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("management auth = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"projects": []any{}})
	})
	mux.HandleFunc("POST /api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&registered)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "p1"})
	})
	mux.HandleFunc("POST /api/v1/projects/p1/rooms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&roomRequest)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "room1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	id, err := createNativeRoom(context.Background(), relay.Endpoint{URL: srv.URL, Token: "token"}, "/ws", options{name: "Discuss", kind: "codex", peer: "claude"}, model.ActorCodex)
	if err != nil {
		t.Fatalf("createNativeRoom: %v", err)
	}
	if id != "room1" {
		t.Fatalf("room id = %q", id)
	}
	if registered["path"] != "/ws" {
		t.Fatalf("project path = %v", registered["path"])
	}
	if roomRequest["host_mode"] != "native" || roomRequest["name"] != "Discuss" {
		t.Fatalf("room request = %v", roomRequest)
	}
	agents, ok := roomRequest["agents"].(map[string]any)
	if !ok {
		t.Fatalf("agents missing: %v", roomRequest)
	}
	codex, _ := agents["codex"].(map[string]any)
	claude, _ := agents["claude"].(map[string]any)
	if codex["runtime"] != "codex" || claude["runtime"] != "claude" {
		t.Fatalf("agents = %v", agents)
	}
	if _, exists := codex["model"]; exists {
		t.Fatalf("runtime override must stay an empty-field selection: %v", codex)
	}
}

func TestCreateNativeRoomReusesRegisteredProjectWithServerDefaults(t *testing.T) {
	projectPosted := false
	var roomRequest map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/service", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"projects": []any{map[string]any{"id": "p9", "root": "/ws"}}})
	})
	mux.HandleFunc("POST /api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		projectPosted = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "unexpected"})
	})
	mux.HandleFunc("POST /api/v1/projects/p9/rooms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&roomRequest)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "room2"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	id, err := createNativeRoom(context.Background(), relay.Endpoint{URL: srv.URL, Token: "token"}, "/ws", options{}, model.ActorClaude)
	if err != nil {
		t.Fatalf("createNativeRoom: %v", err)
	}
	if id != "room2" || projectPosted {
		t.Fatalf("id = %q, projectPosted = %v", id, projectPosted)
	}
	if roomRequest["host_mode"] != "native" {
		t.Fatalf("room request = %v", roomRequest)
	}
	if _, exists := roomRequest["agents"]; exists {
		t.Fatalf("omitted runtimes must keep the Service default pair: %v", roomRequest)
	}
	if _, exists := roomRequest["name"]; exists {
		t.Fatalf("omitted name must stay generated once by the Service: %v", roomRequest)
	}
}

func TestCreateNativeRoomRejectsUnsupportedRuntimeBeforeAnyRequest(t *testing.T) {
	_, err := createNativeRoom(context.Background(), relay.Endpoint{URL: "http://127.0.0.1:0"}, "/ws", options{peer: "grok"}, model.ActorClaude)
	if err == nil || !strings.Contains(err.Error(), "claude or codex only") {
		t.Fatalf("err = %v", err)
	}
}

func TestBindCreateRejectsExplicitRoom(t *testing.T) {
	var out, diagnostic bytes.Buffer
	err := Run(context.Background(), []string{"bind", "--create", "--room", "r1", "--slot", "claude"}, strings.NewReader(""), &out, &diagnostic)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("err = %v", err)
	}
}
