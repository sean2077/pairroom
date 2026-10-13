package relayclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseclient"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/review"
)

// Real CLI parsing, native locators, private files, TLS and stdout boundary;
// deterministic server responses, not SQL/OIDC or vendor/model acceptance.
type enterpriseCLIFixture struct {
	t                                       *testing.T
	root, slotDir, deviceDir, recordDir, ca string
	server                                  *httptest.Server
	state                                   State
	grantToken                              string
	mu                                      sync.Mutex
	operations                              map[string]relay.EnterpriseOperationResult
	intents                                 map[string]relay.EnterpriseOperationIntent
	calls                                   map[string]int
	peerGeneration                          uint64
	queued, claimed                         bool
	drop                                    relay.EnterpriseOperationKind
	stdoutComplete                          *atomic.Bool
	bindingRequest                          map[string]json.RawMessage
	bindingInfo                             enterpriseclient.BindingRequestInfo
	bindingApproved                         bool
	extraBinding                            *model.EnterpriseBinding
	extraTokenHash, extraGrantID            string
	objectHandler                           func(http.ResponseWriter, *http.Request) bool
	attachmentMetadata                      []model.Attachment
	attachmentIncoming                      bool
	historyQueries                          []relay.HistoryQuery
}

func enterpriseFixtureSecret(id string) string {
	sum := sha256.Sum256([]byte(id))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func newEnterpriseCLIFixture(t *testing.T) *enterpriseCLIFixture {
	t.Helper()
	IsolateNativeCaller(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "enterprise-native-session")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	if err := editHooks(root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	f := &enterpriseCLIFixture{t: t, root: root, operations: map[string]relay.EnterpriseOperationResult{}, intents: map[string]relay.EnterpriseOperationIntent{}, calls: map[string]int{}, peerGeneration: 1, grantToken: enterpriseFixtureSecret("narrow-grant")}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.ca = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(f.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	parent, err := enterpriseclient.New(enterpriseclient.Config{Origin: f.server.URL, DeploymentID: "dep-test", Directory: filepath.Join(t.TempDir(), "device"), CAFile: f.ca})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	f.deviceDir = parent.Directory()
	now := time.Now().UTC()
	device := map[string]any{"version": 1, "origin": f.server.URL, "deployment_id": "dep-test", "revision": 1, "device_id": "device-test", "user_id": "user-test", "proof": enterpriseFixtureSecret("device-proof"), "active": map[string]any{"grant_id": "device-access", "access": enterpriseFixtureSecret("access"), "refresh": enterpriseFixtureSecret("refresh"), "expires_at": now.Add(time.Hour), "refresh_expires_at": now.Add(24 * time.Hour)}}
	if err := privatefile.WriteJSON(filepath.Join(f.deviceDir, "device.json"), device); err != nil {
		t.Fatal(err)
	}
	route := EnterpriseTransport{Origin: f.server.URL, DeploymentID: "dep-test", UserID: "user-test", DeviceID: "device-test", AgentID: "agent-test", RoomID: "server-room", RecordID: "record-test", Directory: f.deviceDir, CAFile: f.ca}
	f.state = State{Schema: 4, Enterprise: &route, Room: enterpriseRoutingID(route.Origin, route.DeploymentID, route.RoomID), Slot: model.ActorSlot1, Runtime: model.RuntimeClaude, Workspace: root, BindID: "binding-test", Generation: 1, DynamicBindings: true, AutomaticPeerGeneration: 1, PeerGeneration: 1, SessionID: "enterprise-native-session"}
	f.slotDir, err = secureLANJoinDir(root, "rooms", f.state.Room, "slots", "slot1")
	if err != nil {
		t.Fatal(err)
	}
	local := credentials{BindID: f.state.BindID, Secret: "private-local-relay-capability"}
	if err := privatefile.WriteJSON(filepath.Join(f.slotDir, "state.json"), f.state); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.WriteJSON(filepath.Join(f.slotDir, "credentials"), local); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(f.deviceDir, "bindings"), filepath.Join(f.deviceDir, "bindings", route.RecordID)} {
		if err := privatefile.Mkdir(dir); err != nil {
			t.Fatal(err)
		}
	}
	f.recordDir = filepath.Join(f.deviceDir, "bindings", route.RecordID)
	record := map[string]any{"version": 1, "record_id": route.RecordID, "origin": route.Origin, "deployment_id": route.DeploymentID, "user_id": route.UserID, "device_id": route.DeviceID, "agent_id": route.AgentID, "room_id": route.RoomID, "slot": f.state.Slot, "runtime": f.state.Runtime, "workspace": root, "bind_id": f.state.BindID, "generation": 1, "session_hash": relay.Digest(string(f.state.Runtime) + "\x00" + f.state.SessionID), "credential_hash": relay.Digest(local.Secret), "source_key": enterpriseFixtureSecret("opaque-source"), "agent_operation_id": "agent-operation", "request_id": "request", "request_operation_id": "request-operation", "completion_operation_id": "completion-operation", "observed_generation": 1, "replace": false, "park_enabled": true, "phase": "active", "proof": enterpriseFixtureSecret("binding-proof"), "grant": map[string]any{"id": "narrow-grant", "token": f.grantToken, "expires_at": now.Add(time.Hour)}}
	if err := privatefile.WriteJSON(filepath.Join(f.recordDir, "binding.json"), record); err != nil {
		t.Fatal(err)
	}
	if err := nativeidentity.Reserve(context.Background(), enterpriseClaim(f.state)); err != nil {
		t.Fatal(err)
	}
	if err := rememberSession(f.state); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *enterpriseCLIFixture) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(enterpriseclient.DeploymentHeader, "dep-test")
	if r.URL.Path == "/enterprise/device/v1/deployment" {
		if r.Header.Get("Authorization") != "" {
			f.t.Error("preflight carried credential")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"deployment_id": "dep-test", "origin": f.server.URL})
		return
	}
	if r.Header.Get(enterpriseclient.DeploymentHeader) != "dep-test" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" || r.Header.Get("X-PairRoom-Session") != "" {
		f.t.Error("wrong enterprise native envelope")
		w.WriteHeader(403)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") == "Bearer "+enterpriseFixtureSecret("access") {
		f.serveDevice(w, r)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	digest := sha256.Sum256([]byte(token))
	extra := f.extraBinding != nil && base64.RawURLEncoding.EncodeToString(digest[:]) == f.extraTokenHash
	if r.Header.Get("Authorization") != "Bearer "+f.grantToken && !extra {
		f.t.Error("wrong narrow credential")
		w.WriteHeader(403)
		return
	}
	if f.objectHandler != nil && f.objectHandler(w, r) {
		return
	}
	if r.URL.Path == "/enterprise/device/v1/rooms/server-room/relay/history" {
		var query relay.HistoryQuery
		if json.NewDecoder(r.Body).Decode(&query) != nil {
			w.WriteHeader(400)
			return
		}
		f.calls["history"]++
		f.historyQueries = append(f.historyQueries, query)
		page := relay.HistoryPage{Messages: []relay.Message{}, Sequence: 4, Notice: "History only."}
		for _, result := range f.operations {
			if result.Message != nil && (query.ID == "" || result.Message.ID == query.ID) {
				page.Messages = append(page.Messages, *result.Message)
			}
		}
		page.Total = len(page.Messages)
		_ = json.NewEncoder(w).Encode(page)
		return
	}
	if r.URL.Path == "/enterprise/device/v1/rooms/server-room/relay/context" {
		f.calls["context"]++
		own := relay.Binding{Slot: model.ActorSlot1, Runtime: model.RuntimeClaude, BindID: "binding-test", Generation: 1, Active: true, ParkEnabled: true}
		peer := relay.Binding{Slot: model.ActorSlot2, Runtime: model.RuntimeCodex, BindID: "peer-binding", Generation: f.peerGeneration, Active: true, ParkEnabled: true}
		if extra {
			own = relay.Binding{Slot: model.ActorSlot2, Runtime: f.extraBinding.Runtime, BindID: f.extraBinding.BindID, Generation: f.extraBinding.Generation, Active: true, ParkEnabled: true}
			peer = relay.Binding{Slot: model.ActorSlot1, Runtime: model.RuntimeClaude, BindID: "binding-test", Generation: 1, Active: true, ParkEnabled: true}
		}
		inbox := relay.InboxSummary{}
		if f.queued {
			inbox.Queued = 1
		}
		if f.claimed {
			inbox.Delivering = 1
		}
		summary := relay.Summary{RoomID: "server-room", HostMode: model.HostNative, Sequence: 3, DynamicBindings: true, Protocol: protocol.NativeDynamicVersion, TargetGenerations: map[model.ActorID]uint64{model.ActorSlot1: 1, model.ActorSlot2: f.peerGeneration}, Bindings: map[model.ActorID]relay.BindingSummary{model.ActorSlot1: {Runtime: model.RuntimeClaude, Generation: 1, Active: true, Associated: true, ParkEnabled: true}, model.ActorSlot2: {Runtime: model.RuntimeCodex, Generation: f.peerGeneration, Active: true, Associated: true, ParkEnabled: true}}, Inboxes: map[model.ActorID]relay.InboxSummary{model.ActorSlot1: inbox, model.ActorSlot2: {}}}
		if extra {
			summary.TargetGenerations[model.ActorSlot1] = 1
			summary.Bindings[model.ActorSlot2] = relay.BindingSummary{Runtime: own.Runtime, Generation: own.Generation, Active: true, Associated: true, ParkEnabled: true}
		}
		view := enterpriseclient.RelayContext{Revision: 3, Summary: summary, Binding: own, Peer: peer, PeerGeneration: f.peerGeneration, Bootstrap: protocol.NativeDynamicBootstrap(own.Slot, own.Runtime, peer.Runtime, f.peerGeneration), Collaboration: "Frozen collaboration instructions.", ClaimReady: f.queued && !f.claimed}
		if f.attachmentIncoming && f.queued && !f.claimed {
			view.Next = &relay.EnterpriseIncoming{MessageID: "inbound-message", TargetGeneration: 1, Attachments: f.attachmentMetadata}
		}
		_ = json.NewEncoder(w).Encode(view)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/enterprise/device/v1/rooms/server-room/relay") {
		w.WriteHeader(404)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Error(err)
		return
	}
	for _, private := range []string{f.state.SessionID, f.root, "private-local-relay-capability", "transcript_path", "session_id"} {
		if strings.Contains(string(data), private) {
			f.t.Error("native metadata crossed enterprise wire")
		}
	}
	var command struct {
		ClientID   string                          `json:"client_id"`
		BindingID  string                          `json:"binding_id"`
		Generation uint64                          `json:"generation"`
		Intent     relay.EnterpriseOperationIntent `json:"intent"`
	}
	if json.Unmarshal(data, &command) != nil || command.BindingID != "binding-test" || command.Generation != 1 {
		f.t.Error("invalid operation scope")
		w.WriteHeader(403)
		return
	}
	key := string(command.Intent.Kind) + "/" + command.ClientID
	resolve := strings.HasSuffix(r.URL.Path, "/resolve")
	action := string(command.Intent.Kind)
	if resolve {
		action += "-resolve"
	}
	f.calls[action]++
	if original, ok := f.operations[key]; ok {
		if f.intents[key] != command.Intent {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]string{"outcome": "conflict", "class": "payload_conflict"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"outcome": "replayed", "seq": 4, "revision": 4, "result": original})
		return
	}
	if resolve {
		_ = json.NewEncoder(w).Encode(map[string]string{"outcome": "not_committed"})
		return
	}
	result := relay.EnterpriseOperationResult{Version: 1, Kind: command.Intent.Kind}
	at := time.Now().UTC()
	message := func(source string) *relay.Message {
		return &relay.Message{ID: "message-" + command.ClientID, From: model.ActorSlot1, FromRuntime: model.RuntimeClaude, FromGeneration: 1, To: model.ActorSlot2, TargetGeneration: command.Intent.ExpectedPeerGeneration, Text: command.Intent.Text, Source: source, State: "queued", CreatedAt: at, UpdatedAt: at}
	}
	switch command.Intent.Kind {
	case relay.EnterpriseOperationSend:
		result.Message = message("send")
		if command.Intent.Review != (review.Anchor{}) {
			anchor := command.Intent.Review
			result.Message.Review = &anchor
		}
		if command.Intent.QuoteID != "" {
			for _, prior := range f.operations {
				if prior.Message != nil && prior.Message.ID == command.Intent.QuoteID {
					result.Message.Quote = &model.AgentQuote{FromHandle: "@agent1", Text: prior.Message.Text}
				}
			}
		}
		for _, id := range command.Intent.AttachmentIDs.Slice() {
			for _, meta := range f.attachmentMetadata {
				if id == meta.ID {
					result.Message.Attachments = append(result.Message.Attachments, meta)
				}
			}
		}
	case relay.EnterpriseOperationReport:
		publication := &relay.Publication{BindID: "binding-test", Generation: 1, ReportSeq: command.Intent.ReportSeq, ExpectedPeerGeneration: command.Intent.ExpectedPeerGeneration}
		if strings.Contains(command.Intent.Text, "@agent2") {
			if command.Intent.ExpectedPeerGeneration != f.peerGeneration {
				publication.Outcome = relay.PeerChangedCode
			} else {
				publication.Message = message("stop")
			}
		}
		result.Publication = publication
	case relay.EnterpriseOperationClaim:
		if !f.queued || f.claimed {
			result.Empty = true
		} else {
			result.Claim = &relay.Claim{ID: "inbound-message", Receipt: "private-inbound-receipt", Envelope: "complete inbound envelope", PeerGeneration: f.peerGeneration}
			if f.attachmentIncoming {
				if command.Intent.MessageID != "inbound-message" || command.Intent.ExpectedTargetGeneration != 1 {
					f.t.Error("attachment claim did not pin prepared head")
				}
				result.Claim = nil
				result.StructuredClaim = &relay.StructuredClaim{ID: "inbound-message", Receipt: "private-inbound-receipt", PeerGeneration: f.peerGeneration, Message: relay.Message{ID: "inbound-message", From: model.ActorSlot2, To: model.ActorSlot1, FromRuntime: model.RuntimeClaude, FromGeneration: 1, TargetGeneration: 1, Text: "complete inbound evidence", Source: "send", State: "queued", CreatedAt: at, UpdatedAt: at, Attachments: f.attachmentMetadata}}
			}
			f.queued = false
			f.claimed = true
		}
	case relay.EnterpriseOperationAck:
		if command.Intent.MessageID != "inbound-message" || command.Intent.Receipt != "private-inbound-receipt" {
			f.t.Error("ACK changed original receipt")
		}
		if f.stdoutComplete != nil && !f.stdoutComplete.Load() {
			f.t.Error("ACK preceded full stdout")
		}
		result.HandedOff = true
		f.claimed = false
	default:
		f.t.Error("unexpected fixture operation")
		w.WriteHeader(400)
		return
	}
	f.operations[key] = result
	f.intents[key] = command.Intent
	if f.drop == command.Intent.Kind {
		f.drop = ""
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"outcome": "committed", "seq": 4, "revision": 4, "result": result})
}
func (f *enterpriseCLIFixture) serveDevice(w http.ResponseWriter, r *http.Request) {
	write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	base := "/enterprise/device/v1/rooms/server-room/"
	if r.URL.Path == base+"binding-context" {
		write(enterpriseclient.BindingContext{RoomID: "server-room", UserID: "user-test", Role: "member", OwnerUserID: "owner", Slots: map[model.ActorID]enterpriseclient.BindingSlotContext{model.ActorSlot1: {Slot: model.ActorSlot1, HeadGeneration: 1, Vacant: false, BindingID: "binding-test", AgentID: "agent-test", Runtime: model.RuntimeClaude}, model.ActorSlot2: {Slot: model.ActorSlot2, HeadGeneration: 1, Vacant: true}}})
		return
	}
	if r.URL.Path == "/enterprise/device/v1/agents" {
		write(map[string]any{"outcome": "committed", "kind": "agent.register", "agent_id": "agent-second", "user_id": "user-test", "device_id": "device-test"})
		return
	}
	if r.URL.Path == base+"binding-requests" {
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		f.bindingRequest = body
		var info enterpriseclient.BindingRequestInfo
		decode := func(key string, value any) { _ = json.Unmarshal(body[key], value) }
		decode("request_id", &info.RequestID)
		decode("agent_id", &info.AgentID)
		decode("binding_id", &info.BindingID)
		decode("slot", &info.Slot)
		decode("observed_generation", &info.ObservedGeneration)
		decode("replace", &info.Replace)
		decode("park_enabled", &info.ParkEnabled)
		info.RoomID, info.UserID, info.DeviceID, info.Runtime, info.Status = "server-room", "user-test", "device-test", model.RuntimeClaude, "pending"
		info.RequestedAt = time.Now().UTC()
		info.ExpiresAt = info.RequestedAt.Add(10 * time.Minute)
		f.bindingInfo = info
		var grant struct {
			GrantID   string `json:"grant_id"`
			TokenHash string `json:"token_hash"`
		}
		decode("grant", &grant)
		f.extraGrantID, f.extraTokenHash = grant.GrantID, grant.TokenHash
		write(map[string]any{"outcome": "committed", "receipt": map[string]any{"version": 1, "kind": "request", "request": info}})
		return
	}
	if r.URL.Path == base+"binding-requests/"+f.bindingInfo.RequestID {
		info := f.bindingInfo
		if f.bindingApproved {
			info.Status = "approved"
			info.ApprovedBy = "owner"
		}
		write(map[string]any{"request": info})
		return
	}
	if r.URL.Path == base+"binding-requests/"+f.bindingInfo.RequestID+"/complete" && f.bindingApproved {
		info := f.bindingInfo
		info.Status, info.ApprovedBy = "consumed", "owner"
		binding := model.EnterpriseBinding{Version: 1, RoomID: info.RoomID, Slot: info.Slot, BindID: info.BindingID, Generation: 1, AgentID: info.AgentID, Runtime: info.Runtime, Active: true, ParkEnabled: info.ParkEnabled}
		f.extraBinding = &binding
		write(map[string]any{"outcome": "committed", "receipt": map[string]any{"version": 1, "kind": "complete", "request": info, "binding": binding, "grant_id": f.extraGrantID, "expires_at": time.Now().Add(time.Hour), "seq": 4, "revision": 4}})
		return
	}
	w.WriteHeader(404)
	write(map[string]string{"error": "not_found"})
}

func (f *enterpriseCLIFixture) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[kind]
}
func (f *enterpriseCLIFixture) run(args []string, in io.Reader, out io.Writer) error {
	return Run(context.Background(), args, in, out, io.Discard)
}

type enterpriseObservedWriter struct {
	fixture  *enterpriseCLIFixture
	complete atomic.Bool
	buffer   bytes.Buffer
	fail     bool
}

func (w *enterpriseObservedWriter) Write(p []byte) (int, error) {
	var saved map[string]json.RawMessage
	err := privatefile.ReadJSON(filepath.Join(w.fixture.recordDir, "delivery.json"), 2<<20, &saved)
	var phase string
	var claim relay.Claim
	_ = json.Unmarshal(saved["phase"], &phase)
	_ = json.Unmarshal(saved["claim"], &claim)
	if err != nil || phase != "claimed" || claim.ID != "inbound-message" {
		w.fixture.t.Error("stdout preceded durable local claim receipt")
	}
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	n, err := w.buffer.Write(p)
	w.complete.Store(err == nil && n == len(p))
	return n, err
}

func TestEnterpriseCLIWaitUsesExistingStdoutThenAckBoundary(t *testing.T) {
	f := newEnterpriseCLIFixture(t)
	f.queued = true
	writer := &enterpriseObservedWriter{fixture: f}
	f.stdoutComplete = &writer.complete
	if err := f.run([]string{"wait", "--timeout", "1"}, nil, writer); err != nil {
		t.Fatal(err)
	}
	if writer.buffer.String() != "complete inbound envelope\n" || f.count("claim") != 1 || f.count("ack") != 1 {
		t.Fatal("wait did not release exactly one complete envelope then ACK")
	}
	f.mu.Lock()
	f.queued = true
	f.mu.Unlock()
	failed := &enterpriseObservedWriter{fixture: f, fail: true}
	f.stdoutComplete = &failed.complete
	if err := f.run([]string{"wait", "--timeout", "1"}, nil, failed); err == nil {
		t.Fatal("stdout failure reported success")
	}
	if f.count("ack") != 1 {
		t.Fatal("stdout failure sent ACK")
	}
}

func TestEnterpriseCLIHookWALKeepsOriginalPublicationAfterResponseLoss(t *testing.T) {
	f := newEnterpriseCLIFixture(t)
	f.drop = relay.EnterpriseOperationReport
	hookBytes, _ := json.Marshal(map[string]any{"hook_event_name": "Stop", "session_id": f.state.SessionID, "cwd": f.root, "last_assistant_message": "@agent2 complete original reply", "stop_hook_active": false})
	hook := string(hookBytes)
	var out bytes.Buffer
	_ = f.run([]string{"hook", "--runtime", "claude"}, strings.NewReader(hook), &out)
	var pending State
	if err := readPrivate(filepath.Join(f.slotDir, "state.json"), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Pending == nil || pending.Pending.Seq != 1 || pending.Pending.ExpectedPeerGeneration != 1 || pending.Pending.Text != "@agent2 complete original reply" {
		t.Fatal("Stop failed to preserve original WAL identity")
	}
	f.mu.Lock()
	f.peerGeneration = 2
	f.mu.Unlock()
	out.Reset()
	if err := f.run([]string{"reconcile"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	pending = State{}
	if err := readPrivate(filepath.Join(f.slotDir, "state.json"), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Pending != nil || pending.LastConfirmedSeq != 1 || pending.AutomaticPeerGeneration != 1 || f.count("report") != 1 || f.count("report-resolve") < 1 {
		t.Fatalf("recovery changed key/context or resent accepted Stop: pending=%v confirmed=%d automatic=%d report=%d resolve=%d", pending.Pending != nil, pending.LastConfirmedSeq, pending.AutomaticPeerGeneration, f.count("report"), f.count("report-resolve"))
	}
	if strings.Contains(out.String(), "complete original reply") {
		t.Fatal("reconcile printed private publication body")
	}
}

func TestEnterpriseCLIClaimLossNeverReprintsDuringReconciliation(t *testing.T) {
	f := newEnterpriseCLIFixture(t)
	f.queued = true
	f.drop = relay.EnterpriseOperationClaim
	var out bytes.Buffer
	if err := f.run([]string{"wait", "--timeout", "1"}, nil, &out); err == nil || out.Len() != 0 {
		t.Fatal("lost claim printed an envelope")
	}
	if err := f.run([]string{"reconcile"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "complete inbound envelope") || strings.Contains(out.String(), "private-inbound-receipt") || f.count("claim") != 1 || f.count("ack") != 0 {
		t.Fatal("read-only reconciliation printed/ACKed uncertain stdout")
	}
}

func TestEnterpriseCLICwdAndLogoutDoNotRetargetBinding(t *testing.T) {
	f := newEnterpriseCLIFixture(t)
	other := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", other).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	t.Chdir(other)
	var out bytes.Buffer
	if err := f.run([]string{"status"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if f.count("context") == 0 || !strings.Contains(out.String(), f.state.Room) && !strings.Contains(out.String(), "server-room") {
		t.Fatal("cwd replaced original enterprise route")
	}
	parent, err := enterpriseclient.New(enterpriseConfig(f.state.Enterprise))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if _, err := parent.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.count("context")
	out.Reset()
	hookBytes, _ := json.Marshal(map[string]any{"hook_event_name": "Stop", "session_id": f.state.SessionID, "cwd": f.root, "last_assistant_message": "@agent2 do not send", "stop_hook_active": false})
	hook := string(hookBytes)
	if err := f.run([]string{"hook", "--runtime", "claude"}, strings.NewReader(hook), &out); err != nil && !errors.Is(err, errNoAssociatedBinding) {
		t.Fatal(err)
	}
	if f.count("context") != before || f.count("report") != 0 {
		t.Fatal("logged-out hook used old Binding")
	}
}

func TestEnterpriseCLIBindApprovesSecondSessionWithoutSlotCollision(t *testing.T) {
	f := newEnterpriseCLIFixture(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "second-enterprise-session")
	args := []string{"bind", "--repo", f.root, "--enterprise-service", f.server.URL, "--deployment", "dep-test", "--enterprise-ca", f.ca, "--enterprise-state-dir", f.deviceDir, "--room", "server-room", "--slot", "2"}
	var out bytes.Buffer
	if err := f.run(args, nil, &out); err != nil {
		t.Fatal(err)
	}
	var pending map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = json.Unmarshal(pending["status"], &status)
	if status != "awaiting_owner" || len(pending["approval_url"]) == 0 {
		t.Fatal("bind bypassed explicit Owner approval")
	}
	if _, err := os.Stat(filepath.Join(f.root, ".pairroom", "rooms", f.state.Room, "slots", "slot2", "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending request became active discovery")
	}
	f.mu.Lock()
	f.bindingApproved = true
	f.mu.Unlock()
	out.Reset()
	if err := f.run(args, nil, &out); err != nil {
		t.Fatal(err)
	}
	var second State
	if err := readPrivate(filepath.Join(f.root, ".pairroom", "rooms", f.state.Room, "slots", "slot2", "state.json"), &second); err != nil {
		t.Fatal(err)
	}
	if second.Schema != 4 || second.Slot != model.ActorSlot2 || second.Runtime != model.RuntimeClaude || second.SessionID == f.state.SessionID || second.Enterprise.AgentID == f.state.Enterprise.AgentID || second.Room != f.state.Room {
		t.Fatal("two native sessions collided or adopted a slot Runtime default")
	}
	var original State
	if err := readPrivate(filepath.Join(f.slotDir, "state.json"), &original); err != nil || original.BindID != f.state.BindID || original.SessionID != f.state.SessionID {
		t.Fatal("second session rewrote first slot")
	}
	if strings.Contains(out.String(), second.SessionID) || strings.Contains(out.String(), "private-local-relay-capability") {
		t.Fatal("binding stdout exposed local native identity")
	}
	if _, err := currentEnterpriseBinding(second); err != nil {
		t.Fatal(err)
	}
}
