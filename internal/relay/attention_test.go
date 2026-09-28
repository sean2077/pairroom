package relay

import (
	"context"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func loadRoomEvents(t *testing.T, dir string) []model.Event {
	t.Helper()
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	events, err := log.Load()
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// Every appended fact passes through OnAppend, and only facts where a human
// may be needed classify as attention; ordinary peer relay does not.
func TestAttentionFromAppendedFacts(t *testing.T) {
	dir := t.TempDir()
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []Attention
	e, err := Open(Config{RoomID: "room", Store: log, Lease: time.Nanosecond, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}, OnAppend: func(ev model.Event) {
		if a, ok := AttentionFromEvent(ev); ok {
			got = append(got, a)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	auth := map[model.ActorID]Auth{}
	for _, slot := range model.SlotActors() {
		b, err := e.Bind(slot, BindRequest{BindID: "bind-" + string(slot), CredentialHash: Digest("secret"), SessionID: "session-" + string(slot)})
		if err != nil {
			t.Fatal(err)
		}
		auth[slot] = Auth{Slot: slot, BindID: b.BindID, Generation: b.Generation, SessionID: b.SessionID, Secret: "secret"}
	}
	if _, err := e.Send(auth[model.ActorSlot1], SendRequest{ID: "peer", Text: "review this"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("peer relay raised attention: %+v", got)
	}
	if _, err := e.Report(auth[model.ActorSlot2], 1, "Done. @user please decide"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(auth[model.ActorSlot1], SendRequest{ID: "explicit", Text: "need you", To: model.ActorUser}); err != nil {
		t.Fatal(err)
	}
	if err := e.Failure(auth[model.ActorSlot2], "rate_limit"); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordWake("failed", "command_failed", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordWake("accepted", "", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	// A claimed envelope whose lease expires becomes unknown.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := e.Claim(ctx, auth[model.ActorSlot2], false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := e.Reap(); err != nil {
		t.Fatal(err)
	}
	want := []Attention{
		{Kind: AttentionHumanTurn, Slot: model.ActorSlot2},
		{Kind: AttentionHumanTurn, Slot: model.ActorSlot1},
		{Kind: AttentionAgentFailed, Slot: model.ActorSlot2},
		{Kind: AttentionWakeFailed, Slot: model.ActorSlot2},
		{Kind: AttentionDeliveryUncertain, Slot: model.ActorSlot2},
	}
	if len(got) != len(want) {
		t.Fatalf("attention = %+v", got)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || got[i].Slot != want[i].Slot || got[i].Key == "" {
			t.Fatalf("attention[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The startup probe reads facts without opening a writer: it finds
// unattempted queued input for a bound session and nothing else.
func TestHasWakeWorkReadsFactsWithoutWriting(t *testing.T) {
	kinds := map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}
	for _, tc := range []struct {
		name string
		prep func(t *testing.T, e *Engine, a map[model.ActorID]Auth)
		want bool
	}{
		{"idle room", nil, false},
		{"queued input", func(t *testing.T, e *Engine, a map[model.ActorID]Auth) {
			if _, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "q", Text: "x"}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"attempted head only", func(t *testing.T, e *Engine, a map[model.ActorID]Auth) {
			m, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "q", Text: "x"})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.ReserveWake(m.ID, model.ActorSlot2); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"input behind an attempted head", func(t *testing.T, e *Engine, a map[model.ActorID]Auth) {
			m, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "q", Text: "x"})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.ReserveWake(m.ID, model.ActorSlot2); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "q2", Text: "y"}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"wake disabled", func(t *testing.T, e *Engine, a map[model.ActorID]Auth) {
			if err := e.SetWakeEnabled(false); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "q", Text: "x"}); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"receiver unbound", func(t *testing.T, e *Engine, a map[model.ActorID]Auth) {
			if _, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "q", Text: "x"}); err != nil {
				t.Fatal(err)
			}
			if err := e.Unbind(model.ActorSlot2); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, a, dir := testEngine(t)
			if tc.prep != nil {
				tc.prep(t, e, a)
			}
			events := loadRoomEvents(t, dir)
			got, err := HasWakeWork("room", events, kinds)
			if err != nil || got != tc.want {
				t.Fatalf("HasWakeWork = %v, %v; want %v", got, err, tc.want)
			}
			if again := loadRoomEvents(t, dir); len(again) != len(events) {
				t.Fatalf("probe wrote events: %d -> %d", len(events), len(again))
			}
		})
	}
	if _, err := HasWakeWork("other-room", []model.Event{{Seq: 1, RoomID: "room", Kind: EventWakeConfig, Data: []byte(`{"enabled":true}`)}}, kinds); err == nil {
		t.Fatal("foreign Room events accepted")
	}
}
