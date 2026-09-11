package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func createBindFixture(t *testing.T, own model.RuntimeKind) (string, string, *int) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project's space")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/service", func(w http.ResponseWriter, r *http.Request) {
		rooms := []any{}
		for i := 1; i <= created; i++ {
			rooms = append(rooms, map[string]any{"id": fmt.Sprintf("room%d", i), "project_id": "project", "host_mode": "native", "agents": map[model.ActorID]model.AgentSelection{model.ActorClaude: {Runtime: own}, model.ActorCodex: {Runtime: model.RuntimeCodex}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"projects": []any{map[string]string{"id": "project", "root": root}}, "rooms": rooms})
	})
	mux.HandleFunc("POST /api/v1/projects/project/rooms", func(w http.ResponseWriter, r *http.Request) {
		created++
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("room%d", created)})
	})
	mux.HandleFunc("POST /api/v1/rooms/room1/native-bindings/claude", func(w http.ResponseWriter, r *http.Request) {
		var request relay.BindRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"binding": relay.Binding{BindID: request.BindID, Generation: 1, Slot: model.ActorClaude, Active: true}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	endpoint := filepath.Join(t.TempDir(), "custom service's endpoint.json")
	if err := relay.AtomicJSON(endpoint, relay.Endpoint{URL: server.URL, Token: "test-token"}); err != nil {
		t.Fatal(err)
	}
	return root, endpoint, &created
}

func TestBindCreateMissingHooksDoesNotCreateRoom(t *testing.T) {
	for _, own := range []string{"", "claude"} {
		t.Run("runtime="+own, func(t *testing.T) {
			root, endpoint, created := createBindFixture(t, model.RuntimeClaude)
			var out bytes.Buffer
			err := bind(context.Background(), root, options{slot: "claude", create: true, kind: own, endpoint: endpoint}, &out)
			if err == nil || !strings.Contains(err.Error(), "relay install") || *created != 0 || out.Len() != 0 {
				t.Fatalf("missing hooks result: created=%d output=%q err=%v", *created, out.String(), err)
			}
		})
	}
}

func TestBindCreateFailurePreservesRoomForRecovery(t *testing.T) {
	// The Service default pair can differ from the installed local hook. Its
	// resolved selection remains authoritative; post-create failure must name
	// the created Room instead of encouraging another --create.
	root, endpoint, created := createBindFixture(t, model.RuntimeCodex)
	if err := editHooks(root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := bind(context.Background(), root, options{slot: "claude", create: true, endpoint: endpoint}, &out)
	if err == nil || *created != 1 || !strings.Contains(err.Error(), "Room room1 was created") || !strings.Contains(err.Error(), "--room room1") || !strings.Contains(err.Error(), "--replace") || !strings.Contains(err.Error(), "--service-file") {
		t.Fatalf("missing actionable recovery: created=%d err=%v", *created, err)
	}
	if err := editHooks(root, model.RuntimeCodex, false); err != nil {
		t.Fatal(err)
	}
	if err := bind(context.Background(), root, options{slot: "claude", room: "room1", replace: true, endpoint: endpoint}, &out); err != nil {
		t.Fatal(err)
	}
	if *created != 1 {
		t.Fatal("recovery created another Room")
	}
}

func TestBindCreatePeerJoinPreservesLiteralPaths(t *testing.T) {
	root, endpoint, _ := createBindFixture(t, model.RuntimeClaude)
	if err := editHooks(root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := bind(context.Background(), root, options{slot: "claude", create: true, endpoint: endpoint}, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		PeerJoin string `json:"peer_join"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "function pairroom { ConvertTo-Json -Compress -InputObject @($args) }; "+result.PeerJoin)
	} else {
		command = exec.Command("sh", "-c", "pairroom() { printf '%s\\n' \"$@\"; }; "+result.PeerJoin)
	}
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if runtime.GOOS == "windows" {
		if err := json.Unmarshal(output, &args); err != nil {
			t.Fatal(err)
		}
	} else {
		args = strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	}
	want := []string{"relay", "bind", "--room", "room1", "--slot", "codex", "--service-file", endpoint, "--repo", root}
	if fmt.Sprint(args) != fmt.Sprint(want) {
		t.Fatalf("peer command changed literal paths: got=%q want=%q", args, want)
	}
}

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
