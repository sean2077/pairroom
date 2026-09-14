package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func TestNativeRelayActiveAdmissionNeverReadsHistoricalLog(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorClaude)
	reads := 0
	load := func(path string) ([]model.Event, error) {
		reads++
		if path != filepath.Join(f.room.DataDir, "events.jsonl") {
			t.Error("wrong log")
		}
		return readEventsReadOnly(path)
	}
	for i := 0; i < 100; i++ {
		if err := authenticateNativeRelay(f.native, f.room.DataDir, a, load); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 0 {
		t.Fatalf("active polling reread history %d times", reads)
	}
	bad := a
	bad.Secret = "wrong"
	if err := authenticateNativeRelay(f.native, f.room.DataDir, bad, load); !errors.Is(err, relay.ErrAuth) || reads != 0 {
		t.Fatal("invalid active auth read log or passed", err)
	}
	if err := authenticateNativeRelay(nil, f.room.DataDir, a, load); err != nil || reads != 1 {
		t.Fatal("cold activation skipped durable auth", err)
	}
	if err := f.native.engine.Unbind(a.Slot); err != nil {
		t.Fatal(err)
	}
	if err := authenticateNativeRelay(f.native, f.room.DataDir, a, load); !errors.Is(err, relay.ErrAuth) || reads != 1 {
		t.Fatal("revoked binding was cached", err)
	}
	if err := authenticateNativeRelay(nil, f.room.DataDir, a, load); !errors.Is(err, relay.ErrAuth) || reads != 2 {
		t.Fatal("cold activation used a superseded binding", err)
	}
}

func TestNativeBriefStatusThroughCLIAndRealService(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorClaude)
	if _, err := f.native.engine.Send(a, relay.SendRequest{ID: "proposal", Text: strings.Repeat("PRIVATE_PROPOSAL", 200)}); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(t, []string{"status", "--brief", "--room", f.room.ID, "--slot", "1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Relay relay.Summary `json:"relay"`
	}
	if err := json.Unmarshal(out, &got); err != nil || got.Relay.Inboxes[model.ActorCodex].Queued != 1 {
		t.Fatalf("brief status=%s err=%v", out, err)
	}
	for _, forbidden := range []string{"PRIVATE_PROPOSAL", a.SessionID, a.Secret, "transcript_path", "\"messages\"", "\"audit\""} {
		if bytes.Contains(out, []byte(forbidden)) {
			t.Fatalf("brief status exposed %q", forbidden)
		}
	}
	full, err := f.run(t, []string{"status", "--room", f.room.ID, "--slot", "1"}, nil)
	if err != nil || !bytes.Contains(full, []byte("PRIVATE_PROPOSAL")) {
		t.Fatal("full status compatibility lost", err)
	}
}

func TestNativeBindResumesInCallingSessionWithoutIdentityFlags(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorClaude)
	// Real hook association has already happened. Environment only discovers
	// that exact state; no second association or provider setting is written.
	t.Setenv("CLAUDE_CODE_SESSION_ID", a.SessionID)
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("GROK_SESSION_ID", "")
	var out bytes.Buffer
	if err := relayclient.Run(context.Background(), []string{"bind", "--repo", f.project.Root}, strings.NewReader(""), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if _, nonce := value["bind_nonce"]; nonce {
		t.Fatal("resume asked for another nonce")
	}
	b, err := f.native.engine.Inspect(a)
	if err != nil || b.Generation != a.Generation || b.SessionID != a.SessionID {
		t.Fatal("resume changed official identity", err)
	}
}

func TestNativeBriefStatusInspectsPendingWithoutAssociatingOrCollecting(t *testing.T) {
	f := nativeHTTP(t)
	a, _ := f.bind(t, model.ActorClaude)
	t.Setenv("CLAUDE_CODE_SESSION_ID", a.SessionID)
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("GROK_SESSION_ID", "")
	out, err := f.run(t, []string{"status", "--brief"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Relay relay.Summary `json:"relay"`
	}
	if err := json.Unmarshal(out, &got); err != nil || len(got.Relay.Bindings) != 1 || got.Relay.Bindings[a.Slot].Associated || len(got.Relay.Inboxes) != 0 {
		t.Fatalf("pending status leaked or associated: %s %v", out, err)
	}
	out, err = f.run(t, []string{"wait", "--timeout", "1"}, nil)
	if err == nil || len(out) != 0 {
		t.Fatal("pending inspection authorized collection")
	}
	if f.native.engine.Snapshot().Bindings[a.Slot].SessionID != "" {
		t.Fatal("status completed nonce association")
	}
}
