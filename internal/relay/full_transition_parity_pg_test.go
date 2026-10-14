package relay_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"github.com/sean2077/pairroom/internal/sqlstore/sqltest"
	"github.com/sean2077/pairroom/internal/store"
)

type transitionTrace struct {
	t                      *testing.T
	local                  *relay.Engine
	directory              string
	localEvents            []model.Event
	db                     *sqlstore.Store
	cfg                    sqlstore.Config
	owner, device          sqlstore.AuthContext
	binding                map[model.ActorID]sqlstore.BindingCredential
	localAuth              map[model.ActorID]relay.Auth
	ids, receipts          [2]map[string]string
	bodies                 map[string]string
	localCursor, sqlCursor int
	step                   int
}

// All authorization/binding fixture mutations are public typed Store calls.
// Verified login inputs are synthetic; the storage engines and Relay producers
// are real. There is no vendor CLI, provider verification or model acceptance.
func newTransitionTrace(t *testing.T) *transitionTrace {
	t.Helper()
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	cfg := sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "trace-deployment", RequireSessionAuth: true}
	if err := sqlstore.InitializeDeployment(t.Context(), cfg, "trace-org"); err != nil {
		t.Fatal(err)
	}
	db, err := sqlstore.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := &transitionTrace{t: t, db: db, cfg: cfg, directory: t.TempDir(), binding: map[model.ActorID]sqlstore.BindingCredential{}, localAuth: map[model.ActorID]relay.Auth{}, ids: [2]map[string]string{{}, {}}, receipts: [2]map[string]string{{}, {}}, bodies: map[string]string{}}
	t.Cleanup(func() {
		_ = f.db.Close()
		if f.local != nil {
			_ = f.local.Close()
		}
	})
	identity := enterpriseauth.VerifiedIdentity{ConnectionID: "fixture", Issuer: "https://trace-idp.example.invalid", Subject: "owner"}
	if _, err := db.InitializeIdentity(t.Context(), sqlstore.IdentityConfig{Connections: []sqlstore.TrustConnectionConfig{{ConnectionID: identity.ConnectionID, Issuer: identity.Issuer}}, Bootstrap: identity}); err != nil {
		t.Fatal(err)
	}
	token := sha256.Sum256([]byte("trace-browser-token"))
	if _, err := db.AdmitIdentity(t.Context(), sqlstore.IdentityLoginCommand{ClientID: "trace-login", Completion: enterpriseauth.Completion{Purpose: enterpriseauth.PurposeLogin, Identity: identity}, Session: sqlstore.BrowserSessionCandidate{SessionID: "trace-owner-session", TokenHash: token, CSRFHash: sha256.Sum256([]byte("trace-csrf")), ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}}); err != nil {
		t.Fatal(err)
	}
	owner, err := db.AuthenticateSession(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	f.owner = owner.Auth
	intent, err := model.PrepareEnterpriseRoomIntent("Full transition trace", model.Collaboration{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitRoomCreate(t.Context(), sqlstore.RoomCreateCommand{Auth: f.owner, RoomID: "trace-room", ClientID: "trace-create", Intent: intent}); err != nil {
		t.Fatal(err)
	}
	origin := "https://trace-service.example.invalid"
	access := sha256.Sum256([]byte("trace-device-access"))
	proof := sha256.Sum256([]byte("trace-device-proof"))
	code := sha256.Sum256([]byte("trace-device-code"))
	enrollment := sqlstore.DeviceEnrollmentCommand{RequestID: "trace-device-request", DeviceID: "trace-device", Label: "Trace fixture", Origin: origin, CallbackURI: "http://127.0.0.1:49152/callback", ClientState: base64.RawURLEncoding.EncodeToString(code[:]), ProofHash: proof, Credential: sqlstore.DeviceCredentialCandidate{GrantID: "trace-device-grant", AccessHash: access, RefreshHash: sha256.Sum256([]byte("trace-device-refresh"))}}
	if _, err := db.BeginDeviceEnrollment(t.Context(), enrollment); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DecideDeviceEnrollment(t.Context(), sqlstore.DeviceEnrollmentDecision{Auth: f.owner, ClientID: "trace-device-approve", RequestID: enrollment.RequestID, Approve: true, CodeHash: code}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CompleteDeviceEnrollment(t.Context(), sqlstore.DeviceEnrollmentCompletion{RequestID: enrollment.RequestID, ClientID: "trace-device-complete", Origin: origin, ProofHash: proof, CodeHash: code}); err != nil {
		t.Fatal(err)
	}
	device, err := db.AuthenticateDevice(t.Context(), access, origin)
	if err != nil {
		t.Fatal(err)
	}
	f.device = device.Auth
	f.openLocal()
	return f
}

func (f *transitionTrace) openLocal() {
	log, err := store.Open(f.directory)
	if err != nil {
		f.t.Fatal(err)
	}
	f.local, err = relay.Open(relay.Config{RoomID: "trace-room", Store: log, DynamicBindings: true, OnAppend: func(event model.Event) { f.localEvents = append(f.localEvents, event) }})
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *transitionTrace) sqlSnapshot() relay.EnterpriseRoomSnapshot {
	snapshot, err := f.db.ReadonlySnapshot(f.t.Context(), f.owner, "trace-room")
	if err != nil {
		f.t.Fatal(err)
	}
	return relay.EnterpriseRoomSnapshot{RoomID: snapshot.RoomID, DeploymentID: snapshot.DeploymentID, OrgID: snapshot.OrgID, CreationUserID: snapshot.CreationUserID, HostMode: snapshot.HostMode, Events: snapshot.Events, Seq: snapshot.Seq, Revision: snapshot.Revision}
}
func (f *transitionTrace) bind(slot model.ActorID, kind model.RuntimeKind, label string, replace bool) {
	f.t.Helper()
	request := relay.BindRequest{BindID: "binding-" + label, Runtime: kind, SessionID: "session-" + label, CredentialHash: relay.Digest("secret-" + label), Replace: replace}
	local, err := f.local.Bind(slot, request)
	if err != nil {
		f.t.Fatal(err)
	}
	f.localAuth[slot] = relay.Auth{Slot: slot, Runtime: kind, SessionID: request.SessionID, BindID: request.BindID, Generation: local.Generation, Secret: "secret-" + label}
	source := sha256.Sum256([]byte("source-" + label))
	agent, err := f.db.RegisterAgent(f.t.Context(), f.device, "register-"+label, base64.RawURLEncoding.EncodeToString(source[:]), kind)
	if err != nil {
		f.t.Fatal(err)
	}
	observed := uint64(1)
	if replace {
		observed = local.Generation - 1
	}
	token := sha256.Sum256([]byte("grant-" + label))
	proof := sha256.Sum256([]byte("proof-" + label))
	cmd := sqlstore.BindingRequestCommand{Auth: f.device, ClientID: "request-op-" + label, RequestID: "request-" + label, RoomID: "trace-room", AgentID: agent.Control.AgentID, BindingID: request.BindID, Slot: slot, ObservedGeneration: observed, Replace: replace, ParkEnabled: true, ProofHash: proof, Grant: sqlstore.BindingGrantCandidate{GrantID: "binding-grant-" + label, TokenHash: token}}
	if _, err := f.db.CommitBindingRequest(f.t.Context(), cmd); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.DecideBindingRequest(f.t.Context(), sqlstore.BindingDecisionCommand{Auth: f.owner, RoomID: cmd.RoomID, RequestID: cmd.RequestID, ClientID: "approve-" + label, Approve: true}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.CompleteBindingRequest(f.t.Context(), sqlstore.BindingCompletionCommand{Auth: f.device, RoomID: cmd.RoomID, RequestID: cmd.RequestID, ClientID: "complete-" + label, ProofHash: proof}); err != nil {
		f.t.Fatal(err)
	}
	credential, err := f.db.AuthenticateBinding(f.t.Context(), token, "https://trace-service.example.invalid")
	if err != nil {
		f.t.Fatal(err)
	}
	if credential.Generation != local.Generation {
		f.t.Fatal("binding generations disagree")
	}
	f.binding[slot] = credential
}
func (f *transitionTrace) sqlCommand(slot model.ActorID, id string, intent relay.EnterpriseOperationIntent) sqlstore.RelayCommand {
	snapshot := f.sqlSnapshot()
	cmd := sqlstore.RelayCommand{Auth: f.owner, RoomID: "trace-room", ClientID: id, ExpectedRevision: snapshot.Revision, Intent: intent}
	if slot.ValidParticipant() {
		c := f.binding[slot]
		cmd.Auth, cmd.BindingID, cmd.Generation = c.Auth, c.BindingID, c.Generation
	}
	return cmd
}
func (f *transitionTrace) sqlCommit(slot model.ActorID, id string, intent relay.EnterpriseOperationIntent) sqlstore.Result {
	f.t.Helper()
	result, err := f.db.CommitRelayOperation(f.t.Context(), f.sqlCommand(slot, id, intent))
	if err != nil || result.Outcome != sqlstore.OutcomeCommitted || result.Relay == nil {
		f.t.Fatalf("trace SQL %s: outcome=%s err=%v", id, result.Outcome, err)
	}
	return result
}
func (f *transitionTrace) alias(local, sql, label string) {
	for side, id := range []string{local, sql} {
		if id == "" {
			f.t.Fatal("empty generated identity")
		}
		if prior, ok := f.ids[side][id]; ok && prior != label {
			f.t.Fatal("message alias is not bijective")
		}
		for existing, name := range f.ids[side] {
			if name == label && existing != id {
				f.t.Fatal("message alias collapsed distinct identities")
			}
		}
		f.ids[side][id] = label
	}
}
func (f *transitionTrace) id(side int, label string) string {
	for id, name := range f.ids[side] {
		if name == label {
			return id
		}
	}
	f.t.Fatalf("unknown symbolic message %s", label)
	return ""
}
func (f *transitionTrace) send(slot model.ActorID, label, text string, peer uint64) {
	f.bodies[label] = text
	local, err := f.local.Send(f.localAuth[slot], relay.SendRequest{ID: label, Text: text, ExpectedPeerGeneration: peer})
	if err != nil {
		f.t.Fatal(err)
	}
	sql := f.sqlCommit(slot, label, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: text, ExpectedPeerGeneration: peer})
	f.alias(local.ID, sql.Relay.Message.ID, label)
}
func (f *transitionTrace) claim(slot model.ActorID, label string, stopPark ...bool) (string, string) {
	ctx, cancel := context.WithTimeout(f.t.Context(), time.Second)
	defer cancel()
	park := len(stopPark) == 1 && stopPark[0]
	local, err := f.local.Claim(ctx, f.localAuth[slot], park)
	if err != nil || local == nil {
		f.t.Fatal("Local claim failed", err)
	}
	sql := f.sqlCommit(slot, "claim-"+label, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim, ParkEnabled: park}).Relay.Claim
	if sql == nil || local.ID != f.id(0, label) || sql.ID != f.id(1, label) || local.PeerGeneration != sql.PeerGeneration {
		f.t.Fatal("FIFO claim or peer generation disagreed")
	}
	f.assertEnvelope(slot, label, *local, *sql)
	for side, receipt := range []string{local.Receipt, sql.Receipt} {
		if receipt == "" {
			f.t.Fatal("claim lacks receipt")
		}
		if _, exists := f.receipts[side][receipt]; exists {
			f.t.Fatal("claim reused another delivery receipt")
		}
		f.receipts[side][receipt] = "receipt-" + label
	}
	return local.Receipt, sql.Receipt
}
func (f *transitionTrace) ack(slot model.ActorID, label, lReceipt, sReceipt, id string) {
	if err := f.local.Ack(f.localAuth[slot], f.id(0, label), lReceipt); err != nil {
		f.t.Fatal(err)
	}
	f.sqlCommit(slot, id, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: f.id(1, label), Receipt: sReceipt})
}
func (f *transitionTrace) restart() {
	f.restartWithReaped(1)
}
func (f *transitionTrace) restartWithReaped(wantReaped uint64) {
	if err := f.local.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.openLocal()
	if err := f.db.Close(); err != nil {
		f.t.Fatal(err)
	}
	db, err := sqlstore.Open(f.t.Context(), f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.db = db
	report, err := db.RecoverDeliveries(f.t.Context())
	if err != nil || !report.Complete || report.Reaped != wantReaped {
		f.t.Fatalf("SQL restart reaped=%d want=%d error=%v", report.Reaped, wantReaped, err)
	}
}

// This text-only trace's envelopes carry no random IDs or timestamps. Compare
// the actual full strings without normalization and against an independent
// literal envelope oracle; do not substitute a body-only or nonempty check.
func (f *transitionTrace) assertEnvelope(slot model.ActorID, label string, local, sql relay.Claim) {
	f.t.Helper()
	body, ok := f.bodies[label]
	if !ok {
		f.t.Fatal("claim lacks its original caller body oracle")
	}
	from, runtime, generation := "@agent1", "claude", uint64(1)
	if label == "historical-source" {
		from, runtime = "@agent2", "codex"
	}
	peer := "@agent1"
	if slot == model.ActorSlot1 {
		peer = "@agent2"
		generation = 2 // replacement changes the current peer, not the original source.
	}
	want := fmt.Sprintf("[PairRoom message]\nfrom: %s (%s, generation 1)\npeer: %s, generation %d\n\n%s", from, runtime, peer, generation, body)
	if local.Envelope != sql.Envelope || local.Envelope != want {
		f.t.Fatalf("%s full envelope mismatch: Local=%q SQL=%q want=%q", label, local.Envelope, sql.Envelope, want)
	}
	for other, text := range f.bodies {
		if other != label && text != body && strings.Contains(local.Envelope, text) {
			f.t.Fatal("claim recursively forwarded unrelated retained history")
		}
	}
	if strings.Count(local.Envelope, "[PairRoom message]") != 1 || strings.Contains(local.Envelope, "private late output") {
		f.t.Fatal("claim included nested relay envelope or retained private late output")
	}
}

func (f *transitionTrace) park(slot model.ActorID, enabled bool, id string, changed bool) {
	f.t.Helper()
	localBefore := len(f.localEvents)
	sqlBefore := f.sqlSnapshot()
	auth, credential := f.localAuth[slot], f.binding[slot]
	var oldSQL model.EnterpriseBinding
	for _, event := range sqlBefore.Events {
		if event.Kind == model.EnterpriseBindingUpdatedKind {
			binding, err := model.DecodeEnterpriseBindingUpdate(event)
			if err != nil {
				f.t.Fatal(err)
			}
			if binding.Slot == slot {
				oldSQL = binding
			}
		}
	}
	if err := f.local.ParkAs(auth, enabled); err != nil {
		f.t.Fatal(err)
	}
	command := f.sqlCommand(slot, id, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationPark, ParkEnabled: enabled})
	result, err := f.db.CommitRelayOperation(f.t.Context(), command)
	if err != nil || result.Outcome != sqlstore.OutcomeCommitted || result.Relay == nil || result.Relay.Binding == nil {
		f.t.Fatal("typed Park failed", err)
	}
	wantDelta := 0
	if changed {
		wantDelta = 1
	}
	sqlAfter := f.sqlSnapshot()
	if len(f.localEvents)-localBefore != wantDelta || len(sqlAfter.Events)-len(sqlBefore.Events) != wantDelta || len(result.Facts) != wantDelta {
		f.t.Fatal("Park raw binding fact delta was not exactly changed=1/noop=0")
	}
	wantSQL := oldSQL
	wantSQL.ParkEnabled = enabled
	if !reflect.DeepEqual(*result.Relay.Binding, wantSQL) || wantSQL.AgentID != credential.AgentID || !wantSQL.Active || wantSQL.Runtime != model.RuntimeCodex || wantSQL.Generation != 1 {
		f.t.Fatal("Park changed original Agent/Binding/runtime/generation/active identity")
	}
	if changed {
		localFact, sqlFact := f.localEvents[localBefore], result.Facts[0]
		var localBinding struct {
			relay.Binding
			CredentialHash string `json:"credential_hash"`
		}
		if localFact.Kind != relay.EventBinding || localFact.Actor != slot || json.Unmarshal(localFact.Data, &localBinding) != nil || localBinding.ParkEnabled != enabled || localBinding.BindID != auth.BindID || localBinding.Runtime != auth.Runtime || localBinding.Generation != auth.Generation || !localBinding.Active || localBinding.SessionID != auth.SessionID || localBinding.CredentialHash != relay.Digest(auth.Secret) {
			f.t.Fatal("Local Park binding fact changed a credential or original identity")
		}
		binding, err := model.DecodeEnterpriseBindingUpdate(sqlFact)
		if err != nil || sqlFact.Kind != model.EnterpriseBindingUpdatedKind || sqlFact.Actor != slot || !reflect.DeepEqual(binding, wantSQL) {
			f.t.Fatal("SQL Park binding fact changed original identity", err)
		}
	}
	current, err := f.local.Inspect(auth)
	if err != nil || current.ParkEnabled != enabled || current.BindID != auth.BindID || current.Generation != auth.Generation {
		f.t.Fatal("Local Park lost live original credential", err)
	}
	if _, err := f.db.ReadonlySnapshot(f.t.Context(), credential.Auth, "trace-room"); err != nil {
		f.t.Fatal("SQL Park lost original narrow credential", err)
	}
	replayed, err := f.db.ResolveRelayOperation(f.t.Context(), command)
	if err != nil || replayed.Outcome != sqlstore.OutcomeReplayed || !reflect.DeepEqual(replayed.Relay, result.Relay) || !reflect.DeepEqual(replayed.Facts, result.Facts) || replayed.Seq != result.Seq || replayed.Revision != result.Revision {
		f.t.Fatal("Park original changed/noop accepted receipt did not replay exactly", err)
	}
	if f.local.Snapshot().Bindings[slot].ParkEnabled != enabled {
		f.t.Fatal("independent literal Park policy oracle failed")
	}
}

type traceMessage struct {
	Message                   relay.Message
	Created, Updated, Claimed bool
}
type traceBinding struct {
	Slot         model.ActorID
	Runtime      model.RuntimeKind
	Generation   uint64
	Active, Park bool
}
type traceView struct {
	Protocol string
	Dynamic  bool
	Host     model.HostMode
	Bindings map[model.ActorID]traceBinding
	Targets  map[model.ActorID]uint64
	Messages []traceMessage
}

func (f *transitionTrace) message(side int, m relay.Message) traceMessage {
	alias, ok := f.ids[side][m.ID]
	if !ok {
		f.t.Fatalf("unmapped message identity %s", m.ID)
	}
	m.ID = alias
	if m.RetryOf != "" {
		mapped, ok := f.ids[side][m.RetryOf]
		if !ok {
			f.t.Fatal("RetryOf lost original identity")
		}
		m.RetryOf = mapped
	}
	created, updated, claimed := !m.CreatedAt.IsZero(), !m.UpdatedAt.IsZero(), !m.ClaimedAt.IsZero()
	if !created || !updated || m.UpdatedAt.Before(m.CreatedAt) || claimed && m.ClaimedAt.Before(m.CreatedAt) {
		f.t.Fatal("invalid materialized message time relationship")
	}
	m.CreatedAt, m.UpdatedAt, m.ClaimedAt = time.Time{}, time.Time{}, time.Time{}
	if m.Receipt != "" {
		receipt, ok := f.receipts[side][m.Receipt]
		if !ok {
			f.t.Fatal("private receipt has no captured claim identity")
		}
		m.Receipt = receipt
	}
	return traceMessage{Message: m, Created: created, Updated: updated, Claimed: claimed}
}
func (f *transitionTrace) view(side int, snapshot relay.Snapshot) traceView {
	v := traceView{Protocol: snapshot.Protocol, Dynamic: snapshot.DynamicBindings, Host: snapshot.HostMode, Bindings: map[model.ActorID]traceBinding{}, Targets: snapshot.TargetGenerations, Messages: []traceMessage{}}
	for slot, b := range snapshot.Bindings {
		v.Bindings[slot] = traceBinding{Slot: b.Slot, Runtime: b.Runtime, Generation: b.Generation, Active: b.Active, Park: b.ParkEnabled}
	}
	for _, m := range snapshot.Messages {
		v.Messages = append(v.Messages, f.message(side, m))
	}
	return v
}

type traceFact struct {
	Kind        string
	Actor       model.ActorID
	Message     *traceMessage
	Publication *relay.Publication
	ClientKey   string
}

func (f *transitionTrace) facts(side int, events []model.Event) []traceFact {
	facts := []traceFact{}
	for _, event := range events {
		if event.Kind == relay.EventMessage {
			var fact struct {
				relay.Message
				Receipt   string `json:"receipt"`
				ClientKey string `json:"client_key"`
			}
			if json.Unmarshal(event.Data, &fact) != nil {
				f.t.Fatal("invalid durable message fact")
			}
			fact.Message.Receipt = fact.Receipt
			message := f.message(side, fact.Message)
			facts = append(facts, traceFact{Kind: event.Kind, Actor: event.Actor, Message: &message, ClientKey: fact.ClientKey})
		} else if event.Kind == relay.EventPublication {
			var publication relay.Publication
			if json.Unmarshal(event.Data, &publication) != nil {
				f.t.Fatal("invalid publication fact")
			}
			var message *traceMessage
			if publication.Message != nil {
				m := f.message(side, *publication.Message)
				message = &m
				publication.Message = nil
			}
			facts = append(facts, traceFact{Kind: event.Kind, Actor: event.Actor, Message: message, Publication: &publication})
		}
	}
	return facts
}

// Only generated message IDs and wall-clock values are normalized. Identity
// aliases are bijections; private receipts retain their exact claim/ACK/recovery
// relation. Author, Runtime, generations, state, source and RetryOf stay intact.
// Authentication/setup events and their cursors have different owning contracts;
// each protocol step instead compares its exact Message/Publication fact delta.
func (f *transitionTrace) check(name string, wantFacts int, states map[string]string) {
	f.t.Helper()
	f.step++
	sqlSnapshot := f.sqlSnapshot()
	sqlView, err := relay.ProjectEnterpriseSnapshot(sqlSnapshot)
	if err != nil {
		f.t.Fatal(err)
	}
	localView := f.view(0, f.local.Snapshot())
	normalizedSQL := f.view(1, sqlView)
	if !reflect.DeepEqual(localView, normalizedSQL) {
		l, _ := json.Marshal(localView)
		s, _ := json.Marshal(normalizedSQL)
		f.t.Fatalf("%s semantic mismatch Local=%s SQL=%s", name, l, s)
	}
	gotStates := map[string]string{}
	for _, m := range localView.Messages {
		message := m.Message
		from, runtime, target, source, retryOf := model.ActorSlot1, model.RuntimeClaude, uint64(1), "send", ""
		switch message.ID {
		case "historical-source":
			from, runtime = model.ActorSlot2, model.RuntimeCodex
		case "report":
			source = "stop"
		case "retry":
			source, retryOf = "retry", "retry-original"
		case "new-target":
			target = 2
		}
		if message.Author != "" || message.From != from || message.To != model.OtherParticipant(from) || message.FromRuntime != runtime || message.FromGeneration != 1 || message.TargetGeneration != target || message.Source != source || message.RetryOf != retryOf {
			f.t.Fatalf("%s independent original identity oracle failed for %s", name, message.ID)
		}
		gotStates[message.ID] = message.State
	}
	if !reflect.DeepEqual(gotStates, states) {
		f.t.Fatalf("%s independent state oracle: got=%v want=%v", name, gotStates, states)
	}
	localFacts := f.facts(0, f.localEvents[f.localCursor:])
	sqlFacts := f.facts(1, sqlSnapshot.Events[f.sqlCursor:])
	if len(localFacts) != wantFacts || len(sqlFacts) != wantFacts || !reflect.DeepEqual(localFacts, sqlFacts) {
		l, _ := json.Marshal(localFacts)
		s, _ := json.Marshal(sqlFacts)
		f.t.Fatalf("%s exactly-once fact mismatch want=%d Local=%s SQL=%s", name, wantFacts, l, s)
	}
	f.localCursor, f.sqlCursor = len(f.localEvents), len(sqlSnapshot.Events)
	f.t.Logf("step %02d %s: semantic parity, protocol_facts=%d states=%v", f.step, name, wantFacts, states)
}

func TestLocalSQLFullNativeTransitionTraceParity(t *testing.T) {
	f := newTransitionTrace(t)
	states := map[string]string{}
	f.check("creation-vacant", 0, states)
	f.bind(model.ActorSlot1, model.RuntimeClaude, "a", false)
	f.check("bind-slot1-claude-gen1", 0, states)
	f.bind(model.ActorSlot2, model.RuntimeCodex, "b", false)
	f.check("bind-slot2-codex-gen1", 0, states)
	f.park(model.ActorSlot2, false, "park-off", true)
	f.check("park-off-binding-fact-changed-one", 0, states)
	f.park(model.ActorSlot2, false, "park-off-noop", false)
	f.check("park-off-noop-original-receipt", 0, states)
	f.send(model.ActorSlot1, "park-foreground", "PARK-FOREGROUND full body 中文\nsecond line END-FOREGROUND", 1)
	states["park-foreground"] = "queued"
	f.check("queued-while-stop-park-disabled", 1, states)
	localBefore := len(f.localEvents)
	sqlBefore := f.sqlSnapshot()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	empty, err := f.local.Claim(ctx, f.localAuth[model.ActorSlot2], true)
	cancel()
	if err != nil || empty != nil {
		t.Fatal("disabled Local Stop park consumed queued work", err)
	}
	emptySQL := f.sqlCommit(model.ActorSlot2, "disabled-stop-park-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim, ParkEnabled: true})
	if !emptySQL.Relay.Empty || emptySQL.Relay.Claim != nil || len(emptySQL.Facts) != 0 || len(f.localEvents) != localBefore || len(f.sqlSnapshot().Events) != len(sqlBefore.Events) {
		t.Fatal("disabled SQL/Local Stop park emitted a claim/envelope or raw fact")
	}
	f.check("disabled-stop-park-empty-zero-raw-facts", 0, states)
	f.restartWithReaped(0)
	if f.local.Snapshot().Bindings[model.ActorSlot2].ParkEnabled {
		t.Fatal("Local reopen forgot persisted disabled policy")
	}
	parkProjection, err := relay.ProjectEnterpriseSnapshot(f.sqlSnapshot())
	if err != nil || parkProjection.Bindings[model.ActorSlot2].ParkEnabled {
		t.Fatal("SQL reopen forgot persisted disabled policy", err)
	}
	f.check("reopen-no-delivery-disabled-policy-preserved", 0, states)
	parkLocalReceipt, parkSQLReceipt := f.claim(model.ActorSlot2, "park-foreground")
	states["park-foreground"] = "delivering"
	f.check("foreground-claim-works-while-park-disabled", 1, states)
	f.ack(model.ActorSlot2, "park-foreground", parkLocalReceipt, parkSQLReceipt, "park-foreground-ack")
	states["park-foreground"] = "handed_off"
	f.check("foreground-ack-while-park-disabled", 1, states)
	f.park(model.ActorSlot2, true, "park-on", true)
	f.check("park-on-binding-fact-changed-one", 0, states)
	f.park(model.ActorSlot2, true, "park-on-noop", false)
	f.check("park-on-noop-original-receipt", 0, states)
	f.send(model.ActorSlot1, "park-stop", "PARK-STOP full body 中文\nsecond line END-STOP", 1)
	states["park-stop"] = "queued"
	f.check("queued-while-stop-park-enabled", 1, states)
	parkLocalReceipt, parkSQLReceipt = f.claim(model.ActorSlot2, "park-stop", true)
	states["park-stop"] = "delivering"
	f.check("enabled-stop-park-claims-full-envelope", 1, states)
	f.ack(model.ActorSlot2, "park-stop", parkLocalReceipt, parkSQLReceipt, "park-stop-ack")
	states["park-stop"] = "handed_off"
	f.check("enabled-stop-park-ack", 1, states)
	f.send(model.ActorSlot1, "first", "first native send", 1)
	states["first"] = "queued"
	f.check("send", 1, states)
	l, s := f.claim(model.ActorSlot2, "first")
	states["first"] = "delivering"
	f.check("claim", 1, states)
	f.ack(model.ActorSlot2, "first", l, s, "ack-first")
	states["first"] = "handed_off"
	f.check("ack", 1, states)
	f.ack(model.ActorSlot2, "first", l, s, "ack-first-noop")
	f.check("original-ack-noop", 0, states)
	// Local returns current message state on same-ID send while SQL returns its
	// frozen accepted receipt. Both must preserve identity and append zero facts.
	localAgain, err := f.local.Send(f.localAuth[model.ActorSlot1], relay.SendRequest{ID: "first", Text: "first native send", ExpectedPeerGeneration: 1})
	if err != nil || localAgain.ID != f.id(0, "first") || localAgain.State != "handed_off" {
		t.Fatal("Local same-ID send did not preserve current original")
	}
	accepted, err := f.db.CommitRelayOperation(t.Context(), f.sqlCommand(model.ActorSlot1, "first", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "first native send", ExpectedPeerGeneration: 1}))
	if err != nil || accepted.Outcome != sqlstore.OutcomeReplayed || len(accepted.Facts) != 1 || accepted.Relay.Message.ID != f.id(1, "first") || accepted.Relay.Message.State != "queued" {
		t.Fatal("SQL frozen original send receipt changed")
	}
	f.check("same-id-send-no-duplicate-original-receipt", 0, states)
	f.bodies["report"] = "@agent2 full report body"
	localReport, err := f.local.Report(f.localAuth[model.ActorSlot1], 1, "@agent2 full report body", 1)
	if err != nil {
		t.Fatal(err)
	}
	sqlReport := f.sqlCommit(model.ActorSlot1, "report-one", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationReport, ReportSeq: 1, Text: "@agent2 full report body", ExpectedPeerGeneration: 1})
	f.alias(localReport.Message.ID, sqlReport.Relay.Publication.Message.ID, "report")
	states["report"] = "queued"
	f.check("addressed-report", 1, states)
	l, s = f.claim(model.ActorSlot2, "report")
	states["report"] = "delivering"
	f.check("report-claim", 1, states)
	f.ack(model.ActorSlot2, "report", l, s, "ack-report")
	states["report"] = "handed_off"
	f.check("report-ack", 1, states)
	originalReport, err := f.local.Report(f.localAuth[model.ActorSlot1], 1, "@agent2 full report body", 1)
	if err != nil || originalReport.Message.ID != f.id(0, "report") {
		t.Fatal("Local report replay changed original identity")
	}
	accepted, err = f.db.CommitRelayOperation(t.Context(), f.sqlCommand(model.ActorSlot1, "report-one-replay-transport", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationReport, ReportSeq: 1, Text: "@agent2 full report body", ExpectedPeerGeneration: 1}))
	if err != nil || accepted.Outcome != sqlstore.OutcomeReplayed || len(accepted.Facts) != 1 || accepted.Relay.Publication.Message.ID != f.id(1, "report") {
		t.Fatal("SQL report sequence replay changed original")
	}
	f.check("same-report-sequence-no-duplicate", 0, states)
	f.send(model.ActorSlot1, "late-ack", "original receipt survives recovery", 1)
	states["late-ack"] = "queued"
	f.check("send-for-unknown-ack", 1, states)
	l, s = f.claim(model.ActorSlot2, "late-ack")
	states["late-ack"] = "delivering"
	f.check("claim-for-unknown-ack", 1, states)
	f.restart()
	states["late-ack"] = "unknown"
	f.check("restart-delivering-to-unknown", 1, states)
	f.ack(model.ActorSlot2, "late-ack", l, s, "settle-original-unknown")
	states["late-ack"] = "handed_off"
	f.check("unknown-original-receipt-settlement", 1, states)
	f.send(model.ActorSlot1, "retry-original", "explicit retry preserves native origin", 1)
	states["retry-original"] = "queued"
	f.check("send-for-retry", 1, states)
	l, s = f.claim(model.ActorSlot2, "retry-original")
	states["retry-original"] = "delivering"
	f.check("claim-for-retry", 1, states)
	f.restart()
	states["retry-original"] = "unknown"
	f.check("restart-for-explicit-retry", 1, states)
	localRetry, err := f.local.Retry(f.id(0, "retry-original"))
	if err != nil {
		t.Fatal(err)
	}
	sqlRetry := f.sqlCommit("", "retry-owner", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationRetry, MessageID: f.id(1, "retry-original")})
	f.alias(localRetry.ID, sqlRetry.Relay.Message.ID, "retry")
	f.bodies["retry"] = f.bodies["retry-original"]
	states["retry"] = "queued"
	f.check("owner-explicit-retry", 1, states)
	before := f.local.Sequence()
	if err := f.local.Ack(f.localAuth[model.ActorSlot2], f.id(0, "retry-original"), l); err == nil {
		t.Fatal("pending Retry accepted original ACK")
	}
	result, err := f.db.CommitRelayOperation(t.Context(), f.sqlCommand(model.ActorSlot2, "blocked-original-ack", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: f.id(1, "retry-original"), Receipt: s}))
	var kernel *sqlstore.KernelError
	if !errors.As(err, &kernel) || kernel.Class != sqlstore.ClassInvalidControlState || result.Relay != nil || f.local.Sequence() != before {
		t.Fatal("pending Retry ACK gate changed")
	}
	f.check("pending-retry-refuses-original-ack", 0, states)
	if err := f.local.Cancel(f.id(0, "retry")); err != nil {
		t.Fatal(err)
	}
	f.sqlCommit("", "cancel-retry", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationCancel, MessageID: f.id(1, "retry")})
	states["retry"] = "cancelled"
	f.check("owner-cancels-retry", 1, states)
	f.ack(model.ActorSlot2, "retry-original", l, s, "settle-after-retry-cancel")
	states["retry-original"] = "handed_off"
	f.check("original-ack-after-retry-cancel", 1, states)
	f.send(model.ActorSlot1, "cancel-queued", "explicit cancelled work", 1)
	states["cancel-queued"] = "queued"
	f.check("send-for-cancel", 1, states)
	if err := f.local.Cancel(f.id(0, "cancel-queued")); err != nil {
		t.Fatal(err)
	}
	f.sqlCommit("", "cancel-owner", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationCancel, MessageID: f.id(1, "cancel-queued")})
	states["cancel-queued"] = "cancelled"
	f.check("owner-cancels-queued", 1, states)
	f.send(model.ActorSlot2, "historical-source", "Codex generation one historical source", 1)
	states["historical-source"] = "queued"
	f.check("historical-source-before-replacement", 1, states)
	f.send(model.ActorSlot1, "old-target", "queued to old target generation", 1)
	states["old-target"] = "queued"
	f.check("old-target-before-replacement", 1, states)
	f.bind(model.ActorSlot2, model.RuntimeGrok, "replacement", true)
	states["old-target"] = "cancelled"
	f.check("replacement-grok-gen2-derives-cancel", 0, states)
	late, err := f.local.Report(f.localAuth[model.ActorSlot1], 2, "@agent2 private late output", 1)
	if err != nil || late.Outcome != relay.PeerChangedCode || late.Message != nil {
		t.Fatal("Local late report forwarded")
	}
	lateSQL := f.sqlCommit(model.ActorSlot1, "late-report", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationReport, ReportSeq: 2, Text: "@agent2 private late output", ExpectedPeerGeneration: 1})
	if lateSQL.Relay.Publication.Outcome != relay.PeerChangedCode || lateSQL.Relay.Publication.Message != nil {
		t.Fatal("SQL late report forwarded")
	}
	f.check("late-old-peer-report-terminal-no-body", 1, states)
	for _, events := range [][]model.Event{f.localEvents, f.sqlSnapshot().Events} {
		for _, event := range events {
			if strings.Contains(string(event.Data), "private late output") {
				t.Fatal("terminal peer_changed persisted private late body")
			}
		}
	}
	if _, err := f.local.Send(f.localAuth[model.ActorSlot1], relay.SendRequest{ID: "late-send", Text: "private old target", ExpectedPeerGeneration: 1}); !errors.Is(err, relay.ErrPeerChanged) {
		t.Fatal("Local late send accepted")
	}
	result, err = f.db.CommitRelayOperation(t.Context(), f.sqlCommand(model.ActorSlot1, "late-send", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "private old target", ExpectedPeerGeneration: 1}))
	if !errors.As(err, &kernel) || kernel.Class != sqlstore.ClassPeerChanged || result.Relay != nil {
		t.Fatal("SQL late send accepted")
	}
	f.check("late-old-peer-send-refused", 0, states)
	l, s = f.claim(model.ActorSlot1, "historical-source")
	states["historical-source"] = "delivering"
	f.check("claim-historical-source-with-current-peer2", 1, states)
	f.ack(model.ActorSlot1, "historical-source", l, s, "ack-historical")
	states["historical-source"] = "handed_off"
	f.check("ack-preserves-original-codex-gen1", 1, states)
	f.send(model.ActorSlot1, "new-target", "fresh generation two work", 2)
	states["new-target"] = "queued"
	f.check("fresh-peer-gen2-send", 1, states)
	l, s = f.claim(model.ActorSlot2, "new-target")
	states["new-target"] = "delivering"
	f.check("grok-gen2-claim", 1, states)
	f.ack(model.ActorSlot2, "new-target", l, s, "ack-new-target")
	states["new-target"] = "handed_off"
	f.check("grok-gen2-ack", 1, states)
}

// Archive is an owner-layer compatibility countercase, not another equal
// ordering in the shared trace. Local Management settles receipts while
// draining, before Registry.ArchiveRoom; after archive nativeRelay refuses all
// old credentials (service/native_host_api.go). SQL permits original settlement
// after typed archive. TestNativeHTTPAckSettlesDrainingRuntime separately owns
// the Local drain/ACK boundary; Engine.Close in the core trace is only recovery.
func TestSQLTypedArchiveOriginalReceiptAndNewEffectCompatibilityWindow(t *testing.T) {
	f := newTransitionTrace(t)
	f.bind(model.ActorSlot1, model.RuntimeClaude, "a", false)
	f.bind(model.ActorSlot2, model.RuntimeCodex, "b", false)
	f.send(model.ActorSlot1, "archive-original", "accepted before archive", 1)
	_, receipt := f.claim(model.ActorSlot2, "archive-original")
	snapshot := f.sqlSnapshot()
	result, err := f.db.CommitCatalog(t.Context(), sqlstore.CatalogCommand{Auth: f.owner, ClientID: "archive-room", ExpectedRevision: snapshot.Revision, Intent: sqlstore.CatalogIntent{Kind: "room.archive", RoomID: "trace-room"}})
	if err != nil || result.Outcome != sqlstore.OutcomeCommitted {
		t.Fatal("typed archive failed", err)
	}
	for i, intent := range []relay.EnterpriseOperationIntent{{Kind: relay.EnterpriseOperationSend, Text: "no archived effect", ExpectedPeerGeneration: 1}, {Kind: relay.EnterpriseOperationClaim}, {Kind: relay.EnterpriseOperationRetry, MessageID: f.id(1, "archive-original")}} {
		slot := model.ActorSlot1
		if intent.Kind == relay.EnterpriseOperationClaim {
			slot = model.ActorSlot2
		}
		if intent.Kind == relay.EnterpriseOperationRetry {
			slot = ""
		}
		before := f.sqlSnapshot()
		r, e := f.db.CommitRelayOperation(t.Context(), f.sqlCommand(slot, fmt.Sprintf("archived-new-%d", i), intent))
		var kernel *sqlstore.KernelError
		if !errors.As(e, &kernel) || kernel.Class != sqlstore.ClassInvalidControlState || r.Relay != nil {
			t.Fatal("typed archive admitted new effect")
		}
		after := f.sqlSnapshot()
		if !reflect.DeepEqual(before, after) {
			t.Fatal("archived rejected operation wrote Room state")
		}
	}
	ack := f.sqlCommit(model.ActorSlot2, "archive-original-ack", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: f.id(1, "archive-original"), Receipt: receipt})
	if !ack.Relay.HandedOff || len(ack.Facts) != 1 {
		t.Fatal("typed archived original receipt could not settle exactly once")
	}
	projected, err := relay.ProjectEnterpriseSnapshot(f.sqlSnapshot())
	if err != nil || len(projected.Messages) != 1 || projected.Messages[0].State != "handed_off" || projected.Messages[0].FromRuntime != model.RuntimeClaude || projected.Messages[0].FromGeneration != 1 || projected.Messages[0].TargetGeneration != 1 {
		t.Fatal("archive settlement relabeled original identity", err)
	}
	t.Log("SQL compatibility window: typed archive -> new send/claim/retry refused -> original receipt ACK handed_off; Local uses ACK while draining before archive")
}
