package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/relay"
)

// hookService records relay operations for a synthetic Stop hook; it is not a
// second relay engine or vendor E2E.
type hookService struct {
	mu          sync.Mutex
	calls       []string
	accepted    map[uint64]string
	failConfirm bool
}

func (s *hookService) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := filepath.Base(r.URL.Path)
		var req struct {
			Seq  uint64 `json:"report_seq"`
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, action)
		switch action {
		case "confirm":
			if s.failConfirm {
				http.Error(w, `{"error":"invalid official hook session metadata"}`, http.StatusConflict)
				return
			}
			_ = json.NewEncoder(w).Encode(relay.Binding{BindID: "binding", Generation: 1, SessionID: "session"})
		case "publication":
			_, ok := s.accepted[req.Seq]
			_ = json.NewEncoder(w).Encode(map[string]any{"accepted": ok})
		case "report":
			s.accepted[req.Seq] = req.Text
			_ = json.NewEncoder(w).Encode(relay.Publication{BindID: "binding", Generation: 1, ReportSeq: req.Seq})
		case "wait":
			_, _ = io.WriteString(w, `{"claim":null}`)
		default:
			t.Errorf("unexpected hook operation: %s", action)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	})
}

func (s *hookService) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := s.calls
	s.calls = nil
	return calls
}

func runStopHook(t *testing.T, cwd, text, transcript string) error {
	t.Helper()
	data, _ := json.Marshal(HookInput{Event: "Stop", SessionID: "session", CWD: cwd, TranscriptPath: transcript, LastAssistantMessage: &text})
	var out bytes.Buffer
	return Run(context.Background(), []string{"hook", "--runtime", "claude"}, bytes.NewReader(data), &out, io.Discard)
}

func TestHookSkipsRedundantMetadataCalls(t *testing.T) {
	isolateCaller(t)
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	service := &hookService{accepted: map[uint64]string{}}
	srv := httptest.NewServer(service.handler(t))
	defer srv.Close()
	if err := relay.AtomicJSON(c.State.EndpointPath, relay.Endpoint{URL: srv.URL, Token: "management-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := runStopHook(t, f.args[1], "@codex first", "/transcripts/one.jsonl"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(service.take(), ","); got != "confirm,report,wait" {
		t.Fatalf("first Stop operations = %s", got)
	}
	// The same transcript needs no inspect/confirm round trip: report and park
	// authenticate the session themselves.
	if err := runStopHook(t, f.args[1], "@codex second", "/transcripts/one.jsonl"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(service.take(), ","); got != "report,wait" {
		t.Fatalf("repeated Stop operations = %s", got)
	}
	if err := runStopHook(t, f.args[1], "@codex third", "/transcripts/two.jsonl"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(service.take(), ","); got != "confirm,report,wait" {
		t.Fatalf("new transcript operations = %s", got)
	}
	// Transcript metadata is optional: a rejected reference cannot block the
	// reply, and is retried at the next Stop rather than cached.
	service.failConfirm = true
	if err := runStopHook(t, f.args[1], "@codex fourth", "/transcripts/bad.jsonl"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(service.take(), ","); got != "confirm,report,wait" || service.accepted[4] != "@codex fourth" {
		t.Fatalf("rejected transcript blocked publication: %s", got)
	}
	var state State
	if err := readPrivate(f.statePath, &state); err != nil || state.TranscriptPath != "/transcripts/two.jsonl" {
		t.Fatalf("rejected transcript was cached: %+v %v", state, err)
	}
}

func TestHookKeepsReplyWhileServiceIsStopped(t *testing.T) {
	isolateCaller(t)
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.State.EndpointPath); err != nil {
		t.Fatal(err)
	}
	if err := runStopHook(t, f.args[1], "@codex reply while stopped", ""); err == nil || !strings.Contains(err.Error(), "is PairRoom running") {
		t.Fatalf("stopped Service was not reported: %v", err)
	}
	var state State
	if err := readPrivate(f.statePath, &state); err != nil {
		t.Fatal(err)
	}
	if state.Pending == nil || state.Pending.Text != "@codex reply while stopped" || state.Pending.Seq != 1 {
		t.Fatalf("reply was not retained for reconciliation: %+v", state.Pending)
	}
	// One pending slot holds one reply. A second Stop while still stopped
	// cannot keep its body and never displaces or renumbers the first reply.
	if err := runStopHook(t, f.args[1], "@codex second reply while stopped", ""); err == nil {
		t.Fatal("stopped Service was not reported")
	}
	var second State
	if err := readPrivate(f.statePath, &second); err != nil || second.Pending == nil || second.Pending.Seq != 1 || second.Pending.Text != "@codex reply while stopped" || second.LastSeq != 1 {
		t.Fatalf("second stopped reply not accounted for: %+v %v", second, err)
	}
	service := &hookService{accepted: map[uint64]string{}}
	srv := httptest.NewServer(service.handler(t))
	defer srv.Close()
	if err := relay.AtomicJSON(c.State.EndpointPath, relay.Endpoint{URL: srv.URL, Token: "management-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := runStopHook(t, f.args[1], "@codex next reply", ""); err != nil {
		t.Fatal(err)
	}
	if service.accepted[1] != "@codex reply while stopped" || service.accepted[2] != "@codex next reply" {
		t.Fatalf("retained reply not published in order: %v", service.accepted)
	}
	var settled State
	if err := readPrivate(f.statePath, &settled); err != nil || settled.Pending != nil || settled.LastConfirmedSeq != 2 {
		t.Fatalf("publication not reconciled: %+v %v", settled, err)
	}
}
