package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNativeSummaryIsBoundedBodyFreeAndAuthenticated(t *testing.T) {
	e, auth, _ := testEngine(t)
	text := strings.Repeat("private-evidence-", 512)
	for i := 0; i < 40; i++ {
		if _, err := e.Send(auth["claude"], SendRequest{ID: fmt.Sprintf("m%d", i), Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	// Make one real delivery uncertain without changing or replaying its body.
	claim, err := e.Claim(context.Background(), auth["codex"], false)
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	err = e.reapLocked(true)
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	summary, err := e.AuthSummary(auth["claude"])
	if err != nil {
		t.Fatal(err)
	}
	if summary.Inboxes["codex"].Queued != 39 || summary.Inboxes["codex"].Unknown != 1 || len(summary.Recovery) != 1 || summary.Recovery[0].ID != claim.ID {
		t.Fatalf("summary lost transport counts: %+v", summary)
	}
	data, err := json.Marshal(summary)
	if err != nil || len(data) > 2048 {
		t.Fatalf("summary grew with history: bytes=%d err=%v", len(data), err)
	}
	for _, forbidden := range []string{"private-evidence", "session-claude", "session-codex", "/optional/transcript", "secret-claude", claim.Receipt} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("summary leaked %q", forbidden)
		}
	}
	bad := auth["claude"]
	bad.Secret = "wrong"
	if _, err := e.AuthSummary(bad); !errors.Is(err, ErrAuth) {
		t.Fatal("summary bypassed auth", err)
	}
}

func TestNativeSummaryPendingBindingHasNoPeerOrInbox(t *testing.T) {
	e, auth, _ := testEngine(t)
	_, err := e.Send(auth["claude"], SendRequest{ID: "queued", Text: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Bind("claude", BindRequest{BindID: "replacement", CredentialHash: Digest("new-secret"), NonceHash: Digest("nonce"), Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.AuthSummary(Auth{Slot: "claude", BindID: b.BindID, Generation: b.Generation, Secret: "new-secret"})
	if err != nil || len(s.Bindings) != 1 || s.Bindings["claude"].Associated || len(s.Inboxes) != 0 || len(s.Recovery) != 0 {
		t.Fatalf("pending binding saw peer/inbox: %+v %v", s, err)
	}
}

func TestNativeSequenceAvoidsSnapshotAllocations(t *testing.T) {
	e, auth, _ := testEngine(t)
	_, err := e.Send(auth["claude"], SendRequest{ID: "sample", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := e.Sequence(), e.Snapshot().Sequence; got != want {
		t.Fatalf("sequence=%d want=%d", got, want)
	}
	if allocations := testing.AllocsPerRun(100, func() { _ = e.Sequence() }); allocations != 0 {
		t.Fatalf("cursor lookup allocated: %f", allocations)
	}
}

func TestNativeActiveAuthChecksCurrentGenerationDuringDrain(t *testing.T) {
	e, auth, _ := testEngine(t)
	before := e.Sequence()
	e.SetDraining(true)
	if err := e.CheckAuth(auth["claude"]); err != nil {
		t.Fatal("completion admission blocked by drain", err)
	}
	if e.Sequence() != before {
		t.Fatal("admission appended an event")
	}
	e.SetDraining(false)
	if err := e.Unbind("claude"); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckAuth(auth["claude"]); !errors.Is(err, ErrAuth) {
		t.Fatal("revoked credentials accepted", err)
	}
}
