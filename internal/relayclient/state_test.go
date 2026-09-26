package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// This fixture exercises the real HTTP client, durable state file, and exact
// publication receipt identity. It deliberately does not impersonate a vendor.
func publicationClient(t *testing.T) (*Client, *publicationServer) {
	t.Helper()
	server := &publicationServer{accepted: map[uint64]bool{}}
	httpServer := httptest.NewServer(http.HandlerFunc(server.serve))
	t.Cleanup(httpServer.Close)
	dir := t.TempDir()
	endpointPath := filepath.Join(t.TempDir(), relay.EndpointFile)
	if err := relay.AtomicJSON(endpointPath, relay.Endpoint{URL: httpServer.URL, Token: "management-secret-not-output"}); err != nil {
		t.Fatal(err)
	}
	s := State{Schema: 2, Room: "room", Slot: model.ActorSlot1, Runtime: model.RuntimeClaude, Workspace: dir, EndpointPath: endpointPath, BindID: "binding", Generation: 1, SessionID: "session"}
	if err := relay.AtomicJSON(filepath.Join(dir, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	if err := relay.AtomicJSON(filepath.Join(dir, "credentials"), credentials{BindID: s.BindID, Secret: "private-long-lived-secret"}); err != nil {
		t.Fatal(err)
	}
	c, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c, server
}

type publicationServer struct {
	mu                                                  sync.Mutex
	accepted                                            map[uint64]bool
	reports, queries                                    int
	reported                                            []uint64
	dropBefore, dropAfter, queryUnknown, queryMalformed bool
	acks                                                int
}

func (s *publicationServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var input struct {
		Seq uint64 `json:"report_seq"`
	}
	_ = json.NewDecoder(r.Body).Decode(&input)
	if r.Header.Get("Authorization") != "Relay private-long-lived-secret" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch filepath.Base(r.URL.Path) {
	case "publication":
		s.queries++
		if s.queryUnknown {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if s.queryMalformed {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"accepted": s.accepted[input.Seq]})
	case "report":
		s.reports++
		s.reported = append(s.reported, input.Seq)
		if s.dropBefore {
			http.Error(w, "before acceptance", http.StatusServiceUnavailable)
			return
		}
		s.accepted[input.Seq] = true
		if s.dropAfter {
			http.Error(w, "response lost", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(relay.Publication{BindID: "binding", Generation: 1, ReportSeq: input.Seq})
	case "wait":
		_ = json.NewEncoder(w).Encode(map[string]any{"claim": relay.Claim{ID: "message", Receipt: "receipt", Envelope: "[PairRoom message]\nfrom: @codex\nbody"}})
	case "ack":
		s.acks++
		_, _ = io.WriteString(w, `{"handed_off":true}`)
	}
}
func TestReservePublicationClaimsWALBeforeAnyHTTP(t *testing.T) {
	c, server := publicationClient(t)
	if err := c.ReservePublication("reply @codex"); err != nil {
		t.Fatal(err)
	}
	if c.State.Pending == nil || c.State.Pending.Seq != 1 || c.State.LastSeq != 1 {
		t.Fatalf("reservation did not claim the WAL: %#v", c.State)
	}
	server.mu.Lock()
	reports, queries := server.reports, server.queries
	server.mu.Unlock()
	if reports != 0 || queries != 0 {
		t.Fatalf("reservation must be local-only: reports=%d queries=%d", reports, queries)
	}
	// A second reservation is held behind the head, never overwriting it.
	if err := c.ReservePublication("second"); err != nil {
		t.Fatal(err)
	}
	if c.State.Pending.Seq != 1 || c.State.Pending.Text != "reply @codex" || len(c.State.Held) != 1 || c.State.Held[0].Seq != 2 || c.State.LastSeq != 2 {
		t.Fatalf("second reservation displaced the head: %#v", c.State)
	}
}

func TestReservedReportFailureKeepsReservationForSameSeqReconcile(t *testing.T) {
	c, server := publicationClient(t)
	if err := c.ReservePublication("reply @codex"); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.dropBefore = true
	server.mu.Unlock()
	if err := c.Reconcile(context.Background(), false); !errors.Is(err, relay.ErrUnknown) {
		t.Fatalf("reserved report failure = %v, want ErrUnknown", err)
	}
	if c.State.Pending == nil || c.State.Pending.Seq != 1 {
		t.Fatalf("failed reserved report must keep the WAL reservation: %#v", c.State)
	}
	server.mu.Lock()
	server.dropBefore = false
	server.mu.Unlock()
	if err := c.Reconcile(context.Background(), false); err != nil {
		t.Fatalf("reconcile after reserved failure: %v", err)
	}
	if c.State.Pending != nil || c.State.LastConfirmedSeq != 1 || c.State.LastSeq != 1 {
		t.Fatalf("reconcile must confirm the ORIGINAL sequence: %#v", c.State)
	}
	server.mu.Lock()
	reports := server.reports
	accepted := server.accepted[1]
	server.mu.Unlock()
	if reports != 2 || !accepted {
		t.Fatalf("reports=%d accepted[1]=%v; want one failed then one successful same-seq report", reports, accepted)
	}
}

func TestReservedReportClearsReservationWithoutQuery(t *testing.T) {
	c, server := publicationClient(t)
	if err := c.ReservePublication("reply"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if c.State.Pending != nil || c.State.LastConfirmedSeq != 1 {
		t.Fatalf("state after reserved publish: %#v", c.State)
	}
	server.mu.Lock()
	reports, queries := server.reports, server.queries
	server.mu.Unlock()
	// This process saved the reservation and never sent it: no receipt query.
	if reports != 1 || queries != 0 {
		t.Fatalf("reports=%d queries=%d, want exactly one report", reports, queries)
	}
}

func TestPublicationCrashBeforeAtomicWriteConsumesNothing(t *testing.T) {
	c, server := publicationClient(t)
	save := c.Save
	c.Save = func(State) error { return errors.New("simulated crash before atomic write") }
	if c.Publish(context.Background(), "@codex invisible lost reply") == nil {
		t.Fatal("write failure ignored")
	}
	disk, err := load(c.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if disk.State.LastSeq != 0 || disk.State.Pending != nil || server.reports != 0 || server.queries != 0 {
		t.Fatal("pre-write crash consumed seq or falsely published")
	}
	c.Save = save
	if err := c.Publish(context.Background(), "@codex next reply"); err != nil {
		t.Fatal(err)
	}
	if c.State.LastConfirmedSeq != 1 {
		t.Fatal("undetectable pre-write loss created artificial gap")
	}
}
func TestPublicationReconcilesCrashWindowsWithoutNewIdentity(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "after-atomic-before-service", true: "service-accepted-response-lost"}[after], func(t *testing.T) {
			c, server := publicationClient(t)
			server.dropBefore = !after
			server.dropAfter = after
			if !errors.Is(c.Publish(context.Background(), "@codex authoritative body"), relay.ErrUnknown) {
				t.Fatal("transport uncertainty not surfaced")
			}
			fresh, err := load(c.Dir)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.State.Pending == nil || fresh.State.Pending.Seq != 1 || fresh.State.LastSeq != 1 {
				t.Fatal("atomic pending body/seq missing")
			}
			server.dropBefore = false
			server.dropAfter = false
			if err := fresh.Reconcile(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			if fresh.State.Pending != nil || fresh.State.LastConfirmedSeq != 1 || len(server.accepted) != 1 || server.queries != 1 {
				t.Fatal("reconciliation failed")
			}
			want := 2
			if after {
				want = 1
			}
			if server.reports != want {
				t.Fatalf("resend count=%d want=%d", server.reports, want)
			}
		})
	}
}
func TestPublicationUnknownPreservesPendingAndNeverAllocatesNewSequence(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport-unknown", true: "missing-authoritative-receipt"}[malformed], func(t *testing.T) {
			c, server := publicationClient(t)
			server.dropBefore = true
			_ = c.Publish(context.Background(), "pending")
			server.dropBefore = false
			server.queryUnknown = !malformed
			server.queryMalformed = malformed
			if !errors.Is(c.Publish(context.Background(), "new reply must not replace pending"), relay.ErrUnknown) {
				t.Fatal("unknown query permitted new publication")
			}
			if c.State.LastSeq != 1 || c.State.Pending == nil || !c.State.Pending.Unknown || c.State.Pending.Text != "pending" || server.reports != 1 {
				t.Fatal("unknown branch overwrote pending or retried")
			}
			if err := c.Reconcile(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			if !server.accepted[1] || server.accepted[2] {
				t.Fatal("explicit supplement changed publication identity")
			}
		})
	}
}
func TestPublicationLostLocalConfirmationReconcilesAcceptedWithoutSend(t *testing.T) {
	c, server := publicationClient(t)
	save := c.Save
	c.Save = func(s State) error {
		if s.Pending == nil {
			return errors.New("crash clearing pending")
		}
		return save(s)
	}
	if c.Publish(context.Background(), "@codex accepted") == nil {
		t.Fatal("confirmation failure ignored")
	}
	fresh, err := load(c.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Reconcile(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if server.reports != 1 || fresh.State.LastConfirmedSeq != 1 {
		t.Fatal("accepted reply was resent")
	}
}
func TestExplicitPendingDiscardRetainsConsumedSequence(t *testing.T) {
	c, server := publicationClient(t)
	server.dropBefore = true
	_ = c.Publish(context.Background(), "lost after atomic write")
	if err := c.DiscardPending(); err != nil {
		t.Fatal(err)
	}
	server.dropBefore = false
	if err := c.Publish(context.Background(), "next reply provides gap evidence"); err != nil {
		t.Fatal(err)
	}
	if !server.accepted[2] || server.accepted[1] {
		t.Fatal("explicit discard did not retain consumed seq")
	}
}

// An attempted head with an uncertain outcome is only ever queried by its
// original key; the held replies behind it wait and are never sent early.
func TestUncertainHeadHoldsBacklogWithoutReplay(t *testing.T) {
	c, server := publicationClient(t)
	server.dropAfter = true
	if err := c.Publish(context.Background(), "@codex accepted but response lost"); !errors.Is(err, relay.ErrUnknown) {
		t.Fatalf("lost response = %v, want ErrUnknown", err)
	}
	server.dropAfter = false
	for _, text := range []string{"@codex held two", "@codex held three"} {
		if err := c.ReservePublication(text); err != nil {
			t.Fatal(err)
		}
	}
	server.queryUnknown = true
	if err := c.Reconcile(context.Background(), false); !errors.Is(err, relay.ErrUnknown) {
		t.Fatalf("unavailable receipt = %v, want ErrUnknown", err)
	}
	if server.reports != 1 || c.State.Pending == nil || c.State.Pending.Seq != 1 || !c.State.Pending.Unknown || len(c.State.Held) != 2 {
		t.Fatalf("uncertain head was replayed or overtaken: reports=%d state=%+v", server.reports, c.State)
	}
	// A fresh process sees the same backlog; the head is still queried first.
	fresh, err := load(c.Dir)
	if err != nil {
		t.Fatal(err)
	}
	server.queryUnknown = false
	if err := fresh.Reconcile(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(server.reported) != "[1 2 3]" || server.queries != 2 {
		t.Fatalf("reported=%v queries=%d; want accepted head never resent and held replies once in order", server.reported, server.queries)
	}
	if fresh.State.Pending != nil || fresh.State.Held != nil || fresh.State.LastConfirmedSeq != 3 || fresh.State.LastSeq != 3 {
		t.Fatalf("backlog not settled: %+v", fresh.State)
	}
}

// A crash after the Service accepted a promoted held reply but before the
// local write is recovered by the original key, not by sending it again.
func TestPromotedHeldReplyLostConfirmationIsQueriedNotResent(t *testing.T) {
	c, server := publicationClient(t)
	server.dropBefore = true
	_ = c.Publish(context.Background(), "@codex one")
	server.dropBefore = false
	if err := c.ReservePublication("@codex two"); err != nil {
		t.Fatal(err)
	}
	save := c.Save
	c.Save = func(s State) error {
		if s.LastConfirmedSeq == 2 {
			return errors.New("crash clearing seq 2")
		}
		return save(s)
	}
	if c.Reconcile(context.Background(), false) == nil {
		t.Fatal("confirmation failure ignored")
	}
	fresh, err := load(c.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Reconcile(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(server.reported) != "[1 1 2]" || fresh.State.LastConfirmedSeq != 2 || fresh.State.Pending != nil {
		t.Fatalf("reported=%v state=%+v; accepted seq 2 must not be sent twice", server.reported, fresh.State)
	}
}

func TestExplicitRecoveryAppliesToHeadOnly(t *testing.T) {
	t.Run("discard", func(t *testing.T) {
		c, server := publicationClient(t)
		server.dropBefore = true
		_ = c.Publish(context.Background(), "@codex abandoned")
		if err := c.ReservePublication("@codex held"); err != nil {
			t.Fatal(err)
		}
		if err := c.DiscardPending(); err != nil {
			t.Fatal(err)
		}
		if c.State.Pending == nil || c.State.Pending.Seq != 2 || c.State.Held != nil || c.State.LastSeq != 2 {
			t.Fatalf("discard did not promote the held reply: %+v", c.State)
		}
		server.dropBefore = false
		if err := c.Reconcile(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		// The discarded sequence stays consumed, so the Service observes a gap.
		if server.accepted[1] || !server.accepted[2] || c.State.LastConfirmedSeq != 2 {
			t.Fatalf("discard changed identity: accepted=%v state=%+v", server.accepted, c.State)
		}
	})
	t.Run("resend", func(t *testing.T) {
		c, server := publicationClient(t)
		server.dropBefore = true
		_ = c.Publish(context.Background(), "@codex uncertain")
		if err := c.ReservePublication("@codex held"); err != nil {
			t.Fatal(err)
		}
		server.dropBefore = false
		server.queryUnknown = true
		if err := c.Reconcile(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(server.reported) != "[1 1 2]" || c.State.LastConfirmedSeq != 2 || c.State.Pending != nil {
			t.Fatalf("reported=%v state=%+v; want head supplemented under seq 1, held sent once as seq 2", server.reported, c.State)
		}
	})
}

func TestLoadAcceptsSinglePendingStateFromEarlierCLI(t *testing.T) {
	c, server := publicationClient(t)
	// The exact shape earlier schema-2 CLIs wrote: one pending, no held list.
	legacy := fmt.Sprintf(`{"schema":2,"room":"room","slot":"slot1","runtime":"claude","workspace":%q,"endpoint_path":%q,"bind_id":"binding","generation":1,"session_id":"session","last_seq":4,"last_confirmed_seq":3,"pending":{"seq":4,"text":"@codex legacy pending","at":"2026-09-20T10:00:00Z","unknown":true},"blocks":0}`, c.State.Workspace, c.State.EndpointPath)
	if err := os.WriteFile(filepath.Join(c.Dir, "state.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	fresh, err := load(c.Dir)
	if err != nil {
		t.Fatalf("earlier single-pending state rejected: %v", err)
	}
	if fresh.State.Pending == nil || fresh.State.Pending.Seq != 4 || fresh.State.Held != nil {
		t.Fatalf("legacy pending misread: %+v", fresh.State)
	}
	if err := fresh.Reconcile(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if server.queries != 1 || fmt.Sprint(server.reported) != "[4]" || fresh.State.LastConfirmedSeq != 4 {
		t.Fatalf("legacy pending not reconciled by its original key: queries=%d reported=%v", server.queries, server.reported)
	}
}

func TestLoadRejectsMalformedPublicationBacklog(t *testing.T) {
	held := func(seqs ...uint64) []Pending {
		out := []Pending{}
		for _, seq := range seqs {
			out = append(out, Pending{Seq: seq, Text: "held"})
		}
		return out
	}
	cases := map[string]func(*State){
		"held without head":   func(s *State) { s.Pending = nil; s.Held = held(4) },
		"sequence gap":        func(s *State) { s.Held = held(5); s.LastSeq = 5 },
		"reordered":           func(s *State) { s.Held = held(5, 4); s.LastSeq = 5 },
		"stale watermark":     func(s *State) { s.Held = held(4, 5); s.LastSeq = 4 },
		"attempted held":      func(s *State) { s.Held = []Pending{{Seq: 4, Text: "held", Unknown: true}} },
		"beyond backlog cap":  func(s *State) { s.Held = held(4, 5, 6, 7, 8, 9, 10, 11); s.LastSeq = 11 },
		"oversized held body": func(s *State) { s.Held = []Pending{{Seq: 4, Text: strings.Repeat("x", relay.MaxBodyBytes+1)}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := publicationClient(t)
			s := c.State
			s.LastSeq, s.LastConfirmedSeq, s.Pending = 4, 2, &Pending{Seq: 3, Text: "head"}
			mutate(&s)
			if err := relay.AtomicJSON(filepath.Join(c.Dir, "state.json"), s); err != nil {
				t.Fatal(err)
			}
			if _, err := load(c.Dir); err == nil || !strings.Contains(err.Error(), "backlog") {
				t.Fatalf("malformed backlog accepted: %v", err)
			}
		})
	}
}

func TestReservePublicationRefusesBeyondBacklogBounds(t *testing.T) {
	c, _ := publicationClient(t)
	for i := 1; i <= maxPublicationBacklog; i++ {
		if err := c.ReservePublication(fmt.Sprintf("reply %d", i)); err != nil {
			t.Fatalf("reply %d within the backlog refused: %v", i, err)
		}
	}
	if err := c.ReservePublication("one too many"); !errors.Is(err, errPublicationBacklogFull) || c.State.LastSeq != maxPublicationBacklog {
		t.Fatalf("count cap = %v, last_seq %d", err, c.State.LastSeq)
	}
	// JSON escaping expands a control character sixfold, so a few maximal
	// bodies exceed the reader's limit. The byte guard refuses the reply
	// instead of writing a state file load would reject.
	c, _ = publicationClient(t)
	escaped := strings.Repeat(string(rune(1)), relay.MaxBodyBytes)
	for i := 0; ; i++ {
		err := c.ReservePublication(escaped)
		if errors.Is(err, errPublicationBacklogFull) {
			if i < 1 || i >= maxPublicationBacklog {
				t.Fatalf("byte guard triggered after %d replies", i)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := load(c.Dir); err != nil {
		t.Fatalf("saved backlog is no longer readable: %v", err)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestDeliveryAcknowledgesOnlyAfterCompleteStdout(t *testing.T) {
	c, server := publicationClient(t)
	for _, w := range []io.Writer{brokenWriter{}, shortWriter{}} {
		if deliver(context.Background(), c, false, 1, w) == nil {
			t.Fatal("stdout write failure ignored")
		}
		if server.acks != 0 {
			t.Fatal("ack preceded complete stdout")
		}
	}
	var output bytes.Buffer
	if err := deliver(context.Background(), c, false, 1, &output); err != nil {
		t.Fatal(err)
	}
	if server.acks != 1 || !strings.Contains(output.String(), "body") {
		t.Fatal("successful stdout not acknowledged")
	}
	output.Reset()
	if err := deliver(context.Background(), c, true, 1, &output); err != nil {
		t.Fatal(err)
	}
	var hook map[string]string
	if json.Unmarshal(output.Bytes(), &hook) != nil || hook["decision"] != "block" || !strings.Contains(hook["reason"], "body") {
		t.Fatal("hook continuation not valid JSON")
	}
	fresh, _ := load(c.Dir)
	if fresh.State.Blocks != 1 {
		t.Fatal("actual delivery did not consume block budget")
	}
	for _, secret := range []string{c.Secret, c.Endpoint.Token, "receipt"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("model stdout leaked %s", secret)
		}
	}
	files, _ := os.ReadDir(c.Dir)
	if len(files) != 2 {
		t.Fatalf("unexpected per-message/lock files: %+v", files)
	}
}

func TestLoadRejectsRetiredStateBeforeCredentials(t *testing.T) {
	dir := t.TempDir()
	state := State{Schema: 1, Room: "room", Slot: model.ActorSlot1, Runtime: model.RuntimeClaude, BindID: "bind", Generation: 1, SessionID: "session"}
	if err := relay.AtomicJSON(filepath.Join(dir, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	if _, err := load(dir); err == nil || !strings.Contains(err.Error(), "retired relay state format") {
		t.Fatalf("retired state load error = %v", err)
	}
}

func TestStateDiscoveryIgnoresLegacySlotDirectories(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, ".pairroom", "rooms", "room", "slots", "claude")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "state.json"), []byte("not inspected"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := statePaths(root)
	if err != nil || len(paths) != 0 {
		t.Fatalf("legacy state directory entered discovery: paths=%v err=%v", paths, err)
	}
}
func TestHookInstallPreservesUnrelatedSettingsAndRejectsMissingHooks(t *testing.T) {
	root := t.TempDir()
	if installed(root, model.RuntimeCodex) == nil {
		t.Fatal("zero-hook environment allowed")
	}
	if err := os.Mkdir(filepath.Join(root, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".claude", "settings.json")
	initial := `{"permissions":{"allow":["Read"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"unrelated hook","timeout":12}]}]}}`
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := editHooks(root, model.RuntimeClaude, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := installed(root, model.RuntimeClaude); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Count(string(data), hookCommand(model.RuntimeClaude)) != 2 || !strings.Contains(string(data), "unrelated hook") || !strings.Contains(string(data), `"permissions"`) {
		t.Fatal("installation duplicates or overwrites unrelated settings")
	}
	if err := editHooks(root, model.RuntimeClaude, true); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), hookCommand(model.RuntimeClaude)) || !strings.Contains(string(data), "unrelated hook") {
		t.Fatal("purge damaged unrelated hooks")
	}
}
