package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func sessionGitRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return root
}

func sessionChdir(t *testing.T, path string) {
	t.Helper()
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(before); err != nil {
			t.Error(err)
		}
	})
}

func saveSessionTestState(t *testing.T, s State) {
	t.Helper()
	if err := relay.AtomicJSON(filepath.Join(s.Workspace, ".pairroom", "rooms", s.Room, "slots", string(s.Slot), "state.json"), s); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSessionWorkspaceSurvivesDirectoryChanges(t *testing.T) {
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
		t.Run(string(kind), func(t *testing.T) {
			isolateCaller(t)
			root := sessionGitRoot(t)
			s := callerState(t, root, "room", "session", model.ActorSlot1, kind)
			if err := rememberSession(s); err != nil {
				t.Fatal(err)
			}
			subdir := filepath.Join(root, "src")
			if err := os.Mkdir(subdir, 0700); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("git", "-C", root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "fixture").CombinedOutput(); err != nil {
				t.Fatalf("git commit: %v: %s", err, out)
			}
			linked := filepath.Join(t.TempDir(), "linked")
			if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", linked, "HEAD").CombinedOutput(); err != nil {
				t.Fatalf("git worktree: %v: %s", err, out)
			}
			nested := filepath.Join(root, "nested-repo")
			if out, err := exec.Command("git", "init", "-q", nested).CombinedOutput(); err != nil {
				t.Fatalf("nested git init: %v: %s", err, out)
			}
			for _, path := range []string{root, subdir, linked, nested, sessionGitRoot(t), t.TempDir()} {
				t.Run(filepath.Base(path), func(t *testing.T) {
					sessionChdir(t, path)
					before, _ := os.Getwd()
					for _, action := range []string{"send", "wait", "exchange", "bind", "status", "history", "doctor", "review", "hook"} {
						o := options{repo: "."}
						got, err := resolveSessionWorkspace(context.Background(), action, &o, nativeCaller{runtime: kind, session: "session"})
						if err != nil || got != root || o.room != s.Room || o.slot != string(s.Slot) {
							t.Fatalf("%s from %s: root=%s options=%+v err=%v", action, path, got, o, err)
						}
						if action == "bind" && o.endpoint != s.EndpointPath {
							t.Fatal("resume lost its custom Service endpoint")
						}
					}
					if after, _ := os.Getwd(); after != before {
						t.Fatal("routing changed the file-argument working directory")
					}
				})
			}
		})
	}
}

func TestNativeSessionWorkspaceRejectsConflictsAndDuplicateCreation(t *testing.T) {
	isolateCaller(t)
	root, other := sessionGitRoot(t), sessionGitRoot(t)
	s := callerState(t, root, "room", "session", model.ActorSlot1, model.RuntimeClaude)
	if err := rememberSession(s); err != nil {
		t.Fatal(err)
	}
	caller := nativeCaller{runtime: model.RuntimeClaude, session: "session"}
	for _, o := range []options{
		{repo: other, repoExplicit: true},
		{repo: other, room: "different"},
		{repo: other, slot: "2"},
		{repo: other, endpoint: filepath.Join(other, "endpoint.json")},
	} {
		if _, err := resolveSessionWorkspace(context.Background(), "send", &o, caller); err == nil || !strings.Contains(err.Error(), "conflicts") {
			t.Fatalf("conflicting selector accepted: %+v: %v", o, err)
		}
	}
	if _, err := resolveSessionWorkspace(context.Background(), "bind", &options{repo: other, create: true}, caller); err == nil || !strings.Contains(err.Error(), "already associated") {
		t.Fatalf("directory drift permitted duplicate creation: %v", err)
	}
	for _, c := range []nativeCaller{{runtime: model.RuntimeClaude, session: "new-session"}, {runtime: model.RuntimeCodex, session: "session"}} {
		if got, err := indexedSessions(c); err != nil || len(got) != 0 {
			t.Fatalf("another session/runtime inherited a binding: %+v %v", got, err)
		}
	}
}

func TestNativeSessionLocatorRevalidatesStateAndGeneration(t *testing.T) {
	isolateCaller(t)
	root := sessionGitRoot(t)
	s := callerState(t, root, "room", "session", model.ActorSlot1, model.RuntimeClaude)
	if err := rememberSession(s); err != nil {
		t.Fatal(err)
	}
	caller := nativeCaller{runtime: s.Runtime, session: s.SessionID}
	dir, err := locatorDirectory(caller, false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, locatorFilename(s))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"credentials", "secret", "session_id", "pending", "transcript", "endpoint_path"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("locator copied %s", forbidden)
		}
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("locator is not private: %v", err)
	}
	next := s
	next.Generation++
	saveSessionTestState(t, next)
	if got, err := indexedSessions(caller); err != nil || len(got) != 0 {
		t.Fatalf("stale generation was trusted: %+v %v", got, err)
	}
	if err := rememberSession(next); err != nil {
		t.Fatal(err)
	}
	if err := forgetSession(s); err != nil {
		t.Fatal(err)
	}
	if got, err := indexedSessions(caller); err != nil || len(got) != 1 || got[0].Generation != next.Generation {
		t.Fatalf("old cleanup deleted a new locator: %+v %v", got, err)
	}
	next.SessionID = "replacement"
	next.BindID = "replacement"
	saveSessionTestState(t, next)
	if got, err := indexedSessions(caller); err != nil || len(got) != 0 {
		t.Fatalf("old session followed replacement credentials: %+v %v", got, err)
	}
	if err := rememberSession(next); err != nil {
		t.Fatal(err)
	}
	if err := forgetSession(next); err != nil {
		t.Fatal(err)
	}
	if got, err := indexedSessions(nativeCaller{runtime: next.Runtime, session: next.SessionID}); err != nil || len(got) != 0 {
		t.Fatalf("empty locator directory is not inert: %+v %v", got, err)
	}
	next.Generation = 0
	if err := rememberSession(next); err == nil {
		t.Fatal("unconfirmed attempt was indexed")
	}
}

func TestNativeSessionLocatorMissingAndUnsafePathsFailClosed(t *testing.T) {
	for _, mode := range []string{"missing-workspace", "symlink-index", "symlink-state", "malformed-index"} {
		t.Run(mode, func(t *testing.T) {
			isolateCaller(t)
			root := sessionGitRoot(t)
			s := callerState(t, root, "room", "session", model.ActorSlot1, model.RuntimeClaude)
			if err := rememberSession(s); err != nil {
				t.Fatal(err)
			}
			caller := nativeCaller{runtime: s.Runtime, session: s.SessionID}
			dir, err := locatorDirectory(caller, false)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, locatorFilename(s))
			switch mode {
			case "missing-workspace":
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
			case "malformed-index":
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if mode == "symlink-state" {
					path = filepath.Join(root, ".pairroom", "rooms", "room", "slots", "slot1", "state.json")
				}
				if err := os.Rename(path, path+"-target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-target", path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			if _, err := indexedSessions(caller); err == nil {
				t.Fatal("unsafe/missing locator target accepted")
			}
		})
	}
}

func TestNativeSessionWorkspaceAmbiguityAcrossServices(t *testing.T) {
	isolateCaller(t)
	caller := nativeCaller{runtime: model.RuntimeClaude, session: "session"}
	var states []State
	for range 2 {
		s := callerState(t, sessionGitRoot(t), "room", "session", model.ActorSlot1, model.RuntimeClaude)
		if err := rememberSession(s); err != nil {
			t.Fatal(err)
		}
		states = append(states, s)
	}
	if _, err := resolveSessionWorkspace(context.Background(), "wait", &options{repo: "."}, caller); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("guessed between Services: %v", err)
	}
	o := options{repo: ".", endpoint: states[1].EndpointPath}
	if root, err := resolveSessionWorkspace(context.Background(), "wait", &o, caller); err != nil || root != states[1].Workspace {
		t.Fatalf("explicit Service did not disambiguate: %s %v", root, err)
	}
}

func TestNativeSessionWorkspaceColdDiscoveryAndNoWarmHTTP(t *testing.T) {
	isolateCaller(t)
	root := sessionGitRoot(t)
	s := callerState(t, root, "room", "session", model.ActorSlot1, model.RuntimeClaude)
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/service" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("unexpected discovery request")
		}
		_ = json.NewEncoder(w).Encode(serviceSnapshot{Projects: []serviceProject{{ID: "project", Root: root}}})
	}))
	t.Cleanup(server.Close)
	endpoint, err := defaultEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(endpoint), 0700); err != nil {
		t.Fatal(err)
	}
	if err := relay.AtomicJSON(endpoint, relay.Endpoint{URL: server.URL, Token: "fixture"}); err != nil {
		t.Fatal(err)
	}
	s.EndpointPath = endpoint
	saveSessionTestState(t, s)
	sessionChdir(t, t.TempDir())
	caller := nativeCaller{runtime: s.Runtime, session: s.SessionID}
	o := options{repo: "."}
	if got, err := resolveSessionWorkspace(context.Background(), "wait", &o, caller); err != nil || got != root || reads.Load() != 1 {
		t.Fatalf("cold lookup failed: %s %v reads=%d", got, err, reads.Load())
	}
	if err := rememberSession(s); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if got, err := resolveSessionWorkspace(context.Background(), "wait", &options{repo: "."}, caller); err != nil || got != root || reads.Load() != 1 {
		t.Fatalf("warm lookup depended on Management: %s %v reads=%d", got, err, reads.Load())
	}
}

func TestNativeSessionWorkspaceInitialHintsAndHookMismatch(t *testing.T) {
	isolateCaller(t)
	root, other := sessionGitRoot(t), sessionGitRoot(t)
	t.Setenv("CLAUDE_PROJECT_DIR", root)
	caller := nativeCaller{runtime: model.RuntimeClaude, session: "session"}
	o := options{repo: other}
	if _, err := resolveSessionWorkspace(context.Background(), "bind", &o, caller); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("conflicting initial checkout was guessed: %v", err)
	}
	o.repoExplicit = true
	if got, err := resolveSessionWorkspace(context.Background(), "bind", &o, caller); err != nil || got != other {
		t.Fatalf("explicit initial workspace ignored: %s %v", got, err)
	}
	s := callerState(t, root, "room", "session", model.ActorSlot1, model.RuntimeClaude)
	if err := rememberSession(s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session")
	_, err := resolveSessionWorkspace(context.Background(), "hook", &options{repo: t.TempDir()}, nativeCaller{runtime: model.RuntimeClaude, session: "different"})
	if !errors.Is(err, errHookSessionMismatch) {
		t.Fatalf("hook identity drift became a silent no-op: %v", err)
	}
	if err := validateCommandCaller(&Client{State: State{Runtime: model.RuntimeClaude, SessionID: "replacement"}}); !errors.Is(err, relay.ErrAuth) {
		t.Fatal("slot-lock replacement race was not revalidated")
	}
}

func TestNativeSessionCLIFileArgumentsStayRelativeToInvocation(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{envelope: "incoming evidence"})
	var s State
	if err := readPrivate(f.statePath, &s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", s.SessionID)
	// A normal, explicit call upgrades an old binding without rotating it.
	if err := f.run(context.Background(), "peer", strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	sessionChdir(t, t.TempDir())
	if err := os.WriteFile("body.md", []byte("body from current directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("evidence.md", []byte("local evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"send", "--id", "cwd-file", "--text-file", "body.md", "--ref", "evidence.md"}, strings.NewReader(""), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	text := f.messages["cwd-file"].Text
	f.mu.Unlock()
	if !strings.HasPrefix(text, "body from current directory") {
		t.Fatalf("wrong relative body: %s", text)
	}
	refs := parseMessageReferences(t, text)
	if len(refs) != 1 {
		t.Fatalf("missing file reference: %s", text)
	}
	out.Reset()
	if err := Run(context.Background(), []string{"wait", "--timeout", "1", "--output-file", "incoming.txt"}, strings.NewReader(""), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile("incoming.txt"); err != nil || string(got) != "incoming evidence\n" {
		t.Fatalf("output path changed: %s %v", got, err)
	}
	if f.count("send") != 1 || f.count("wait") != 1 || f.count("ack") != 1 {
		t.Fatal("transport contract changed")
	}
	if _, err := os.Stat(filepath.Join(s.Workspace, "incoming.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("output was written relative to the binding workspace")
	}
	if err := Run(context.Background(), []string{"unbind", "--local-only"}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, err := indexedSessions(nativeCaller{runtime: s.Runtime, session: s.SessionID}); err != nil || len(got) != 0 {
		t.Fatalf("local unbind retained locator: %+v %v", got, err)
	}
}

func TestNativeSessionExplicitRoomJoinOutsideGitCreatesLocator(t *testing.T) {
	root, endpoint, created := createBindFixture(t, model.RuntimeClaude)
	*created = 1
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := editHooks(root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	sessionChdir(t, t.TempDir())
	if err := Run(context.Background(), []string{"bind", "--room", "room1", "--service-file", endpoint}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if states, err := indexedSessions(nativeCaller{runtime: model.RuntimeClaude, session: "official-session"}); err != nil || len(states) != 1 || states[0].Workspace != root {
		t.Fatalf("bind did not create confirmed locator: %+v %v", states, err)
	}
}

func TestNativeSessionStopHookPublishesAndCollectsOutsideGit(t *testing.T) {
	isolateCaller(t)
	root := sessionGitRoot(t)
	s := callerState(t, root, "room", "session", model.ActorSlot1, model.RuntimeClaude)
	var reports, acks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Relay fixture-secret" || r.Header.Get("X-PairRoom-Session") != s.SessionID || r.Header.Get("X-PairRoom-Bind") != s.BindID {
			t.Error("hook lost binding authentication")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch filepath.Base(r.URL.Path) {
		case "inspect":
			_ = json.NewEncoder(w).Encode(relay.Binding{BindID: s.BindID, Slot: s.Slot, Generation: s.Generation, SessionID: s.SessionID, Active: true})
		case "report":
			var input struct {
				Seq  uint64 `json:"report_seq"`
				Text string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Text != "reply from changed cwd" {
				t.Error("hook lost reply")
			}
			reports.Add(1)
			_ = json.NewEncoder(w).Encode(relay.Publication{BindID: s.BindID, Generation: s.Generation, ReportSeq: input.Seq})
		case "wait":
			_ = json.NewEncoder(w).Encode(map[string]any{"claim": relay.Claim{ID: "message", Receipt: "receipt", Envelope: "peer reply"}})
		case "ack":
			acks.Add(1)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected hook operation: %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	if err := relay.AtomicJSON(s.EndpointPath, relay.Endpoint{URL: server.URL, Token: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := relay.AtomicJSON(filepath.Join(root, ".pairroom", "rooms", s.Room, "slots", string(s.Slot), "credentials"), credentials{BindID: s.BindID, Secret: "fixture-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := rememberSession(s); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	sessionChdir(t, cwd)
	payload, err := json.Marshal(map[string]any{"hook_event_name": "Stop", "session_id": s.SessionID, "cwd": cwd, "last_assistant_message": "reply from changed cwd", "stop_hook_active": false})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"hook", "--runtime", "claude"}, bytes.NewReader(payload), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if reports.Load() != 1 || acks.Load() != 1 || !strings.Contains(out.String(), `"decision":"block"`) || !strings.Contains(out.String(), "peer reply") {
		t.Fatalf("hook became inert after cd: reports=%d acks=%d output=%s", reports.Load(), acks.Load(), out.String())
	}
}
