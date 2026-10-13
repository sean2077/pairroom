package enterpriseclient

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/relay"
)

// This controlled TLS peer exercises client journals and real shared Relay
// preparation/replay. It is not SQL, OIDC or authenticated-vendor acceptance.
type nativeWireDevice struct{ user, device string }
type nativeWireRequest struct {
	payload  bindingRequestPayload
	info     BindingRequestInfo
	approved bool
}
type nativeWireOperation struct {
	intent relay.EnterpriseOperationIntent
	reply  relayReply
}
type nativeWireRenewal struct {
	bindingID  string
	generation uint64
	grant      bindingGrantPayload
	reply      bindingReply
}
type nativeWireFixture struct {
	t                             *testing.T
	server                        *httptest.Server
	ca                            string
	mu                            sync.Mutex
	devices                       map[string]nativeWireDevice
	agents                        map[string]string
	runtimes                      map[string]model.RuntimeKind
	requests                      map[string]*nativeWireRequest
	grants                        map[string]*model.EnterpriseBinding
	ops                           map[string]nativeWireOperation
	events                        []model.Event
	revision                      uint64
	bodies                        []string
	dropKind                      relay.EnterpriseOperationKind
	posts, reads                  int
	contextFailures               int
	contextFailureClass           string
	contextEntered                chan struct{}
	blockContext                  bool
	renewals                      map[string]nativeWireRenewal
	dropRenewal                   bool
	renewCalls, resolveRenewCalls int
}
type nativeEndpoint struct {
	parent  *Client
	binding *BindingClient
	auth    relay.Auth
	cfg     Config
	opts    BindingOptions
}

func newNativeWireFixture(t *testing.T) *nativeWireFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AppData", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	f := &nativeWireFixture{t: t, devices: map[string]nativeWireDevice{}, agents: map[string]string{}, runtimes: map[string]model.RuntimeKind{}, requests: map[string]*nativeWireRequest{}, grants: map[string]*model.EnterpriseBinding{}, ops: map[string]nativeWireOperation{}, renewals: map[string]nativeWireRenewal{}}
	intent, err := model.PrepareEnterpriseRoomIntent("Native TLS fixture", model.Collaboration{})
	if err != nil {
		t.Fatal(err)
	}
	birth, err := model.NewEnterpriseRoomCreation("room-native", "dep-test", "org-test", "owner-fixture", intent, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.events = []model.Event{birth}
	f.revision = 1
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.ca = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(f.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *nativeWireFixture) snapshot() relay.EnterpriseRoomSnapshot {
	return relay.EnterpriseRoomSnapshot{RoomID: "room-native", DeploymentID: "dep-test", OrgID: "org-test", CreationUserID: "owner-fixture", HostMode: "native", Events: append([]model.Event(nil), f.events...), Seq: uint64(len(f.events)), Revision: f.revision}
}
func (f *nativeWireFixture) endpoint(t *testing.T, id string, slot model.ActorID, kind model.RuntimeKind) *nativeEndpoint {
	t.Helper()
	cfg := Config{Origin: f.server.URL, DeploymentID: "dep-test", Directory: filepath.Join(t.TempDir(), "private"), CAFile: f.ca}
	parent, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(parent.Close)
	credential, err := newCredential()
	if err != nil {
		t.Fatal(err)
	}
	credential.ExpiresAt = time.Now().Add(time.Hour)
	credential.RefreshExpiresAt = time.Now().Add(24 * time.Hour)
	proof, _ := newSecret()
	s := state{Version: 1, Origin: f.server.URL, DeploymentID: "dep-test", UserID: "user-" + id, DeviceID: "device-" + id, Active: &credential, Proof: proof}
	if err := parent.save(&s); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.devices[string(credential.Access)] = nativeWireDevice{s.UserID, s.DeviceID}
	f.mu.Unlock()
	auth := relay.Auth{Runtime: kind, Slot: slot, BindID: "binding-" + id, SessionID: "official-session-" + id, Secret: "private-local-credential-" + id}
	claim := nativeidentity.Claim{Runtime: kind, SessionID: auth.SessionID, Association: nativeidentity.Enterprise(f.server.URL, "dep-test", "room-native"), BindID: auth.BindID}
	if err := nativeidentity.Reserve(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	opts := BindingOptions{RecordID: "record-" + id, RoomID: "room-native", Workspace: t.TempDir(), Auth: auth, ObservedGeneration: 1, ParkEnabled: true}
	binding, err := parent.PrepareBinding(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return &nativeEndpoint{parent, binding, auth, cfg, opts}
}
func (f *nativeWireFixture) approve(t *testing.T, endpoint *nativeEndpoint) {
	t.Helper()
	meta, err := endpoint.binding.ResumeBinding(context.Background())
	if err != nil || meta.Status != "awaiting_owner" {
		t.Fatalf("pending bind: %s %v", meta.Status, err)
	}
	f.mu.Lock()
	f.requests[meta.RequestID].approved = true
	f.mu.Unlock()
	meta, err = endpoint.binding.ResumeBinding(context.Background())
	if err != nil || meta.Status != "active" {
		t.Fatalf("approved bind: %s %v", meta.Status, err)
	}
	endpoint.auth.Generation = meta.Generation
	if err := nativeidentity.Reserve(context.Background(), nativeidentity.Claim{Runtime: endpoint.auth.Runtime, SessionID: endpoint.auth.SessionID, Association: nativeidentity.Enterprise(f.server.URL, "dep-test", "room-native"), BindID: meta.BindID, Generation: meta.Generation}); err != nil {
		t.Fatal(err)
	}
}
func (f *nativeWireFixture) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(DeploymentHeader, "dep-test")
	if r.URL.Path == devicePath+"/deployment" {
		if r.Header.Get("Authorization") != "" {
			f.t.Error("preflight disclosed bearer")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"deployment_id": "dep-test", "origin": f.server.URL})
		return
	}
	if r.Header.Get(DeploymentHeader) != "dep-test" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
		f.t.Error("invalid native envelope")
		w.WriteHeader(403)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = append(f.bodies, string(data))
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	device, isDevice := f.devices[token]
	write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	fail := func() { w.WriteHeader(403); write(map[string]string{"outcome": "refused", "class": "authorization"}) }
	if r.URL.Path == devicePath+"/agents" {
		if !isDevice {
			fail()
			return
		}
		var body struct {
			ClientID, SourceKey string
			Runtime             model.RuntimeKind
		}
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(data, &raw)
		_ = json.Unmarshal(raw["source_key"], &body.SourceKey)
		_ = json.Unmarshal(raw["runtime"], &body.Runtime)
		key := device.device + "/" + body.SourceKey
		agent := f.agents[key]
		if agent == "" {
			agent = fmt.Sprintf("agent-%d", len(f.agents)+1)
			f.agents[key] = agent
			f.runtimes[agent] = body.Runtime
		}
		write(operationResponse{Outcome: "committed", Kind: "agent.register", AgentID: agent, UserID: device.user, DeviceID: device.device})
		return
	}
	base := devicePath + "/rooms/room-native/"
	if r.URL.Path == base+"binding-context" {
		if !isDevice {
			fail()
			return
		}
		slots := map[model.ActorID]BindingSlotContext{}
		for _, slot := range model.SlotActors() {
			entry := BindingSlotContext{Slot: slot, HeadGeneration: 1, Vacant: true}
			for _, b := range f.grants {
				if b.Active && b.Slot == slot {
					entry.Vacant = false
					entry.HeadGeneration = b.Generation
					entry.BindingID = b.BindID
					entry.AgentID = b.AgentID
					entry.Runtime = b.Runtime
				}
			}
			slots[slot] = entry
		}
		write(BindingContext{RoomID: "room-native", UserID: device.user, Role: "member", OwnerUserID: "owner-fixture", Slots: slots})
		return
	}
	if strings.HasPrefix(r.URL.Path, base+"binding-requests") {
		if !isDevice {
			fail()
			return
		}
		if r.URL.Path == base+"binding-requests" {
			var p bindingRequestPayload
			if json.Unmarshal(data, &p) != nil {
				fail()
				return
			}
			q := f.requests[p.RequestID]
			if q == nil {
				q = &nativeWireRequest{payload: p, info: BindingRequestInfo{RequestID: p.RequestID, RoomID: "room-native", Slot: p.Slot, ObservedGeneration: p.ObservedGeneration, Replace: p.Replace, AgentID: p.AgentID, Runtime: f.runtimes[p.AgentID], BindingID: p.BindingID, UserID: device.user, DeviceID: device.device, ParkEnabled: p.ParkEnabled, Status: "pending", RequestedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}}
				f.requests[p.RequestID] = q
			}
			write(bindingReply{Outcome: "committed", Receipt: &bindingReceipt{Version: 1, Kind: "request", Request: &q.info}})
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, base+"binding-requests/"), "/")
		q := f.requests[parts[0]]
		if q == nil || q.info.UserID != device.user || q.info.DeviceID != device.device {
			fail()
			return
		}
		if len(parts) == 1 {
			info := q.info
			if q.approved && info.Status == "pending" {
				info.Status = "approved"
				info.ApprovedBy = "owner-fixture"
			}
			write(map[string]any{"request": info})
			return
		}
		if parts[1] == "resolve-request" {
			write(bindingReply{Outcome: "replayed", Receipt: &bindingReceipt{Version: 1, Kind: "request", Request: &q.info}})
			return
		}
		var p struct {
			ClientID string `json:"client_id"`
			Proof    secret `json:"client_proof"`
		}
		_ = json.Unmarshal(data, &p)
		if !q.approved || verifier(p.Proof) != q.payload.ProofHash {
			fail()
			return
		}
		if parts[1] == "resolve-completion" && q.info.Status != "consumed" {
			write(bindingReply{Outcome: "not_committed"})
			return
		}
		var bound *model.EnterpriseBinding
		for _, b := range f.grants {
			if b.BindID == q.info.BindingID {
				bound = b
			}
		}
		if bound == nil {
			generation := q.info.ObservedGeneration
			if q.info.Replace {
				generation++
				for _, old := range f.grants {
					if old.Slot == q.info.Slot && old.Active {
						old.Active = false
						at := time.Now().UTC()
						old.RetiredAt = &at
						old.RetiredReason = "binding.replaced"
						fact, _ := model.NewEnterpriseBindingUpdate(*old, at)
						fact.Seq = uint64(len(f.events) + 1)
						f.events = append(f.events, fact)
					}
				}
			}
			bound = &model.EnterpriseBinding{Version: 1, RoomID: "room-native", Slot: q.info.Slot, BindID: q.info.BindingID, Generation: generation, AgentID: q.info.AgentID, Runtime: q.info.Runtime, Active: true, ParkEnabled: q.info.ParkEnabled}
			fact, _ := model.NewEnterpriseBindingUpdate(*bound, time.Now().UTC())
			fact.Seq = uint64(len(f.events) + 1)
			f.events = append(f.events, fact)
			f.revision++
			f.grants[q.payload.Grant.TokenHash] = bound
			q.info.Status = "consumed"
			q.info.ApprovedBy = "owner-fixture"
		}
		write(bindingReply{Outcome: "committed", Receipt: &bindingReceipt{Version: 1, Kind: "complete", Request: &q.info, Binding: bound, GrantID: q.payload.Grant.GrantID, ExpiresAt: time.Now().UTC().Add(time.Hour), Seq: uint64(len(f.events)), Revision: f.revision}})
		return
	}
	if strings.HasPrefix(r.URL.Path, base+"bindings/") && isDevice {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, base+"bindings/"), "/")
		if len(parts) != 2 {
			fail()
			return
		}
		var p struct {
			ClientID   string              `json:"client_id"`
			Generation uint64              `json:"generation"`
			Grant      bindingGrantPayload `json:"grant"`
		}
		if json.Unmarshal(data, &p) != nil {
			fail()
			return
		}
		if original, ok := f.renewals[p.ClientID]; ok {
			if original.bindingID != parts[0] || original.generation != p.Generation || original.grant != p.Grant {
				fail()
				return
			}
			f.resolveRenewCalls++
			reply := original.reply
			reply.Outcome = "replayed"
			write(reply)
			return
		}
		if parts[1] == "resolve-renewal" {
			f.resolveRenewCalls++
			write(bindingReply{Outcome: "not_committed"})
			return
		}
		var identity *model.EnterpriseBinding
		for _, bound := range f.grants {
			if bound.BindID == parts[0] && bound.Generation == p.Generation && bound.Active {
				identity = bound
			}
		}
		if identity == nil {
			fail()
			return
		}
		for key, bound := range f.grants {
			if bound.BindID == identity.BindID {
				delete(f.grants, key)
			}
		}
		copy := *identity
		f.grants[p.Grant.TokenHash] = &copy
		f.revision++
		f.renewCalls++
		reply := bindingReply{Outcome: "committed", Receipt: &bindingReceipt{Version: 1, Kind: "renew", Binding: &copy, GrantID: p.Grant.GrantID, ExpiresAt: time.Now().UTC().Add(time.Hour), Seq: uint64(len(f.events)), Revision: f.revision}}
		f.renewals[p.ClientID] = nativeWireRenewal{parts[0], p.Generation, p.Grant, reply}
		if f.dropRenewal {
			f.dropRenewal = false
			closeResponse(w)
			return
		}
		write(reply)
		return
	}
	bound := f.grants[verifier(secret(token))]
	if bound == nil || !bound.Active {
		fail()
		return
	}
	if r.URL.Path == base+"relay/context" {
		f.reads++
		if f.blockContext {
			entered := f.contextEntered
			f.mu.Unlock()
			select {
			case entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			f.mu.Lock()
			return
		}
		if f.contextFailures > 0 {
			f.contextFailures--
			class := f.contextFailureClass
			if class == "" {
				class = "connection_failed"
			}
			w.WriteHeader(503)
			write(map[string]string{"outcome": "unknown", "class": class})
			return
		}
		view, err := relay.DescribeEnterpriseSnapshot(f.snapshot(), bound.Slot, bound.BindID, bound.Generation, time.Now().UTC())
		if err != nil {
			f.t.Error(err)
			w.WriteHeader(500)
			return
		}
		write(view)
		return
	}
	if r.URL.Path == base+"relay" || r.URL.Path == base+"relay/resolve" {
		var body struct {
			ClientID   string                          `json:"client_id"`
			BindingID  string                          `json:"binding_id"`
			Generation uint64                          `json:"generation"`
			Intent     relay.EnterpriseOperationIntent `json:"intent"`
		}
		if json.Unmarshal(data, &body) != nil || body.BindingID != bound.BindID || body.Generation != bound.Generation {
			fail()
			return
		}
		key := body.BindingID + "/" + string(body.Intent.Kind) + "/" + body.ClientID
		if original, ok := f.ops[key]; ok {
			if original.intent != body.Intent {
				w.WriteHeader(409)
				write(map[string]string{"outcome": "conflict", "class": "payload_conflict"})
				return
			}
			reply := original.reply
			reply.Outcome = "replayed"
			write(reply)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/resolve") {
			write(relayReply{Outcome: "not_committed"})
			return
		}
		f.posts++
		prepare, err := relay.PrepareEnterpriseOperation(f.snapshot(), relay.EnterpriseOperationCaller{Actor: bound.Slot, UserID: "user-fixture", BindingID: bound.BindID, Generation: bound.Generation, ClientID: body.ClientID}, body.Intent, relay.EnterpriseOperationIDs{MessageID: fmt.Sprintf("message-%d", f.posts), Receipt: fmt.Sprintf("private-receipt-%d", f.posts)})
		if err != nil {
			f.t.Error(err)
			w.WriteHeader(500)
			return
		}
		plan, err := prepare(time.Now().UTC())
		if err != nil {
			w.WriteHeader(409)
			write(map[string]string{"outcome": "refused", "class": "invalid_control_state"})
			return
		}
		for _, fact := range plan.Facts {
			fact.ID = model.NewID("event")
			fact.Seq = uint64(len(f.events) + 1)
			f.events = append(f.events, fact)
		}
		f.revision++
		reply := relayReply{Outcome: "committed", Seq: uint64(len(f.events)), Revision: f.revision, Result: &plan.Result}
		f.ops[key] = nativeWireOperation{body.Intent, reply}
		if f.dropKind == body.Intent.Kind {
			f.dropKind = ""
			closeResponse(w)
			return
		}
		write(reply)
		return
	}
	w.WriteHeader(404)
	write(map[string]string{"error": "not_found"})
}

func TestEnterpriseBindingRequiresApprovalAndKeepsVendorIdentityLocal(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "one", model.ActorSlot1, model.RuntimeClaude)
	ctx := context.Background()
	meta, err := e.binding.ResumeBinding(ctx)
	if err != nil || meta.Status != "awaiting_owner" {
		t.Fatalf("pending: %s %v", meta.Status, err)
	}
	if _, err := e.binding.Context(ctx, e.auth); err == nil {
		t.Fatal("unapproved endpoint read a Room")
	}
	f.mu.Lock()
	if len(f.grants) != 0 {
		t.Fatal("request minted grant before Owner approval")
	}
	f.requests[meta.RequestID].approved = true
	f.mu.Unlock()
	meta, err = e.binding.ResumeBinding(ctx)
	if err != nil || meta.Status != "active" {
		t.Fatalf("complete: %s %v", meta.Status, err)
	}
	e.auth.Generation = meta.Generation
	if err := nativeidentity.Reserve(ctx, nativeidentity.Claim{Runtime: e.auth.Runtime, SessionID: e.auth.SessionID, Association: nativeidentity.Enterprise(f.server.URL, "dep-test", "room-native"), BindID: e.auth.BindID, Generation: meta.Generation}); err != nil {
		t.Fatal(err)
	}
	var binding relay.Binding
	if err := e.binding.Relay(ctx, e.auth, "confirm", map[string]string{"session_id": e.auth.SessionID, "transcript_path": filepath.Join(e.opts.Workspace, "private-transcript")}, &binding); err != nil {
		t.Fatal(err)
	}
	if binding.SessionID != e.auth.SessionID {
		t.Fatal("local confirmation lost official identity")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, body := range f.bodies {
		for _, private := range []string{e.auth.SessionID, e.auth.Secret, e.opts.Workspace, "session_id", "transcript_path", "credential_hash"} {
			if strings.Contains(body, private) {
				t.Fatalf("private native identity crossed the wire: %s", private)
			}
		}
	}
}

func TestEnterpriseTwoEndpointsUseSharedRelayAndAckJournal(t *testing.T) {
	f := newNativeWireFixture(t)
	a := f.endpoint(t, "one", model.ActorSlot1, model.RuntimeClaude)
	b := f.endpoint(t, "two", model.ActorSlot2, model.RuntimeClaude)
	f.approve(t, a)
	f.approve(t, b)
	ctx := context.Background()
	var sent relay.Message
	if err := a.binding.Relay(ctx, a.auth, "send", map[string]any{"id": "explicit-send", "text": "full peer reply", "expected_peer_generation": uint64(1)}, &sent); err != nil {
		t.Fatal(err)
	}
	var delivery struct {
		Claim              *relay.Claim `json:"claim"`
		ForegroundRequired bool         `json:"foreground_required"`
	}
	if err := b.binding.Relay(ctx, b.auth, "wait", map[string]any{"timeout_seconds": 1, "park": false}, &delivery); err != nil {
		t.Fatal(err)
	}
	if delivery.Claim == nil || delivery.Claim.ID != sent.ID || !strings.Contains(delivery.Claim.Envelope, "full peer reply") {
		t.Fatal("claim lost complete body")
	}
	journal, err := b.binding.readDelivery()
	if err != nil || journal.Phase != "claimed" || journal.Claim.Receipt != delivery.Claim.Receipt {
		t.Fatal("envelope escaped before its receipt was durable")
	}
	var ack struct {
		HandedOff bool `json:"handed_off"`
	}
	if err := b.binding.Relay(ctx, b.auth, "ack", map[string]string{"id": delivery.Claim.ID, "receipt": delivery.Claim.Receipt}, &ack); err != nil || !ack.HandedOff {
		t.Fatal("stored claim did not settle after stdout signal")
	}
	if journal, err := b.binding.readDelivery(); err != nil || journal.Version != 0 {
		t.Fatal("settled receive journal stayed active")
	}
	f.mu.Lock()
	before := f.posts
	f.mu.Unlock()
	if err := b.binding.Relay(ctx, b.auth, "wait", map[string]any{"timeout_seconds": 1, "park": false}, &delivery); err != nil || delivery.Claim != nil {
		t.Fatalf("empty wait: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != before || f.reads == 0 {
		t.Fatal("empty polling wrote operation receipts")
	}
}

func TestEnterpriseLostClaimReconcilesWithoutReturningOldEnvelope(t *testing.T) {
	f := newNativeWireFixture(t)
	a := f.endpoint(t, "sender", model.ActorSlot1, model.RuntimeCodex)
	b := f.endpoint(t, "receiver", model.ActorSlot2, model.RuntimeClaude)
	f.approve(t, a)
	f.approve(t, b)
	ctx := context.Background()
	var sent relay.Message
	if err := a.binding.Relay(ctx, a.auth, "send", map[string]any{"id": "send-before-loss", "text": "must not print twice", "expected_peer_generation": uint64(1)}, &sent); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.dropKind = relay.EnterpriseOperationClaim
	f.mu.Unlock()
	var result map[string]any
	if err := b.binding.Relay(ctx, b.auth, "wait", map[string]any{"timeout_seconds": 1}, &result); err == nil {
		t.Fatal("lost claim response reported success")
	}
	if result != nil {
		t.Fatal("lost claim returned envelope")
	}
	parent, err := New(b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	reopened, err := parent.OpenBinding(b.opts.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := reopened.ReconcileDelivery(ctx, b.auth, false)
	if err != nil || len(ops) != 1 || ops[0].Outcome != "replayed" || ops[0].MessageID != sent.ID {
		t.Fatalf("claim verification: %+v %v", ops, err)
	}
	encoded, _ := json.Marshal(ops)
	if strings.Contains(string(encoded), "must not print twice") || strings.Contains(string(encoded), "receipt") {
		t.Fatal("reconciliation released private envelope/receipt")
	}
	entries, err := os.ReadDir(filepath.Join(reopened.dir, "uncertain-deliveries"))
	if err != nil || len(entries) != 1 {
		t.Fatal("uncertain original receipt was discarded")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	claims := 0
	for _, op := range f.ops {
		if op.intent.Kind == relay.EnterpriseOperationClaim {
			claims++
		}
	}
	if claims != 1 {
		t.Fatal("reconcile reissued claim")
	}
}

func TestEnterpriseLogoutAndNewDeviceCannotDiscoverOldBinding(t *testing.T) {
	f := newNativeWireFixture(t)
	endpoint := f.endpoint(t, "original", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, endpoint)
	if _, err := endpoint.parent.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := endpoint.binding.Metadata(context.Background()); err != ErrNoSession {
		t.Fatalf("logout left Binding discoverable: %v", err)
	}
	s, err := endpoint.parent.read()
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := newCredential()
	credential.ExpiresAt = time.Now().Add(time.Hour)
	credential.RefreshExpiresAt = time.Now().Add(24 * time.Hour)
	s.Active = &credential
	s.LoggedOut = false
	s.DeviceID = "different-device"
	s.Proof, _ = newSecret()
	if err := endpoint.parent.save(&s); err != nil {
		t.Fatal(err)
	}
	if _, err := endpoint.binding.Metadata(context.Background()); err != ErrNoSession {
		t.Fatalf("new Device revived old binding: %v", err)
	}
	if _, err := endpoint.binding.Context(context.Background(), endpoint.auth); err != ErrNoSession {
		t.Fatal("new login used old Binding bearer")
	}
}

func TestEnterpriseContextPollingBackoffHasJitterAndFiniteCeilings(t *testing.T) {
	for _, idle := range []bool{false, true} {
		limit := 10 * time.Second
		if idle {
			limit = 5 * time.Second
		}
		var previous time.Duration
		for attempt := 0; attempt < 100; attempt++ {
			low, high := contextPollDelay(attempt, idle, 0), contextPollDelay(attempt, idle, 200)
			if low < 800*time.Millisecond || high > limit || low >= high || high < previous {
				t.Fatalf("bad bounded jitter/backoff at %d: %v..%v", attempt, low, high)
			}
			previous = high
		}
	}
	for _, err := range []error{ErrNoSession, ErrBindingCredentialExpired, &Error{Outcome: "refused", Class: "authorization"}, &Error{Outcome: "unknown", Class: "deployment_mismatch"}, &Error{Outcome: "unknown", Class: "invalid_response"}} {
		if transientContextFailure(err) {
			t.Fatal("permission/configuration failure entered reconnect loop")
		}
	}
}

func TestEnterpriseContextOutageWaitIsBoundedAndNeverRetriesEffects(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "backoff", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, e)
	f.mu.Lock()
	f.contextFailures = 100
	f.mu.Unlock()
	started := time.Now()
	var reply map[string]any
	if err := e.binding.Relay(context.Background(), e.auth, "wait", map[string]any{"timeout_seconds": 1}, &reply); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("one-second read-only wait exceeded its bounded window: %v", elapsed)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reads < 1 || f.reads > 2 || f.posts != 0 {
		t.Fatalf("outage amplified reads/effects: reads=%d effects=%d", f.reads, f.posts)
	}
}

func TestEnterpriseContextCancellationInterruptsAnActualInFlightRead(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "cancel", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, e)
	f.mu.Lock()
	f.blockContext = true
	f.contextEntered = make(chan struct{}, 1)
	entered := f.contextEntered
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		var result map[string]any
		done <- e.binding.Relay(ctx, e.auth, "wait", map[string]any{"timeout_seconds": 30}, &result)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("actual context GET did not arrive")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled collector reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not bound stalled read")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 || f.reads != 1 {
		t.Fatal("canceled read retried or claimed")
	}
}

func TestEnterpriseRenewalKeepsFrozenCandidateAcrossClientRestart(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "renew", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, e)
	ctx := context.Background()
	before, err := e.binding.read()
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	events := len(f.events)
	f.dropRenewal = true
	f.mu.Unlock()
	if _, err := e.binding.RenewCredential(ctx, e.auth); err == nil {
		t.Fatal("lost renewal response reported success")
	}
	pending, err := e.binding.read()
	if err != nil || pending.Renewal == nil || pending.Renewal.Phase != "unknown" || pending.Grant.ID != before.Grant.ID {
		t.Fatal("renewal candidate was not preserved before effect")
	}
	parent, err := New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	reopened, err := parent.OpenBinding(e.opts.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := reopened.RenewCredential(ctx, e.auth)
	if err != nil || meta.Generation != 1 || meta.BindID != before.BindID {
		t.Fatalf("renewal recovery: %+v %v", meta, err)
	}
	after, err := reopened.read()
	if err != nil || after.Renewal != nil || after.Grant.ID != pending.Renewal.Grant.ID || after.Grant.Token != pending.Renewal.Grant.Token || after.SourceKey != before.SourceKey || after.Proof != before.Proof {
		t.Fatal("recovery changed original renewal candidate or admission identity")
	}
	if _, err := reopened.Context(ctx, e.auth); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renewCalls != 1 || f.resolveRenewCalls != 1 || len(f.events) != events || f.grants[verifier(before.Grant.Token)] != nil {
		t.Fatal("renewal duplicated effect, changed Room facts or revived old grant")
	}
}

func TestEnterpriseAgentSourceSurvivesBindingCandidateReplacement(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "stable-source", model.ActorSlot1, model.RuntimeClaude)
	first, err := e.binding.read()
	if err != nil {
		t.Fatal(err)
	}
	opts := e.opts
	opts.RecordID = "replacement-record"
	opts.Auth.BindID = "replacement-binding"
	opts.Auth.Secret = "replacement-local-secret"
	opts.Replace = true
	next, err := e.parent.PrepareBinding(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := next.read()
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceKey != second.SourceKey || first.Proof == second.Proof || first.Grant.Token == second.Grant.Token || string(second.SourceKey) == second.SessionHash {
		t.Fatal("replacement changed canonical Agent source or reused credential material")
	}
}
