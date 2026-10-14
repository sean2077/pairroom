package sqlstore

import (
	"encoding/json"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"testing"
	"time"
)

func TestStoredOriginalHumanAuthorMatchesImmutableOriginOnly(t *testing.T) {
	event := model.Event{Seq: 1, ID: "human-event", RoomID: "room-authors", Kind: relay.EventMessage, Actor: model.ActorUser, CreatedAt: time.Now().UTC()}
	payload := humanSendPayload{From: model.ActorUser, To: model.ActorSlot1, Author: relay.EnterpriseHumanAuthor("user-one"), ID: "human-message", Text: "text", State: "queued", Source: "send", CreatedAt: event.CreatedAt, UpdatedAt: event.CreatedAt}
	event.Data, _ = json.Marshal(payload)
	encoding, e := encodeStoredFact(event, userFactOrigin("user-one"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = encodeStoredFact(event, userFactOrigin("user-two")); e == nil {
		t.Fatal("encoded forged original human author")
	}
	var record storedEventRecord
	if json.Unmarshal(encoding, &record) != nil {
		t.Fatal("bad test record")
	}
	other := "user-two"
	record.OriginUser = &other
	bad, _ := json.Marshal(record)
	if _, e = decodeStoredFact(event.RoomID, event.Seq, event.ID, 2, event.Kind, string(event.Actor), bad, "user", &other); e == nil {
		t.Fatal("accepted rewritten origin with untouched original author")
	}
	payload.State = "injected"
	payload.Receipt = "accepted-receipt"
	event.Data, _ = json.Marshal(payload)
	if _, e = encodeStoredFact(event, userFactOrigin("user-two")); e != nil {
		t.Fatal("delivery update lost historical author", e)
	}
	event.Actor = model.ActorSystem
	if _, e = encodeStoredFact(event, operatorFactOrigin()); e != nil {
		t.Fatal("operator terminal update lost historical author", e)
	}
	payload.Author = ""
	payload.State = "queued"
	payload.Receipt = ""
	event.Actor = model.ActorUser
	event.Data, _ = json.Marshal(payload)
	if _, e = encodeStoredFact(event, userFactOrigin("user-one")); e != nil {
		t.Fatal("legacy empty author was guessed", e)
	}
}
