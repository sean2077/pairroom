package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

// injectAppendError reports an append outcome a transactional backend may
// produce when it cannot establish the commit. Local JSONL never returns one
// (a local write or sync failure poisons the writer instead), so a controlled
// double is the only way to reach the engine's fail-closed boundary. Its two
// modes are indistinguishable to a caller: "the fact landed, then reporting
// failed" and "nothing landed" both surface as one non-nil error.
type injectAppendError struct {
	Persistence
	enabled bool
	after   bool // land the real append first, then report the injected error
}

var errInjectedAppend = errors.New("injected append failure")

func (s *injectAppendError) Append(event *model.Event) error {
	if !s.enabled {
		return s.Persistence.Append(event)
	}
	if s.after {
		if err := s.Persistence.Append(event); err != nil {
			return err
		}
	}
	return errInjectedAppend
}

type failClosedFixture struct {
	engine    *Engine
	auth      map[model.ActorID]Auth
	injected  *injectAppendError
	dir       string
	appends   *int
	attention *int
	receipt   string
	durable   int
}

func failClosedEngine(t *testing.T) *failClosedFixture {
	t.Helper()
	dir := t.TempDir()
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	injected := &injectAppendError{Persistence: log}
	appends, attention := 0, 0
	e, err := Open(Config{
		RoomID:      "room",
		Store:       injected,
		Runtimes:    map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex},
		OnAppend:    func(model.Event) { appends++ },
		OnAttention: func(Attention) { attention++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	f := &failClosedFixture{engine: e, injected: injected, dir: dir, appends: &appends, attention: &attention, auth: map[model.ActorID]Auth{}}
	for _, slot := range model.SlotActors() {
		secret := "secret-" + string(slot)
		session := "session-" + string(slot)
		b, err := e.Bind(slot, BindRequest{BindID: "bind-" + string(slot), CredentialHash: Digest(secret), SessionID: session})
		if err != nil {
			t.Fatal(err)
		}
		a := Auth{Slot: slot, BindID: b.BindID, Generation: b.Generation, SessionID: session, Secret: secret}
		if _, err := e.ConfirmSession(a, session, "/optional/transcript"); err != nil {
			t.Fatal(err)
		}
		f.auth[slot] = a
	}
	return f
}

// baseline snapshots the durable log and the notification counters before the
// injected failure, so the failure itself can be asserted against them.
func (f *failClosedFixture) baseline(t *testing.T) (int, int) {
	t.Helper()
	f.durable = len(traceFacts(t, f.engine))
	return *f.appends, *f.attention
}

// assertFailClosed pins the whole boundary of one rejected append: the
// operation reports its error, the engine fails closed, nothing notifies, and
// no further write reaches the log. Whether the uncertain fact itself is
// durable is asserted by the caller after reopening, which is the only
// sanctioned recovery.
func (f *failClosedFixture) assertFailClosed(t *testing.T, opErr error, after bool, appendsBefore, attentionBefore int) {
	t.Helper()
	if opErr == nil || !errors.Is(opErr, errInjectedAppend) {
		t.Fatalf("rejected append did not surface its error: %v", opErr)
	}
	if f.engine.Fatal() == nil {
		t.Fatal("an uncertain append did not fail the Room closed")
	}
	if *f.appends != appendsBefore || *f.attention != attentionBefore {
		t.Fatal("a rejected append still fired append/attention notifications")
	}
	landed := f.durable
	if after {
		landed++ // the injected failure landed the fact before reporting it
	}
	if got := len(traceFacts(t, f.engine)); got != landed {
		t.Fatalf("durable facts after the rejected append = %d, want %d", got, landed)
	}
	if _, err := f.engine.Send(f.auth[model.ActorSlot1], SendRequest{ID: "after-failure", Text: "must not be written"}); err == nil {
		t.Fatal("the Room accepted a write after an uncertain append")
	}
	if got := len(traceFacts(t, f.engine)); got != landed {
		t.Fatalf("a write after failure still reached the log: %d facts", got)
	}
}

// reopen closes the failed Room and replays the same directory, the only
// recovery the boundary sanctions.
func (f *failClosedFixture) reopen(t *testing.T) *Engine {
	t.Helper()
	_ = f.engine.Close() // the fail-closed Room takes no more writes; release its store
	return reopenEngine(t, f.dir)
}

type failClosedCase struct {
	name string
	// setup produces the visible state the failing operation acts on.
	setup func(t *testing.T, f *failClosedFixture) string
	// fail runs the operation whose append must be rejected, and proves the
	// operation itself reported no success.
	fail func(t *testing.T, f *failClosedFixture, id string) error
	// reopened asserts the replayed truth for the durable effect.
	reopened func(t *testing.T, f *failClosedFixture, restored *Engine, after bool, id string)
}

// The engine's append boundary is the single choke point every operation
// funnels through. These cases pin, for each kind of transition, that an
// append error published as fail-closed is never treated as an absence proof
// in either direction: the caller gets no success, notifications stay silent,
// later writes are refused, and only reopening the log reveals whether the
// fact lives.
func TestAppendErrorIsFailClosedAndOnlyReopenReplaysTruth(t *testing.T) {
	cases := []failClosedCase{
		{
			name:  "send",
			setup: func(t *testing.T, f *failClosedFixture) string { return "fc-send" },
			fail: func(t *testing.T, f *failClosedFixture, id string) error {
				message, err := f.engine.Send(f.auth[model.ActorSlot1], SendRequest{ID: id, Text: "fail-closed body"})
				if message.ID != "" {
					t.Fatalf("Send published a message it could not commit: %+v", message)
				}
				return err
			},
			reopened: func(t *testing.T, f *failClosedFixture, restored *Engine, after bool, id string) {
				messages := restored.Snapshot().Messages
				if !after {
					if len(messages) != 0 {
						t.Fatalf("reopen resurrected a send that never committed: %+v", messages)
					}
					return
				}
				// The request ID is the idempotency key; the durable message
				// carries the engine-assigned identity, so pin the fact by its
				// content and state instead.
				if len(messages) != 1 || messages[0].State != "queued" || messages[0].From != model.ActorSlot1 || messages[0].Text != "fail-closed body" {
					t.Fatalf("reopen did not replay the durable send: %+v", messages)
				}
			},
		},
		{
			name: "claim",
			setup: func(t *testing.T, f *failClosedFixture) string {
				message, err := f.engine.Send(f.auth[model.ActorSlot1], SendRequest{ID: "fc-claim", Text: "claim body"})
				if err != nil {
					t.Fatal(err)
				}
				return message.ID
			},
			fail: func(t *testing.T, f *failClosedFixture, id string) error {
				claim, err := f.engine.Claim(context.Background(), f.auth[model.ActorSlot2], false)
				if claim != nil {
					t.Fatalf("claim released an envelope whose fact was uncertain: %+v", claim)
				}
				// The projection must not advance either: no envelope left the
				// engine, so the message stays queued for its Slot.
				if state := f.engine.Snapshot().Messages[0].State; state != "queued" {
					t.Fatalf("failed claim advanced the projection: %q", state)
				}
				return err
			},
			reopened: func(t *testing.T, f *failClosedFixture, restored *Engine, after bool, id string) {
				messages := restored.Snapshot().Messages
				if len(messages) != 1 || messages[0].ID != id {
					t.Fatalf("reopen lost the original message: %+v", messages)
				}
				want := "queued"
				if after {
					// The claim fact is durable, and reopening cannot prove the
					// envelope never reached stdout, so it recovers to unknown
					// with the original identity — never by replaying the claim.
					want = "unknown"
				}
				if messages[0].State != want {
					t.Fatalf("reopened claim state = %q, want %q", messages[0].State, want)
				}
			},
		},
		{
			name: "ack",
			setup: func(t *testing.T, f *failClosedFixture) string {
				message, err := f.engine.Send(f.auth[model.ActorSlot1], SendRequest{ID: "fc-ack", Text: "ack body"})
				if err != nil {
					t.Fatal(err)
				}
				claim, err := f.engine.Claim(context.Background(), f.auth[model.ActorSlot2], false)
				if err != nil || claim == nil {
					t.Fatalf("setup claim: %+v %v", claim, err)
				}
				f.receipt = claim.Receipt
				return message.ID
			},
			fail: func(t *testing.T, f *failClosedFixture, id string) error {
				err := f.engine.Ack(f.auth[model.ActorSlot2], id, f.receipt)
				if err == nil {
					t.Fatal("Ack settled a message whose receipt fact was uncertain")
				}
				if state := f.engine.Snapshot().Messages[0].State; state != "delivering" {
					t.Fatalf("failed ack advanced the projection: %q", state)
				}
				return err
			},
			reopened: func(t *testing.T, f *failClosedFixture, restored *Engine, after bool, id string) {
				messages := restored.Snapshot().Messages
				if len(messages) != 1 || messages[0].ID != id {
					t.Fatalf("reopen lost the original message: %+v", messages)
				}
				want := "unknown" // the setup claim is reaped: a previous writer cannot prove stdout
				if after {
					want = "handed_off"
				}
				if messages[0].State != want {
					t.Fatalf("reopened ack state = %q, want %q", messages[0].State, want)
				}
			},
		},
		{
			name: "cancel",
			setup: func(t *testing.T, f *failClosedFixture) string {
				message, err := f.engine.Send(f.auth[model.ActorSlot1], SendRequest{ID: "fc-cancel", Text: "cancel body"})
				if err != nil {
					t.Fatal(err)
				}
				return message.ID
			},
			fail: func(t *testing.T, f *failClosedFixture, id string) error {
				err := f.engine.Cancel(id)
				if err == nil {
					t.Fatal("Cancel reported success for an uncertain append")
				}
				return err
			},
			reopened: func(t *testing.T, f *failClosedFixture, restored *Engine, after bool, id string) {
				messages := restored.Snapshot().Messages
				if len(messages) != 1 || messages[0].ID != id {
					t.Fatalf("reopen lost the original message: %+v", messages)
				}
				want := "queued"
				if after {
					want = "cancelled"
				}
				if messages[0].State != want {
					t.Fatalf("reopened cancel state = %q, want %q", messages[0].State, want)
				}
			},
		},
		{
			name:  "replace",
			setup: func(t *testing.T, f *failClosedFixture) string { return "" },
			fail: func(t *testing.T, f *failClosedFixture, id string) error {
				binding, err := f.engine.Bind(model.ActorSlot1, BindRequest{
					BindID:         "fc-replacement",
					CredentialHash: Digest("replacement-secret"),
					SessionID:      "fc-replacement-session",
					Replace:        true,
				})
				if binding.BindID != "" {
					t.Fatalf("replace reported a binding it could not commit: %+v", binding)
				}
				return err
			},
			reopened: func(t *testing.T, f *failClosedFixture, restored *Engine, after bool, id string) {
				binding := restored.Snapshot().Bindings[model.ActorSlot1]
				wantID, wantGeneration := "bind-slot1", uint64(1)
				if after {
					wantID, wantGeneration = "fc-replacement", 2
				}
				if binding.BindID != wantID || binding.Generation != wantGeneration {
					t.Fatalf("reopened slot1 binding = %s/generation %d, want %s/%d", binding.BindID, binding.Generation, wantID, wantGeneration)
				}
			},
		},
	}
	for _, tc := range cases {
		for _, after := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/after=%v", tc.name, after), func(t *testing.T) {
				f := failClosedEngine(t)
				id := ""
				if tc.setup != nil {
					id = tc.setup(t, f)
				}
				appendsBefore, attentionBefore := f.baseline(t)
				f.injected.enabled, f.injected.after = true, after
				opErr := tc.fail(t, f, id)
				f.injected.enabled = false
				f.assertFailClosed(t, opErr, after, appendsBefore, attentionBefore)
				restored := f.reopen(t)
				tc.reopened(t, f, restored, after, id)
			})
		}
	}
}

// Open must stay fail-closed for a missing store, exactly like the former
// concrete pointer field: an untyped nil and a typed-nil pointer both return
// the validation error instead of panicking inside the first method call.
func TestOpenRefusesNilAndTypedNilStore(t *testing.T) {
	runtimes := map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}
	if _, err := Open(Config{RoomID: "room", Runtimes: runtimes}); err == nil || !strings.Contains(err.Error(), "requires a published Room store") {
		t.Fatalf("untyped nil store: %v", err)
	}
	var concrete *store.JSONLStore
	if _, err := Open(Config{RoomID: "room", Store: concrete, Runtimes: runtimes}); err == nil || !strings.Contains(err.Error(), "requires a published Room store") {
		t.Fatalf("typed-nil store: %v", err)
	}
}
