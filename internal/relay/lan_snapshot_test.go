package relay

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/review"
)

func TestLANStatusTailBudgetsWorstJSONEscapingAndCompleteEvidence(t *testing.T) {
	e := historyFixture(400)
	a := Auth{Slot: model.ActorSlot2, BindID: "remote", Generation: 1, MemberKey: Digest("remote")}
	e.bindings[a.Slot] = bindingFact{Binding: Binding{Slot: a.Slot, BindID: a.BindID, Generation: a.Generation, RemoteKey: a.MemberKey, Active: true}}
	e.bindings[model.ActorSlot1] = bindingFact{Binding: Binding{Slot: model.ActorSlot1, SessionID: "private session", TranscriptPath: "private transcript"}}
	text := strings.Repeat("<", MaxBodyBytes)
	quote := strings.Repeat("\x01", MaxBodyBytes)
	anchor := &review.Anchor{Schema: 1, Workspace: strings.Repeat("<", 2048), Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), DirtySHA256: strings.Repeat("c", 64)}
	if err := anchor.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, id := range e.order[len(e.order)-5:] {
		m := e.messages[id]
		m.Text = text
		m.Quote.Text = quote
		m.Review = anchor
		m.From, m.To = model.ActorSlot1, model.ActorSlot2
		m.Attachments = nil
		for i := 0; i < 8; i++ {
			m.Attachments = append(m.Attachments, model.Attachment{ID: "att-" + strings.Repeat("a", 24), Name: strings.Repeat("<", 256), Kind: "file", MediaType: "text/plain", Source: "lan", SHA256: strings.Repeat("f", 64), Size: 1, CreatedAt: time.Now().UTC()})
		}
		e.putMessage(m)
	}
	before := e.Sequence()
	tail, err := e.AuthSnapshotTail(a)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(tail)
	if err != nil || len(encoded) >= lanSnapshotResponseBudget || len(tail.Messages) != 1 || len(tail.Audit) != SnapshotAuditLimit || tail.TotalMessages != 400 || tail.TotalAudit != 400 {
		t.Fatalf("status exceeded its full encoded budget or lost totals: messages=%d bytes=%d err=%v", len(tail.Messages), len(encoded), err)
	}
	newest := tail.Messages[0]
	if newest.ID != "m-399" || newest.Text != text || newest.Quote == nil || newest.Quote.Text != quote || len(newest.Attachments) != 8 || newest.Review.Workspace != anchor.Workspace {
		t.Fatal("encoded budget clipped the newest body, quote or evidence")
	}
	if strings.Contains(string(encoded), "private session") || strings.Contains(string(encoded), "private transcript") || strings.Contains(string(encoded), "private-claim") || !strings.Contains(tail.Notice, "history") {
		t.Fatal("LAN status leaked local identities/receipts or omitted history guidance")
	}
	page, err := e.AuthHistory(a, HistoryQuery{ID: "m-398"})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].Text != text || e.Sequence() != before {
		t.Fatal("omitted history was lost or status changed delivery")
	}
	tail.Messages[0].Attachments[0].Name = "changed"
	tail.Messages[0].Review.Workspace = "changed"
	if e.messages["m-399"].Attachments[0].Name == "changed" || e.messages["m-399"].Review.Workspace == "changed" {
		t.Fatal("LAN status aliases durable evidence")
	}
	bad := a
	bad.Generation++
	if _, err := e.AuthSnapshotTail(bad); !errors.Is(err, ErrAuth) {
		t.Fatal("status tail bypassed exact generation authorization")
	}
}
