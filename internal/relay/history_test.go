package relay

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func TestNativePendingOutlivesChatWindow(t *testing.T) {
	e, a, _ := testEngine(t)
	oldest, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "old", Text: "old unresolved"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 301; i++ {
		if _, err := e.Send(a[model.ActorSlot1], SendRequest{ID: fmt.Sprint(i), Text: "human report", To: model.ActorUser}); err != nil {
			t.Fatal(err)
		}
	}
	tail := e.SnapshotTail()
	if len(tail.Messages) != 300 {
		t.Fatal(len(tail.Messages))
	}
	page, err := e.AuthHistory(a[model.ActorSlot2], HistoryQuery{Pending: true, Limit: 1})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != oldest.ID || page.Total != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	seq := e.Sequence()
	page, err = e.History(HistoryQuery{ID: oldest.ID})
	if err != nil || page.Messages[0].State != "queued" || e.Sequence() != seq {
		t.Fatal("history changed delivery")
	}
}

func TestNativeHistoryStableCursorAndBudget(t *testing.T) {
	e, a, _ := testEngine(t)
	ids := []string{}
	for i := 0; i < 8; i++ {
		m, err := e.Send(a[model.ActorSlot1], SendRequest{ID: fmt.Sprint(i), Text: strings.Repeat("x", MaxBodyBytes)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	first, err := e.History(HistoryQuery{Pending: true, Limit: 100})
	if err != nil || len(first.Messages) != 4 || !first.HasMore {
		t.Fatalf("%+v %v", first, err)
	}
	if err := e.Cancel(ids[0]); err != nil {
		t.Fatal(err)
	}
	second, err := e.History(HistoryQuery{Pending: true, Cursor: first.NextCursor})
	if err != nil || len(second.Messages) != 4 || second.Messages[0].ID != ids[4] || second.HasMore {
		t.Fatalf("%+v %v", second, err)
	}
	latest, _ := e.History(HistoryQuery{Limit: 2})
	older, err := e.History(HistoryQuery{Limit: 2, Cursor: latest.NextCursor})
	if err != nil || older.Messages[0].ID != ids[5] {
		t.Fatalf("%+v %v", older, err)
	}
	for _, q := range []HistoryQuery{{Limit: 101}, {Cursor: "pending:1"}, {Pending: true, Cursor: "history:1"}, {Cursor: "history:-1"}, {Cursor: "history:999"}, {ID: ids[0], Pending: true}} {
		if _, err := e.History(q); err == nil {
			t.Fatalf("accepted %+v", q)
		}
	}
	bad := a[model.ActorSlot2]
	bad.Secret = "wrong"
	if _, err := e.AuthHistory(bad, HistoryQuery{}); err == nil {
		t.Fatal("auth bypass")
	}
	e.mu.Lock()
	m := e.messages[ids[1]]
	m.Receipt = "private-receipt"
	e.putMessage(m)
	e.mu.Unlock()
	page, _ := e.History(HistoryQuery{ID: ids[1]})
	data, _ := json.Marshal(page)
	if strings.Contains(string(data), "private-receipt") {
		t.Fatal("leaked receipt")
	}
}

func TestNativeUserSendLookupIsReadOnly(t *testing.T) {
	e, a, _ := testEngine(t)
	if _, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "same", Text: "peer"}); err != nil {
		t.Fatal(err)
	}
	absent, err := e.UserSendReceipt("same")
	if err != nil || absent.Found {
		t.Fatal("cross-sender ID lookup")
	}
	m, err := e.SendUser(SendRequest{ID: "same", Text: "user", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	seq := e.Sequence()
	found, err := e.UserSendReceipt("same")
	if err != nil || !found.Found || found.Message.ID != m.ID || e.Sequence() != seq {
		t.Fatalf("%+v %v", found, err)
	}
}

func TestNativeIndexMatchesHistoryAfterRandomTransitionsAndReplay(t *testing.T) {
	e, a, dir := testEngine(t)
	rng := rand.New(rand.NewSource(22))
	ids := []string{}
	for i := 0; i < 120; i++ {
		if len(ids) == 0 || rng.Intn(3) == 0 {
			m, err := e.Send(a[model.ActorSlot1], SendRequest{ID: fmt.Sprint(i), Text: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, m.ID)
		} else {
			id := ids[rng.Intn(len(ids))]
			m := e.messages[id]
			states := []string{"queued", "delivering", "unknown", "handed_off", "cancelled"}
			m.State = states[rng.Intn(len(states))]
			m.ClaimedAt = time.Now()
			m.UpdatedAt = time.Now()
			if err := e.append(EventMessage, model.ActorSystem, messageFact{Message: m}); err != nil {
				t.Fatal(err)
			}
		}
		assertNativeIndex(t, e)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Config{RoomID: "room", Store: log})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertNativeIndex(t, reopened)
}

func assertNativeIndex(t *testing.T, e *Engine) {
	t.Helper()
	want := []string{}
	counts := map[model.ActorID]InboxSummary{}
	queues := map[model.ActorID][]string{}
	for _, id := range e.order {
		m := e.messages[id]
		if unresolvedState(m.State) && m.To.ValidParticipant() {
			want = append(want, id)
		}
		c := counts[m.To]
		switch m.State {
		case "queued":
			c.Queued++
			queues[m.To] = append(queues[m.To], id)
		case "delivering":
			c.Delivering++
		case "unknown":
			c.Unknown++
		}
		counts[m.To] = c
	}
	if len(want) != len(e.unresolved) || len(want) > 0 && !reflect.DeepEqual(want, e.unresolved) {
		t.Fatalf("pending index mismatch %v %v", want, e.unresolved)
	}
	for _, slot := range model.SlotActors() {
		if counts[slot] != e.counts[slot] || len(queues[slot]) != len(e.queued[slot]) || len(queues[slot]) > 0 && !reflect.DeepEqual(queues[slot], e.queued[slot]) {
			t.Fatalf("slot %s index mismatch", slot)
		}
	}
}
