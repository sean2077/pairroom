package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func historyFixture(count int) *Engine {
	e := &Engine{
		cfg:      Config{RoomID: "history", Now: time.Now, Lease: DeliveryLease},
		messages: map[string]Message{}, inFlight: map[string]struct{}{},
		bindings: map[model.ActorID]bindingFact{},
	}
	for i := 0; i < count; i++ {
		e.putMessage(Message{ID: fmt.Sprintf("m-%d", i), State: "handed_off", Text: strings.Repeat("x", 100), Receipt: "private-claim", Quote: &model.AgentQuote{Text: "quote"}, Attachments: []model.Attachment{{ID: "image", Name: "original"}}})
		e.audit = append(e.audit, Audit{Seq: uint64(i + 1), Kind: EventMessage})
	}
	return e
}

func TestNativeSnapshotTailIsBoundedAndDetached(t *testing.T) {
	e := historyFixture(5000)
	full, tail := e.Snapshot(), e.SnapshotTail()
	if len(full.Messages) != 5000 || len(full.Audit) != 5000 || tail.TotalMessages != 5000 || tail.TotalAudit != 5000 {
		t.Fatal("windowing lost the full history or totals")
	}
	if len(tail.Messages) != SnapshotMessageLimit || len(tail.Audit) != SnapshotAuditLimit || tail.Messages[0].ID != "m-4700" || tail.Messages[299].ID != "m-4999" {
		t.Fatal("tail is not the bounded chronological window")
	}
	allBytes, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	tailBytes, err := json.Marshal(tail)
	if err != nil || len(tailBytes)*8 >= len(allBytes) || strings.Contains(string(tailBytes), "private-claim") {
		t.Fatal("tail copied full history or leaked a delivery receipt")
	}
	tail.Messages[0].Quote.Text = "changed"
	tail.Messages[0].Attachments[0].Name = "changed"
	tail.Audit[0].Kind = "changed"
	original := e.messages["m-4700"]
	if original.Quote.Text != "quote" || original.Attachments[0].Name != "original" || e.audit[len(e.audit)-SnapshotAuditLimit].Kind != EventMessage {
		t.Fatal("browser projection aliases mutable engine state")
	}
}

func TestNativeSnapshotTailBudgetsQuoteAndMessageTextWithoutClipping(t *testing.T) {
	e := historyFixture(12)
	text := strings.Repeat("x", MaxBodyBytes)
	quote := strings.Repeat("q", MaxBodyBytes)
	for id, m := range e.messages {
		m.Text, m.Quote = text, &model.AgentQuote{Text: quote}
		e.messages[id] = m
	}
	tail := e.SnapshotTail()
	if len(tail.Messages) != 2 || tail.TotalMessages != 12 {
		t.Fatal("large bodies and quotes bypassed the text budget")
	}
	for _, m := range tail.Messages {
		if m.Text != text || m.Quote.Text != quote {
			t.Fatal("windowing clipped a complete message")
		}
	}
	if empty := historyFixture(0).SnapshotTail(); len(empty.Messages) != 0 || empty.TotalMessages != 0 {
		t.Fatal("empty history returned invented messages")
	}
}

func TestNativeInFlightIndexTracksAllTerminalTransitions(t *testing.T) {
	for _, finish := range []string{"ack", "expire", "replace", "close"} {
		t.Run(finish, func(t *testing.T) {
			e, auth, _ := testEngine(t)
			_, err := e.Send(auth[model.ActorSlot1], SendRequest{ID: "work", Text: "work"})
			if err != nil {
				t.Fatal(err)
			}
			claim, err := e.Claim(context.Background(), auth[model.ActorSlot2], false)
			if err != nil || !e.Busy() || len(e.inFlight) != 1 {
				t.Fatalf("claim did not enter the index: %v", err)
			}
			switch finish {
			case "ack":
				err = e.Ack(auth[model.ActorSlot2], claim.ID, claim.Receipt)
			case "expire":
				m := e.messages[claim.ID]
				m.ClaimedAt = time.Now().Add(-2 * DeliveryLease)
				e.messages[m.ID] = m
				err = e.Reap()
			case "replace":
				_, err = e.Bind(model.ActorSlot2, BindRequest{BindID: "replacement", CredentialHash: Digest("replacement-secret"), SessionID: "replacement-session", Replace: true})
			case "close":
				err = e.Close()
			}
			if err != nil || e.Busy() || len(e.inFlight) != 0 {
				t.Fatalf("terminal transition left a stale in-flight entry: %v", err)
			}
		})
	}
}

func TestNativePayloadComparisonIncludesAttachmentIdentity(t *testing.T) {
	original := Message{From: model.ActorSlot1, To: model.ActorSlot2, Text: "same", Attachments: []model.Attachment{{ID: "first", SHA256: "digest"}}}
	changed := cloneMessage(original)
	changed.Attachments[0].ID = "second"
	if sameMessagePayload(original, changed) {
		t.Fatal("same text hid a changed attachment")
	}
	changed = cloneMessage(original)
	changed.Attachments[0].SHA256 = "different"
	if sameMessagePayload(original, changed) {
		t.Fatal("same attachment ID hid changed content")
	}
	changed = cloneMessage(original)
	changed.State, changed.Receipt, changed.TargetGeneration = "cancelled", "private", 42
	if !sameMessagePayload(original, changed) {
		t.Fatal("transport state changed the payload identity")
	}
}

func BenchmarkNativeHistoryProjection(b *testing.B) {
	e := historyFixture(10000)
	b.Run("full", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = e.Snapshot()
		}
	})
	b.Run("tail", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = e.SnapshotTail()
		}
	})
}

func BenchmarkNativeIdleReap(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprintf("history-%d", count), func(b *testing.B) {
			e := historyFixture(count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := e.Reap(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
