package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestDiscoveryPrunesNonBindingAndRetiredDirectories(t *testing.T) {
	root := t.TempDir()
	callerState(t, root, "room", "session", model.ActorSlot1, model.RuntimeGrok)
	for _, rel := range []string{"logs", "rooms/room/media", "rooms/room/slots/claude", "rooms/room/slots/slot1/staging"} {
		path := filepath.Join(root, ".pairroom", filepath.FromSlash(rel))
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		// A dangling link proves discovery never enters the unrelated subtree.
		if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(path, "link")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	paths, err := statePaths(root)
	want := filepath.Join(root, ".pairroom", "rooms", "room", "slots", "slot1", "state.json")
	if err != nil || len(paths) != 1 || paths[0] != want {
		t.Fatalf("discovery entered unrelated data: %v %v", paths, err)
	}
}

func TestDiscoveryRejectsSymlinksAtEveryBindingLevel(t *testing.T) {
	for _, rel := range []string{".pairroom", ".pairroom/rooms", ".pairroom/rooms/room", ".pairroom/rooms/room/slots", ".pairroom/rooms/room/slots/slot1", ".pairroom/rooms/room/slots/slot1/state.json"} {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if _, err := statePaths(root); err == nil {
				t.Fatal("followed a binding symlink")
			}
		})
	}
}

func TestNativeCallerInfersMissingSelectorOnlyForItsSession(t *testing.T) {
	isolateCaller(t)
	root := t.TempDir()
	callerState(t, root, "own", "own-session", model.ActorSlot2, model.RuntimeGrok)
	callerState(t, root, "other", "other-session", model.ActorSlot1, model.RuntimeGrok)
	t.Setenv("GROK_SESSION_ID", "own-session")
	for _, o := range []options{{}, {room: "own"}, {slot: "2"}} {
		if err := applyCallerDefaults(root, "wait", &o); err != nil || o.room != "own" || o.slot != "slot2" {
			t.Fatalf("own session required redundant selectors: %+v %v", o, err)
		}
	}
	for _, o := range []options{{room: "other"}, {slot: "1"}} {
		if err := applyCallerDefaults(root, "wait", &o); err == nil {
			t.Fatal("explicit selector crossed native session boundary")
		}
	}
}

func TestUnconfirmedAndRetiredStateNeverEntersAnyDiscoveryPath(t *testing.T) {
	for _, kind := range []string{"unconfirmed", "retired"} {
		t.Run(kind, func(t *testing.T) {
			isolateCaller(t)
			root := t.TempDir()
			s := callerState(t, root, "room", "session", model.ActorSlot1, model.RuntimeGrok)
			if kind == "unconfirmed" {
				s.Generation = 0
			} else {
				s.Schema = 1
			}
			path := filepath.Join(root, ".pairroom", "rooms", "room", "slots", "slot1", "state.json")
			if err := relay.AtomicJSON(path, s); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GROK_SESSION_ID", "session")
			if err := applyCallerDefaults(root, "status", &options{}); err == nil {
				t.Fatal("status selected non-current binding")
			}
			if err := applyCallerDefaults(root, "bind", &options{create: true}); err != nil {
				t.Fatalf("non-current record blocked a new binding: %v", err)
			}
			if paths, err := boundHookCandidates([]string{path}, model.RuntimeGrok, "session"); err != nil || len(paths) != 0 {
				t.Fatalf("hook selected non-current binding: %v %v", paths, err)
			}
			if err := resolveSlotDefaults(root, &options{}); err == nil {
				t.Fatal("terminal discovery selected non-current binding")
			}
		})
	}
}

func TestNativeCallerRejectsAllControlCharacters(t *testing.T) {
	isolateCaller(t)
	for _, value := range []string{"native\tsession", "native\x7fsession", "native\u0085session"} {
		t.Setenv("GROK_SESSION_ID", value)
		if _, err := currentNativeCaller(); err == nil {
			t.Fatal("unsafe session metadata accepted")
		}
	}
}

func TestNativeStatusAndReconcileDefaultToBoundedSummary(t *testing.T) {
	for _, action := range []string{"status", "reconcile"} {
		t.Run(action, func(t *testing.T) {
			f := newForegroundFixture(t, foregroundFixtureOptions{summary: &relay.Summary{}})
			var out bytes.Buffer
			if err := f.run(context.Background(), action, strings.NewReader(""), &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			if f.count("summary") != 1 || f.count("status") != 0 || out.Len() > 4096 {
				t.Fatal("default status requested full history")
			}
		})
	}
}

func TestSendReceiptSizeDoesNotGrowWithBody(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	body := strings.Repeat("large-private-body", 10000)
	var out, diagnostic bytes.Buffer
	if err := f.run(context.Background(), "send", strings.NewReader(body), &out, &diagnostic, "--id", "large"); err != nil {
		t.Fatal(err)
	}
	if out.Len() > 256 || strings.Contains(out.String()+diagnostic.String(), "large-private-body") {
		t.Fatalf("send reflected body into context (%d bytes)", out.Len())
	}
	var receipt publicationReceipt
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil || receipt.ClientID != "large" || receipt.Published != "outgoing-large" {
		t.Fatalf("recovery identity lost: %+v %v", receipt, err)
	}
	t.Logf("body=%d bytes, stdout receipt=%d bytes, diagnostic=%d bytes", len(body), out.Len(), diagnostic.Len())
}

func TestSendRejectsReusedIDWithDifferentBodyAndMalformedReceipt(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	if err := f.run(context.Background(), "send", strings.NewReader("v1"), io.Discard, io.Discard, "--id", "same"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := f.run(context.Background(), "send", strings.NewReader("v2"), &out, io.Discard, "--id", "same")
	if err == nil || !strings.Contains(err.Error(), "different body") || out.Len() != 0 || f.count("wait") != 0 {
		t.Fatalf("send silently reused a different publication: %v %s", err, out.String())
	}
	bad := newForegroundFixture(t, foregroundFixtureOptions{invalidReceipt: true})
	if err := bad.run(context.Background(), "send", strings.NewReader("v1"), &out, io.Discard, "--id", "same"); err == nil || out.Len() != 0 {
		t.Fatal("send emitted success without a valid receipt")
	}
}

func TestSkillInstallHonorsEachNativeConfigRoot(t *testing.T) {
	IsolateNativeCaller(t)
	for _, tc := range []struct {
		kind model.RuntimeKind
		env  string
	}{{model.RuntimeClaude, "CLAUDE_CONFIG_DIR"}, {model.RuntimeCodex, "CODEX_HOME"}, {model.RuntimeGrok, "GROK_HOME"}} {
		t.Run(string(tc.kind), func(t *testing.T) {
			home, custom := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv(tc.env, custom)
			for i := 0; i < 2; i++ {
				if err := installSkill(tc.kind); err != nil {
					t.Fatal(err)
				}
			}
			body, err := os.ReadFile(filepath.Join(custom, "skills", "pairroom-relay", "SKILL.md"))
			if err != nil || string(body) != skillContent {
				t.Fatalf("skill did not follow %s: %v", tc.env, err)
			}
			if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
				t.Fatal("install leaked into another runtime profile")
			}
		})
	}
}

func TestOptionalWakeLookupCannotExhaustCollectionBudget(t *testing.T) {
	// Exchange no longer looks up the peer at all; status keeps the optional
	// wake advice behind a bounded lookup that cannot stall inspection.
	f := newForegroundFixture(t, foregroundFixtureOptions{peerLookupStalled: true, summary: &relay.Summary{Inboxes: map[model.ActorID]relay.InboxSummary{model.ActorSlot2: {Queued: 1}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out, diagnostic bytes.Buffer
	if err := f.run(ctx, "exchange", strings.NewReader("review"), &out, &diagnostic, "--id", "one"); err != nil {
		t.Fatal(err)
	}
	if f.count("send") != 1 || f.count("peer") != 0 || f.count("wait") != 1 || f.count("ack") != 1 {
		t.Fatal("exchange looked up optional metadata, suppressed collection or repeated publication")
	}
	if strings.Contains(diagnostic.String(), "wake_command") || !strings.Contains(out.String(), "Review finding") {
		t.Fatal("exchange lost input or invented a vendor wake")
	}
	out.Reset()
	if err := f.run(ctx, "status", strings.NewReader(""), &out, io.Discard, "--brief"); err != nil {
		t.Fatal(err)
	}
	if f.count("peer") != 1 || strings.Contains(out.String(), "wake_command") || !strings.Contains(out.String(), "queued_inbox_hints") {
		t.Fatalf("stalled optional lookup changed status: %s", out.String())
	}
}

func TestLocalJoinOmitsOnlyDefaultServiceEndpoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	standard, err := defaultEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	want := "pairroom relay bind --room room --slot 2"
	if got := localBindCommand(standard, "room", model.ActorSlot2); got != want {
		t.Fatalf("default join needs no endpoint flag: %s", got)
	}
	custom := filepath.Join(t.TempDir(), "custom service", relay.EndpointFile)
	if got := localBindCommand(custom, "room", model.ActorSlot2); got != want+" --service-file "+quoteShellPath(custom) {
		t.Fatalf("custom join lost its endpoint: %s", got)
	}
}

func TestSessionCommandsAcceptDocumentedRuntimeAlias(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{summary: &relay.Summary{}})
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session")
	if err := f.run(context.Background(), "status", strings.NewReader(""), io.Discard, io.Discard, "--runtime", "cc"); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--runtime", "--peer-runtime"} {
		err := Run(context.Background(), []string{"bind", "--create", "--repo", "missing", flag, "unknown"}, strings.NewReader(""), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "invalid "+flag) {
			t.Fatalf("runtime validation followed workspace I/O: %v", err)
		}
	}
}

func TestConfiguredSkillRootDoesNotRequireDefaultHome(t *testing.T) {
	IsolateNativeCaller(t)
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("GROK_HOME", t.TempDir())
	if err := installSkill(model.RuntimeGrok); err != nil {
		t.Fatal(err)
	}
}

func TestForegroundCommandsShareLongWaitDefault(t *testing.T) {
	for _, action := range []string{"wait", "exchange"} {
		var diagnostic bytes.Buffer
		if err := Run(context.Background(), []string{action, "--help"}, strings.NewReader(""), io.Discard, &diagnostic); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(diagnostic.String(), "default 3600") {
			t.Fatalf("%s lost the CLI-side wait default", action)
		}
	}
}
