package sqlstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func recoverySendIntent(text string) relay.EnterpriseOperationIntent {
	return relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: text, ExpectedPeerGeneration: 1}
}
func recoveryClaimIntent() relay.EnterpriseOperationIntent {
	return relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}
}
func TestRecoveryCodecProvenanceIsVersionedAndIndexBound(t *testing.T) {
	event := model.Event{ID: "event-1", RoomID: "room-1", Seq: 1, Kind: "room.note", Actor: model.ActorUser, CreatedAt: time.Date(2026, 10, 13, 1, 2, 3, 123456789, time.UTC), Data: json.RawMessage(`{"body":"unchanged Local event"}`)}
	original, _ := json.Marshal(event)
	user := "u-owner"
	encoded, e := encodeStoredFact(event, userFactOrigin(user))
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := decodeStoredFact(event.RoomID, event.Seq, event.ID, 2, event.Kind, string(event.Actor), encoded, "user", &user)
	if e != nil {
		t.Fatal(e)
	}
	after, _ := json.Marshal(decoded)
	if !bytes.Equal(original, after) {
		t.Fatal("SQL wrapper changed Local model.Event bytes")
	}
	if _, e = decodeStoredFact(event.RoomID, event.Seq, event.ID, 1, event.Kind, string(event.Actor), original, "user", &user); e == nil {
		t.Fatal("legacy unprovenanced encoding guessed as current")
	}
	other := "u-other"
	for _, origin := range []struct {
		kind string
		user *string
	}{{"operator", nil}, {"user", nil}, {"user", &other}} {
		if _, e = decodeStoredFact(event.RoomID, event.Seq, event.ID, 2, event.Kind, string(event.Actor), encoded, origin.kind, origin.user); e == nil {
			t.Fatal("provenance index disagreement accepted")
		}
	}
	if _, e = encodeStoredFact(event, operatorFactOrigin()); e == nil {
		t.Fatal("operator was allowed to impersonate human actor")
	}
	event.Actor = model.ActorSystem
	encoded, e = encodeStoredFact(event, operatorFactOrigin())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = decodeStoredFact(event.RoomID, event.Seq, event.ID, 2, event.Kind, string(event.Actor), encoded, "operator", nil); e != nil {
		t.Fatal(e)
	}
	bad := bytes.Replace(encoded, []byte(`"codec_version":2`), []byte(`"codec_version":2,"codec_version":2`), 1)
	if _, e = decodeStoredFact(event.RoomID, event.Seq, event.ID, 2, event.Kind, string(event.Actor), bad, "operator", nil); e == nil {
		t.Fatal("duplicate wrapper fields accepted")
	}
}
func TestRecoverySchemaRefusesOldSameVersionShapeBeforeOwnership(t *testing.T) {
	for _, column := range []string{"metadata", "origin"} {
		t.Run(column, func(t *testing.T) {
			f := newRawFixture(t)
			if column == "metadata" {
				mustExec(t, f.admin, `ALTER TABLE schema_meta DROP COLUMN fact_codec_version`)
			} else {
				mustExec(t, f.admin, `ALTER TABLE room_events DROP COLUMN origin_kind CASCADE`)
			}
			if s, e := Open(context.Background(), f.cfg); e == nil {
				s.Close()
				t.Fatal("old same-version schema admitted")
			}
			var owners int
			if e := f.admin.QueryRow(context.Background(), `SELECT count(*) FROM ownership`).Scan(&owners); e != nil || owners != 0 {
				t.Fatal("shape refusal changed ownership")
			}
			if e := ApplySchema(context.Background(), f.admin, f.schema); e == nil {
				t.Fatal("ApplySchema silently migrated unsupported same-version shape")
			}
		})
	}
}
func TestRecoveryPaginatesAllRoomsIncludingArchived(t *testing.T) {
	b, _, _ := recoveryPGPair(t)
	ctx := context.Background()
	intent, _ := model.PrepareEnterpriseRoomIntent("Paged Room", model.Collaboration{})
	for i := 0; i < recoveryPageSize+1; i++ {
		room := fmt.Sprintf("page-%03d", i)
		if _, e := b.f.store.CommitRoomCreate(ctx, RoomCreateCommand{Auth: b.owner.Auth, RoomID: room, ClientID: "create-" + room, Intent: intent}); e != nil {
			t.Fatal(e)
		}
	}
	mustExec(t, b.f.admin, `UPDATE rooms SET archived_at=now() WHERE room_id LIKE 'page-%'`)
	recoveryRestart(t, b)
	before := recoveryCounts(t, b)
	report, e := b.f.store.RecoverDeliveries(ctx)
	if e != nil || report.Rooms != recoveryPageSize+3 || report.Reaped != 0 || !report.Complete {
		t.Fatalf("paged recovery: %+v %v", report, e)
	}
	if after := recoveryCounts(t, b); after[0] != before[0] || after[4] != before[4] {
		t.Fatal("zero-fact startup changed Room facts/revision")
	}
}
