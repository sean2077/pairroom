package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	s := State{Schema: 1, Room: "room", Slot: model.ActorClaude, Runtime: model.RuntimeClaude, Workspace: dir, EndpointPath: endpointPath, BindID: "binding", Generation: 1, SessionID: "session"}
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
		http.Error(w, "unauthorized", 401)
		return
	}
	switch filepath.Base(r.URL.Path) {
	case "publication":
		s.queries++
		if s.queryUnknown {
			http.Error(w, "unavailable", 503)
			return
		}
		if s.queryMalformed {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"accepted": s.accepted[input.Seq]})
	case "report":
		s.reports++
		if s.dropBefore {
			http.Error(w, "before acceptance", 503)
			return
		}
		s.accepted[input.Seq] = true
		if s.dropAfter {
			http.Error(w, "response lost", 503)
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
