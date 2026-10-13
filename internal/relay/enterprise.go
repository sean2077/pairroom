package relay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// This file is the enterprise wiring boundary for one Room engine: a minimal
// text-only human-send command executed through an injected port, with a
// per-Room command gate, a read-only rebuild path and the explicit recovery
// boundary. The Local engine paths are untouched; every legacy mutator is
// rejected at its entry on an enterprise engine.

const (
	// enterpriseCommandBudget bounds the whole enterprise command — gate
	// queueing, SQL and apply/rebuild — taking the earlier of this budget and
	// the caller's deadline. It matches the kernel's operation budget.
	enterpriseCommandBudget = 60 * time.Second
	// enterpriseOpenBudget bounds the constructor and its snapshot read.
	enterpriseOpenBudget = 30 * time.Second
	// enterpriseCloseBudget bounds the Close() wrapper around CloseContext.
	enterpriseCloseBudget = 30 * time.Second
	// enterpriseClientKeyPrefix mirrors the kernel's principal-scoped durable
	// client key form ("user/<UserID>/<clientID>").
	enterpriseClientKeyPrefix = "user/"
)

// Stable outcome/class vocabulary shared with the SQL port; the adapter maps
// its kernel outcomes onto these.
const (
	EnterpriseOutcomeCommitted    = "committed"
	EnterpriseOutcomeReplayed     = "replayed"
	EnterpriseOutcomeRefused      = "refused"
	EnterpriseOutcomeNotCommitted = "not_committed"
	EnterpriseOutcomeUnknown      = "unknown"

	EnterpriseClassRevisionConflict = "revision_conflict"
	EnterpriseClassPayloadConflict  = "payload_conflict"
	EnterpriseClassAuthorization    = "authorization"
)

// ErrEnterpriseReadOnly rejects every legacy mutator on an enterprise engine
// at its entry, before any persistent, transient or external effect. The
// append path rejects as well, but only as a last line of defense.
var ErrEnterpriseReadOnly = errors.New("relay: legacy mutators are unavailable on an enterprise engine")

// EnterpriseAuth is the authenticated principal the port acts as.
type EnterpriseAuth struct {
	Deployment string
	UserID     string
	// SessionID is the enterprise application's authenticated session ID,
	// never a vendor-native SessionID or a caller-supplied command field.
	SessionID      string
	DeviceGrantID  string
	BindingGrantID string
}

// EnterpriseSendIntent is the typed stable intent of one human send.
type EnterpriseSendIntent struct {
	To   string
	Text string
}

// EnterpriseSendCommand is one human-send command for the port.
type EnterpriseSendCommand struct {
	Auth             EnterpriseAuth
	RoomID           string
	ClientID         string
	Intent           EnterpriseSendIntent
	ExpectedRevision uint64
	ExpectedSeq      uint64
	Fact             model.Event
}

// EnterpriseSendResult mirrors the kernel result: the fact that won, with its
// sequence and the room revision, as independent fields.
type EnterpriseSendResult struct {
	Outcome  string
	Fact     model.Event
	Seq      uint64
	Revision uint64
}

// EnterpriseRoomSnapshot is the authenticated read-only snapshot the
// constructor and rebuild path consume.
type EnterpriseRoomSnapshot struct {
	DeploymentID   string
	OrgID          string
	CreationUserID string
	RoomID         string
	HostMode       string
	Events         []model.Event
	Seq            uint64
	Revision       uint64
}

// EnterpriseCommandError carries the port's stable outcome and class.
type EnterpriseCommandError struct {
	Outcome string
	Class   string
	Err     error
}

func (e *EnterpriseCommandError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("relay enterprise %s (%s): %v", e.Outcome, e.Class, e.Err)
	}
	return fmt.Sprintf("relay enterprise %s (%s)", e.Outcome, e.Class)
}

func (e *EnterpriseCommandError) Unwrap() error { return e.Err }

// enterpriseDeployment is fixed at construction; the acting user is per
// command. openAuth is used for the initial snapshot only.
type enterpriseState struct {
	deployment          string
	orgID               string
	creationUserID      string
	creationEventID     string
	creationPayloadHash [sha256.Size]byte
	openAuth            EnterpriseAuth
}

// EnterprisePort is implemented by the SQL command port (the sqlstore
// adapter); this package never imports the SQL side.
type EnterprisePort interface {
	CommitHumanSend(ctx context.Context, cmd EnterpriseSendCommand) (EnterpriseSendResult, error)
	ResolveHumanSend(ctx context.Context, auth EnterpriseAuth, roomID, clientID string, intent EnterpriseSendIntent) (EnterpriseSendResult, error)
	ReadonlySnapshot(ctx context.Context, auth EnterpriseAuth, roomID string) (EnterpriseRoomSnapshot, error)
}

// nilEnterprisePort reports whether the port is nil, including a typed-nil
// pointer stored in the interface.
func nilEnterprisePort(port EnterprisePort) bool {
	if port == nil {
		return true
	}
	value := reflect.ValueOf(port)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	}
	return false
}

// emptyEngine builds the empty engine shell both Open and OpenEnterprise
// replay into, over an isolated configuration copy: the shared Runtimes map
// (and any other map the configuration carries) is cloned so a failed replay
// can never pollute the caller's or an existing engine's state.
func emptyEngine(cfg Config) *Engine {
	cfg = cloneConfig(cfg)
	return &Engine{cfg: cfg, bindings: map[model.ActorID]bindingFact{}, seenBinds: map[string]bool{}, messages: map[string]Message{}, inFlight: map[string]struct{}{}, sends: map[string]string{}, reports: map[string]Publication{}, lastReport: map[string]uint64{}, changed: make(chan struct{}), wakeEnabled: true, wakeReserved: map[string]bool{}, waiters: map[model.ActorID]int{}}
}

// cloneConfig isolates the configuration's shared maps.
func cloneConfig(cfg Config) Config {
	if cfg.Runtimes != nil {
		runtimes := make(map[model.ActorID]model.RuntimeKind, len(cfg.Runtimes))
		for slot, kind := range cfg.Runtimes {
			runtimes[slot] = kind
		}
		cfg.Runtimes = runtimes
	}
	return cfg
}

// requireLocalMutation rejects a legacy mutator at its entry on an
// enterprise engine. It is the entry gate; append's own rejection is only the
// last line of defense.
func (e *Engine) requireLocalMutation() error {
	if e.enterprise {
		return ErrEnterpriseReadOnly
	}
	return nil
}

// OpenEnterprise constructs the enterprise wiring for one Room from an
// authenticated read-only snapshot: the snapshot is replayed through the same
// apply implementation, its identity and contiguity are validated first, and
// the engine adopts the resulting projection. It never calls the reap-writing
// Open, never fabricates Bindings or Runtimes from current configuration
// defaults, and the shared SQL store is only borrowed — nothing here owns or
// closes it.
func OpenEnterprise(ctx context.Context, cfg Config, port EnterprisePort, auth EnterpriseAuth) (*Engine, error) {
	if nilEnterprisePort(port) {
		return nil, errors.New("relay: enterprise port is required")
	}
	if cfg.RoomID == "" || auth.Deployment == "" || auth.UserID == "" {
		return nil, errors.New("relay: enterprise Room, deployment and user are required")
	}
	if cfg.SharedSlot != "" {
		return nil, errors.New("relay: enterprise Room cannot inherit LAN sharing")
	}
	for slot, runtime := range cfg.Runtimes {
		if !slot.ValidParticipant() || runtime != "" && runtime != model.RuntimeAwaitingBinding {
			return nil, errors.New("relay: enterprise Room cannot inherit Runtime selections")
		}
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Lease <= 0 {
		cfg.Lease = DeliveryLease
	}
	openCtx, cancel := context.WithTimeout(ctx, enterpriseOpenBudget)
	defer cancel()
	snapshot, err := port.ReadonlySnapshot(openCtx, auth, cfg.RoomID)
	if err != nil {
		return nil, err
	}
	if err := validateEnterpriseSnapshot(cfg.RoomID, auth.Deployment, snapshot); err != nil {
		return nil, err
	}
	e := emptyEngine(cfg)
	e.enterprise = true
	e.enterprisePort = port
	e.enterpriseState = enterpriseState{
		deployment: auth.Deployment, orgID: snapshot.OrgID, creationUserID: snapshot.CreationUserID, openAuth: auth,
		creationEventID: snapshot.Events[0].ID, creationPayloadHash: sha256.Sum256(snapshot.Events[0].Data),
	}
	e.gate = make(chan struct{}, 1)
	if err := e.replaySnapshotLocked(snapshot); err != nil {
		return nil, err
	}
	e.enterpriseSeq, e.enterpriseRevision = snapshot.Seq, snapshot.Revision
	return e, nil
}

// validateEnterpriseSnapshot refuses a snapshot that is not this Room's
// native-hosted, contiguous log — before any projection is built.
func validateEnterpriseSnapshot(roomID, deploymentID string, snapshot EnterpriseRoomSnapshot) error {
	if snapshot.RoomID != roomID {
		return fmt.Errorf("relay: enterprise snapshot Room %q does not match %q", snapshot.RoomID, roomID)
	}
	if snapshot.HostMode != string(model.HostNative) {
		return fmt.Errorf("relay: enterprise snapshot host mode %q is not native", snapshot.HostMode)
	}
	if snapshot.DeploymentID != deploymentID || snapshot.OrgID == "" || snapshot.CreationUserID == "" {
		return errors.New("relay: enterprise snapshot is missing or conflicts with trusted origin metadata")
	}
	if len(snapshot.Events) == 0 {
		return errors.New("relay: enterprise snapshot requires a creation fact")
	}
	if uint64(len(snapshot.Events)) != snapshot.Seq {
		return fmt.Errorf("relay: enterprise snapshot carries %d events for sequence %d", len(snapshot.Events), snapshot.Seq)
	}
	for i, event := range snapshot.Events {
		if event.RoomID != roomID || event.Seq != uint64(i+1) {
			return fmt.Errorf("relay: enterprise snapshot event %d is not the contiguous fact %d of %q", i, i+1, roomID)
		}
		if i > 0 && event.Kind == model.EnterpriseRoomCreatedKind {
			return errors.New("relay: enterprise snapshot contains a duplicate creation fact")
		}
		if event.Kind == model.EnterpriseMembershipKind {
			if _, err := model.DecodeEnterpriseMembershipChange(event); err != nil {
				return fmt.Errorf("relay: enterprise membership history: %w", err)
			}
		}
		if event.Kind == model.EnterpriseOwnershipKind {
			if _, err := model.DecodeEnterpriseOwnershipChange(event); err != nil {
				return fmt.Errorf("relay: enterprise ownership history: %w", err)
			}
		}
		if event.Kind == model.EnterpriseRoomMetadataKind {
			if _, err := model.DecodeEnterpriseRoomMetadata(event); err != nil {
				return fmt.Errorf("relay: enterprise Room metadata: %w", err)
			}
		}
	}
	creation, err := model.DecodeEnterpriseRoomCreation(snapshot.Events[0])
	if err != nil {
		return fmt.Errorf("relay: enterprise creation: %w", err)
	}
	if creation.Enterprise.DeploymentID != snapshot.DeploymentID || creation.Enterprise.OrgID != snapshot.OrgID || creation.Enterprise.InitialOwnerUserID != snapshot.CreationUserID {
		return errors.New("relay: enterprise creation conflicts with trusted origin metadata")
	}
	return nil
}

// replaySnapshotLocked replays a snapshot into this engine through the same
// apply implementation — no callbacks, no reap, no writes — over an isolated
// configuration copy, and adopts the durable-derived projection (including
// the replayed configuration) only after the whole projection validated; a
// failure leaves the existing projection untouched.
func (e *Engine) replaySnapshotLocked(snapshot EnterpriseRoomSnapshot) error {
	// Creation validation already fixed Native v9 and vacant slots. Start every
	// replay from those durable choices, never the previous projection's Runtime.
	cfg := cloneConfig(e.cfg)
	cfg.DynamicBindings = true
	initializeDynamicRuntimes(&cfg)
	fresh := emptyEngine(cfg)
	fresh.enterprise = true
	fresh.replaying = true
	for _, event := range snapshot.Events {
		if err := fresh.apply(event); err != nil {
			return fmt.Errorf("relay: enterprise replay at event %d: %w", event.Seq, err)
		}
	}
	fresh.replaying = false
	fresh.derived = nil
	if fresh.sequence != snapshot.Seq {
		return fmt.Errorf("relay: enterprise replay reached sequence %d, snapshot records %d", fresh.sequence, snapshot.Seq)
	}
	e.adoptDurableLocked(fresh)
	return nil
}

// adoptDurableLocked replaces every fact-derived field with the validated
// replayed state — the complete fact-derived projection, including the
// delivery projection, the LAN attempt counters rebuilt from replayed facts
// and the replayed configuration. Process-local observations (waiters,
// nudges, turn ends, the change channel and the gate) are preserved.
func (e *Engine) adoptDurableLocked(fresh *Engine) {
	e.cfg.DynamicBindings = fresh.cfg.DynamicBindings
	e.cfg.Runtimes = fresh.cfg.Runtimes
	e.lanWakeResults = fresh.lanWakeResults
	e.lanInvites = fresh.lanInvites
	e.lanRequests = fresh.lanRequests
	e.lanLatest = fresh.lanLatest
	e.lanAdmitted = fresh.lanAdmitted
	e.lanAttempts = fresh.lanAttempts
	e.lanKeyAttempts = fresh.lanKeyAttempts
	e.lanMember = fresh.lanMember
	e.lanTransfers = fresh.lanTransfers
	e.lastUserMessage = fresh.lastUserMessage
	e.bindings = fresh.bindings
	e.seenBinds = fresh.seenBinds
	e.messages = fresh.messages
	e.order = fresh.order
	e.positions = fresh.positions
	e.queued = fresh.queued
	e.unresolved = fresh.unresolved
	e.counts = fresh.counts
	e.lastWake = fresh.lastWake
	e.lastOutbound = fresh.lastOutbound
	e.lastInbound = fresh.lastInbound
	e.inFlight = fresh.inFlight
	e.sends = fresh.sends
	e.reports = fresh.reports
	e.lastReport = fresh.lastReport
	e.audit = fresh.audit
	e.sequence = fresh.sequence
	e.wakeEnabled = fresh.wakeEnabled
	e.wakeReservations = fresh.wakeReservations
	e.wakeReserved = fresh.wakeReserved
	e.delivery = fresh.delivery
}

// beginEnterpriseCommand takes the earlier of the caller's deadline and the
// command budget, then queues for the per-Room gate; queueing counts inside
// the same budget. After the token is acquired the context is re-checked so a
// cancellation racing the token never starts work.
func (e *Engine) beginEnterpriseCommand(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	opCtx, cancel := context.WithTimeout(ctx, enterpriseCommandBudget)
	select {
	case e.gate <- struct{}{}:
		if err := opCtx.Err(); err != nil {
			<-e.gate
			cancel()
			return nil, nil, err
		}
		return opCtx, func() { <-e.gate; cancel() }, nil
	case <-opCtx.Done():
		cancel()
		return nil, nil, opCtx.Err()
	}
}

// enterpriseAdmitted applies the post-token admission: the engine must be
// healthy (not failed closed, not closed, not draining) with a live context.
func (e *Engine) enterpriseAdmitted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.healthy()
}

// enterpriseClientKey is the principal-scoped durable client key, identical to
// the form the kernel's typed admission validates.
func enterpriseClientKey(userID, clientID string) string {
	return enterpriseClientKeyPrefix + userID + "/" + clientID
}

// decodeMessageFactPayload parses a stored message fact's payload.
func decodeMessageFactPayload(event model.Event) (Message, error) {
	var fact messageFact
	if err := json.Unmarshal(event.Data, &fact); err != nil {
		return Message{}, err
	}
	return fact.Message, nil
}

// ExecuteHumanSend runs one text-only human send for one authenticated user:
// the candidate is built by the shared builder, committed through the port
// under the command gate with the per-command identity, and only a confirmed
// commit applies and notifies — once. The deployment and Room are fixed at
// construction; the user is per command, and several users share this engine
// and its gate. A replayed result returns the original stored message without
// touching the projection cursor; a lost outcome fails the engine closed and
// stays there — read-only verification cannot clear it, and restoring write
// capability is a separately reviewed step this slice does not implement. A
// second revision conflict is terminal for the engine: it fails closed and
// signals, and subsequent commands are refused with zero port calls.
func (e *Engine) ExecuteHumanSend(ctx context.Context, auth EnterpriseAuth, req SendRequest) (Message, error) {
	if !e.enterprise {
		return Message{}, errors.New("relay: ExecuteHumanSend requires an enterprise engine")
	}
	if auth.UserID == "" || auth.Deployment != e.enterpriseState.deployment {
		return Message{}, errors.New("relay: the enterprise command identity must be a user of this engine's deployment")
	}
	if !req.To.ValidParticipant() {
		return Message{}, errors.New("select a native target slot")
	}
	if req.AttachmentIDs != nil || req.QuoteID != "" || req.Review != nil {
		return Message{}, errors.New("relay: enterprise human send is text-only in this slice")
	}
	opCtx, release, err := e.beginEnterpriseCommand(ctx)
	if err != nil {
		return Message{}, err
	}
	defer release()
	if err := e.enterpriseAdmitted(opCtx); err != nil {
		return Message{}, err
	}

	commit, err := e.commitEnterpriseSendLocked(opCtx, auth, req)
	if err != nil {
		var commandErr *EnterpriseCommandError
		if errors.As(err, &commandErr) && commandErr.Class == EnterpriseClassRevisionConflict {
			// One pure re-plan after a read-only rebuild under the same
			// per-command identity: the first attempt had no effect, so
			// replaying the decision against the rebuilt projection is safe.
			if rerr := e.rebuildEnterpriseLocked(opCtx, auth); rerr != nil {
				return Message{}, rerr
			}
			commit, err = e.commitEnterpriseSendLocked(opCtx, auth, req)
		}
	}
	if err != nil {
		var commandErr *EnterpriseCommandError
		if errors.As(err, &commandErr) && commandErr.Class == EnterpriseClassRevisionConflict {
			// The second conflict is terminal: fail the engine closed and
			// signal; every subsequent command is refused with zero port calls.
			e.mu.Lock()
			e.fatal = err
			e.signal()
			e.mu.Unlock()
			return Message{}, err
		}
		if errors.As(err, &commandErr) && commandErr.Class == EnterpriseClassPayloadConflict {
			return Message{}, ErrSendPayloadConflict
		}
		if errors.As(err, &commandErr) && commandErr.Outcome == EnterpriseOutcomeUnknown {
			e.mu.Lock()
			e.fatal = err
			e.signal()
			e.mu.Unlock()
		}
		return Message{}, err
	}

	switch commit.Outcome {
	case EnterpriseOutcomeCommitted:
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.sequence != commit.Seq-1 {
			e.fatal = fmt.Errorf("relay: enterprise commit sequence %d does not follow the projection %d", commit.Seq, e.sequence)
			e.signal()
			return Message{}, e.fatal
		}
		if err := e.apply(commit.Fact); err != nil {
			e.fatal = err
			e.signal()
			return Message{}, err
		}
		// The original post-commit contract, exactly once: signal, OnAppend,
		// then OnAttention for the fact and its derived transitions.
		e.signal()
		if e.cfg.OnAppend != nil {
			e.cfg.OnAppend(commit.Fact)
		}
		derived := e.derived
		e.derived = nil
		if e.cfg.OnAttention != nil {
			if attention, ok := AttentionFromEvent(commit.Fact); ok {
				e.cfg.OnAttention(attention)
			}
			for _, attention := range derived {
				e.cfg.OnAttention(attention)
			}
		}
		e.enterpriseSeq, e.enterpriseRevision = commit.Seq, commit.Revision
		message, err := decodeMessageFactPayload(commit.Fact)
		if err != nil {
			e.fatal = err
			e.signal()
			return Message{}, err
		}
		return cloneMessage(message), nil
	case EnterpriseOutcomeReplayed:
		e.mu.Lock()
		defer e.mu.Unlock()
		message, err := decodeMessageFactPayload(commit.Fact)
		if err != nil {
			e.fatal = err
			e.signal()
			return Message{}, err
		}
		if _, ok := e.messages[message.ID]; !ok {
			// The projection does not know the original fact: refuse to apply
			// it silently, and fail closed instead.
			e.fatal = fmt.Errorf("relay: replayed operation %s is not in the projection", message.ID)
			e.signal()
			return Message{}, e.fatal
		}
		// The canonical original result; the anchors are untouched (a replayed
		// original result revision never lowers the current cursor).
		return cloneMessage(message), nil
	default:
		return Message{}, fmt.Errorf("relay: unexpected enterprise outcome %q", commit.Outcome)
	}
}

// commitEnterpriseSendLocked builds the candidate for the per-command
// identity and performs one commit attempt.
func (e *Engine) commitEnterpriseSendLocked(ctx context.Context, auth EnterpriseAuth, req SendRequest) (EnterpriseSendResult, error) {
	e.mu.Lock()
	_, fact, err := e.buildEnterpriseHumanSendLocked(auth, req)
	expectedRevision, expectedSeq := e.enterpriseRevision, e.enterpriseSeq
	e.mu.Unlock()
	if err != nil {
		return EnterpriseSendResult{}, err
	}
	return e.enterprisePort.CommitHumanSend(ctx, EnterpriseSendCommand{
		Auth:             auth,
		RoomID:           e.cfg.RoomID,
		ClientID:         req.ID,
		Intent:           EnterpriseSendIntent{To: string(req.To), Text: req.Text},
		ExpectedRevision: expectedRevision,
		ExpectedSeq:      expectedSeq,
		Fact:             fact,
	})
}

// buildEnterpriseHumanSendLocked composes the shared, pure candidate builder
// pieces in the same order the Local path uses them, with the principal-scoped
// client key of the per-command identity.
func (e *Engine) buildEnterpriseHumanSendLocked(auth EnterpriseAuth, req SendRequest) (Message, model.Event, error) {
	if err := validateSendRequest(req); err != nil {
		return Message{}, model.Event{}, err
	}
	m := e.newSendCandidateLocked(model.ActorUser, req.To, req, false)
	if len(req.AttachmentIDs) > 0 {
		return Message{}, model.Event{}, errors.New("relay: enterprise human send is text-only in this slice")
	}
	if err := e.mergeQuoteAndLimitsLocked(&m, req); err != nil {
		return Message{}, model.Event{}, err
	}
	if _, err := e.envelope(m); err != nil {
		return Message{}, model.Event{}, err
	}
	fact, err := model.NewEvent(e.cfg.RoomID, EventMessage, model.ActorUser, messageFact{Message: m, ClientKey: enterpriseClientKey(auth.UserID, req.ID)})
	if err != nil {
		return Message{}, model.Event{}, err
	}
	return m, fact, nil
}

// rebuildEnterpriseLocked performs the read-only rebuild under the current
// command identity: an authenticated snapshot is re-validated, replayed
// through the same apply, and adopted only after the whole projection built;
// no callbacks, no reap, no writes, and the fatal/closed/draining state and
// the gate are preserved. A rebuild that advances the projection signals
// WaitChanges waiters (never OnAppend/OnAttention).
func (e *Engine) rebuildEnterpriseLocked(ctx context.Context, auth EnterpriseAuth) error {
	snapshot, err := e.enterprisePort.ReadonlySnapshot(ctx, auth, e.cfg.RoomID)
	if err != nil {
		var commandErr *EnterpriseCommandError
		if errors.As(err, &commandErr) && commandErr.Outcome == EnterpriseOutcomeRefused && commandErr.Class == EnterpriseClassAuthorization {
			// A definite per-request authorization refusal (for example the
			// command's own user was revoked between attempts) must not poison
			// the shared Room: other live members keep their existing ACL.
			// Room-level faults — state_invalid, lease_lost, incomplete or
			// unknown results and every other error — stay fail-closed below.
			return err
		}
		e.mu.Lock()
		e.fatal = err
		e.signal()
		e.mu.Unlock()
		return err
	}
	if err := validateEnterpriseSnapshot(e.cfg.RoomID, e.enterpriseState.deployment, snapshot); err != nil {
		e.mu.Lock()
		e.fatal = err
		e.signal()
		e.mu.Unlock()
		return err
	}
	if snapshot.OrgID != e.enterpriseState.orgID || snapshot.CreationUserID != e.enterpriseState.creationUserID || snapshot.Events[0].ID != e.enterpriseState.creationEventID || sha256.Sum256(snapshot.Events[0].Data) != e.enterpriseState.creationPayloadHash {
		err := errors.New("relay: enterprise rebuild changed the Room origin")
		e.mu.Lock()
		e.fatal = err
		e.signal()
		e.mu.Unlock()
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if snapshot.Seq < e.sequence {
		e.fatal = fmt.Errorf("relay: enterprise rebuild snapshot sequence %d is behind the projection %d", snapshot.Seq, e.sequence)
		e.signal()
		return e.fatal
	}
	previous := e.sequence
	if err := e.replaySnapshotLocked(snapshot); err != nil {
		e.fatal = err
		e.signal()
		return err
	}
	e.enterpriseSeq, e.enterpriseRevision = snapshot.Seq, snapshot.Revision
	if e.sequence != previous {
		// The projection advanced without an append: wake cursor waiters,
		// and stay silent otherwise.
		e.signal()
	}
	return nil
}

// CloseContext closes an enterprise engine with a cancellable, self-bounded
// wait for the command gate: the wait takes the earlier of the caller's
// deadline and the close budget, the context is re-checked after the token is
// acquired, and a timed-out or cancelled wait reports the error without
// claiming the engine closed. The shared SQL store is never closed here — it
// belongs to the deployment owner. On a Local engine it falls back to Close.
func (e *Engine) CloseContext(ctx context.Context) error {
	if !e.enterprise {
		return e.Close()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, enterpriseCloseBudget)
	defer cancel()
	select {
	case e.gate <- struct{}{}:
		if err := opCtx.Err(); err != nil {
			<-e.gate
			return err
		}
	case <-opCtx.Done():
		return opCtx.Err()
	}
	defer func() { <-e.gate }()
	e.mu.Lock()
	defer e.mu.Unlock()
	// Re-check the effective operation context: it is the earlier of the
	// caller's deadline and the close budget, so an expiry racing the token
	// (including the internal budget's) never claims the close.
	if err := opCtx.Err(); err != nil {
		return err
	}
	if e.closed {
		return nil
	}
	// Enterprise close performs no reap and no write of any kind.
	e.closed = true
	e.signal()
	return nil
}
