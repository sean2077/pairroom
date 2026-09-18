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
)

func writeSlotState(t *testing.T, root, room, slot string, pid int, name string) {
	t.Helper()
	slot = normalizeSlot(slot)
	dir := filepath.Join(root, ".pairroom", "rooms", room, "slots", slot)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := State{Schema: 2, Room: room, Slot: model.ActorID(slot), BindID: "bind-" + room + slot, Generation: 1, SessionID: "session-" + room + slot, HarnessPID: pid, HarnessName: name}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUnbindLocalOnlyRemovesFilesWithoutContactingService(t *testing.T) {
	isolateCaller(t)
	root := t.TempDir()
	writeSlotState(t, root, "room1", "slot1", 0, "")
	dir := filepath.Join(root, ".pairroom", "rooms", "room1", "slots", "slot1")
	if err := os.WriteFile(filepath.Join(dir, "credentials"), []byte(`{"bind_id":"bind-room1slot1","secret":"s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := unbindLocalOnly(context.Background(), root, dir, options{}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"unbound":"local-only"`) || !strings.Contains(out.String(), "stays active") {
		t.Fatalf("local-only receipt: %s", out.String())
	}
	for _, name := range []string{"state.json", "credentials"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still present: %v", name, err)
		}
	}
}

func TestLocalOnlyFlagAppliesOnlyToUnbind(t *testing.T) {
	err := Run(context.Background(), []string{"status", "--local-only"}, nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--local-only applies only to unbind") {
		t.Fatalf("status --local-only: %v", err)
	}
}

func TestRunUnbindLocalOnlyDoesNotNeedService(t *testing.T) {
	isolateCaller(t)
	root := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	writeSlotState(t, root, "room1", "slot1", 0, "")
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"unbind", "--repo", root, "--room", "room1", "--slot", "1", "--local-only"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"unbound":"local-only"`) {
		t.Fatalf("run local-only receipt: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".pairroom", "rooms", "room1", "slots", "slot1", "state.json")); !os.IsNotExist(err) {
		t.Fatalf("state.json still present: %v", err)
	}
}

func stubLineage(t *testing.T, pid int, name string, ok bool) {
	t.Helper()
	IsolateNativeCaller(t)
	harnessAncestor = func() (int, string, bool) { return pid, name, ok }
}

func TestResolveSlotDefaultsSoleBinding(t *testing.T) {
	stubLineage(t, 0, "", false)
	root := t.TempDir()
	writeSlotState(t, root, "room1", "claude", 0, "")
	var o options
	if err := resolveSlotDefaults(root, &o); err != nil {
		t.Fatalf("resolveSlotDefaults: %v", err)
	}
	if o.room != "room1" || o.slot != "slot1" {
		t.Fatalf("resolved %q/%q", o.room, o.slot)
	}
}

func TestResolveSlotDefaultsSoleBindingChecksRecognizedCaller(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pid       int
		harness   string
		wantError bool
	}{
		{"same session", 111, "claude", false},
		{"different runtime", 222, "codex", true},
		{"different session", 222, "claude", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeSlotState(t, root, "room", "claude", 111, "claude")
			stubLineage(t, tc.pid, tc.harness, true)
			var o options
			err := resolveSlotDefaults(root, &o)
			if (err != nil) != tc.wantError {
				t.Fatalf("resolve = %+v, %v", o, err)
			}
			if tc.wantError && (o.room != "" || o.slot != "" || !strings.Contains(err.Error(), "--room room --slot slot1")) {
				t.Fatalf("mismatch must require explicit selection: %+v %v", o, err)
			}
		})
	}
}

func TestResolveSlotDefaultsRecognizedCallerRequiresRecordedLineage(t *testing.T) {
	root := t.TempDir()
	writeSlotState(t, root, "legacy", "claude", 0, "")
	stubLineage(t, 222, "claude", true)
	var o options
	if err := resolveSlotDefaults(root, &o); err == nil {
		t.Fatal("unknown binding lineage silently selected")
	}
	o = options{room: "legacy", slot: "claude"}
	if err := resolveSlotDefaults(root, &o); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSlotDefaultsLineageSelectsOwnSlot(t *testing.T) {
	root := t.TempDir()
	writeSlotState(t, root, "roomA", "claude", 111, "claude")
	writeSlotState(t, root, "roomB", "codex", 222, "codex")
	stubLineage(t, 222, "codex", true)
	var o options
	if err := resolveSlotDefaults(root, &o); err != nil {
		t.Fatalf("resolveSlotDefaults: %v", err)
	}
	if o.room != "roomB" || o.slot != "slot2" {
		t.Fatalf("resolved %q/%q", o.room, o.slot)
	}
}

func TestResolveSlotDefaultsAmbiguityFailsWithCandidates(t *testing.T) {
	root := t.TempDir()
	writeSlotState(t, root, "roomA", "claude", 111, "claude")
	writeSlotState(t, root, "roomB", "codex", 222, "codex")
	stubLineage(t, 0, "", false)
	var o options
	err := resolveSlotDefaults(root, &o)
	if err == nil {
		t.Fatal("ambiguous workspace must fail")
	}
	if !strings.Contains(err.Error(), "--room roomA --slot slot1") || !strings.Contains(err.Error(), "--room roomB --slot slot2") {
		t.Fatalf("error must list candidates: %v", err)
	}
}

func TestResolveSlotDefaultsUnmatchedLineageFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeSlotState(t, root, "roomA", "claude", 111, "claude")
	writeSlotState(t, root, "roomB", "codex", 222, "codex")
	stubLineage(t, 999, "claude", true) // PID matches no recorded binding
	var o options
	if err := resolveSlotDefaults(root, &o); err == nil {
		t.Fatal("unmatched lineage must not silently pick a slot")
	}
}

func TestResolveSlotDefaultsNoBinding(t *testing.T) {
	var o options
	err := resolveSlotDefaults(t.TempDir(), &o)
	if err == nil || !strings.Contains(err.Error(), "run pairroom relay bind first") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveSlotDefaultsSkipsIncompleteBinding(t *testing.T) {
	// A bind that never confirmed (no generation/session id) must not be
	// auto-selected for foreground commands; the caller is told to bind first.
	root := t.TempDir()
	dir := filepath.Join(root, ".pairroom", "rooms", "room1", "slots", "slot1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := State{Schema: 2, Room: "room1", Slot: model.ActorSlot1, Runtime: model.RuntimeClaude, BindID: "bind-incomplete"}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	stubLineage(t, 0, "", false)
	var o options
	if err := resolveSlotDefaults(root, &o); err == nil || !strings.Contains(err.Error(), "run pairroom relay bind first") {
		t.Fatalf("incomplete binding must be skipped: %v", err)
	}
}

func TestResolveSlotDefaultsExplicitFlagsSkipScan(t *testing.T) {
	o := options{room: "r1", slot: "claude"}
	if err := resolveSlotDefaults(t.TempDir(), &o); err != nil {
		t.Fatalf("explicit flags must win without scanning: %v", err)
	}
}

func TestResolveSlotDefaultsPartialFlagsRejected(t *testing.T) {
	var o options
	o.slot = "claude"
	if err := resolveSlotDefaults(t.TempDir(), &o); err == nil {
		t.Fatal("partial explicit flags must be rejected, not silently completed")
	}
}

func TestStateToleratesPreLineageFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"schema":2,"room":"r","slot":"slot1","runtime":"claude","workspace":"/ws","endpoint_path":"/ep","bind_id":"b","generation":1,"last_seq":0,"last_confirmed_seq":0,"blocks":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var s State
	if err := readPrivate(path, &s); err != nil {
		t.Fatalf("pre-lineage state must stay readable: %v", err)
	}
	if s.HarnessPID != 0 || s.HarnessName != "" {
		t.Fatalf("unexpected lineage defaults: %+v", s)
	}
}

func TestFindHarnessAncestorWalksStubbedTable(t *testing.T) {
	original := processTable
	processTable = func() (map[int]procInfo, error) {
		return map[int]procInfo{
			os.Getpid(): {ppid: 100, name: "pwsh"},
			100:         {ppid: 200, name: "Claude.exe"},
			200:         {ppid: 300, name: "WindowsTerminal.exe"},
		}, nil
	}
	t.Cleanup(func() { processTable = original })
	pid, name, ok := findHarnessAncestor()
	if !ok || pid != 100 || name != "claude" {
		t.Fatalf("ancestor = %d/%q/%v", pid, name, ok)
	}
}
