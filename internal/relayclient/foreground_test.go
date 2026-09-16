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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Real CLI parsing, workspace discovery, private state and HTTP transport;
// synthetic service replies, not vendor/model E2E or a second relay engine.
type foregroundFixtureOptions struct {
	failAction      string
	emptyInbox      bool
	invalidReceipt  bool
	incompleteClaim bool
	missingClaim    bool
	outgoingState   string
	summary         *relay.Summary
	peer            *relay.Binding
	envelope        string
	waitEntered     chan struct{}
	ackAfterWrite   *atomic.Bool
}

type foregroundFixture struct {
	args      []string
	statePath string
	mu        sync.Mutex
	calls     map[string]int
	messages  map[string]relay.Message
	waits     []int
	parks     []bool
}

func newForegroundFixture(t *testing.T, opts foregroundFixtureOptions) *foregroundFixture {
	t.Helper()
	IsolateNativeCaller(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if data, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, data)
	}
	f := &foregroundFixture{
		args:     []string{"--repo", root, "--room", "room", "--slot", "1"},
		calls:    map[string]int{},
		messages: map[string]relay.Message{},
	}
	peer := opts.peer
	if peer == nil {
		peer = &relay.Binding{Slot: model.ActorCodex, Runtime: model.RuntimeCodex, SessionID: "peer-session"}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/api/v1/relay/room/claude/") ||
			r.Header.Get("Authorization") != "Relay private-long-lived-secret" ||
			r.Header.Get("X-PairRoom-Bind") != "binding" ||
			r.Header.Get("X-PairRoom-Generation") != "1" ||
			r.Header.Get("X-PairRoom-Session") != "session" {
			t.Error("request lost its exact associated binding")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		action := filepath.Base(r.URL.Path)
		f.mu.Lock()
		f.calls[action]++
		f.mu.Unlock()
		if opts.failAction == action {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		switch action {
		case "send":
			var req relay.SendRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			f.mu.Lock()
			msg, exists := f.messages[req.ID]
			if !exists {
				state := opts.outgoingState
				if state == "" {
					state = "queued"
				}
				msg = relay.Message{ID: "outgoing-" + req.ID, From: model.ActorClaude, To: model.ActorCodex, Text: req.Text, State: state}
				f.messages[req.ID] = msg
			}
			f.mu.Unlock()
			if opts.invalidReceipt {
				_, _ = io.WriteString(w, `{}`)
				return
			}
			_ = json.NewEncoder(w).Encode(msg)
		case "wait":
			var req struct {
				Park    bool `json:"park"`
				Seconds int  `json:"timeout_seconds"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			f.mu.Lock()
			f.waits = append(f.waits, req.Seconds)
			f.parks = append(f.parks, req.Park)
			f.mu.Unlock()
			if opts.waitEntered != nil {
				close(opts.waitEntered)
				<-r.Context().Done()
				return
			}
			if opts.missingClaim {
				_, _ = io.WriteString(w, `{}`)
				return
			}
			if opts.emptyInbox {
				_, _ = io.WriteString(w, `{"claim":null}`)
				return
			}
			envelope := opts.envelope
			if envelope == "" {
				envelope = "[PairRoom message]\nfrom: @codex\n\nReview finding"
			}
			claim := relay.Claim{ID: "incoming", Receipt: "receipt", Envelope: envelope}
			if opts.incompleteClaim {
				claim.Receipt = ""
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"claim": claim})
		case "ack":
			if opts.ackAfterWrite != nil && !opts.ackAfterWrite.Load() {
				t.Error("ack preceded the complete stdout record")
			}
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req["id"] != "incoming" || req["receipt"] != "receipt" {
				t.Error("ack lost exact claim identity")
			}
			_, _ = io.WriteString(w, `{"handed_off":true}`)
		case "summary":
			if opts.summary == nil {
				t.Error("unexpected summary request")
				http.Error(w, "unexpected operation", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(opts.summary)
		case "peer":
			_ = json.NewEncoder(w).Encode(peer)
		default:
			t.Errorf("unexpected side effect: %s", action)
			http.Error(w, "unexpected operation", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	endpoint := filepath.Join(t.TempDir(), relay.EndpointFile)
	if err := relay.AtomicJSON(endpoint, relay.Endpoint{URL: srv.URL, Token: "management-secret-not-output"}); err != nil {
		t.Fatal(err)
	}
	dir, err := secureDir(root, ".pairroom", "rooms", "room", "slots", "claude")
	if err != nil {
		t.Fatal(err)
	}
	f.statePath = filepath.Join(dir, "state.json")
	state := State{Schema: 1, Room: "room", Slot: model.ActorClaude, Runtime: model.RuntimeClaude, Workspace: root, EndpointPath: endpoint, BindID: "binding", Generation: 1, SessionID: "session", Blocks: relay.MaxBlocks}
	if err := relay.AtomicJSON(f.statePath, state); err != nil {
		t.Fatal(err)
	}
	if err := relay.AtomicJSON(filepath.Join(dir, "credentials"), credentials{BindID: "binding", Secret: "private-long-lived-secret"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *foregroundFixture) count(action string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[action]
}

func (f *foregroundFixture) run(ctx context.Context, action string, in io.Reader, out, diagnostic io.Writer, args ...string) error {
	argv := append([]string{action}, f.args...)
	argv = append(argv, args...)
	return Run(ctx, argv, in, out, diagnostic)
}

func TestExchangeValidationBeforeWorkspaceOrPublication(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"exchange"}, "stable --id"},
		{[]string{"exchange", "--id", "unsafe;command"}, "stable --id"},
		{[]string{"exchange", "--id", strings.Repeat("a", 129)}, "stable --id"},
		{[]string{"exchange", "--id", "review-1", "--to", "@user"}, "peer only"},
		{[]string{"exchange", "--id", "review-1", "--timeout", "-1"}, "0–21600"},
		{[]string{"exchange", "--id", "review-1", "--timeout", "21601"}, "0–21600"},
		{[]string{"wait", "--timeout", "21601"}, "0–21600"},
	} {
		var out bytes.Buffer
		err := Run(context.Background(), append(tc.args, "--repo", filepath.Join(t.TempDir(), "absent")), strings.NewReader("proposal"), &out, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
			t.Fatalf("args=%v err=%v output=%q", tc.args, err, out.String())
		}
	}
}

type foregroundRecordWriter struct {
	bytes.Buffer
	written *atomic.Bool
}

func (w *foregroundRecordWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if err == nil && n == len(p) && strings.HasSuffix(string(p), "\n") {
		w.written.Store(true)
	}
	return n, err
}

func TestExchangeSendsOnceReturnsOnlyIncomingEnvelopeAndPreservesState(t *testing.T) {
	var written atomic.Bool
	f := newForegroundFixture(t, foregroundFixtureOptions{ackAfterWrite: &written})
	before, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	out := &foregroundRecordWriter{written: &written}
	var diagnostic bytes.Buffer
	err = f.run(context.Background(), "exchange", strings.NewReader("private proposal"), out, &diagnostic, "--id", "review-1")
	if err != nil || f.count("send") != 1 || f.count("wait") != 1 || f.count("ack") != 1 {
		t.Fatalf("exchange failed: %v", err)
	}
	if out.String() != "[PairRoom message]\nfrom: @codex\n\nReview finding\n" {
		t.Fatalf("stdout is not the exact next envelope: %q", out.String())
	}
	for _, forbidden := range []string{"private proposal", "private-long-lived-secret", "management-secret-not-output", "receipt"} {
		if strings.Contains(diagnostic.String(), forbidden) || strings.Contains(out.String(), forbidden) {
			t.Fatalf("leaked unnecessary context: %q", forbidden)
		}
	}
	if !strings.Contains(diagnostic.String(), "review-1") {
		t.Fatal("publication identity missing from receipt")
	}
	if !strings.Contains(diagnostic.String(), `"queued_delivery"`) || !strings.Contains(diagnostic.String(), "pairroom relay wait --room room --slot 2") || !strings.Contains(diagnostic.String(), `"wake_command"`) {
		t.Fatalf("queued exchange receipt omitted collection hint: %q", diagnostic.String())
	}
	f.mu.Lock()
	if len(f.waits) != 1 || f.waits[0] != 30 || f.parks[0] {
		t.Errorf("foreground wait altered the HTTP park contract: %v %v", f.waits, f.parks)
	}
	f.mu.Unlock()
	after, err := os.ReadFile(f.statePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("active exchange modified hook block/sequence/binding state")
	}
}

func TestForegroundWaitBoundsEachHTTPRequestAndPreservesShortWait(t *testing.T) {
	for _, seconds := range []string{"0", "1", "30", "31", "600", "1800", "3600", "21600"} {
		f := newForegroundFixture(t, foregroundFixtureOptions{})
		if err := f.run(context.Background(), "wait", strings.NewReader(""), io.Discard, io.Discard, "--timeout", seconds); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		if len(f.waits) != 1 || f.waits[0] < 1 || f.waits[0] > 30 || f.parks[0] || (seconds == "1" && f.waits[0] != 1) {
			t.Errorf("timeout=%s: polls=%v parks=%v", seconds, f.waits, f.parks)
		}
		f.mu.Unlock()
		if f.count("send") != 0 || f.count("ack") != 1 {
			t.Fatal("wait published or failed to acknowledge")
		}
	}
}

func TestExchangeReturnsUserSteeringWithoutPretendingItIsPeerReply(t *testing.T) {
	envelope := "[PairRoom message]\nfrom: @user\n\nStop review; I changed the requirement."
	f := newForegroundFixture(t, foregroundFixtureOptions{envelope: envelope})
	var out bytes.Buffer
	if err := f.run(context.Background(), "exchange", strings.NewReader("proposal"), &out, io.Discard, "--id", "review-1"); err != nil {
		t.Fatal(err)
	}
	if out.String() != envelope+"\n" || f.count("wait") != 1 {
		t.Fatal("FIFO user input was skipped or rewritten as a correlated reply")
	}
}

func TestExchangeStopsOnSendWaitAndAckFailures(t *testing.T) {
	for _, action := range []string{"send", "wait", "ack"} {
		t.Run(action, func(t *testing.T) {
			f := newForegroundFixture(t, foregroundFixtureOptions{failAction: action})
			var out, diagnostic bytes.Buffer
			err := f.run(context.Background(), "exchange", strings.NewReader("proposal"), &out, &diagnostic, "--id", "review-1")
			if err == nil || f.count("send") != 1 {
				t.Fatalf("failed operation retried or hidden: %v", err)
			}
			if action == "send" {
				if f.count("wait") != 0 || !strings.Contains(err.Error(), "SAME --id review-1") || diagnostic.Len() != 0 {
					t.Fatal("uncertain publication started collection or lost recovery identity")
				}
			} else if f.count("wait") != 1 || !strings.Contains(err.Error(), "publication outgoing-review-1 confirmed") {
				t.Fatal("collection failure was treated as publication failure")
			}
			if action == "ack" {
				if out.Len() == 0 || f.count("ack") != 1 || !strings.Contains(err.Error(), "never automatically replay") {
					t.Fatal("uncertain acknowledgement retried or hid stdout")
				}
			} else if out.Len() != 0 || f.count("ack") != 0 {
				t.Fatal("failure acknowledged a nonexistent delivery")
			}
		})
	}
}

func TestExchangeOutputFailureNeverAcknowledges(t *testing.T) {
	for _, out := range []io.Writer{brokenWriter{}, shortWriter{}} {
		f := newForegroundFixture(t, foregroundFixtureOptions{})
		if err := f.run(context.Background(), "exchange", strings.NewReader("proposal"), out, io.Discard, "--id", "review-1"); err == nil {
			t.Fatal("output failure ignored")
		}
		if f.count("send") != 1 || f.count("wait") != 1 || f.count("ack") != 0 {
			t.Fatal("partial output was acknowledged or retried")
		}
	}
}

func TestExchangeReceiptOutputFailureDoesNotResendOrClaim(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	err := f.run(context.Background(), "exchange", strings.NewReader("proposal"), io.Discard, brokenWriter{}, "--id", "review-1")
	if err == nil || !strings.Contains(err.Error(), "publication outgoing-review-1 confirmed") || f.count("send") != 1 || f.count("wait") != 0 {
		t.Fatalf("receipt failure lost publication state: %v", err)
	}
}

func TestExchangeTimeoutResumesCollectionNotPublication(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{emptyInbox: true})
	var out bytes.Buffer
	err := f.run(context.Background(), "exchange", strings.NewReader("proposal"), &out, io.Discard, "--id", "review-1", "--timeout", "1")
	if !errors.Is(err, errExchangeWaiting) || !strings.Contains(err.Error(), "pairroom relay wait --repo") || !strings.Contains(err.Error(), "not another send/exchange") {
		t.Fatalf("timeout recovery is unsafe: %v", err)
	}
	if out.Len() != 0 || f.count("send") != 1 || f.count("wait") != 1 || f.count("ack") != 0 {
		t.Fatal("empty timeout published/acknowledged or returned fake completion")
	}
	if err := f.run(context.Background(), "wait", strings.NewReader(""), &out, io.Discard, "--timeout", "1"); err != nil || f.count("send") != 1 {
		t.Fatalf("legacy empty wait no longer works: %v", err)
	}
}

func TestExchangeCancellationKeepsConfirmedPublicationAndDoesNotRetry(t *testing.T) {
	entered := make(chan struct{})
	f := newForegroundFixture(t, foregroundFixtureOptions{waitEntered: entered})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-entered:
			cancel()
		case <-ctx.Done():
		}
	}()
	err := f.run(ctx, "exchange", strings.NewReader("proposal"), io.Discard, io.Discard, "--id", "review-1")
	if err == nil || !strings.Contains(err.Error(), "publication outgoing-review-1 confirmed") || f.count("send") != 1 || f.count("wait") != 1 || f.count("ack") != 0 {
		t.Fatalf("cancelled exchange retried or lost publication outcome: %v", err)
	}
}

func TestExchangeRejectsMalformedReceiptsAndIncompleteClaims(t *testing.T) {
	for _, opts := range []foregroundFixtureOptions{{invalidReceipt: true}, {incompleteClaim: true}, {missingClaim: true}, {outgoingState: "unknown"}, {outgoingState: "cancelled"}} {
		f := newForegroundFixture(t, opts)
		var out bytes.Buffer
		if err := f.run(context.Background(), "exchange", strings.NewReader("proposal"), &out, io.Discard, "--id", "review-1"); err == nil {
			t.Fatal("invalid receipt accepted")
		}
		if f.count("ack") != 0 || out.Len() != 0 {
			t.Fatal("invalid receipt released or acknowledged a message")
		}
		if !opts.incompleteClaim && !opts.missingClaim && f.count("wait") != 0 {
			t.Fatal("unconfirmed or uncertain outgoing state started collection")
		}
	}
}

func TestExchangeStableIDCannotSilentlyPublishDifferentBody(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	if err := f.run(context.Background(), "exchange", strings.NewReader("v1"), io.Discard, io.Discard, "--id", "review-1"); err != nil {
		t.Fatal(err)
	}
	err := f.run(context.Background(), "exchange", strings.NewReader("v2"), io.Discard, io.Discard, "--id", "review-1")
	if err == nil || !strings.Contains(err.Error(), "different body") || f.count("send") != 2 || f.count("wait") != 1 {
		t.Fatalf("reused publication silently changed meaning: %v", err)
	}
}

func TestStopHookPollLimitIsUnchanged(t *testing.T) {
	var out bytes.Buffer
	if err := deliver(context.Background(), nil, true, 31, &out); err == nil || out.Len() != 0 {
		t.Fatal("foreground budget leaked into the Stop hook")
	}
}

func TestLegacySendRemainsReceiptOnly(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	var out bytes.Buffer
	if err := f.run(context.Background(), "send", strings.NewReader("proposal"), &out, io.Discard, "--id", "review-1"); err != nil {
		t.Fatal(err)
	}
	var msg relay.Message
	if err := json.Unmarshal(out.Bytes(), &msg); err != nil || msg.Text != "proposal" || f.count("send") != 1 || f.count("wait") != 0 || f.count("ack") != 0 {
		t.Fatalf("send acquired exchange semantics: msg=%+v err=%v", msg, err)
	}
}

func TestSendQueuedReceiptAddsPeerCollectionHint(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	var out, diagnostic bytes.Buffer
	if err := f.run(context.Background(), "send", strings.NewReader("proposal"), &out, &diagnostic, "--id", "review-1"); err != nil {
		t.Fatal(err)
	}
	var result struct {
		QueuedDelivery *queuedDeliveryHint `json:"queued_delivery"`
	}
	if err := json.Unmarshal(diagnostic.Bytes(), &result); err != nil || result.QueuedDelivery == nil || !strings.Contains(result.QueuedDelivery.Notice, "not handed off") || result.QueuedDelivery.Command != "pairroom relay wait --room room --slot 2" || result.QueuedDelivery.WakeCommand != codexWakeCommand(model.RuntimeCodex, "peer-session") {
		t.Fatalf("queued send hint = %+v, err=%v", result, err)
	}
	if strings.Contains(diagnostic.String(), "proposal") {
		t.Fatalf("queued send hint exposed body: %q", diagnostic.String())
	}
	if !strings.Contains(result.QueuedDelivery.WakeCommand, codexWakeNudge) {
		t.Fatalf("wake command lost fixed nudge: %q", result.QueuedDelivery.WakeCommand)
	}
}

func TestSendQueuedClaudePeerOmitsWakeCommand(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{peer: &relay.Binding{Slot: model.ActorCodex, Runtime: model.RuntimeClaude, SessionID: "peer-session"}})
	var out, diagnostic bytes.Buffer
	if err := f.run(context.Background(), "send", strings.NewReader("proposal"), &out, &diagnostic, "--id", "review-1"); err != nil {
		t.Fatal(err)
	}
	var result struct {
		QueuedDelivery *queuedDeliveryHint `json:"queued_delivery"`
	}
	if err := json.Unmarshal(diagnostic.Bytes(), &result); err != nil || result.QueuedDelivery == nil || result.QueuedDelivery.WakeCommand != "" {
		t.Fatalf("non-Codex peer exposed wake command: %+v, err=%v", result, err)
	}
}

func TestSendQueuedPeerLookupFailureOmitsWakeCommand(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{failAction: "peer"})
	var out, diagnostic bytes.Buffer
	if err := f.run(context.Background(), "send", strings.NewReader("proposal"), &out, &diagnostic, "--id", "review-1"); err != nil {
		t.Fatal(err)
	}
	var result struct {
		QueuedDelivery *queuedDeliveryHint `json:"queued_delivery"`
	}
	if err := json.Unmarshal(diagnostic.Bytes(), &result); err != nil || result.QueuedDelivery == nil || result.QueuedDelivery.WakeCommand != "" || f.count("send") != 1 {
		t.Fatalf("failed optional peer lookup changed send result: %+v, err=%v", result, err)
	}
}

func TestSendHandedOffReceiptOmitsQueuedHint(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{outgoingState: "handed_off"})
	var out, diagnostic bytes.Buffer
	if err := f.run(context.Background(), "send", strings.NewReader("proposal"), &out, &diagnostic, "--id", "review-1"); err != nil {
		t.Fatal(err)
	}
	if diagnostic.Len() != 0 {
		t.Fatalf("non-queued send emitted a collection hint: %q", diagnostic.String())
	}
}

func TestBriefStatusAddsHintsOnlyForQueuedInboxes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		summary relay.Summary
		want    int
	}{
		{
			name: "queued self and peer",
			summary: relay.Summary{Inboxes: map[model.ActorID]relay.InboxSummary{
				model.ActorClaude: {Queued: 2},
				model.ActorCodex:  {Queued: 1, Delivering: 3},
			}},
			want: 2,
		},
		{
			name: "delivering only",
			summary: relay.Summary{Inboxes: map[model.ActorID]relay.InboxSummary{
				model.ActorCodex: {Delivering: 1},
			}},
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newForegroundFixture(t, foregroundFixtureOptions{summary: &tc.summary})
			var out bytes.Buffer
			if err := f.run(context.Background(), "status", strings.NewReader(""), &out, io.Discard, "--brief"); err != nil {
				t.Fatal(err)
			}
			var result struct {
				QueuedInboxHints []queuedInboxHint `json:"queued_inbox_hints"`
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.QueuedInboxHints) != tc.want {
				t.Fatalf("status hints = %+v, err=%v", result.QueuedInboxHints, err)
			}
			if tc.want == 2 {
				if result.QueuedInboxHints[0].Command != "pairroom relay wait --room room --slot 1" || result.QueuedInboxHints[0].WakeCommand != "" || result.QueuedInboxHints[1].Command != "pairroom relay wait --room room --slot 2" || result.QueuedInboxHints[1].WakeCommand != codexWakeCommand(model.RuntimeCodex, "peer-session") || !strings.Contains(result.QueuedInboxHints[1].Notice, "peer's associated native session") {
					t.Fatalf("status hints are not actionable: %+v", result.QueuedInboxHints)
				}
			}
		})
	}
}

func TestExchangeTextArgumentUsesTheSamePublicationPath(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	var out bytes.Buffer
	if err := f.run(context.Background(), "exchange", strings.NewReader("unused"), &out, io.Discard, "--id", "review-1", "--text", "explicit proposal"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.messages["review-1"].Text != "explicit proposal" || len(f.messages) != 1 {
		t.Fatal("exchange lost explicit body")
	}
}
