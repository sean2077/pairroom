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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
)

// The real append-only relay backs this narrow local wire fixture. It tests
// CLI journals and runtime/session cutovers without starting a vendor agent.
type dynamicCLIWire struct {
	root, room, endpoint           string
	engine                         *relay.Engine
	dropBind, dropReport, dropSend atomic.Bool
	reports, queries               atomic.Int64
}

func newDynamicCLIWire(t *testing.T) *dynamicCLIWire {
	t.Helper()
	IsolateNativeCaller(t)
	return newNativeCLIWireAt(t, sessionGitRoot(t), "dynamic-room", true)
}

// Several Rooms/workspaces in one test deliberately share the same per-user
// private history, as one official native session does during a rebind.
func newNativeCLIWireAt(t *testing.T, root, room string, dynamic bool) *dynamicCLIWire {
	t.Helper()
	f := &dynamicCLIWire{root: root, room: room}
	log, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtimes := map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}
	if dynamic {
		runtimes = nil
	}
	f.engine, err = relay.Open(relay.Config{RoomID: room, Store: log, DynamicBindings: dynamic, Runtimes: runtimes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.engine.Close() })
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
		if err := editHooks(f.root, kind, false); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/service", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(serviceSnapshot{Projects: []serviceProject{{ID: "project", Root: f.root}}, Rooms: []serviceRoom{{ID: room, ProjectID: "project", HostMode: model.HostNative, Lifecycle: roomLifecycleActive, DynamicBindings: dynamic, Agents: map[model.ActorID]model.AgentSelection{model.ActorSlot1: {Runtime: model.RuntimeClaude}, model.ActorSlot2: {Runtime: model.RuntimeCodex}}}}})
	})
	bindHandler := func(w http.ResponseWriter, r *http.Request) {
		var req relay.BindRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad bind", 400)
			return
		}
		binding, replaced, err := f.engine.BindReport(model.ActorID(r.PathValue("slot")), req)
		if err != nil {
			dynamicWireResult(w, nil, err)
			return
		}
		if f.dropBind.Swap(false) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		bootstrap, generation, err := f.engine.BootstrapContextForBinding(binding)
		dynamicWireResult(w, map[string]any{"binding": binding, "replaced": replaced, "bootstrap": bootstrap, "dynamic_bindings": dynamic, "peer_generation": generation}, err)
	}
	mux.HandleFunc("POST /api/v1/rooms/"+room+"/native-bindings", bindHandler)
	mux.HandleFunc("POST /api/v1/rooms/"+room+"/native-bindings/{slot}", bindHandler)
	mux.HandleFunc("POST /api/v1/relay/"+room+"/{slot}/{action}", func(w http.ResponseWriter, r *http.Request) {
		generation, _ := strconv.ParseUint(r.Header.Get("X-PairRoom-Generation"), 10, 64)
		auth := relay.Auth{Slot: model.ActorID(r.PathValue("slot")), Runtime: model.RuntimeKind(r.Header.Get("X-PairRoom-Runtime")), BindID: r.Header.Get("X-PairRoom-Bind"), Generation: generation, SessionID: r.Header.Get("X-PairRoom-Session"), Secret: strings.TrimPrefix(r.Header.Get("Authorization"), "Relay ")}
		var req struct {
			ReportSeq              uint64        `json:"report_seq"`
			Text                   string        `json:"text"`
			ExpectedPeerGeneration uint64        `json:"expected_peer_generation"`
			ID                     string        `json:"id"`
			Receipt                string        `json:"receipt"`
			To                     model.ActorID `json:"to"`
			Park                   bool          `json:"park"`
			Timeout                int           `json:"timeout_seconds"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		var value any
		var err error
		switch r.PathValue("action") {
		case "inspect":
			value, err = f.engine.Inspect(auth)
		case "summary":
			value, err = f.engine.AuthSummary(auth)
		case "peer":
			value, err = f.engine.Peer(auth)
		case "status":
			value, err = f.engine.AuthSnapshot(auth)
		case "send":
			value, err = f.engine.Send(auth, relay.SendRequest{ID: req.ID, Text: req.Text, To: req.To, ExpectedPeerGeneration: req.ExpectedPeerGeneration})
			if err == nil && f.dropSend.Swap(false) {
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
		case "unbind":
			err = f.engine.UnbindAs(auth)
			value = map[string]bool{"unbound": err == nil}
		case "report":
			f.reports.Add(1)
			value, err = f.engine.Report(auth, req.ReportSeq, req.Text, req.ExpectedPeerGeneration)
			if err == nil && f.dropReport.Swap(false) {
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
		case "publication":
			f.queries.Add(1)
			var p relay.Publication
			var accepted bool
			p, accepted, err = f.engine.Publication(auth, req.ReportSeq)
			value = map[string]any{"accepted": accepted, "publication": p}
		case "wait":
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			var claim *relay.Claim
			claim, err = f.engine.Claim(ctx, auth, req.Park)
			if errors.Is(err, context.DeadlineExceeded) {
				err = nil
			}
			value = map[string]any{"claim": claim}
		case "ack":
			err = f.engine.Ack(auth, req.ID, req.Receipt)
			value = map[string]bool{"handed_off": err == nil}
		default:
			t.Errorf("unexpected dynamic fixture action %s", r.PathValue("action"))
			http.Error(w, "unused", 404)
			return
		}
		dynamicWireResult(w, value, err)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	f.endpoint = filepath.Join(t.TempDir(), relay.EndpointFile)
	if err := relay.AtomicJSON(f.endpoint, relay.Endpoint{URL: server.URL, Token: "test-token"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func dynamicWireResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		code := ""
		if errors.Is(err, relay.ErrPeerChanged) {
			code = relay.PeerChangedCode
		}
		if errors.Is(err, relay.ErrSendPayloadConflict) {
			code = relay.SendPayloadConflictCode
		}
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error(), "code": code})
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

func (f *dynamicCLIWire) run(t *testing.T, kind model.RuntimeKind, session string, args ...string) ([]byte, error) {
	t.Helper()
	for _, env := range sessionEnvVars {
		t.Setenv(env, "")
	}
	t.Setenv(sessionEnvVars[kind], session)
	args = append(args, "--repo", f.root)
	if len(args) > 0 && args[0] == "bind" {
		args = append(args, "--room", f.room, "--service-file", f.endpoint)
	}
	var out bytes.Buffer
	err := Run(context.Background(), args, bytes.NewReader(nil), &out, io.Discard)
	return out.Bytes(), err
}

func (f *dynamicCLIWire) state(t *testing.T, slot model.ActorID) *Client {
	t.Helper()
	c, err := load(filepath.Join(f.root, ".pairroom", "rooms", f.room, "slots", string(slot)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.close)
	return c
}

func TestDynamicAutoBindLostResponseRecoversItsOriginalVacancy(t *testing.T) {
	f := newDynamicCLIWire(t)
	f.dropBind.Store(true)
	if _, err := f.run(t, model.RuntimeCodex, "first", "bind"); err == nil {
		t.Fatal("lost response reported success")
	}
	attemptPath := filepath.Join(f.root, ".pairroom", "rooms", "dynamic-room", "bind-attempts", autoBindKey(model.RuntimeCodex, "first"), bindAttemptFile)
	var attempt bindAttempt
	if err := readPrivate(attemptPath, &attempt); err != nil || attempt.State.Slot != "" || attempt.State.Runtime != model.RuntimeCodex {
		t.Fatalf("unassigned attempt missing: %+v %v", attempt, err)
	}
	if _, err := f.run(t, model.RuntimeCodex, "first", "bind"); err != nil {
		t.Fatal(err)
	}
	first := f.state(t, model.ActorSlot1)
	if first.State.BindID != attempt.State.BindID || first.State.Generation != 1 || first.State.Runtime != model.RuntimeCodex || first.State.AutomaticPeerGeneration != 1 || f.engine.Snapshot().Bindings[model.ActorSlot2].Active {
		t.Fatal("retry changed seat or binding generation")
	}
	if _, err := os.Stat(attemptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed auto attempt retained: %v", err)
	}
	if _, err := f.run(t, model.RuntimeCodex, "second", "bind"); err != nil {
		t.Fatalf("same-runtime distinct session: %v", err)
	}
	if got := f.state(t, model.ActorSlot2).State; got.Runtime != model.RuntimeCodex || got.SessionID != "second" {
		t.Fatal("second caller identity guessed from slot")
	}
	if _, err := f.run(t, model.RuntimeGrok, "third", "bind"); err == nil {
		t.Fatal("third auto join displaced an active binding")
	}
	if _, err := f.run(t, model.RuntimeGrok, "third", "bind", "--replace"); err == nil || !strings.Contains(err.Error(), "--slot") {
		t.Fatal("automatic replace did not require a seat")
	}
	if _, err := f.run(t, model.RuntimeGrok, "third", "bind", "--slot", "2", "--replace"); err != nil {
		t.Fatalf("runtime replacement: %v", err)
	}
	replacement := f.state(t, model.ActorSlot2).State
	if replacement.Runtime != model.RuntimeGrok || replacement.Generation != 2 || replacement.SessionID != "third" {
		t.Fatalf("replacement identity: %+v", replacement)
	}
	if _, err := f.engine.Inspect(first.localAuth()); err != nil {
		t.Fatal("replacing the peer revoked the unchanged source")
	}
}

func TestDynamicLateStopNeverUsesAReplacementClaimGeneration(t *testing.T) {
	f := newDynamicCLIWire(t)
	if _, err := f.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeCodex, "old-peer", "bind"); err != nil {
		t.Fatal(err)
	}
	oldPeer := f.state(t, model.ActorSlot2)
	if _, err := f.run(t, model.RuntimeGrok, "new-peer", "bind", "--slot", "2", "--replace"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Inspect(oldPeer.localAuth()); !errors.Is(err, relay.ErrAuth) {
		t.Fatal("replaced credentials remained valid")
	}
	if _, err := f.run(t, model.RuntimeGrok, "new-peer", "send", "--id", "new-context", "--text", "a new peer request"); err != nil {
		t.Fatal(err)
	}
	output, err := f.run(t, model.RuntimeClaude, "source", "wait", "--timeout", "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"from: @agent2", "grok, generation 2", "peer: @agent2, generation 2", "Automatic publication is paused"} {
		if !strings.Contains(string(output), fragment) {
			t.Fatalf("new claim context omitted %q: %s", fragment, output)
		}
	}
	hook := HookInput{Event: "Stop", SessionID: "source", CWD: f.root}
	body := "a delayed old response @agent2"
	hook.LastAssistantMessage = &body
	input, _ := json.Marshal(hook)
	var out, diagnostic bytes.Buffer
	if err := Run(context.Background(), []string{"hook", "--runtime", "claude"}, bytes.NewReader(input), &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	state := f.state(t, model.ActorSlot1).State
	if !state.AutomaticPaused || state.AutomaticPeerGeneration != 1 || state.PeerGeneration != 2 || state.Pending == nil || state.Pending.Text != body || state.Pending.ExpectedPeerGeneration != 1 || state.Pending.Outcome != relay.PeerChangedCode || state.Pending.Unknown {
		t.Fatalf("late response target or journal changed: %+v", state)
	}
	if len(f.engine.Snapshot().Messages) != 1 {
		t.Fatal("late response reached successor inbox")
	}
	if _, err := f.run(t, model.RuntimeClaude, "source", "send", "--id", "implicit", "--text", body); err == nil {
		t.Fatal("paused source published implicitly")
	}
	if _, err := f.run(t, model.RuntimeClaude, "source", "send", "--id", "wrong-generation", "--peer-generation", "1", "--text", body); err == nil || !strings.Contains(err.Error(), "nothing was published") {
		t.Fatalf("stale explicit target was not definite rejection: %v", err)
	}
	if _, err := f.run(t, model.RuntimeClaude, "source", "send", "--id", "explicit-decision", "--peer-generation", "2", "--text", body); err != nil {
		t.Fatal(err)
	}
	if len(f.engine.Snapshot().Messages) != 2 {
		t.Fatal("intentional new-generation send missing")
	}
	reports := f.reports.Load()
	if err := f.state(t, model.ActorSlot1).Reconcile(context.Background(), true); !errors.Is(err, relay.ErrPeerChanged) {
		t.Fatalf("force retargeted terminal WAL: %v", err)
	}
	if f.reports.Load() != reports {
		t.Fatal("terminal peer_changed was automatically replayed")
	}
}

func TestDynamicAcceptedLostResponseReconcilesBeforePeerReplacement(t *testing.T) {
	f := newDynamicCLIWire(t)
	if _, err := f.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeCodex, "old-peer", "bind"); err != nil {
		t.Fatal(err)
	}
	c := f.state(t, model.ActorSlot1)
	if err := c.ReservePublication("original accepted response @agent2"); err != nil {
		t.Fatal(err)
	}
	f.dropReport.Store(true)
	if err := c.Reconcile(context.Background(), false); !errors.Is(err, relay.ErrUnknown) {
		t.Fatalf("lost response: %v", err)
	}
	if _, err := f.run(t, model.RuntimeGrok, "new-peer", "bind", "--slot", "2", "--replace"); err != nil {
		t.Fatal(err)
	}
	c = f.state(t, model.ActorSlot1)
	if err := c.Reconcile(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if c.State.Pending != nil || c.State.LastConfirmedSeq != 1 || f.reports.Load() != 1 || f.queries.Load() != 1 {
		t.Fatal("accepted original was resubmitted or not settled")
	}
	messages := f.engine.Snapshot().Messages
	if len(messages) != 1 || messages[0].TargetGeneration != 1 {
		t.Fatal("recovery retargeted the accepted historical message")
	}
}

func TestDynamicBindSameSessionDoesNotResetResponseFence(t *testing.T) {
	f := newDynamicCLIWire(t)
	if _, err := f.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeCodex, "old-peer", "bind"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeGrok, "new-peer", "bind", "--slot", "2", "--replace"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeClaude, "source", "bind", "--slot", "1", "--replace"); err != nil {
		t.Fatal(err)
	}
	state := f.state(t, model.ActorSlot1).State
	if state.Generation != 2 || state.AutomaticPeerGeneration != 1 || !state.AutomaticPaused {
		t.Fatalf("same-session rotation authorized old Stops for a new peer: %+v", state)
	}
	if _, err := f.run(t, model.RuntimeClaude, "fresh-source", "bind", "--slot", "1", "--replace"); err != nil {
		t.Fatal(err)
	}
	state = f.state(t, model.ActorSlot1).State
	if state.Generation != 3 || state.AutomaticPeerGeneration != 2 || state.AutomaticPaused {
		t.Fatalf("fresh session did not establish its own context: %+v", state)
	}
}

func TestDynamicUnbindRebindSameSessionRetainsOriginalContextAndBody(t *testing.T) {
	f := newDynamicCLIWire(t)
	if _, err := f.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeCodex, "old-peer", "bind"); err != nil {
		t.Fatal(err)
	}
	original := f.state(t, model.ActorSlot1)
	const body = "an unfinished response @agent2"
	if err := original.ReservePublication(body); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeClaude, "source", "unbind"); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(f.root, ".pairroom", "retired", original.State.BindID)
	var archive State
	if err := readPrivate(filepath.Join(retired, "state.json"), &archive); err != nil || archive.Pending == nil || archive.Pending.Text != body || archive.Pending.ExpectedPeerGeneration != 1 {
		t.Fatalf("unbind lost original private publication: %+v %v", archive.Pending, err)
	}
	if _, err := os.Stat(filepath.Join(retired, "credentials")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retirement retained an unnecessary local secret: %v", err)
	}
	// Restoring only the workspace must still recover the old fence from its
	// retired evidence instead of treating the live native session as new.
	historyDir, err := responseHistoryDirectory(original.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(historyDir, "context.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeGrok, "new-peer", "bind", "--slot", "2", "--replace"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
		t.Fatal(err)
	}
	rebound := f.state(t, model.ActorSlot1)
	if rebound.State.Generation != 2 || rebound.State.AutomaticPeerGeneration != 1 || !rebound.State.AutomaticPaused || rebound.State.AutomaticScopeChanged {
		t.Fatalf("same-scope rebind reset the original fence: %+v", rebound.State)
	}
	if err := rebound.ReservePublication(body); err != nil {
		t.Fatal(err)
	}
	if err := rebound.Reconcile(context.Background(), false); !errors.Is(err, relay.ErrPeerChanged) || rebound.State.Pending == nil || rebound.State.Pending.Text != body || rebound.State.Pending.ExpectedPeerGeneration != 1 {
		t.Fatalf("late rebound Stop was retargeted: %+v %v", rebound.State.Pending, err)
	}
	if len(f.engine.Snapshot().Messages) != 0 {
		t.Fatal("rebound old response reached the new peer")
	}
}

func TestDynamicUnbindRebindAnotherRoomRetainsAmbiguousResponseLocally(t *testing.T) {
	for _, separateWorkspace := range []bool{false, true} {
		name := "same-workspace"
		if separateWorkspace {
			name = "different-workspace"
		}
		t.Run(name, func(t *testing.T) {
			f := newDynamicCLIWire(t)
			if _, err := f.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.run(t, model.RuntimeClaude, "source", "unbind"); err != nil {
				t.Fatal(err)
			}
			root := f.root
			if separateWorkspace {
				root = sessionGitRoot(t)
			}
			next := newNativeCLIWireAt(t, root, "another-room", true)
			if _, err := next.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
				t.Fatal(err)
			}
			c := next.state(t, model.ActorSlot1)
			if !c.State.AutomaticPaused || !c.State.AutomaticScopeChanged {
				t.Fatalf("same-generation different Room reset automatic context: %+v", c.State)
			}
			const body = "late output for the previous Room @agent2"
			if err := c.ReservePublication(body); err != nil {
				t.Fatal(err)
			}
			if err := c.Reconcile(context.Background(), true); !errors.Is(err, errResponseScopeChanged) || c.State.Pending == nil || c.State.Pending.Text != body || c.State.Pending.Outcome != "context_changed" {
				t.Fatalf("cross-Room late output was not retained: %+v %v", c.State.Pending, err)
			}
			if next.reports.Load() != 0 || len(next.engine.Snapshot().Messages) != 0 {
				t.Fatal("unprovable response scope contacted the successor Room")
			}
			if _, err := next.run(t, model.RuntimeClaude, "source", "send", "--id", "deliberate", "--peer-generation", "1", "--text", body); err != nil {
				t.Fatalf("explicit recovery failed: %v", err)
			}
		})
	}
}

func TestLegacyUnbindThenDynamicBindDoesNotAuthorizeOldResponse(t *testing.T) {
	IsolateNativeCaller(t)
	f := newNativeCLIWireAt(t, sessionGitRoot(t), "legacy-room", false)
	if _, err := f.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, model.ActorSlot1).State; got.DynamicBindings || got.AutomaticPaused {
		t.Fatal("recording response history changed legacy relay behavior")
	}
	if _, err := f.run(t, model.RuntimeClaude, "source", "unbind"); err != nil {
		t.Fatal(err)
	}
	next := newNativeCLIWireAt(t, f.root, "dynamic-room", true)
	if _, err := next.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
		t.Fatal(err)
	}
	c := next.state(t, model.ActorSlot1)
	const body = "legacy output discussing @agent2"
	if !c.State.AutomaticScopeChanged || !c.State.AutomaticPaused {
		t.Fatal("v8 session was treated as having a fresh v9 response boundary")
	}
	if err := c.ReservePublication(body); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background(), false); !errors.Is(err, errResponseScopeChanged) || c.State.Pending == nil || c.State.Pending.Text != body || c.State.Pending.Outcome != "context_changed" || next.reports.Load() != 0 {
		t.Fatalf("legacy output acquired new stable-handle meaning: %+v %v", c.State.Pending, err)
	}
}

func TestDynamicLostSendReceiptRecoversOriginalGenerationAfterPause(t *testing.T) {
	f := newDynamicCLIWire(t)
	if _, err := f.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeCodex, "old-peer", "bind"); err != nil {
		t.Fatal(err)
	}
	f.dropSend.Store(true)
	_, err := f.run(t, model.RuntimeClaude, "source", "send", "--id", "original", "--text", "original body")
	if err == nil || !strings.Contains(err.Error(), "SAME --id original --peer-generation 1") {
		t.Fatalf("lost receipt omitted its original generation: %v", err)
	}
	if _, err := f.run(t, model.RuntimeGrok, "new-peer", "bind", "--slot", "2", "--replace"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeClaude, "source", "peer"); err != nil {
		t.Fatal(err)
	}
	if !f.state(t, model.ActorSlot1).State.AutomaticPaused {
		t.Fatal("peer observation did not pause automatic publication")
	}
	if _, err := f.run(t, model.RuntimeClaude, "source", "send", "--id", "original", "--peer-generation", "1", "--text", "original body"); err != nil {
		t.Fatalf("accepted original receipt became inaccessible after peer change: %v", err)
	}
	if _, err := f.run(t, model.RuntimeClaude, "source", "send", "--id", "original", "--peer-generation", "2", "--text", "original body"); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("same ID was retargeted to the replacement: %v", err)
	}
	if messages := f.engine.Snapshot().Messages; len(messages) != 1 || messages[0].TargetGeneration != 1 {
		t.Fatal("receipt recovery changed the accepted message")
	}
}

func TestDynamicSendIDConflictNamesPeerGeneration(t *testing.T) {
	f := newDynamicCLIWire(t)
	if _, err := f.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeCodex, "peer", "bind"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, model.RuntimeClaude, "source", "send", "--id", "conflict", "--peer-generation", "1", "--text", "original body"); err != nil {
		t.Fatal(err)
	}
	// Only the peer generation changes; the body stays identical.
	_, err := f.run(t, model.RuntimeClaude, "source", "send", "--id", "conflict", "--peer-generation", "2", "--text", "original body")
	if err == nil || !strings.Contains(err.Error(), "--id conflict") || !strings.Contains(err.Error(), "peer generation") || !strings.Contains(err.Error(), "nothing new was published") {
		t.Fatalf("duplicate --id conflict did not name the peer generation: %v", err)
	}
	if messages := f.engine.Snapshot().Messages; len(messages) != 1 || messages[0].TargetGeneration != 1 {
		t.Fatal("conflicting retry published a second message")
	}
}
