package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	reported    []uint64
	failConfirm bool
	failReport  uint64
	rejectWait  bool
	release     string
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
		if s.release != "" {
			w.Header().Set(relay.VersionHeader, s.release)
		}
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
			s.reported = append(s.reported, req.Seq)
			if req.Seq == s.failReport {
				http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			s.accepted[req.Seq] = req.Text
			_ = json.NewEncoder(w).Encode(relay.Publication{BindID: "binding", Generation: 1, ReportSeq: req.Seq})
		case "wait":
			if s.rejectWait {
				http.Error(w, `{"error":"unknown relay operation"}`, http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"claim":null}`)
		case "summary":
			_ = json.NewEncoder(w).Encode(relay.Summary{})
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
	_, err := runStopHookDiagnostic(t, cwd, text, transcript)
	return err
}

func runStopHookDiagnostic(t *testing.T, cwd, text, transcript string) (string, error) {
	t.Helper()
	data, _ := json.Marshal(HookInput{Event: "Stop", SessionID: "session", CWD: cwd, TranscriptPath: transcript, LastAssistantMessage: &text})
	var out, diagnostic bytes.Buffer
	err := Run(context.Background(), []string{"hook", "--runtime", "claude"}, bytes.NewReader(data), &out, &diagnostic)
	return diagnostic.String(), err
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

func TestHookKeepsRepliesWhileServiceIsStopped(t *testing.T) {
	isolateCaller(t)
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.State.EndpointPath); err != nil {
		t.Fatal(err)
	}
	stopped := []string{"@codex first reply while stopped", "no handle: private second reply", "@codex third reply while stopped"}
	for i, text := range stopped {
		if err := runStopHook(t, f.args[1], text, ""); err == nil || !strings.Contains(err.Error(), "is PairRoom running") {
			t.Fatalf("stopped Service was not reported for Stop %d: %v", i+1, err)
		}
	}
	// Each Stop keeps its own sequence in arrival order; none displaces or
	// renumbers an earlier one, and nothing is marked uncertain.
	var state State
	if err := readPrivate(f.statePath, &state); err != nil {
		t.Fatal(err)
	}
	if state.LastSeq != 3 || state.LastConfirmedSeq != 0 || state.Pending == nil || state.Pending.Seq != 1 || state.Pending.Text != stopped[0] || state.Pending.Unknown || len(state.Held) != 2 {
		t.Fatalf("stopped replies were not all retained: %+v", state)
	}
	for i, held := range state.Held {
		if held.Seq != uint64(i+2) || held.Text != stopped[i+1] || held.Unknown {
			t.Fatalf("held reply %d = %+v", i, held)
		}
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
	// The head was saved by an earlier process, so its original key is queried
	// first; the held replies were never sent and go out once, in order.
	if got := strings.Join(service.take(), ","); got != "publication,report,report,report,report,wait" {
		t.Fatalf("returning Service operations = %s", got)
	}
	if got := fmt.Sprint(service.reported); got != "[1 2 3 4]" {
		t.Fatalf("retained replies not published in order: %s", got)
	}
	for i, text := range append(stopped, "@codex next reply") {
		if service.accepted[uint64(i+1)] != text {
			t.Fatalf("seq %d published %q, want %q", i+1, service.accepted[uint64(i+1)], text)
		}
	}
	var settled State
	if err := readPrivate(f.statePath, &settled); err != nil || settled.Pending != nil || settled.Held != nil || settled.LastConfirmedSeq != 4 || settled.LastSeq != 4 {
		t.Fatalf("publication not reconciled: %+v %v", settled, err)
	}
}

// relay status reconciles before reporting, so it publishes the whole saved
// backlog in order; the docs promise this side effect, and doctor/history as
// the read-only alternatives.
func TestStatusPublishesSavedBacklog(t *testing.T) {
	isolateCaller(t)
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.State.EndpointPath); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"@codex one", "@codex two", "@codex three"} {
		_ = runStopHook(t, f.args[1], text, "")
	}
	service := &hookService{accepted: map[uint64]string{}}
	srv := httptest.NewServer(service.handler(t))
	defer srv.Close()
	if err := relay.AtomicJSON(c.State.EndpointPath, relay.Endpoint{URL: srv.URL, Token: "management-secret"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), append([]string{"status"}, f.args...), strings.NewReader(""), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(service.reported); got != "[1 2 3]" {
		t.Fatalf("status did not publish the saved backlog in order: %s", got)
	}
	var settled State
	if err := readPrivate(f.statePath, &settled); err != nil || settled.Pending != nil || settled.Held != nil || settled.LastConfirmedSeq != 3 {
		t.Fatalf("status left the backlog unsettled: %+v %v", settled, err)
	}
}

func TestHookBacklogCapReportsUnretainedReply(t *testing.T) {
	isolateCaller(t)
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.State.EndpointPath); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 8; i++ {
		diagnostic, _ := runStopHookDiagnostic(t, f.args[1], fmt.Sprintf("@codex stopped reply %d", i), "")
		if strings.Contains(diagnostic, "NOT retained") {
			t.Fatalf("Stop %d within the backlog was reported lost: %s", i, diagnostic)
		}
	}
	diagnostic, err := runStopHookDiagnostic(t, f.args[1], "@codex ninth reply beyond the cap", "")
	if err == nil {
		t.Fatal("stopped Service was not reported")
	}
	if !strings.Contains(diagnostic, "this reply was NOT retained behind 8 unpublished earlier replies") || !strings.Contains(diagnostic, "relay send") {
		t.Fatalf("reply beyond the cap was dropped without an explicit diagnostic: %q", diagnostic)
	}
	var state State
	if err := readPrivate(f.statePath, &state); err != nil {
		t.Fatal(err)
	}
	// The refused reply consumed no sequence, so it cannot fabricate a gap.
	if state.LastSeq != 8 || state.Pending == nil || state.Pending.Seq != 1 || len(state.Held) != 7 || state.Held[6].Text != "@codex stopped reply 8" {
		t.Fatalf("cap changed the retained backlog: %+v", state)
	}
	for _, held := range state.Held {
		if strings.Contains(held.Text, "ninth") {
			t.Fatal("reply beyond the cap was saved")
		}
	}
	// The Service returns but seq 3 cannot be settled yet: seqs 1 and 2 go
	// out, which frees room, so a further Stop is held behind the uncertain
	// head instead of being lost, and nothing behind seq 3 is sent early.
	service := &hookService{accepted: map[uint64]string{}, failReport: 3}
	srv := httptest.NewServer(service.handler(t))
	defer srv.Close()
	if err := relay.AtomicJSON(c.State.EndpointPath, relay.Endpoint{URL: srv.URL, Token: "management-secret"}); err != nil {
		t.Fatal(err)
	}
	diagnostic, err = runStopHookDiagnostic(t, f.args[1], "@codex reply after partial progress", "")
	if err != nil || strings.Contains(diagnostic, "NOT retained") || !strings.Contains(diagnostic, "publication pending") {
		t.Fatalf("partial progress did not retain the new reply: %q %v", diagnostic, err)
	}
	if got := fmt.Sprint(service.reported); got != "[1 2 3]" {
		t.Fatalf("partial progress reports = %s", got)
	}
	var partial State
	if err := readPrivate(f.statePath, &partial); err != nil || partial.Pending == nil || partial.Pending.Seq != 3 || !partial.Pending.Unknown || len(partial.Held) != 6 || partial.LastSeq != 9 || partial.Held[5].Text != "@codex reply after partial progress" {
		t.Fatalf("partial progress state: %+v %v", partial, err)
	}
	service.mu.Lock()
	service.failReport = 0
	service.reported = nil
	service.mu.Unlock()
	var out bytes.Buffer
	if err := Run(context.Background(), append([]string{"reconcile"}, f.args...), strings.NewReader(""), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	// The uncertain head is queried by its key and then supplemented under
	// the same sequence because the Service reports it absent.
	if got := fmt.Sprint(service.reported); got != "[3 4 5 6 7 8 9]" {
		t.Fatalf("relay reconcile did not publish the backlog in order: %s", got)
	}
	var settled State
	if err := readPrivate(f.statePath, &settled); err != nil || settled.Pending != nil || settled.Held != nil || settled.LastConfirmedSeq != 9 {
		t.Fatalf("backlog not settled: %+v %v", settled, err)
	}
}
