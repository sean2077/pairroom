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
	t.Helper()
	for _, key := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "GROK_SESSION_ID", "CLAUDECODE"} {
		t.Setenv(key, "")
	}
	before := harnessAncestor
	harnessAncestor = func() (int, string, bool) { return 0, "", false }
	t.Cleanup(func() { harnessAncestor = before })
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
	}{{"CLAUDE_CODE_SESSION_ID", model.RuntimeClaude}, {"CODEX_THREAD_ID", model.RuntimeCodex}} {
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
	t.Setenv("CODEX_THREAD_ID", "outer")
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
	t.Setenv("CODEX_THREAD_ID", "thread-b")
	for _, action := range []string{"wait", "exchange", "status", "bind"} {
		o := options{}
		if err := applyCallerDefaults(root, action, &o); err != nil || o.room != want.Room || o.slot != string(want.Slot) {
			t.Fatalf("%s guessed shared-PID binding: %+v %v", action, o, err)
		}
		if action == "bind" && (!o.cont || o.session != want.SessionID || o.endpoint != want.EndpointPath) {
			t.Fatalf("resume lost session/custom Service defaults: %+v", o)
		}
	}
	for _, o := range []options{{room: "room-a", slot: "claude"}, {room: "room-b"}} {
		if err := applyCallerDefaults(root, "wait", &o); err == nil {
			t.Fatal("wrong-session or partial explicit target accepted")
		}
	}
	t.Setenv("CODEX_THREAD_ID", "new-thread")
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
	if err := applyCallerDefaults(root, "bind", &o); err != nil || o.cont || o.session != "" {
		t.Fatalf("metadata bypassed nonce: %+v %v", o, err)
	}
	if err := applyCallerDefaults(root, "send", &options{}); err == nil {
		t.Fatal("pending binding granted collection")
	}
	callerState(t, root, "associated", "session", model.ActorClaude, model.RuntimeClaude)
	if err := applyCallerDefaults(root, "bind", &options{create: true}); err == nil {
		t.Fatal("create duplicated an already-associated session")
	}
	if err := applyCallerDefaults(root, "bind", &options{session: "another"}); err == nil {
		t.Fatal("explicit session mismatch ignored")
	}
	if err := applyCallerDefaults(root, "bind", &options{kind: "codex"}); err == nil {
		t.Fatal("runtime mismatch ignored")
	}
}

func TestNativeCreateRunsInsideHarnessWithoutRuntimeOrSlotFlags(t *testing.T) {
	isolateCaller(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "launching-session")
	root, endpoint, created := createBindFixture(t, model.RuntimeClaude)
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
		Nonce string `json:"bind_nonce"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Nonce == "" || *created != 1 {
		t.Fatalf("create did not preserve nonce association: %s %v", out.String(), err)
	}
	var s State
	if err := readPrivate(filepath.Join(root, ".pairroom", "rooms", "room1", "slots", "claude", "state.json"), &s); err != nil || s.SessionID != "" {
		t.Fatalf("create forged official association: %+v %v", s, err)
	}
}

func TestNativeCallerRecognizesUnsupportedGrokWithoutMisBinding(t *testing.T) {
	isolateCaller(t)
	harnessAncestor = func() (int, string, bool) { return 4, "grok", true }
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-claude")
	err := applyCallerDefaults(t.TempDir(), "bind", &options{create: true})
	if err == nil || !strings.Contains(err.Error(), "Grok Build was detected") {
		t.Fatalf("unsupported harness was treated as Claude: %v", err)
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
