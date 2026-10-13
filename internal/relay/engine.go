package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
	"github.com/sean2077/pairroom/internal/protocol"
)

type Config struct {
	// DynamicBindings selects the Native v9 binding-time Runtime contract.
	// The provisioned Room fixes it; legacy Native Rooms keep their selections.
	DynamicBindings bool
	// SharedSlot is the remote participant of a Native LAN Room. It cannot be locally bound.
	SharedSlot model.ActorID
	RoomID     string
	// Store is the Room's durable boundary; every append error stays fail-closed.
	Store    Persistence
	Runtimes map[model.ActorID]model.RuntimeKind
	Media    *attachment.Store
	Now      func() time.Time
	Lease    time.Duration
	// CommitBinding serializes global native identity ownership with the active
	// Room writer. It must call appendFact exactly once or return an error.
	CommitBinding func(Binding, func() error) error
	// OnAppend observes each durably appended fact after it is applied. It runs
	// under the Engine lock and must not block or call back into the Engine.
	OnAppend func(model.Event)
	// OnAttention receives each body-free attention observation of a durable
	// append, including transitions a binding fact derives atomically (an
	// old-generation delivery becoming unknown), which no standalone message
	// fact records. Same constraints as OnAppend.
	OnAttention func(Attention)
}

type Engine struct {
	lanWakeResults  map[string]string
	lanInvites      map[string]LANInvite
	lanRequests     map[string]LANJoinRequest
	lanAdmitted     map[string]bool
	lanAttempts     map[string]int
	lanKeyAttempts  map[string]int
	lanMember       *LANMember
	lanTransfers    lanTransferSet
	lastUserMessage string
	mu              sync.Mutex
	cfg             Config
	bindings        map[model.ActorID]bindingFact
	seenBinds       map[string]bool
	messages        map[string]Message
	order           []string
	positions       map[string]int
	queued          map[model.ActorID][]string
	unresolved      []string
	counts          map[model.ActorID]InboxSummary
	lastWake        map[model.ActorID]WakeObservation
	lastOutbound    map[model.ActorID]string // newest message from the slot to its peer
	lastInbound     map[model.ActorID]string // newest message from the peer to the slot
	inFlight        map[string]struct{}      // rebuilt from message facts; never scans terminal history
	sends           map[string]string
	reports         map[string]Publication
	lastReport      map[string]uint64
	audit           []Audit
	sequence        uint64
	replaying       bool
	changed         chan struct{}
	closed          bool
	draining        bool
	fatal           error
	// wakeEnabled defaults true (per-Room default-on, DP2); only a durable
	// native.wake.updated fact changes it. wakeReserved dedupes reservations
	// across replay and runtime; waiters counts Claim long-polls blocked per
	// slot so WakeCandidate can see live foreground/park collectors.
	wakeEnabled      bool
	wakeReservations []WakeReservation
	wakeReserved     map[string]bool
	waiters          map[model.ActorID]int
	// wakeNudges and turnEnded are in-memory projections for the outstanding
	// nudge rule: the newest possibly delivered nudge per target and the last
	// observed Turn end (a Stop park that released nothing, or StopFailure).
	// Replay rebuilds reservations/outcomes and Claude handoff progress from
	// durable facts. Turn ends are process-local, so replayed Codex nudges use
	// the conservative mid-Turn rule; later binding activity alone never clears
	// a Claude wake.
	wakeNudges map[model.ActorID]wakeNudge
	turnEnded  map[model.ActorID]time.Time
	// derived holds attention transitions applied by the current append only.
	derived  []Attention
	delivery *deliveryProjection
	// enterprise wiring; zero on Local engines. The gate serializes whole
	// enterprise commands (plan -> SQL -> apply/rebuild) and every legacy
	// mutator is rejected at its entry when enterprise is set. The deployment
	// is fixed at construction; each command carries its own authenticated
	// user, so several users share one engine and one gate.
	enterprise         bool
	enterprisePort     EnterprisePort
	enterpriseState    enterpriseState
	enterpriseSeq      uint64
	enterpriseRevision uint64
	gate               chan struct{}
}

func Open(cfg Config) (*Engine, error) {
	if nilPersistence(cfg.Store) || cfg.RoomID == "" {
		return nil, errors.New("native relay requires a published Room store")
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Lease <= 0 {
		cfg.Lease = DeliveryLease
	}
	initializeDynamicRuntimes(&cfg)
	e := &Engine{cfg: cfg, bindings: map[model.ActorID]bindingFact{}, seenBinds: map[string]bool{}, messages: map[string]Message{}, inFlight: map[string]struct{}{}, sends: map[string]string{}, reports: map[string]Publication{}, lastReport: map[string]uint64{}, changed: make(chan struct{}), wakeEnabled: true, wakeReserved: map[string]bool{}, waiters: map[model.ActorID]int{}}
	events, err := cfg.Store.Load()
	if err != nil {
		return nil, err
	}
	e.replaying = true
	for _, ev := range events {
		if ev.RoomID != cfg.RoomID {
			return nil, errors.New("native relay Room identity mismatch")
		}
		if err := e.apply(ev); err != nil {
			return nil, fmt.Errorf("native replay at event %d: %w", ev.Seq, err)
		}
	}
	e.replaying = false
	e.pruneLANRequestsLocked()
	e.derived = nil // replayed history is not new attention
	// A previous writer cannot prove whether a claimed envelope reached stdout.
	// Preserve queued work; no delivery recovery is an automatic replay.
	// Use the same transition as lease expiry/close, including its private
	// receipt. Dropping it here would prevent the original claimer's late ack.
	if err := e.reapLocked(true); err != nil {
		return nil, err
	}
	return e, nil
}

func Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func RandomID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func validID(s string) bool {
	return s != "" && len(s) <= 256 && !strings.ContainsAny(s, "/\\\r\n\x00") && strings.TrimSpace(s) == s
}
func validHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}
func same(a, b string) bool                          { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func bindingKey(id string, generation uint64) string { return fmt.Sprintf("%s/%d", id, generation) }
func reportKey(id string, generation, seq uint64) string {
	return fmt.Sprintf("%s/%d/%d", id, generation, seq)
}

func (e *Engine) healthy() error {
	if err := e.available(); err != nil {
		return err
	}
	if e.draining {
		return ErrClosed
	}
	return nil
}

// available permits completion of admitted work while draining, but never
// permits effects after closure or an uncertain store write.
func (e *Engine) available() error {
	if e.fatal != nil {
		return fmt.Errorf("native relay fail-closed: %w", e.fatal)
	}
	if e.closed {
		return ErrClosed
	}
	return nil
}

// Fatal reports the fail-closed store/replay error, if any, so the Service can
// surface this Room runtime as failed instead of reporting a healthy Active
// phase while every relay operation can only error.
func (e *Engine) Fatal() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fatal
}

func (e *Engine) signal() { close(e.changed); e.changed = make(chan struct{}) }
func (e *Engine) append(kind string, actor model.ActorID, payload any) error {
	if e.enterprise {
		// Last line of defense only: every enterprise mutator is rejected at
		// its entry; Local entries are rejected by requireLocalMutation.
		return ErrEnterpriseReadOnly
	}
	if e.closed || e.fatal != nil {
		return ErrClosed
	}
	ev, err := model.NewEvent(e.cfg.RoomID, kind, actor, payload)
	if err != nil {
		return err
	}
	ev.CreatedAt = e.cfg.Now()
	e.derived = nil
	if err := e.cfg.Store.Append(&ev); err != nil {
		// Fail closed: a non-nil append error never establishes that no effect
		// exists (the write may have landed), so the Room accepts no further
		// writes until it is reopened and the durable facts are replayed.
		e.fatal = err
		e.signal()
		return err
	}
	if err := e.apply(ev); err != nil {
		e.fatal = err
		e.signal()
		return err
	}
	e.signal()
	if e.cfg.OnAppend != nil {
		e.cfg.OnAppend(ev)
	}
	derived := e.derived
	e.derived = nil
	if e.cfg.OnAttention != nil {
		if a, ok := AttentionFromEvent(ev); ok {
			e.cfg.OnAttention(a)
		}
		for _, a := range derived {
			e.cfg.OnAttention(a)
		}
	}
	return nil
}
func (e *Engine) apply(ev model.Event) error {
	if e.enterprise && (ev.Kind == EventBinding || ev.Kind == EventLANInvite || ev.Kind == EventLANJoin || ev.Kind == EventLANMember) {
		return errors.New("enterprise Room cannot replay Local or LAN binding authority")
	}
	if !e.enterprise && (ev.Kind == model.EnterpriseBindingUpdatedKind || ev.Kind == model.EnterpriseMembershipKind || ev.Kind == model.EnterpriseOwnershipKind || ev.Kind == model.EnterpriseRoomMetadataKind) {
		return errors.New("local or LAN Room cannot replay enterprise authority history")
	}
	e.sequence = ev.Seq
	e.initIndexes()
	detail := ""
	switch ev.Kind {
	case model.EnterpriseRoomMetadataKind:
		// The catalog owns current metadata and access. Replaying a rename,
		// archive or Project association cannot change a Binding or ACL.
		if _, err := model.DecodeEnterpriseRoomMetadata(ev); err != nil {
			return err
		}
	case model.EnterpriseMembershipKind:
		if _, err := model.DecodeEnterpriseMembershipChange(ev); err != nil {
			return err
		}
	case model.EnterpriseOwnershipKind:
		// These facts describe history only. Live SQL rows, never replay,
		// authorize membership and ownership.
		if _, err := model.DecodeEnterpriseOwnershipChange(ev); err != nil {
			return err
		}
	case model.EnterpriseBindingUpdatedKind:
		if err := e.applyEnterpriseBinding(ev); err != nil {
			return err
		}
	case EventBinding:
		var b bindingFact
		if err := json.Unmarshal(ev.Data, &b); err != nil {
			return err
		}
		if b.RemoteKey != "" || b.Slot == e.cfg.SharedSlot {
			return errors.New("local binding event cannot authorize a LAN peer")
		}
		if err := e.applyBinding(ev, b); err != nil {
			return err
		}
	case EventLANInvite, EventLANJoin, EventLANMember:
		if err := e.applyLAN(ev); err != nil {
			return err
		}
	case EventMessage:
		var fact messageFact
		if err := json.Unmarshal(ev.Data, &fact); err != nil {
			return err
		}
		if !validID(fact.ID) || !validMessageState(fact.State) {
			return errors.New("invalid native message fact")
		}
		if err := e.validateDynamicMessage(fact.Message); err != nil {
			return err
		}
		fact.Message.Receipt = fact.Receipt
		e.putMessage(fact.Message)
		e.observeWakeHandoffLocked(fact.Message)
		if fact.ClientKey != "" {
			e.sends[fact.ClientKey] = fact.ID
		}
	case EventPublication, EventPublicationGap:
		var p Publication
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return err
		}
		if p.ReportSeq == 0 || p.Generation == 0 || !validID(p.BindID) {
			return errors.New("invalid native publication fact")
		}
		if p.Outcome != "" && (!e.cfg.DynamicBindings || p.Outcome != PeerChangedCode || p.Message != nil) {
			return errors.New("invalid native publication outcome")
		}
		if e.cfg.DynamicBindings {
			b := e.bindings[ev.Actor]
			if !b.Active || b.BindID != p.BindID || b.Generation != p.Generation {
				return errors.New("publication does not match its active Native binding")
			}
			if p.Message != nil {
				if p.Message.From != ev.Actor || p.Message.FromGeneration != p.Generation || p.Message.To.ValidParticipant() && p.Message.TargetGeneration != p.ExpectedPeerGeneration {
					return errors.New("publication source or target generation changed")
				}
				if err := e.validateDynamicMessage(*p.Message); err != nil {
					return err
				}
			}
		}
		e.reports[reportKey(p.BindID, p.Generation, p.ReportSeq)] = p
		e.lastReport[bindingKey(p.BindID, p.Generation)] = p.ReportSeq
		if p.Message != nil {
			e.putMessage(*p.Message)
		}
		if p.GapFrom != 0 {
			detail = fmt.Sprintf("publication gap: %d–%d", p.GapFrom, p.GapTo)
		}
	case EventFailure:
		var p struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return err
		}
		detail = p.Error
	case EventWakeConfig:
		var p struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return err
		}
		e.wakeEnabled = p.Enabled
		if p.Enabled {
			detail = "wake enabled"
		} else {
			detail = "wake disabled"
		}
	case EventWakeReserved:
		var r WakeReservation
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return err
		}
		if !validID(r.MessageID) || !r.Target.ValidParticipant() {
			return errors.New("invalid native wake reservation fact")
		}
		if e.wakeReserved[r.MessageID] {
			return errors.New("duplicate native wake reservation fact")
		}
		r.At = ev.CreatedAt
		e.wakeReserved[r.MessageID] = true
		e.wakeReservations = append(e.wakeReservations, r)
		e.indexWakeReservation(r)
		e.noteWakeNudgeLocked(r)
		detail = "wake reserved"
	case EventWakeAttempted:
		var p struct {
			MessageID string        `json:"message_id,omitempty"`
			Outcome   string        `json:"outcome"`
			Reason    string        `json:"reason,omitempty"`
			Target    model.ActorID `json:"target"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return err
		}
		if !wakeOutcomes[p.Outcome] || !p.Target.ValidParticipant() {
			return errors.New("invalid native wake attempt fact")
		}
		if p.Reason != "" && !wakeReasons[p.Reason] {
			return errors.New("invalid native wake attempt reason")
		}
		if (p.Outcome == "accepted" || p.Outcome == "submitted") != (p.Reason == "") {
			return errors.New("native wake attempt reason does not match outcome")
		}
		if p.MessageID != "" {
			m, ok := e.messages[p.MessageID]
			if !ok || p.Target != e.cfg.SharedSlot || m.To != p.Target || !e.wakeReserved[p.MessageID] || p.Outcome == "suppressed" {
				return errors.New("LAN wake outcome lacks matching reserved message")
			}
			if e.lanWakeResults == nil {
				e.lanWakeResults = make(map[string]string)
			}
			if _, exists := e.lanWakeResults[p.MessageID]; exists {
				return errors.New("duplicate LAN wake outcome")
			}
			e.lanWakeResults[p.MessageID] = p.Outcome + "/" + p.Reason
		}
		if n, ok := e.wakeNudges[p.Target]; ok && !n.Settled && p.Outcome != "suppressed" && (p.MessageID == "" || p.MessageID == n.MessageID) {
			// The single wake worker per target appends a reservation's outcome
			// next for that target; a definite failure delivered no nudge.
			n.Settled, n.Failed = true, p.Outcome == "failed"
			e.wakeNudges[p.Target] = n
		}
		e.lastWake[p.Target] = WakeObservation{MessageID: p.MessageID, Outcome: p.Outcome, Reason: p.Reason, At: ev.CreatedAt}
		e.indexWakeObservation(p.Target, e.lastWake[p.Target])
		detail = "wake " + p.Outcome
		if p.Reason != "" {
			detail += " (" + p.Reason + ")"
		}
	default:
		if strings.HasPrefix(ev.Kind, "native.") {
			return fmt.Errorf("unsupported native event %q", ev.Kind)
		}
		return nil
	}
	e.audit = append(e.audit, Audit{Seq: ev.Seq, Kind: ev.Kind, Actor: ev.Actor, At: ev.CreatedAt, Detail: detail})
	return nil
}
func (e *Engine) applyBinding(ev model.Event, b bindingFact) error {
	if e.enterprise || b.enterprise != nil {
		return errors.New("local or LAN binding codec cannot carry enterprise identity")
	}
	if !b.Slot.ValidParticipant() || b.Generation == 0 || !validID(b.BindID) || (b.Active && b.RemoteKey == "" && !validHash(b.CredentialHash)) {
		return errors.New("invalid native binding fact")
	}
	old := e.bindings[b.Slot]
	if err := e.validateBindingGenerationLocked(b, old); err != nil {
		return err
	}
	if err := e.validateDynamicBinding(b, old); err != nil {
		return err
	}
	return e.applyBindingProjection(ev, b)
}

// validateBindingGenerationLocked is shared by the Local/LAN and enterprise
// fact codecs. Identity validation stays at each codec's own boundary.
func (e *Engine) validateBindingGenerationLocked(b, old bindingFact) error {
	if b.Generation < old.Generation || (b.Generation == old.Generation && old.BindID != "" && b.BindID != old.BindID) {
		return errors.New("native binding generation regressed")
	}
	if e.cfg.DynamicBindings && b.Generation > old.Generation && (b.Generation != old.Generation+1 || !b.Active || e.seenBinds[b.BindID]) {
		return errors.New("dynamic Native binding requires a fresh active generation")
	}
	return nil
}

// applyBindingProjection is the one binding/inbox transition implementation.
// Both codecs reach it only after their identities and generation validate.
func (e *Engine) applyBindingProjection(ev model.Event, b bindingFact) error {
	old := e.bindings[b.Slot]
	if !b.Active || b.Generation != old.Generation || b.SessionID != old.SessionID {
		// A replaced, unbound or re-associated session cannot consume a
		// nudge queued for its predecessor, nor end its Turn.
		delete(e.wakeNudges, b.Slot)
		delete(e.turnEnded, b.Slot)
	}
	e.bindings[b.Slot] = b
	if e.cfg.DynamicBindings {
		e.cfg.Runtimes[b.Slot] = vacantRuntime(b.Slot, e.cfg.SharedSlot)
		if b.Active {
			e.cfg.Runtimes[b.Slot] = b.Runtime
		}
	}
	e.seenBinds[b.BindID] = true
	// Revocation is atomic with invalidating old-generation inbox work. Work
	// already handed off cannot be undone, nor does an empty inbox prove idle.
	for _, id := range append([]string(nil), e.unresolved...) {
		m := e.messages[id]
		if m.To == b.Slot && (!b.Active || m.TargetGeneration != b.Generation) {
			if m.State == "queued" {
				m.State = "cancelled"
				m.UpdatedAt = ev.CreatedAt
				e.putMessage(m)
			}
			if m.State == "delivering" {
				m.State = "unknown"
				m.UpdatedAt = ev.CreatedAt
				e.putMessage(m)
				// Replay discards this list at Open; only live appends report it.
				e.derived = append(e.derived, Attention{Kind: AttentionDeliveryUncertain, Slot: m.To, Key: m.ID})
			}
		}
	}
	return nil
}

func validMessageState(s string) bool {
	switch s {
	case "queued", "delivering", "handed_off", "unknown", "cancelled", "human":
		return true
	}
	return false
}

// appendBindingFact records one binding fact in its durable form: an admitted
// LAN member binding is written through the LAN membership event, an ordinary
// owner binding as a plain native binding fact. It is the Event Log half of
// commitBinding without the Registry hook.
func (e *Engine) appendBindingFact(b bindingFact) error {
	if b.RemoteKey != "" {
		return e.appendLANBinding(b)
	}
	return e.append(EventBinding, b.Slot, b)
}
func (e *Engine) commitBinding(b bindingFact) error {
	if err := e.quiesceLANTransfersLocked(b); err != nil {
		return err
	}
	if e.cfg.CommitBinding != nil {
		return e.cfg.CommitBinding(b.Binding, func() error { return e.appendBindingFact(b) })
	}
	return e.appendBindingFact(b)
}

// Bind associates a slot. See BindReport for the replaced generation's work.
func (e *Engine) Bind(slot model.ActorID, req BindRequest) (Binding, error) {
	if err := e.requireLocalMutation(); err != nil {
		return Binding{}, err
	}
	b, _, err := e.BindReport(slot, req)
	return b, err
}

// maxReplacedIDs bounds each ID list a replacement reports; maxPreviousScan
// bounds how much retained history the handed-off lookup inspects.
const (
	maxReplacedIDs  = 8
	maxPreviousScan = 5000
)

// ReplacedIDs is a bounded, oldest-first (handed-off: newest-first) sample of
// message IDs with the exact number it was taken from.
type ReplacedIDs struct {
	IDs   []string `json:"ids"`
	Count int      `json:"count"`
}

func (r *ReplacedIDs) add(id string) {
	r.Count++
	if len(r.IDs) < maxReplacedIDs {
		r.IDs = append(r.IDs, id)
	}
}

// Replaced lists body-free transport IDs of a replaced generation's inbox so
// the new session can inspect them with history --id. Nothing is requeued or
// retried, and no state here proves model acceptance.
//
// Cancelled (was queued) and Unknown (was claimed, unacknowledged) are what
// this bind invalidated; AlreadyUnknown were unknown before it. Their counts are
// exact. HandedOff were written to the previous collector's stdout, may have
// run and were not invalidated; they come from the newest retained history,
// so HandedOff.Count is exact only when ScanComplete.
type Replaced struct {
	Generation     uint64      `json:"generation"`
	Cancelled      ReplacedIDs `json:"cancelled"`
	Unknown        ReplacedIDs `json:"unknown"`
	AlreadyUnknown ReplacedIDs `json:"already_unknown"`
	HandedOff      ReplacedIDs `json:"handed_off"`
	ScanComplete   bool        `json:"scan_complete"`
}

// BindReport is Bind plus, for an explicit replacement of an active binding,
// what happened to that binding's inbox. Idempotent recovery reports nothing.
func (e *Engine) BindReport(slot model.ActorID, req BindRequest) (Binding, *Replaced, error) {
	if err := e.requireLocalMutation(); err != nil {
		return Binding{}, nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var err error
	slot, err = e.resolveBindSlotLocked(slot, req)
	if err != nil {
		return Binding{}, nil, err
	}
	old := e.bindings[slot]
	r := &Replaced{Generation: old.Generation, ScanComplete: true}
	// Classify the current work set before this bind invalidates it; it is
	// bounded by unresolved work, never by terminal history.
	for _, id := range e.unresolved {
		m := e.messages[id]
		if m.To != slot || m.TargetGeneration != old.Generation {
			continue
		}
		switch m.State {
		case "queued":
			r.Cancelled.add(id)
		case "delivering":
			r.Unknown.add(id)
		case "unknown":
			r.AlreadyUnknown.add(id)
		}
	}
	b, err := e.bindLocked(slot, req)
	if err != nil || !old.Active || b.Generation == old.Generation {
		return b, nil, err
	}
	for i, scanned := len(e.order)-1, 0; i >= 0; i-- {
		if scanned == maxPreviousScan {
			r.ScanComplete = false
			break
		}
		scanned++
		if m := e.messages[e.order[i]]; m.To == slot && m.TargetGeneration == old.Generation && m.State == "handed_off" {
			r.HandedOff.add(m.ID)
		}
	}
	if r.Cancelled.Count+r.Unknown.Count+r.AlreadyUnknown.Count+r.HandedOff.Count == 0 && r.ScanComplete {
		return b, nil, nil
	}
	return b, r, nil
}

func (e *Engine) bindLocked(slot model.ActorID, req BindRequest) (Binding, error) {
	if err := e.healthy(); err != nil {
		return Binding{}, err
	}
	// Association happens at bind: the caller presents the official session id it
	// read from its harness environment (Claude Code CLAUDE_CODE_SESSION_ID, Codex
	// CODEX_SESSION_ID), so the binding materializes that identity immediately and
	// commitBinding runs the global (runtime, session) uniqueness check at once.
	if slot == e.cfg.SharedSlot {
		return Binding{}, errors.New("LAN peer slot requires owner-admitted remote membership")
	}
	if !slot.ValidParticipant() || !validID(req.BindID) || !validHash(req.CredentialHash) || !validID(req.SessionID) {
		return Binding{}, errors.New("invalid binding request")
	}
	if e.cfg.DynamicBindings {
		if !req.Runtime.Valid() {
			return Binding{}, errors.New("dynamic Native binding requires its actual runtime")
		}
	} else if req.Runtime != "" && (!req.Runtime.Valid() || req.Runtime.Canonical() != e.cfg.Runtimes[slot].Canonical()) {
		return Binding{}, errors.New("calling runtime does not match this legacy Native slot")
	}
	old := e.bindings[slot]
	if old.Active {
		// Recovery of an in-flight bind is idempotent without consuming another
		// generation. The same session must identify itself identically.
		if old.BindID == req.BindID && same(old.CredentialHash, req.CredentialHash) && old.SessionID == req.SessionID && (!e.cfg.DynamicBindings || old.Runtime == req.Runtime.Canonical()) {
			return old.Binding, nil
		}
		if !req.Replace {
			return Binding{}, ErrOccupied
		}
	}
	if e.seenBinds[req.BindID] {
		return Binding{}, errors.New("revoked bind ID cannot be reused")
	}
	b := bindingFact{Binding: Binding{Slot: slot, BindID: req.BindID, Generation: old.Generation + 1, Active: true, ParkEnabled: true, LastActivity: e.cfg.Now(), SessionID: req.SessionID}, CredentialHash: req.CredentialHash}
	if e.cfg.DynamicBindings {
		b.Runtime = req.Runtime.Canonical()
		if err := e.validateDynamicBinding(b, old); err != nil {
			return Binding{}, err
		}
	}
	if err := e.commitBinding(b); err != nil {
		return Binding{}, err
	}
	return b.Binding, nil
}
func (e *Engine) auth(a Auth, pending bool) (bindingFact, error) {
	if err := e.healthy(); err != nil {
		return bindingFact{}, err
	}
	return e.authenticate(a, pending)
}

func (e *Engine) authenticate(a Auth, pending bool) (bindingFact, error) {
	if err := e.available(); err != nil {
		return bindingFact{}, err
	}
	b, ok := e.bindings[a.Slot]
	if !ok || !b.Active || a.BindID != b.BindID || a.Generation != b.Generation {
		return bindingFact{}, ErrAuth
	}
	if (e.cfg.DynamicBindings || a.Runtime != "") && (!a.Runtime.Valid() || a.Runtime.Canonical() != e.bindingRuntimeLocked(a.Slot)) {
		return bindingFact{}, ErrAuth
	}
	if b.RemoteKey != "" {
		if a.MemberKey == "" || !same(a.MemberKey, b.RemoteKey) || a.SessionID != "" || a.Secret != "" {
			return bindingFact{}, ErrAuth
		}
		return b, nil
	}
	if a.MemberKey != "" || !same(Digest(a.Secret), b.CredentialHash) {
		return bindingFact{}, ErrAuth
	}
	if b.SessionID == "" {
		if !pending {
			return bindingFact{}, ErrAuth
		}
	} else if a.SessionID != b.SessionID {
		return bindingFact{}, ErrAuth
	}
	b.LastActivity = e.cfg.Now()
	e.bindings[a.Slot] = b
	return b, nil
}
func (e *Engine) Inspect(a Auth) (Binding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, true)
	return b.Binding, err
}

// ConfirmSession is the Stop hook's reverse check. The binding already carries
// the session id captured from the harness environment at bind, so the official
// hook session must match it; a mismatch fails closed instead of associating.
// A match opportunistically records the transcript path, which the environment
// does not carry, keeping `relay peer` references available.
func (e *Engine) ConfirmSession(a Auth, sessionID, transcript string) (Binding, error) {
	if err := e.requireLocalMutation(); err != nil {
		return Binding{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, false)
	if err != nil {
		return Binding{}, err
	}
	if a.SessionID != sessionID || !validID(sessionID) || len(transcript) > 4096 || strings.ContainsAny(transcript, "\x00\r\n") {
		return Binding{}, errors.New("invalid official hook session metadata")
	}
	if b.RemoteKey != "" || b.SessionID != sessionID {
		return Binding{}, ErrAuth
	}
	if b.TranscriptPath == transcript {
		return b.Binding, nil
	}
	b.TranscriptPath = transcript
	if err := e.commitBinding(b); err != nil {
		return Binding{}, err
	}
	return b.Binding, nil
}
func (e *Engine) Unbind(slot model.ActorID) error {
	if err := e.requireLocalMutation(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return err
	}
	b, ok := e.bindings[slot]
	if !ok || !b.Active {
		return nil
	}
	return e.commitBinding(planLocalRetirement(b, e.cfg.Now()))
}
func (e *Engine) Park(slot model.ActorID, enabled bool) error {
	if err := e.requireLocalMutation(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return err
	}
	if slot == e.cfg.SharedSlot {
		// The shared slot's park state belongs to its admitted member: only that
		// member's own ParkAs may change it, exactly as it is the only side that
		// can bind it.
		return errors.New("the LAN peer slot's park state is controlled by its admitted member")
	}
	b, ok := e.bindings[slot]
	if !ok || !b.Active {
		return ErrAuth
	}
	b, changed := planParkBinding(b, enabled)
	if !changed {
		return nil
	}
	// A park toggle changes no identity: append its binding fact directly so it
	// cannot run the Registry CommitBinding checkpoint, whose write failure
	// would poison the whole Registry fail-closed until restart.
	return e.appendBindingFact(b)
}
func (e *Engine) Peer(a Auth) (Binding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return Binding{}, err
	}
	slot := model.OtherParticipant(a.Slot)
	peer := e.bindings[slot].Binding
	peer.Slot = slot
	peer.Runtime = e.cfg.Runtimes[slot].Canonical()
	return peer, nil
}

func validateBody(text string) error {
	if !utf8.ValidString(text) || len(text) > MaxBodyBytes {
		return fmt.Errorf("relay text must be UTF-8 and at most %d bytes", MaxBodyBytes)
	}
	return nil
}
func (e *Engine) generation(slot model.ActorID) uint64 {
	b := e.bindings[slot]
	if b.Active {
		return b.Generation
	}
	return b.Generation + 1
}
func (e *Engine) makeMessage(from, to model.ActorID, text, source string) Message {
	now := e.cfg.Now()
	return planMessage(e.messagePlanSource(from, to), model.NewID("relay"), now, text, source)
}
func (e *Engine) Report(a Auth, seq uint64, text string, expectedPeerGeneration ...uint64) (Publication, error) {
	if err := e.requireLocalMutation(); err != nil {
		return Publication{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return Publication{}, err
	}
	if seq == 0 {
		return Publication{}, errors.New("report_seq must be positive")
	}
	if err := validateBody(text); err != nil {
		return Publication{}, err
	}
	key := reportKey(a.BindID, a.Generation, seq)
	if existing, ok := e.reports[key]; ok {
		e.noteLANActivityLocked(a)
		return clonePublication(existing), nil
	}
	decision, err := decidePublication(e.publicationPlanState(a.Slot), seq, text, expectedPeerGeneration)
	if err != nil {
		return Publication{}, err
	}
	p := decision.publication
	if decision.to != "" && p.Outcome == "" {
		m := e.makeMessage(a.Slot, decision.to, text, "stop")
		p.Message = &m
	}
	// Acceptance and optional enqueue share one durable append. A non-routed
	// boundary records only its idempotency receipt, never the private reply.
	if err := e.append(decision.kind, a.Slot, p); err != nil {
		return Publication{}, err
	}
	e.noteLANActivityLocked(a)
	return clonePublication(p), nil
}
func (e *Engine) Publication(a Auth, seq uint64) (Publication, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return Publication{}, false, err
	}
	p, ok := e.reports[reportKey(a.BindID, a.Generation, seq)]
	return clonePublication(p), ok, nil
}
func (e *Engine) Send(a Auth, req SendRequest) (Message, error) {
	if err := e.requireLocalMutation(); err != nil {
		return Message{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return Message{}, err
	}
	to := model.OtherParticipant(a.Slot)
	if req.To == model.ActorUser {
		to = model.ActorUser
	} else if req.To != "" && req.To != to {
		return Message{}, errors.New("explicit relay targets only the peer or @user")
	}
	m, err := e.sendLocked(a.Slot, to, req, bindingKey(a.BindID, a.Generation)+"/send/"+req.ID)
	if err == nil {
		e.noteLANActivityLocked(a)
	}
	return m, err
}
func (e *Engine) SendUser(req SendRequest) (Message, error) {
	if err := e.requireLocalMutation(); err != nil {
		return Message{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return Message{}, err
	}
	if !req.To.ValidParticipant() {
		return Message{}, errors.New("select a native target slot")
	}
	if e.cfg.SharedSlot.ValidParticipant() {
		req.author = "host_owner"
	}
	return e.sendLocked(model.ActorUser, req.To, req, "user/"+req.ID)
}
func (e *Engine) sendLocked(from, to model.ActorID, req SendRequest, key string) (Message, error) {
	if err := validateSendRequest(req); err != nil {
		return Message{}, err
	}
	original, retry := e.messages[e.sends[key]]
	guardPeer := e.cfg.DynamicBindings && from.ValidParticipant() && to.ValidParticipant()
	if guardPeer && !retry && (req.ExpectedPeerGeneration == 0 || req.ExpectedPeerGeneration != e.generation(to)) {
		return Message{}, ErrPeerChanged
	}
	m := e.newSendCandidateLocked(from, to, req, guardPeer)
	if len(req.AttachmentIDs) > 0 {
		values, err := e.resolveSendAttachmentsLocked(req, original, retry)
		if err != nil {
			return Message{}, err
		}
		m.Attachments = values
	}
	if err := e.mergeQuoteAndLimitsLocked(&m, req); err != nil {
		return Message{}, err
	}
	if retry {
		if !sameMessagePayload(original, m) || guardPeer && original.TargetGeneration != req.ExpectedPeerGeneration {
			return Message{}, ErrSendPayloadConflict
		}
		return cloneMessage(original), nil
	}
	if _, err := e.envelope(m); err != nil {
		return Message{}, err
	}
	if err := e.append(EventMessage, from, messageFact{Message: m, ClientKey: key}); err != nil {
		return Message{}, err
	}
	return cloneMessage(m), nil
}

// validateSendRequest performs the send admission checks in the Local flow's
// original order; it is shared by sendLocked and the enterprise executor.
func validateSendRequest(req SendRequest) error {
	if !validID(req.ID) {
		return errors.New("client message ID is required")
	}
	if err := validateBody(req.Text); err != nil {
		return err
	}
	if strings.TrimSpace(req.Text) == "" && len(req.AttachmentIDs) == 0 {
		return errors.New("message text or attachments required")
	}
	if req.Review != nil {
		if err := req.Review.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// newSendCandidateLocked builds the candidate message for one send: identity
// and routing state from makeMessage, the peer-generation stamp when guarded,
// and the author/review provenance. It is pure — no cache reads, no append,
// no notification — and shared by the Local path and the enterprise executor.
func (e *Engine) newSendCandidateLocked(from, to model.ActorID, req SendRequest, guardPeer bool) Message {
	m := e.makeMessage(from, to, req.Text, "send")
	if guardPeer {
		m.TargetGeneration = req.ExpectedPeerGeneration
	}
	m.Author = req.author
	if req.Review != nil {
		v := *req.Review
		m.Review = &v
	}
	return m
}

// resolveSendAttachmentsLocked resolves the requested attachments: accepted
// identities for a retry, fresh media-store resolution otherwise; the Local
// flow's error classes are preserved verbatim.
func (e *Engine) resolveSendAttachmentsLocked(req SendRequest, original Message, retry bool) ([]model.Attachment, error) {
	var values []model.Attachment
	var err error
	if retry {
		var resolve func(string) (model.Attachment, error)
		if e.cfg.Media != nil {
			resolve = func(id string) (model.Attachment, error) {
				a, _, err := e.cfg.Media.Resolve(id)
				return a, err
			}
		}
		values, err = acceptedAttachments(original, req.AttachmentIDs, resolve)
	} else if e.cfg.Media == nil {
		return nil, errors.New("attachment store unavailable")
	} else {
		values, err = e.cfg.Media.ResolveMany(req.AttachmentIDs)
	}
	if err != nil {
		return nil, unavailableAttachment(err)
	}
	return values, nil
}

// mergeQuoteAndLimitsLocked merges a quoted message's attachment identities
// into the candidate and enforces the attachment limits; shared by the Local
// path and the enterprise executor.
func (e *Engine) mergeQuoteAndLimitsLocked(m *Message, req SendRequest) error {
	if req.QuoteID != "" {
		quote, ok := e.messages[req.QuoteID]
		if !ok {
			return errors.New("quoted message is not in this Room")
		}
		m.Quote = &model.AgentQuote{FromHandle: e.handle(quote.From), Text: quote.Text}
		if e.enterprise && quote.From == model.ActorUser && ValidEnterpriseHumanAuthor(quote.Author) {
			m.Quote.Author = quote.Author
		}
		// Like embedded quoted input, preserve image identity without recursively
		// expanding peer correlation links or duplicating the same attachment.
		seen := make(map[string]bool, len(m.Attachments))
		for _, a := range m.Attachments {
			seen[a.ID] = true
		}
		for _, a := range quote.Attachments {
			if !seen[a.ID] {
				m.Attachments = append(m.Attachments, a)
				seen[a.ID] = true
			}
		}
	}
	if len(m.Attachments) > attachment.MaxImagesPerMessage {
		return errors.New("too many message and quoted attachments")
	}
	var total int64
	for _, a := range m.Attachments {
		total += a.Size
	}
	if total > attachment.MaxTotalImageBytes {
		return errors.New("message and quoted attachments exceed total image limit")
	}
	return nil
}
func (e *Engine) Failure(a Auth, category string) error {
	if err := e.requireLocalMutation(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return err
	}
	// Do not echo vendor error_details/last_assistant_message: those may include
	// provider secrets or a partial response. Failure is visibility, not relay.
	// StopFailure also ends the native Turn without a continuation.
	e.noteLANActivityLocked(a)
	e.observeTurnEndLocked(a.Slot)
	allowed := map[string]bool{"rate_limit": true, "overloaded": true, "authentication_failed": true, "server_error": true, "invalid_request": true, "model_not_found": true, "max_output_tokens": true}
	if !allowed[category] {
		category = "unknown"
	}
	return e.append(EventFailure, a.Slot, map[string]string{"error": category})
}
func (e *Engine) handle(actor model.ActorID) string {
	if actor == model.ActorUser {
		return "@user"
	}
	if e.cfg.DynamicBindings {
		return model.NativeParticipantIdentityFor(actor).MentionHandle
	}
	return model.ParticipantIdentities(e.cfg.Runtimes)[actor].MentionHandle
}
func (e *Engine) envelopeInput(m Message) model.AgentInput {
	input := model.AgentInput{From: m.From, To: m.To, FromHandle: e.handle(m.From), Text: m.Text, Quote: m.Quote}
	if e.cfg.DynamicBindings {
		input.FromRuntime, input.FromGeneration = m.FromRuntime, m.FromGeneration
		input.PeerHandle = e.handle(model.OtherParticipant(m.To))
		input.PeerGeneration = e.peerGenerationLocked(m.To)
	}
	if m.From == model.ActorUser && m.Author != "" {
		if e.enterprise && ValidEnterpriseHumanAuthor(m.Author) {
			input.Author = m.Author
		} else if strings.HasPrefix(m.Author, "lan:") {
			input.FromHandle = protocol.RemoteRoomOwnerHandle
			input.Text += "\n" + protocol.SharedRoomEnvelopeNotice
		} else {
			input.FromHandle = protocol.LocalRoomOwnerHandle
		}
	}
	if m.Review != nil {
		input.Text += m.Review.Envelope()
	}
	return input
}

func (e *Engine) envelope(m Message) (string, error) {
	input := e.envelopeInput(m)
	for _, a := range m.Attachments {
		if e.cfg.Media == nil {
			return "", errors.New("attachment store unavailable")
		}
		metadata, path, err := e.cfg.Media.Resolve(a.ID)
		if err != nil {
			return "", err
		}
		if a.SHA256 == "" || !strings.EqualFold(a.SHA256, metadata.SHA256) {
			return "", errors.New("attachment content changed since message acceptance")
		}
		input.Attachments = append(input.Attachments, model.AgentAttachment{Attachment: metadata, Path: path})
	}
	return prompt.Envelope(input), nil
}

// Claim waits without claiming. Only the final, non-cancelled stage appends a
// delivering fact before releasing the envelope. A timeout never consumes FIFO.
// A Stop park that releases no envelope is the Service's Turn-end observation:
// the harness stops without a continuation.
func (e *Engine) Claim(ctx context.Context, a Auth, park bool) (*Claim, error) {
	if err := e.requireLocalMutation(); err != nil {
		return nil, err
	}
	observed := false
	for {
		e.mu.Lock()
		b, err := e.auth(a, false)
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			if park {
				e.observeTurnEndLocked(a.Slot)
			}
			e.mu.Unlock()
			return nil, err
		}
		if !observed {
			e.noteLANActivityLocked(a)
			observed = true
		}
		if park && !b.ParkEnabled {
			e.observeTurnEndLocked(a.Slot)
			e.mu.Unlock()
			return nil, nil
		}
		if err := e.reapLocked(false); err != nil {
			e.mu.Unlock()
			return nil, err
		}
		busy := e.counts[a.Slot].Delivering > 0
		var head *Message
		if ids := e.queued[a.Slot]; len(ids) > 0 {
			m := e.messages[ids[0]]
			head = &m
		}
		next := selectClaimMessage(head, busy, a.Generation)
		if !busy && next != nil {
			envelope, err := e.envelope(*next)
			if err != nil {
				e.mu.Unlock()
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				e.mu.Unlock()
				return nil, err
			}
			receipt, err := RandomID()
			if err != nil {
				e.mu.Unlock()
				return nil, err
			}
			fact := planClaimFact(*next, receipt, e.cfg.Now())
			if err := e.append(EventMessage, a.Slot, fact); err != nil {
				e.mu.Unlock()
				return nil, err
			}
			result := &Claim{ID: next.ID, Receipt: receipt, Envelope: envelope, PeerGeneration: e.peerGenerationLocked(a.Slot)}
			e.mu.Unlock()
			return result, nil
		}
		// A Stop park only delays the native harness unless a peer reply is
		// plausibly imminent. Queued or in-flight input still waits normally.
		if park && len(e.queued[a.Slot]) == 0 && !busy && !e.replyExpectedLocked(a.Slot) {
			e.observeTurnEndLocked(a.Slot)
			e.mu.Unlock()
			return nil, nil
		}
		changed := e.changed
		// Register the blocked long-poll so WakeCandidate can atomically see
		// live foreground/park collectors for this slot. The count is transient
		// process state, never a durable fact.
		e.waiters[a.Slot]++
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			e.mu.Lock()
			e.waiters[a.Slot]--
			if park {
				e.observeTurnEndLocked(a.Slot)
			}
			e.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
		}
		e.mu.Lock()
		e.waiters[a.Slot]--
		e.mu.Unlock()
	}
}
func (e *Engine) Ack(a Auth, id, receipt string) error {
	if err := e.requireLocalMutation(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// Draining blocks new work, not receipts for already released envelopes.
	if _, err := e.authenticate(a, false); err != nil {
		return err
	}
	if err := e.reapLocked(false); err != nil {
		return err
	}
	m, ok := e.messages[id]
	pendingRetry := false
	if m.State == "unknown" {
		for _, key := range e.unresolved {
			if isPendingRetry(e.messages[key], id) {
				pendingRetry = true
				break
			}
		}
	}
	decision, err := decideAcknowledgement(m, ok, a.Slot, a.Generation, receipt, pendingRetry)
	if err != nil || decision == acknowledgementNoChange {
		return err
	}
	return e.append(EventMessage, a.Slot, planAcknowledgementFact(m, e.cfg.Now()))
}
func (e *Engine) Cancel(id string) error {
	if err := e.requireLocalMutation(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return err
	}
	m, ok := e.messages[id]
	change, err := decideCancellation(m, ok)
	if err != nil || !change {
		return err
	}
	return e.append(EventMessage, model.ActorUser, planCancellationFact(m, e.cfg.Now()))
}
func (e *Engine) Retry(id string) (Message, error) {
	if err := e.requireLocalMutation(); err != nil {
		return Message{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return Message{}, err
	}
	if err := e.reapLocked(false); err != nil {
		return Message{}, err
	}
	old, ok := e.messages[id]
	var targetGeneration uint64
	if old.To != model.ActorUser {
		targetGeneration = e.generation(old.To)
	}
	pendingRetry := false
	for _, key := range e.unresolved {
		if isPendingRetry(e.messages[key], id) {
			pendingRetry = true
			break
		}
	}
	if err := decideRetry(old, ok, e.cfg.DynamicBindings, targetGeneration, pendingRetry); err != nil {
		return Message{}, err
	}
	// Preserve the Local producer's clock-before-ID ordering. No candidate is
	// materialized for a rejected retry.
	now := e.cfg.Now()
	fact := planRetryFact(old, e.cfg.DynamicBindings, targetGeneration, model.NewID("relay"), now)
	if _, err := e.envelope(fact.Message); err != nil {
		return Message{}, err
	}
	if err := e.append(EventMessage, model.ActorUser, fact); err != nil {
		return Message{}, err
	}
	return cloneMessage(fact.Message), nil
}
func (e *Engine) reapLocked(all bool) error {
	now := e.cfg.Now()
	// Planning never appends or applies. Local still persists each expiry in
	// order, stopping on the first append failure and retaining earlier facts.
	inFlight := make([]Message, 0, len(e.inFlight))
	for id := range e.inFlight {
		inFlight = append(inFlight, e.messages[id])
	}
	for _, fact := range planExpiryFacts(inFlight, now, e.cfg.Lease, all) {
		if err := e.append(EventMessage, model.ActorSystem, fact); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) Reap() error {
	if err := e.requireLocalMutation(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	return e.reapLocked(false)
}
func (e *Engine) SetDraining(value bool) {
	// Explicit enterprise semantics: draining is safe flow control — it gates
	// new enterprise commands at enterpriseAdmitted and takes no gate, no
	// durable work and no external effect.
	e.mu.Lock()
	defer e.mu.Unlock()
	e.draining = value
	e.signal()
}

// TryBeginIdleClose checks delivery admission and closes it under the same
// lock as Claim. Service wake/HTTP leases are serialized by the caller.
func (e *Engine) TryBeginIdleClose() bool {
	if e.enterprise {
		// Enterprise Rooms never idle-close themselves: Room lifetime and the
		// ownership lease belong to the deployment owner, and the Local close
		// path reaps. CloseContext is the explicit enterprise shutdown.
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.healthy() != nil || len(e.inFlight) > 0 {
		return false
	}
	for _, count := range e.waiters {
		if count > 0 {
			return false
		}
	}
	e.draining = true
	e.signal()
	return true
}
func (e *Engine) Busy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.inFlight) != 0
}
func (e *Engine) Close() error {
	if e.enterprise {
		// Enterprise shutdown is the cancellable, self-bounded CloseContext; a
		// timed-out wait reports the error and does not claim the engine
		// closed. The shared SQL store is never closed here.
		return e.CloseContext(context.Background())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	err := e.reapLocked(true)
	e.closed = true
	e.signal()
	return errors.Join(err, e.cfg.Store.Close())
}
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}
func (e *Engine) snapshotLocked() Snapshot {
	return e.snapshotRangeLocked(0, 0)
}
func (e *Engine) WaitChanges(ctx context.Context, after uint64) error {
	e.mu.Lock()
	if err := e.available(); err != nil {
		e.mu.Unlock()
		return err
	}
	if e.sequence > after {
		e.mu.Unlock()
		return nil
	}
	changed := e.changed
	e.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	}
}
func cloneMessage(m Message) Message {
	m.Receipt = ""
	if m.Review != nil {
		v := *m.Review
		m.Review = &v
	}
	m.Attachments = append([]model.Attachment(nil), m.Attachments...)
	if m.Quote != nil {
		v := *m.Quote
		m.Quote = &v
	}
	return m
}
func clonePublication(p Publication) Publication {
	if p.Message != nil {
		m := cloneMessage(*p.Message)
		p.Message = &m
	}
	return p
}

// BindingFromEvent exposes only the public projection for the Registry rebuild.
func BindingFromEvent(ev model.Event) (Binding, error) {
	var fact bindingFact
	if ev.Kind != EventBinding {
		return Binding{}, errors.New("not a native binding event")
	}
	if err := json.Unmarshal(ev.Data, &fact); err != nil {
		return Binding{}, err
	}
	if fact.RemoteKey != "" || !fact.Slot.ValidParticipant() || fact.Generation == 0 || !validID(fact.BindID) {
		return Binding{}, errors.New("invalid native binding event")
	}
	return fact.Binding, nil
}

// StableActors makes presentation deterministic without implying turn ownership.
func StableActors(values map[model.ActorID]Binding) []model.ActorID {
	result := make([]model.ActorID, 0, len(values))
	for actor := range values {
		result = append(result, actor)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// AuthenticateBindingEvent is a read-only activation gate, not a substitute for
// the Engine's locked authentication at the effect boundary.
func AuthenticateBindingEvent(ev model.Event, a Auth) error {
	var fact bindingFact
	if ev.Kind != EventBinding || json.Unmarshal(ev.Data, &fact) != nil {
		return ErrAuth
	}
	if !fact.Active || fact.Slot != a.Slot || fact.BindID != a.BindID || fact.Generation != a.Generation || !same(Digest(a.Secret), fact.CredentialHash) {
		return ErrAuth
	}
	if fact.Runtime != "" && (!a.Runtime.Valid() || a.Runtime.Canonical() != fact.Runtime) {
		return ErrAuth
	}
	if fact.SessionID != "" && fact.SessionID != a.SessionID {
		return ErrAuth
	}
	return nil
}
func (e *Engine) UnbindAs(a Auth) error {
	if err := e.requireLocalMutation(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, true)
	if err != nil {
		return err
	}
	return e.commitBinding(planLocalRetirement(b, e.cfg.Now()))
}
func (e *Engine) ParkAs(a Auth, enabled bool) error {
	if err := e.requireLocalMutation(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, false)
	if err != nil {
		return err
	}
	b, changed := planParkBinding(b, enabled)
	if !changed {
		return nil
	}
	// See Park: a park toggle must not run the Registry CommitBinding hook.
	return e.appendBindingFact(b)
}
func (e *Engine) AuthSnapshot(a Auth) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, true)
	if err != nil {
		return Snapshot{}, err
	}
	if b.SessionID == "" && b.RemoteKey == "" {
		return Snapshot{RoomID: e.cfg.RoomID, HostMode: model.HostNative, Bindings: map[model.ActorID]Binding{a.Slot: b.Binding}, Notice: "Binding is not associated with an official session; rebind inside the native session. No inbox access before association."}, nil
	}
	return e.snapshotLocked(), nil
}
