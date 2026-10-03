package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
)

func TestNativeBrowserTailPreservesFullExportAndAuthentication(t *testing.T) {
	f := nativeHTTP(t)
	for i := 0; i < 305; i++ {
		if _, err := f.native.engine.SendUser(relay.SendRequest{ID: fmt.Sprintf("history-%d", i), To: model.ActorSlot1, Text: "complete message"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/v1/snapshot?tail=1", 300},
		{"/api/v1/snapshot", 305},
		{"/api/v1/export?tail=1", 305},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+f.native.token)
		w := httptest.NewRecorder()
		f.native.boundary(http.HandlerFunc(f.native.serve)).ServeHTTP(w, req)
		var value struct {
			Relay relay.TailSnapshot `json:"relay"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &value) != nil || len(value.Relay.Messages) != tc.want {
			t.Fatalf("%s: status %d, messages %d", tc.path, w.Code, len(value.Relay.Messages))
		}
		if tc.want == 300 && (value.Relay.TotalMessages != 305 || value.Relay.TotalAudit < 305 || len(value.Relay.Audit) != relay.SnapshotAuditLimit) {
			t.Fatalf("bounded response lost totals: %+v", value.Relay)
		}
	}
	w := httptest.NewRecorder()
	f.native.boundary(http.HandlerFunc(f.native.serve)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?tail=1", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatal("tail bypassed the existing authentication boundary")
	}
}

func TestNativeBrowserConditionalTailRefresh(t *testing.T) {
	f := nativeHTTP(t)
	for i := 0; i < 4; i++ {
		if _, err := f.native.engine.SendUser(relay.SendRequest{ID: fmt.Sprintf("large-%d", i), To: model.ActorSlot1, Text: strings.Repeat("x", relay.MaxBodyBytes)}); err != nil {
			t.Fatal(err)
		}
	}
	sequence := f.native.engine.Sequence()
	query := fmt.Sprintf("?tail=1&known_sequence=%d", sequence)
	res := inspectionRequest(t, f, "/api/v1/snapshot"+query)
	var value struct {
		Relay   relay.TailSnapshot `json:"relay"`
		Summary relay.Summary      `json:"summary"`
	}
	if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &value) != nil || !value.Relay.TailUnchanged || len(value.Relay.Messages) != 0 || value.Relay.TotalMessages != 4 || value.Summary.Inboxes[model.ActorSlot1].Queued != 4 || res.Body.Len() > 8192 {
		t.Fatalf("conditional snapshot did not omit repeated evidence: %d, %d bytes", res.Code, res.Body.Len())
	}
	if f.native.engine.Sequence() != sequence {
		t.Fatal("conditional read changed the log")
	}
	if _, err := f.native.engine.SendUser(relay.SendRequest{ID: "new", To: model.ActorSlot1, Text: "new complete evidence"}); err != nil {
		t.Fatal(err)
	}
	res = inspectionRequest(t, f, "/api/v1/snapshot"+query)
	value.Relay = relay.TailSnapshot{}
	if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &value) != nil || value.Relay.TailUnchanged || len(value.Relay.Messages) == 0 || value.Relay.Messages[len(value.Relay.Messages)-1].Text != "new complete evidence" || value.Relay.TotalMessages != 5 {
		t.Fatal("a changed sequence did not refresh the bounded window")
	}
	for _, path := range []string{"/api/v1/export" + query, "/api/v1/snapshot"} {
		res = inspectionRequest(t, f, path)
		value.Relay = relay.TailSnapshot{}
		if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &value) != nil || value.Relay.TailUnchanged || len(value.Relay.Messages) != 5 {
			t.Fatal("conditional read changed complete export or default snapshot")
		}
	}
	for _, bad := range []string{"", "-1", "abc", "18446744073709551616", "1&known_sequence=2"} {
		if res := inspectionRequest(t, f, "/api/v1/snapshot?tail=1&known_sequence="+bad); res.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid sequence %q: %d", bad, res.Code)
		}
	}
	res = httptest.NewRecorder()
	f.native.http.Handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot"+query, nil))
	if res.Code != http.StatusUnauthorized {
		t.Fatal("conditional tail bypassed authentication")
	}
}

func TestNativeEventStreamExitsOnFatalWriter(t *testing.T) {
	log, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := relay.Open(relay.Config{RoomID: "room", Store: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SendUser(relay.SendRequest{ID: "fail", To: model.ActorSlot1, Text: "work"}); err == nil {
		t.Fatal("closed store accepted work")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		n := &nativeHostRuntime{engine: e}
		n.events(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx))
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("fatal writer kept the event stream alive or spinning")
	}
}

type invalidatedWakeRelay struct {
	*relay.Engine
	before func() error
}

func (r invalidatedWakeRelay) ReserveWake(id string, slot model.ActorID) error {
	if err := r.before(); err != nil {
		return err
	}
	return r.Engine.ReserveWake(id, slot)
}

func TestNativeWakerRejectsChangeAtReservationBoundary(t *testing.T) {
	f := nativeHTTP(t)
	_ = f.bind(t, model.ActorSlot2)
	message, err := f.native.engine.SendUser(relay.SendRequest{ID: "race", To: model.ActorSlot2, Text: "work"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	w := newNativeWaker(nativeWakerConfig{
		Relay: invalidatedWakeRelay{Engine: f.native.engine, before: func() error { return f.native.engine.Cancel(message.ID) }},
		Wait:  func(context.Context, time.Duration) error { return nil },
		Run:   func(context.Context, string, ...string) error { calls++; return nil },
	})
	if err := w.Wake(context.Background(), message.ID); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(w.attempts) != 0 || len(f.native.engine.WakeReservations()) != 0 {
		t.Fatal("stale observation executed a vendor command or consumed rate quota")
	}
	audit := f.native.engine.Snapshot().Audit
	if !strings.Contains(audit[len(audit)-1].Detail, "invalid_message") {
		t.Fatal("rejected reservation lost its redacted outcome")
	}
}
