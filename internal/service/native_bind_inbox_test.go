package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Resuming a bound session names input already waiting for it, and the brief
// status restates each slot's transport facts in one readable line.
func TestResumedBindNamesQueuedInputAndStatusShowsPresence(t *testing.T) {
	f := nativeHTTP(t)
	a := f.bind(t, model.ActorSlot1)
	kind := f.room.Agents[a.Slot].Runtime
	bindArgs := []string{"bind", "--room", f.room.ID, "--slot", string(a.Slot), "--service-file", f.endpoint}
	var first struct {
		Queued int `json:"inbox_queued"`
	}
	out, err := f.runAs(t, kind, a.SessionID, bindArgs, nil)
	if err != nil || json.Unmarshal(out, &first) != nil || first.Queued != 0 {
		t.Fatalf("resume with an empty inbox reported %d: %v %s", first.Queued, err, out)
	}
	for _, id := range []string{"one", "two"} {
		if _, err := f.native.engine.SendUser(relay.SendRequest{ID: id, To: a.Slot, Text: "private task " + id}); err != nil {
			t.Fatal(err)
		}
	}
	var resumed struct {
		Queued int    `json:"inbox_queued"`
		Notice string `json:"inbox_notice"`
	}
	out, err = f.runAs(t, kind, a.SessionID, bindArgs, nil)
	if err != nil || json.Unmarshal(out, &resumed) != nil {
		t.Fatalf("resume: %v %s", err, out)
	}
	if resumed.Queued != 2 || !strings.Contains(resumed.Notice, "pairroom relay wait --room "+f.room.ID+" --slot 1") {
		t.Fatalf("resumed bind = %+v", resumed)
	}
	if strings.Contains(string(out), "private task") {
		t.Fatal("bind output carried a queued message body")
	}
	out, err = f.runAs(t, kind, a.SessionID, []string{"status", "--brief", "--room", f.room.ID, "--slot", string(a.Slot)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Presence []string `json:"presence"`
	}
	if json.Unmarshal(out, &status) != nil || len(status.Presence) != 2 ||
		!strings.HasPrefix(status.Presence[0], "you slot1: ") || !strings.Contains(status.Presence[0], "2 queued") ||
		!strings.HasPrefix(status.Presence[1], "peer slot2: not bound") {
		t.Fatalf("presence = %q", status.Presence)
	}
}
