package relay

import (
	"context"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/review"
	"github.com/sean2077/pairroom/internal/store"
	"strings"
	"testing"
	"time"
)

func TestReviewAnchorIsImmutablePayloadAndSurvivesReplay(t *testing.T) {
	e, a, dir := testEngine(t)
	anchor := &review.Anchor{Schema: 1, Workspace: "/project", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), DirtySHA256: strings.Repeat("c", 64)}
	req := SendRequest{ID: "review", Text: "inspect change", Review: anchor}
	m, err := e.Send(a[model.ActorSlot1], req)
	if err != nil {
		t.Fatal(err)
	}
	anchor.Head = strings.Repeat("d", 40)
	if _, err := e.Send(a[model.ActorSlot1], req); err == nil {
		t.Fatal("changed evidence reused ID")
	}
	page, _ := e.History(HistoryQuery{ID: m.ID})
	if page.Messages[0].Review.Head != strings.Repeat("b", 40) {
		t.Fatal("anchor alias mutated log")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	claim, err := e.Claim(ctx, a[model.ActorSlot2], false)
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	if !strings.Contains(claim.Envelope, strings.Repeat("b", 40)) || !strings.Contains(claim.Envelope, "not approval") {
		t.Fatal("review omitted in envelope")
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(Config{RoomID: "room", Store: log})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	retried, err := reloaded.Retry(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Review == nil || retried.Review.Head != m.Review.Head {
		t.Fatal("Retry dropped original version")
	}
}
