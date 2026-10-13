package enterpriseservice

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type historyTestBackend struct {
	*relayHTTPTestBackend
	snapshot sqlstore.RoomSnapshot
}

func (b *historyTestBackend) ReadonlySnapshot(ctx context.Context, auth sqlstore.AuthContext, room string) (sqlstore.RoomSnapshot, error) {
	_, err := b.relayHTTPTestBackend.ReadonlySnapshot(ctx, auth, room)
	return b.snapshot, err
}
func historyHTTPFixture(t *testing.T) (*Handler, *historyTestBackend, string) {
	t.Helper()
	_, original, secret := newRelayHTTPFixture(t)
	b := &historyTestBackend{relayHTTPTestBackend: original}
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	intent, _ := model.PrepareEnterpriseRoomIntent("history fixture", model.Collaboration{})
	birth, err := model.NewEnterpriseRoomCreation("room-1", "deployment-1", "org-1", "user-1", intent, at)
	if err != nil {
		t.Fatal(err)
	}
	birth.Seq = 1
	events := []model.Event{birth}
	for i, slot := range model.SlotActors() {
		fact, err := model.NewEnterpriseBindingUpdate(model.EnterpriseBinding{Version: 1, RoomID: "room-1", Slot: slot, BindID: []string{"binding-1", "binding-2"}[i], Generation: 1, AgentID: []string{"agent-1", "agent-2"}[i], Runtime: model.RuntimeClaude, Active: true, ParkEnabled: true}, at)
		if err != nil {
			t.Fatal(err)
		}
		fact.Seq = uint64(len(events) + 1)
		events = append(events, fact)
	}
	data, _ := json.Marshal(map[string]any{"id": "message-1", "from": "slot2", "to": "slot1", "from_runtime": "claude", "from_generation": 1, "target_generation": 1, "text": "authorized full body", "state": "unknown", "source": "send", "created_at": at, "updated_at": at, "claimed_at": at, "receipt": "collector-private-receipt"})
	events = append(events, model.Event{ID: "message-event", RoomID: "room-1", Seq: 4, Kind: relay.EventMessage, Actor: model.ActorSlot2, CreatedAt: at, Data: data})
	b.snapshot = sqlstore.RoomSnapshot{RoomID: "room-1", DeploymentID: "deployment-1", OrgID: "org-1", CreationUserID: "user-1", HostMode: "native", Events: events, Seq: 4, Revision: 4}
	transactions, _ := enterpriseauth.NewMemoryStore(8)
	h, err := New(Config{DeploymentID: "deployment-1", PublicOrigin: serviceOrigin}, &fakeProvider{}, transactions, b)
	if err != nil {
		t.Fatal(err)
	}
	return h, b, secret
}

func TestEnterpriseHistoryHTTPRechecksAuthorizationAndExposesOnlyPublicEvidence(t *testing.T) {
	h, b, secret := historyHTTPFixture(t)
	w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/relay/history", relay.HistoryQuery{Pending: true, Limit: 1}, secret, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "authorized full body") || strings.Contains(w.Body.String(), "collector-private-receipt") || strings.Contains(w.Body.String(), "event_encoding") || b.authCalls != 2 || b.snapshotCalls != 1 || b.commitCalls != 0 || b.resolveCalls != 0 {
		t.Fatalf("history boundary: status=%d auth=%d snapshot=%d", w.Code, b.authCalls, b.snapshotCalls)
	}
	b.revokeAtRelease = true
	b.authCalls = 0
	w = nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/relay/history", relay.HistoryQuery{}, secret, nil)
	if w.Code < 400 || strings.Contains(w.Body.String(), "authorized full body") {
		t.Fatal("revocation at release disclosed history")
	}
}

func TestEnterpriseHistoryHTTPRejectsForeignRoomBrowserEnvelopeAndBadCursor(t *testing.T) {
	h, b, secret := historyHTTPFixture(t)
	for _, edit := range []func(*http.Request){func(r *http.Request) { r.Header.Set("Cookie", "session=private") }, func(r *http.Request) { r.Header.Set("Origin", serviceOrigin) }, func(r *http.Request) { r.Header.Set(DeviceDeploymentHeader, "other") }} {
		w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/relay/history", relay.HistoryQuery{}, secret, edit)
		if w.Code < 400 {
			t.Fatal("browser/wrong-deployment request accepted")
		}
	}
	if b.authCalls != 0 || b.snapshotCalls != 0 {
		t.Fatal("invalid envelope reached authority")
	}
	for _, target := range []struct {
		room string
		q    relay.HistoryQuery
	}{{"foreign-room", relay.HistoryQuery{}}, {"room-1", relay.HistoryQuery{Cursor: "pending:100"}}} {
		w := nativeRequest(t, h, "/enterprise/device/v1/rooms/"+target.room+"/relay/history", target.q, secret, nil)
		if w.Code < 400 || strings.Contains(w.Body.String(), "authorized full body") {
			t.Fatal("foreign history/cursor released content")
		}
	}
}
