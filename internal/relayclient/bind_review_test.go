package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func serveDefaultPairForTest(mux *http.ServeMux, agents map[model.ActorID]model.AgentSelection) {
	mux.HandleFunc("GET /api/v1/agent-pair-profiles", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"default_profile_id": "", "profiles": []any{}})
	})
	mux.HandleFunc("GET /api/v1/agent-catalog", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"defaults": agents})
	})
}

func TestBindCreateMissingIdentityHasNoProvisioning(t *testing.T) {
	for _, slot := range []string{"claude", "codex"} {
		for _, recognized := range []bool{false, true} {
			t.Run(slot+map[bool]string{false: "-plain", true: "-harness"}[recognized], func(t *testing.T) {
				root, endpoint, created := createBindFixture(t, model.RuntimeClaude)
				if err := editHooks(root, model.RuntimeClaude, false); err != nil {
					t.Fatal(err)
				}
				if err := editHooks(root, model.RuntimeCodex, false); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CLAUDE_CODE_SESSION_ID", "")
				t.Setenv("CODEX_SESSION_ID", "")
				stubLineage(t, 4242, "claude", recognized)
				var out bytes.Buffer
				err := bind(context.Background(), root, options{create: true, slot: slot, endpoint: endpoint}, &out)
				if err == nil || !strings.Contains(err.Error(), "inside") || *created != 0 || out.Len() != 0 {
					t.Fatalf("missing identity must reject before creation: created=%d err=%v", *created, err)
				}
				if _, err := os.Stat(filepath.Join(root, ".pairroom")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed preflight left local binding state")
				}
			})
		}
	}
}

func TestBindLostResponseReusesAttemptIdentity(t *testing.T) {
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
		t.Run(string(kind), func(t *testing.T) { testBindLostResponseReusesAttemptIdentity(t, kind) })
	}
}

func testBindLostResponseReusesAttemptIdentity(t *testing.T, kind model.RuntimeKind) {
	t.Helper()
	isolateCaller(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := editHooks(root, kind, false); err != nil {
		t.Fatal(err)
	}
	stubLineage(t, 4242, string(kind), true)
	t.Setenv(sessionEnvVars[kind], "official-session")
	var mu sync.Mutex
	var accepted relay.BindRequest
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/service", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"projects": []any{map[string]string{"id": "project", "root": root}},
			"rooms":    []any{map[string]any{"id": "room", "project_id": "project", "host_mode": "native", "lifecycle": "active", "agents": map[model.ActorID]model.AgentSelection{model.ActorSlot1: {Runtime: kind}, model.ActorSlot2: {Runtime: model.RuntimeCodex}}}},
		})
	})
	mux.HandleFunc("POST /api/v1/rooms/room/native-bindings/slot1", func(w http.ResponseWriter, r *http.Request) {
		var req relay.BindRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			accepted = req
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close() // Service accepted, but the CLI never received the response.
			return
		}
		if req != accepted {
			t.Error("recovery changed original binding request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"binding": relay.Binding{BindID: accepted.BindID, Generation: 1, Slot: model.ActorSlot1, Active: true, SessionID: accepted.SessionID}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	endpoint := filepath.Join(t.TempDir(), "endpoint.json")
	if err := relay.AtomicJSON(endpoint, relay.Endpoint{URL: srv.URL, Token: "test-token"}); err != nil {
		t.Fatal(err)
	}
	o := options{room: "room", slot: "claude", endpoint: endpoint}
	var out bytes.Buffer
	if err := bind(context.Background(), root, o, &out); err == nil {
		t.Fatal("lost response must be uncertain")
	}
	dir := filepath.Join(root, ".pairroom", "rooms", "room", "slots", "slot1")
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unconfirmed bind entered discovery")
	}
	var pending bindAttempt
	if err := readPrivate(filepath.Join(dir, bindAttemptFile), &pending); err != nil {
		t.Fatal(err)
	}
	if err := bind(context.Background(), root, o, &out); err != nil {
		t.Fatal(err)
	}
	var current State
	if err := readPrivate(filepath.Join(dir, "state.json"), &current); err != nil {
		t.Fatal(err)
	}
	if current.BindID != pending.State.BindID || current.Generation != 1 {
		t.Fatal("recovery rotated binding")
	}
	if _, err := os.Stat(filepath.Join(dir, bindAttemptFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("confirmed attempt not cleaned")
	}
	if strings.Contains(out.String(), pending.Credentials.Secret) {
		t.Fatal("credential exposed in bind output")
	}
}

func TestBindPromotionRecoveryPreservesPublicationWAL(t *testing.T) {
	dir := t.TempDir()
	s := State{Schema: 2, Room: "room", Slot: model.ActorSlot1, Runtime: model.RuntimeClaude, Workspace: "/project", BindID: "bind", SessionID: "session"}
	cred := credentials{BindID: "bind", Secret: "private-test-secret"}
	if err := relay.AtomicJSON(filepath.Join(dir, bindAttemptFile), bindAttempt{State: s, Credentials: cred}); err != nil {
		t.Fatal(err)
	}
	s.Generation = 1
	s.LastSeq = 7
	s.LastConfirmedSeq = 6
	s.Pending = &Pending{Seq: 7, Text: "pending complete reply", Unknown: true}
	if err := relay.AtomicJSON(filepath.Join(dir, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	if err := relay.AtomicJSON(filepath.Join(dir, "credentials"), cred); err != nil {
		t.Fatal(err)
	}
	got, staged, err := prepareBindAttempt(dir, "/project", "endpoint", options{room: "room"}, s.Slot, s.Runtime, s.SessionID)
	if err != nil || !staged || got.State.LastSeq != 7 || got.State.LastConfirmedSeq != 6 || got.State.Pending == nil || got.State.Pending.Text != s.Pending.Text {
		t.Fatalf("interrupted promotion lost publication WAL: staged=%v err=%v", staged, err)
	}
}

func TestHookIdentityDivergenceIsNotAnUnrelatedSession(t *testing.T) {
	newBound := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "state.json")
		s := State{Schema: 2, Room: "room", Slot: model.ActorSlot1, Runtime: model.RuntimeClaude, BindID: "bind", Generation: 1, SessionID: "bound", HarnessPID: 4242, HarnessName: "claude"}
		if err := relay.AtomicJSON(path, s); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// Exact environment metadata is the only divergence evidence: the harness
	// reported the bound session while this invocation belongs to another one.
	t.Run("environment", func(t *testing.T) {
		path := newBound(t)
		stubLineage(t, 4242, "claude", true)
		t.Setenv("CLAUDE_CODE_SESSION_ID", "bound")
		if _, err := boundHookCandidates([]string{path}, model.RuntimeClaude, "changed"); !errors.Is(err, errHookSessionMismatch) {
			t.Fatalf("divergence not visible: %v", err)
		}
	})
	// A shared or reused harness PID is not session identity: a fresh session in
	// the same host process stays inert instead of inheriting another session's
	// binding, and so does a caller with no exact evidence for its own id.
	t.Run("lineage", func(t *testing.T) {
		path := newBound(t)
		stubLineage(t, 4242, "claude", true)
		t.Setenv("CLAUDE_CODE_SESSION_ID", "")
		got, err := boundHookCandidates([]string{path}, model.RuntimeClaude, "changed")
		if err != nil || len(got) != 0 {
			t.Fatalf("shared harness process decided session identity: candidates=%v err=%v", got, err)
		}
		stubLineage(t, 4343, "claude", true)
		t.Setenv("CLAUDE_CODE_SESSION_ID", "unrelated")
		got, err = boundHookCandidates([]string{path}, model.RuntimeClaude, "unrelated")
		if err != nil || len(got) != 0 {
			t.Fatalf("unbound caller was not ignored: %v", err)
		}
	})
}

func TestRetiredBindFlagsAreRejected(t *testing.T) {
	for _, flag := range []string{"--continue", "--session-id"} {
		var out, diagnostic bytes.Buffer
		err := Run(context.Background(), []string{"bind", flag}, strings.NewReader(""), &out, &diagnostic)
		if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") || out.Len() != 0 {
			t.Fatalf("retired flag accepted: %s", flag)
		}
	}
}

func TestHookExactSessionSurvivesSharedHarnessProcess(t *testing.T) {
	stubLineage(t, 4242, "claude", true)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "second-session")
	var paths []string
	for _, session := range []string{"first-session", "second-session"} {
		path := filepath.Join(t.TempDir(), "state.json")
		s := State{Schema: 2, Room: session, Slot: model.ActorSlot1, Runtime: model.RuntimeClaude, BindID: session, Generation: 1, SessionID: session, HarnessPID: 4242, HarnessName: "claude"}
		if err := relay.AtomicJSON(path, s); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	got, err := boundHookCandidates(paths, model.RuntimeClaude, "second-session")
	if err != nil || len(got) != 1 || got[0] != filepath.Dir(paths[1]) {
		t.Fatalf("shared process caused false identity failure: candidates=%v err=%v", got, err)
	}
}

func TestBindReplaceRequiresExplicitBacklogDecision(t *testing.T) {
	dir := t.TempDir()
	s := State{Schema: 2, Room: "room", Slot: model.ActorSlot1, Runtime: model.RuntimeClaude, Workspace: "/project", BindID: "bind", Generation: 1, SessionID: "session", LastSeq: 5, LastConfirmedSeq: 3,
		Pending: &Pending{Seq: 4, Text: "uncertain head", Unknown: true}, Held: []Pending{{Seq: 5, Text: "held"}}}
	if err := relay.AtomicJSON(filepath.Join(dir, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	if err := relay.AtomicJSON(filepath.Join(dir, "credentials"), credentials{BindID: "bind", Secret: "private-test-secret"}); err != nil {
		t.Fatal(err)
	}
	replace := options{room: "room", replace: true}
	for _, want := range []string{"2 unpublished Stop replies (oldest seq 4)", "1 unpublished Stop replies (oldest seq 5)"} {
		if _, _, err := prepareBindAttempt(dir, "/project", "endpoint", replace, s.Slot, s.Runtime, s.SessionID); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("replace error = %v, want %q", err, want)
		}
		// Each explicit discard drops only the head; the next reply is still guarded.
		c, err := loadLocal(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.DiscardPending(); err != nil {
			t.Fatal(err)
		}
	}
	next, staged, err := prepareBindAttempt(dir, "/project", "endpoint", replace, s.Slot, s.Runtime, s.SessionID)
	if err != nil || !staged || next.State.BindID == s.BindID {
		t.Fatalf("replace after explicit discards: staged=%v err=%v", staged, err)
	}
}

func TestBindAttemptRetryAndExplicitReplacementHaveSeparateIntent(t *testing.T) {
	dir := t.TempDir()
	s := State{Schema: 2, Room: "room", Slot: model.ActorSlot1, Runtime: model.RuntimeClaude, Workspace: "/project", BindID: "old-attempt", SessionID: "session"}
	pending := bindAttempt{State: s, Credentials: credentials{BindID: s.BindID, Secret: "private-test-secret"}, Replace: true}
	if err := relay.AtomicJSON(filepath.Join(dir, bindAttemptFile), pending); err != nil {
		t.Fatal(err)
	}
	retry, staged, err := prepareBindAttempt(dir, "/project", "endpoint", options{room: "room"}, s.Slot, s.Runtime, s.SessionID)
	if err != nil || !staged || retry.State.BindID != s.BindID || !retry.Replace {
		t.Fatalf("ordinary retry lost original replacement intent: staged=%v err=%v", staged, err)
	}
	next, staged, err := prepareBindAttempt(dir, "/project", "endpoint", options{room: "room", replace: true}, s.Slot, s.Runtime, s.SessionID)
	if err != nil || !staged || next.State.BindID == s.BindID || !next.Replace {
		t.Fatalf("explicit replacement cannot escape a revoked pending ID: staged=%v err=%v", staged, err)
	}
}
