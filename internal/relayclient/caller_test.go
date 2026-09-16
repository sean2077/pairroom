package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func isolateCaller(t *testing.T) {
	IsolateNativeCaller(t)
}

func callerState(t *testing.T, root, room, session string, slot model.ActorID, kind model.RuntimeKind) State {
	t.Helper()
	dir, err := secureDir(root, ".pairroom", "rooms", room, "slots", string(slot))
	if err != nil {
		t.Fatal(err)
	}
	s := State{Schema: 1, Room: room, Slot: slot, Runtime: kind, Workspace: root, BindID: "binding", Generation: 1, SessionID: session, EndpointPath: filepath.Join(root, "custom-endpoint.json"), HarnessPID: 123, HarnessName: string(kind)}
	if err := relay.AtomicJSON(filepath.Join(dir, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNativeCallerUsesSessionMetadataWithoutProcessVisibility(t *testing.T) {
	for _, tc := range []struct {
		key  string
		kind model.RuntimeKind
	}{{"CLAUDE_CODE_SESSION_ID", model.RuntimeClaude}, {"CODEX_SESSION_ID", model.RuntimeCodex}, {"GROK_SESSION_ID", model.RuntimeGrok}} {
		t.Run(tc.key, func(t *testing.T) {
			isolateCaller(t)
			t.Setenv(tc.key, "native-session")
			got, err := currentNativeCaller()
			if err != nil || got.runtime != tc.kind || got.session != "native-session" || callerRuntime(options{}) != tc.kind {
				t.Fatalf("caller=%+v err=%v", got, err)
			}
			o := options{}
			if err := applyCallerDefaults(t.TempDir(), "install", &o); err != nil || o.kind != string(tc.kind) {
				t.Fatalf("install did not infer native runtime: %+v %v", o, err)
			}
		})
	}
}

func TestNativeCallerRejectsAmbiguityAndIgnoresOuterHarnessHints(t *testing.T) {
	isolateCaller(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "inner")
	t.Setenv("CODEX_SESSION_ID", "outer")
	if _, err := currentNativeCaller(); err == nil {
		t.Fatal("ambiguous environment guessed a runtime")
	}
	harnessAncestor = func() (int, string, bool) { return 123, "claude", true }
	got, err := currentNativeCaller()
	if err != nil || got.runtime != model.RuntimeClaude || got.session != "inner" {
		t.Fatalf("nearest harness did not win over outer metadata: %+v %v", got, err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "bad\r\nheader")
	if _, err := currentNativeCaller(); err == nil {
		t.Fatal("unsafe session metadata accepted")
	}
}

func TestNativeCallerSelectsDesktopSessionNotSharedPID(t *testing.T) {
	isolateCaller(t)
	root := t.TempDir()
	callerState(t, root, "room-a", "thread-a", model.ActorClaude, model.RuntimeCodex)
	want := callerState(t, root, "room-b", "thread-b", model.ActorCodex, model.RuntimeCodex)
	t.Setenv("CODEX_SESSION_ID", "thread-b")
	for _, action := range []string{"wait", "exchange", "status", "bind"} {
		o := options{}
		if err := applyCallerDefaults(root, action, &o); err != nil || o.room != want.Room || o.slot != string(want.Slot) {
			t.Fatalf("%s guessed shared-PID binding: %+v %v", action, o, err)
		}
		if action == "bind" && o.endpoint != want.EndpointPath {
			t.Fatalf("resume lost the custom Service endpoint default: %+v", o)
		}
	}
	for _, o := range []options{{room: "room-a", slot: "claude"}, {room: "room-b"}} {
		if err := applyCallerDefaults(root, "wait", &o); err == nil {
			t.Fatal("wrong-session or partial explicit target accepted")
		}
	}
	t.Setenv("CODEX_SESSION_ID", "new-thread")
	if err := applyCallerDefaults(root, "wait", &options{}); err == nil {
		t.Fatal("unassociated session fell back to an unrelated inbox")
	}
}

func TestNativeCallerNeverAssociatesPendingStateOrDuplicatesCreatedRoom(t *testing.T) {
	isolateCaller(t)
	root := t.TempDir()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session")
	callerState(t, root, "pending", "", model.ActorClaude, model.RuntimeClaude)
	o := options{}
	if err := applyCallerDefaults(root, "bind", &o); err != nil || o.room != "" {
		t.Fatalf("incomplete binding was auto-selected for bind: %+v %v", o, err)
	}
	if err := applyCallerDefaults(root, "send", &options{}); err == nil || !strings.Contains(err.Error(), "incomplete binding") {
		t.Fatalf("incomplete binding granted collection: %v", err)
	}
	o = options{}
	if err := applyCallerDefaults(root, "status", &o); err != nil || o.room != "pending" || o.slot != "claude" {
		t.Fatalf("unique incomplete binding not diagnosable via status: %+v %v", o, err)
	}
	callerState(t, root, "other-pending", "", model.ActorClaude, model.RuntimeClaude)
	if err := applyCallerDefaults(root, "status", &options{}); err == nil || !strings.Contains(err.Error(), "multiple pending") {
		t.Fatalf("two incomplete bindings were guessed: %v", err)
	}
	o = options{room: "pending", slot: "claude"}
	if err := applyCallerDefaults(root, "status", &o); err != nil || o.room != "pending" {
		t.Fatalf("explicit incomplete-binding status lost: %+v %v", o, err)
	}
	callerState(t, root, "associated", "session", model.ActorClaude, model.RuntimeClaude)
	if err := applyCallerDefaults(root, "bind", &options{create: true}); err == nil {
		t.Fatal("create duplicated an already-associated session")
	}
	if err := applyCallerDefaults(root, "bind", &options{kind: "codex"}); err == nil {
		t.Fatal("runtime mismatch ignored")
	}
}

func TestNativeCreateRunsInsideHarnessWithoutRuntimeOrSlotFlags(t *testing.T) {
	root, endpoint, created := createBindFixture(t, model.RuntimeClaude)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "launching-session")
	if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	if err := editHooks(root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"bind", "--create", "--repo", root, "--service-file", endpoint}, strings.NewReader(""), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Binding relay.Binding `json:"binding"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || *created != 1 || result.Binding.SessionID != "launching-session" {
		t.Fatalf("create did not associate from the environment: %s %v", out.String(), err)
	}
	if bytes.Contains(out.Bytes(), []byte("bind_nonce")) {
		t.Fatal("create still emitted a bind nonce")
	}
	var s State
	if err := readPrivate(filepath.Join(root, ".pairroom", "rooms", "room1", "slots", "claude", "state.json"), &s); err != nil || s.SessionID != "launching-session" {
		t.Fatalf("create did not record the environment association: %+v %v", s, err)
	}
}

func TestNativeCallerRecognizesGrokWithoutOuterSessionMisBinding(t *testing.T) {
	isolateCaller(t)
	harnessAncestor = func() (int, string, bool) { return 4, "grok", true }
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-claude")
	err := applyCallerDefaults(t.TempDir(), "bind", &options{create: true})
	if err != nil || callerRuntime(options{}) != model.RuntimeGrok {
		t.Fatalf("Grok was treated as its outer Claude harness: %v", err)
	}
}

func TestHarnessAncestorHandlesUppercaseWindowsExecutable(t *testing.T) {
	before := processTable
	processTable = func() (map[int]procInfo, error) {
		return map[int]procInfo{os.Getpid(): {ppid: 7, name: "pairroom.exe"}, 7: {ppid: 1, name: "CODEX.EXE"}}, nil
	}
	t.Cleanup(func() { processTable = before })
	_, name, ok := findHarnessAncestor()
	if !ok || name != "codex" {
		t.Fatal("uppercase native executable not recognized")
	}
}

func TestExplicitInstallCanPreparePeerHarnessHooks(t *testing.T) {
	isolateCaller(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "own-session")
	o := options{kind: "codex"}
	if err := applyCallerDefaults(t.TempDir(), "install", &o); err != nil || o.kind != "codex" {
		t.Fatalf("explicit peer hook setup was treated as rebinding: %+v %v", o, err)
	}
}
