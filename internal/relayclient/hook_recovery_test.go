package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Real CLI, private WAL and HTTP boundaries; a stalled local Service response,
// not a vendor/model E2E. Synchronization uses request cancellation, not sleeps.
func TestHookSlowSenderStillCollects(t *testing.T) {
	// Shrink both budgets; the invariant is that they bound the sender, not
	// their production magnitudes. Keep metadata inside publication as shipped.
	publication, metadata := hookPublicationBudget, hookMetadataBudget
	hookPublicationBudget, hookMetadataBudget = 400*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { hookPublicationBudget, hookMetadataBudget = publication, metadata })
	for _, stalled := range []string{"confirm", "report"} {
		t.Run(stalled, func(t *testing.T) {
			isolateCaller(t)
			f := newForegroundFixture(t, foregroundFixtureOptions{})
			c, err := load(filepath.Dir(f.statePath))
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var calls []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				action := filepath.Base(r.URL.Path)
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				calls = append(calls, action)
				mu.Unlock()
				if action == stalled {
					<-r.Context().Done()
					return
				}
				switch action {
				case "report":
					_ = json.NewEncoder(w).Encode(relay.Publication{BindID: "binding", Generation: 1, ReportSeq: 1})
				case "wait":
					_ = json.NewEncoder(w).Encode(map[string]any{"claim": relay.Claim{ID: "incoming", Receipt: "receipt", Envelope: "[PairRoom message]\nqueued peer reply"}})
				case "ack":
					_, _ = io.WriteString(w, `{"handed_off":true}`)
				default:
					t.Errorf("unexpected relay operation: %s", action)
					http.Error(w, "unexpected", http.StatusBadRequest)
				}
			}))
			defer srv.Close()
			if err := relay.AtomicJSON(c.State.EndpointPath, relay.Endpoint{URL: srv.URL, Token: "management-secret"}); err != nil {
				t.Fatal(err)
			}
			text := "@codex retained sender reply"
			hook := HookInput{Event: "Stop", SessionID: "session", CWD: f.args[1], LastAssistantMessage: &text}
			if stalled == "confirm" {
				hook.TranscriptPath = "/transcripts/new.jsonl"
			}
			data, _ := json.Marshal(hook)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var out, diagnostic bytes.Buffer
			if err := Run(ctx, []string{"hook", "--runtime", "claude"}, bytes.NewReader(data), &out, &diagnostic); err != nil {
				t.Fatal(err)
			}
			var decision struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			}
			if err := json.Unmarshal(out.Bytes(), &decision); err != nil || decision.Decision != "block" || !strings.Contains(decision.Reason, "queued peer reply") {
				t.Fatalf("sender starved receive-side delivery: %s (%v)", out.String(), err)
			}
			mu.Lock()
			got := strings.Join(calls, ",")
			mu.Unlock()
			want := "report,wait,ack"
			if stalled == "confirm" {
				want = "confirm," + want
			}
			if got != want {
				t.Fatalf("unexpected replay or missing collection: %s", got)
			}
			var state State
			if err := readPrivate(f.statePath, &state); err != nil {
				t.Fatal(err)
			}
			if stalled == "report" {
				if state.LastSeq != 1 || state.Pending == nil || state.Pending.Seq != 1 || state.Pending.Text != text || !strings.Contains(diagnostic.String(), "publication pending") {
					t.Fatalf("uncertain original publication was not retained: %+v / %s", state, diagnostic.String())
				}
			} else if state.Pending != nil || state.LastConfirmedSeq != 1 || state.TranscriptPath != "" {
				t.Fatalf("optional metadata timeout blocked report or was cached: %+v", state)
			}
		})
	}
}

func TestDeliveryRequiresAffirmativeAcknowledgement(t *testing.T) {
	for _, body := range []string{"", `{}`, `null`, `{"handed_off":false}`, `{"handed_off":"true"}`, `<html>error</html>`, `{"handed_off":true}`} {
		t.Run(body, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				action := filepath.Base(r.URL.Path)
				mu.Lock()
				calls = append(calls, action)
				mu.Unlock()
				switch action {
				case "wait":
					_ = json.NewEncoder(w).Encode(map[string]any{"claim": relay.Claim{ID: "incoming", Receipt: "receipt", Envelope: "complete incoming message"}})
				case "ack":
					var receipt map[string]string
					if json.NewDecoder(r.Body).Decode(&receipt) != nil || receipt["id"] != "incoming" || receipt["receipt"] != "receipt" {
						t.Error("ack lost the exact claim identity")
					}
					_, _ = io.WriteString(w, body)
				default:
					t.Errorf("unexpected operation: %s", action)
				}
			}))
			defer srv.Close()
			c := &Client{State: State{Room: "room", Slot: model.ActorSlot1}, Endpoint: relay.Endpoint{URL: srv.URL}, HTTP: srv.Client()}
			var out bytes.Buffer
			delivered, err := deliverOnce(context.Background(), c, false, 1, &out)
			confirmed := body == `{"handed_off":true}`
			if delivered != confirmed || (err == nil) != confirmed {
				t.Fatalf("HTTP 200 without affirmative ack was treated as handoff: delivered=%v err=%v", delivered, err)
			}
			if !confirmed && !strings.Contains(err.Error(), "never automatically replay") {
				t.Fatalf("uncertainty guidance lost: %v", err)
			}
			mu.Lock()
			got := strings.Join(calls, ",")
			mu.Unlock()
			if got != "wait,ack" || out.String() != "complete incoming message\n" {
				t.Fatalf("delivery was repeated or clipped: calls=%s stdout=%q", got, out.String())
			}
		})
	}
}

// A Stop hook that has already written its block decision must not exit
// nonzero: harnesses ignore stdout from a failed hook, so the model would never
// see the envelope the Service is about to mark unknown.
func TestHookKeepsBlockDecisionWhenAcknowledgementIsUncertain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dropAcks   int
		wantAcks   int
		wantNotice bool
	}{
		{name: "transient transport failure is re-acknowledged", dropAcks: 1, wantAcks: 2},
		{name: "persistent transport failure keeps the decision", dropAcks: 99, wantAcks: 3, wantNotice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateCaller(t)
			f := newForegroundFixture(t, foregroundFixtureOptions{})
			c, err := load(filepath.Dir(f.statePath))
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			acks := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				action := filepath.Base(r.URL.Path)
				_, _ = io.Copy(io.Discard, r.Body)
				switch action {
				case "report":
					_ = json.NewEncoder(w).Encode(relay.Publication{BindID: "binding", Generation: 1, ReportSeq: 1})
				case "wait":
					_ = json.NewEncoder(w).Encode(map[string]any{"claim": relay.Claim{ID: "incoming", Receipt: "receipt", Envelope: "[PairRoom message]\npeer reply"}})
				case "ack":
					mu.Lock()
					acks++
					drop := acks <= tc.dropAcks
					mu.Unlock()
					if drop {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err == nil {
							_ = conn.Close()
						}
						return
					}
					_, _ = io.WriteString(w, `{"handed_off":true}`)
				default:
					t.Errorf("unexpected relay operation: %s", action)
				}
			}))
			defer srv.Close()
			if err := relay.AtomicJSON(c.State.EndpointPath, relay.Endpoint{URL: srv.URL, Token: "management-secret"}); err != nil {
				t.Fatal(err)
			}
			text := "@codex please review"
			data, _ := json.Marshal(HookInput{Event: "Stop", SessionID: "session", CWD: f.args[1], LastAssistantMessage: &text})
			var out, diagnostic bytes.Buffer
			if err := Run(context.Background(), []string{"hook", "--runtime", "claude"}, bytes.NewReader(data), &out, &diagnostic); err != nil {
				t.Fatalf("hook failed after writing its decision, so the harness would drop it: %v", err)
			}
			var decision struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			}
			if err := json.Unmarshal(out.Bytes(), &decision); err != nil || decision.Decision != "block" || !strings.Contains(decision.Reason, "peer reply") {
				t.Fatalf("block decision lost: %q (%v)", out.String(), err)
			}
			mu.Lock()
			got := acks
			mu.Unlock()
			if got != tc.wantAcks {
				t.Fatalf("acks = %d, want %d", got, tc.wantAcks)
			}
			if strings.Contains(diagnostic.String(), "acknowledgement was not recorded") != tc.wantNotice {
				t.Fatalf("diagnostic = %q", diagnostic.String())
			}
		})
	}
}
