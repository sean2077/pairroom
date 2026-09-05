package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestEventCursorPrefersNativeReconnectHeader(t *testing.T) {
	for _, test := range []struct {
		query, header string
		want          uint64
		invalid       bool
	}{
		{"", "", 0, false}, {"4", "", 4, false}, {"4", "9", 9, false},
		{"invalid", "9", 9, false}, {"9", "invalid", 0, true},
		{"-1", "", 0, true}, {"18446744073709551616", "", 0, true},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/events?since="+test.query, nil)
		r.Header.Set("Last-Event-ID", test.header)
		got, err := eventCursor(r)
		if (err != nil) != test.invalid || (!test.invalid && got != test.want) {
			t.Errorf("query=%q header=%q: got %d, %v", test.query, test.header, got, err)
		}
	}
}

func TestReplayResetBoundaries(t *testing.T) {
	tail := []model.Event{{Seq: 0}, {Seq: 5}, {Seq: 6}}
	for _, test := range []struct {
		since uint64
		reset bool
	}{{0, true}, {3, true}, {4, false}, {5, false}, {6, false}, {7, true}} {
		if got := replayNeedsReset(tail, test.since, 6); got != test.reset {
			t.Errorf("cursor=%d reset=%v want=%v", test.since, got, test.reset)
		}
	}
	if replayNeedsReset(nil, 0, 0) || !replayNeedsReset(nil, 0, 1) {
		t.Fatal("incorrect empty-tail recovery")
	}
}

func TestEventsResumeAndExplicitResetAPI(t *testing.T) {
	server, engine := newTestServer(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Replay occurs before the live loop; no timing-dependent stream wait.
	request := func(query, header string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/events?since="+query, nil).WithContext(ctx)
		r.Header.Set("Last-Event-ID", header)
		w := httptest.NewRecorder()
		server.events(w, r)
		return w
	}
	if response := request("bad", ""); response.Code != http.StatusBadRequest || !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("invalid cursor response: %d %s", response.Code, response.Body.String())
	}
	_, latest := engine.ReplayEvents()
	response := request("0", fmt.Sprint(latest))
	if strings.Contains(response.Body.String(), "id: 1\n") {
		t.Fatal("header ignored; replayed older events")
	}
	response = request("18446744073709551615", "")
	if !strings.Contains(response.Body.String(), "event: reset\n") || strings.Contains(response.Body.String(), "\nid:") {
		t.Fatalf("future cursor must request a snapshot without advancing ID: %s", response.Body.String())
	}
	for i := 0; i < 610; i++ {
		if err := engine.UpdateSettings(model.RoomSettings{StallWarningSeconds: 300 + i}); err != nil {
			t.Fatal(err)
		}
	}
	response = request("1", "")
	if !strings.Contains(response.Body.String(), "snapshot_required") || strings.Contains(response.Body.String(), "event: pairroom") {
		t.Fatalf("expired cursor silently skipped history: %s", response.Body.String())
	}
}

func TestSnapshotRejectsMalformedWindowLimits(t *testing.T) {
	server, _ := newTestServer(t, "")
	for _, value := range []string{"oops", "-1", "1001", "9999999999999999999999"} {
		w := httptest.NewRecorder()
		server.snapshot(w, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?message_limit="+value, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("limit=%q status=%d", value, w.Code)
		}
	}
	for _, value := range []string{"", "0", "1", "1000"} {
		w := httptest.NewRecorder()
		server.snapshot(w, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?message_limit="+value, nil))
		if w.Code != http.StatusOK {
			t.Errorf("limit=%q status=%d", value, w.Code)
		}
	}
}
