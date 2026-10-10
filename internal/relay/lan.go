package relay

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

const (
	EventLANInvite = "native.lan.invite"
	EventLANJoin   = "native.lan.join"
	EventLANMember = "native.lan.member"
)

// LAN records contain only public key fingerprints and Room transport IDs.
// The peer's vendor session, workspace, transcript and private key stay local.
type LANInvite struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
	Consumed  bool      `json:"consumed"`
}
type LANJoinRequest struct {
	InviteID  string            `json:"invite_id"`
	RequestID string            `json:"request_id"`
	Key       string            `json:"fingerprint"`
	Runtime   model.RuntimeKind `json:"runtime"`
	Label     string            `json:"label,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}
type LANMember struct {
	RequestID string  `json:"request_id"`
	InviteID  string  `json:"invite_id"`
	Binding   Binding `json:"binding"`
}
type LANState struct {
	Invites []LANInvite      `json:"invites"`
	Pending []LANJoinRequest `json:"pending"`
	Member  *LANMember       `json:"member,omitempty"`
}
type Prepared struct {
	Message Message `json:"message"`
	Digest  string  `json:"digest"`
}
type StructuredClaim struct {
	ID      string  `json:"id"`
	Receipt string  `json:"receipt"`
	Message Message `json:"message"`
}

func (e *Engine) initLAN() {
	if e.lanInvites == nil {
		e.lanInvites = make(map[string]LANInvite)
	}
	if e.lanRequests == nil {
		e.lanRequests = make(map[string]LANJoinRequest)
	}
}
func (e *Engine) applyLAN(ev model.Event) error {
	e.initLAN()
	if !e.cfg.SharedSlot.ValidParticipant() {
		return errors.New("LAN fact requires a provisioned shared slot")
	}
	switch ev.Kind {
	case EventLANInvite:
		var v LANInvite
		if json.Unmarshal(ev.Data, &v) != nil || !validID(v.ID) || v.ExpiresAt.IsZero() || v.Consumed {
			return errors.New("invalid LAN invitation fact")
		}
		if _, ok := e.lanInvites[v.ID]; ok {
			return errors.New("duplicate LAN invitation")
		}
		e.lanInvites[v.ID] = v
	case EventLANJoin:
		var j LANJoinRequest
		if json.Unmarshal(ev.Data, &j) != nil || !validID(j.RequestID) || !validID(j.InviteID) || !validHash(j.Key) || !j.Runtime.Valid() || len(j.Label) > 128 || !utf8.ValidString(j.Label) || strings.ContainsFunc(j.Label, unicode.IsControl) || j.CreatedAt.IsZero() {
			return errors.New("invalid LAN join request fact")
		}
		v, ok := e.lanInvites[j.InviteID]
		if !ok || v.Consumed || !j.CreatedAt.Before(v.ExpiresAt) {
			return errors.New("LAN join request lacks a live invitation")
		}
		if _, ok := e.lanRequests[j.RequestID]; ok {
			return errors.New("duplicate LAN join request")
		}
		e.lanRequests[j.RequestID] = j
	case EventLANMember:
		var m LANMember
		if json.Unmarshal(ev.Data, &m) != nil {
			return errors.New("invalid LAN membership fact")
		}
		j, ok := e.lanRequests[m.RequestID]
		b := m.Binding
		if !ok || j.InviteID != m.InviteID || j.Key != b.RemoteKey || b.Slot != e.cfg.SharedSlot || b.Runtime != j.Runtime || b.SessionID != "" || b.TranscriptPath != "" || !validHash(b.RemoteKey) {
			return errors.New("LAN membership does not match admitted request")
		}
		v, ok := e.lanInvites[m.InviteID]
		if !ok {
			return errors.New("LAN membership lacks invitation")
		}
		old := e.bindings[b.Slot]
		continuing := e.lanMember != nil && e.lanMember.RequestID == m.RequestID && old.BindID == b.BindID && old.Generation == b.Generation
		if !continuing && (v.Consumed || old.Active || !b.Active || b.Generation != old.Generation+1) {
			return errors.New("LAN membership admission conflicts with current member")
		}
		if !continuing && !ev.CreatedAt.Before(v.ExpiresAt) {
			return errors.New("LAN membership accepted after invitation expiry")
		}
		if old.RemoteKey != "" && old.Runtime != b.Runtime {
			return errors.New("LAN runtime selection is immutable after admission")
		}
		if err := e.applyBinding(ev, bindingFact{Binding: b}); err != nil {
			return err
		}
		v.Consumed = true
		e.lanInvites[m.InviteID] = v
		e.cfg.Runtimes[b.Slot] = b.Runtime
		e.lanMember = &m
	default:
		return errors.New("unsupported LAN fact")
	}
	return nil
}

func LANBindingFromEvent(ev model.Event) (Binding, error) {
	var m LANMember
	if ev.Kind != EventLANMember || json.Unmarshal(ev.Data, &m) != nil || !m.Binding.Slot.ValidParticipant() || !validHash(m.Binding.RemoteKey) || !m.Binding.Runtime.Valid() || m.Binding.SessionID != "" || m.Binding.TranscriptPath != "" {
		return Binding{}, errors.New("invalid LAN membership event")
	}
	return m.Binding, nil
}
func (e *Engine) appendLANBinding(b bindingFact) error {
	if e.lanMember == nil {
		return errors.New("LAN membership is missing")
	}
	m := *e.lanMember
	m.Binding = b.Binding
	return e.append(EventLANMember, model.ActorSystem, m)
}
func (e *Engine) CreateLANInvite(owner ...Auth) (LANInvite, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.authorizeLANOwnerLocked(owner); err != nil {
		return LANInvite{}, err
	}
	e.initLAN()
	if err := e.healthy(); err != nil {
		return LANInvite{}, err
	}
	if !e.cfg.SharedSlot.ValidParticipant() {
		return LANInvite{}, errors.New("Room was not created for LAN sharing")
	}
	if e.bindings[e.cfg.SharedSlot].Active {
		return LANInvite{}, ErrOccupied
	}
	for _, v := range e.lanInvites {
		if !v.Consumed && e.cfg.Now().Before(v.ExpiresAt) {
			return v, nil
		}
	}
	id, err := RandomID()
	if err != nil {
		return LANInvite{}, err
	}
	v := LANInvite{ID: id, ExpiresAt: e.cfg.Now().Add(10 * time.Minute)}
	return v, e.append(EventLANInvite, model.ActorSystem, v)
}
func (e *Engine) RequestLANJoin(j LANJoinRequest) (LANJoinRequest, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initLAN()
	if err := e.healthy(); err != nil {
		return LANJoinRequest{}, "", err
	}
	if !validID(j.RequestID) || !validHash(j.Key) || !j.Runtime.Valid() || len(j.Label) > 128 || !utf8.ValidString(j.Label) || strings.ContainsFunc(j.Label, unicode.IsControl) {
		return LANJoinRequest{}, "", errors.New("invalid join request")
	}
	if prior, ok := e.lanRequests[j.RequestID]; ok {
		if prior.Key != j.Key || prior.InviteID != j.InviteID || prior.Runtime != j.Runtime {
			return LANJoinRequest{}, "", ErrAuth
		}
		return prior, e.joinStatusLocked(prior), nil
	}
	v, ok := e.lanInvites[j.InviteID]
	if !ok || v.Consumed || !e.cfg.Now().Before(v.ExpiresAt) {
		return LANJoinRequest{}, "", errors.New("invitation expired or consumed")
	}
	if len(e.lanRequests) >= 4096 {
		return LANJoinRequest{}, "", errors.New("Room join history limit reached")
	}
	pending := 0
	for _, p := range e.lanRequests {
		if p.InviteID == v.ID {
			pending++
		}
	}
	if pending >= 32 {
		return LANJoinRequest{}, "", errors.New("too many pending requests for this invitation")
	}
	j.CreatedAt = e.cfg.Now()
	if err := e.append(EventLANJoin, model.ActorSystem, j); err != nil {
		return LANJoinRequest{}, "", err
	}
	return j, "pending", nil
}
func (e *Engine) joinStatusLocked(j LANJoinRequest) string {
	if e.lanMember != nil && e.lanMember.RequestID == j.RequestID {
		if e.lanMember.Binding.Active {
			return "accepted"
		}
		return "revoked"
	}
	v := e.lanInvites[j.InviteID]
	if v.Consumed || !e.cfg.Now().Before(v.ExpiresAt) {
		return "expired"
	}
	return "pending"
}
func (e *Engine) LANJoinStatus(requestID, key string) (LANJoinRequest, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return LANJoinRequest{}, "", err
	}
	j, ok := e.lanRequests[requestID]
	if !ok || !same(j.Key, key) {
		return LANJoinRequest{}, "", ErrAuth
	}
	return j, e.joinStatusLocked(j), nil
}
func (e *Engine) AcceptLANJoin(requestID, key string, owner ...Auth) (Binding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.authorizeLANOwnerLocked(owner); err != nil {
		return Binding{}, err
	}
	if err := e.healthy(); err != nil {
		return Binding{}, err
	}
	j, ok := e.lanRequests[requestID]
	if !ok || !same(j.Key, key) {
		return Binding{}, ErrAuth
	}
	if e.lanMember != nil && e.lanMember.RequestID == requestID && e.lanMember.Binding.Active {
		return e.lanMember.Binding, nil
	}
	v := e.lanInvites[j.InviteID]
	if v.Consumed || !e.cfg.Now().Before(v.ExpiresAt) {
		return Binding{}, errors.New("invitation expired or consumed")
	}
	old := e.bindings[e.cfg.SharedSlot]
	if old.Active {
		return Binding{}, ErrOccupied
	}
	if kind := e.cfg.Runtimes[e.cfg.SharedSlot]; kind.Valid() && kind != j.Runtime {
		return Binding{}, errors.New("peer runtime selection is immutable; create another shared Room for a different runtime")
	}
	id, err := RandomID()
	if err != nil {
		return Binding{}, err
	}
	b := Binding{Slot: e.cfg.SharedSlot, Runtime: j.Runtime, RemoteKey: key, BindID: id, Generation: old.Generation + 1, Active: true, ParkEnabled: true, LastActivity: e.cfg.Now()}
	m := LANMember{RequestID: requestID, InviteID: j.InviteID, Binding: b}
	appendFact := func() error { return e.append(EventLANMember, model.ActorSystem, m) }
	if e.cfg.CommitBinding != nil {
		err = e.cfg.CommitBinding(b, appendFact)
	} else {
		err = appendFact()
	}
	return b, err
}
func (e *Engine) RevokeLANMember(owner ...Auth) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.authorizeLANOwnerLocked(owner); err != nil {
		return err
	}
	if err := e.healthy(); err != nil {
		return err
	}
	if e.lanMember == nil || !e.lanMember.Binding.Active {
		return nil
	}
	b := e.bindings[e.cfg.SharedSlot]
	b.Active = false
	b.LastActivity = e.cfg.Now()
	return e.commitBinding(b)
}
func (e *Engine) LANState() LANState {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := LANState{Invites: []LANInvite{}, Pending: []LANJoinRequest{}}
	for _, v := range e.lanInvites {
		if !v.Consumed && e.cfg.Now().Before(v.ExpiresAt) {
			result.Invites = append(result.Invites, v)
		}
	}
	for _, j := range e.lanRequests {
		if e.joinStatusLocked(j) == "pending" {
			result.Pending = append(result.Pending, j)
		}
	}
	if e.lanMember != nil {
		v := *e.lanMember
		result.Member = &v
	}
	return result
}
func (e *Engine) LANAuth(key string) (Auth, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.bindings[e.cfg.SharedSlot]
	if err := e.healthy(); err != nil {
		return Auth{}, err
	}
	if !b.Active || key == "" || !same(b.RemoteKey, key) {
		return Auth{}, ErrAuth
	}
	return Auth{Slot: b.Slot, BindID: b.BindID, Generation: b.Generation, MemberKey: key}, nil
}
func (e *Engine) Runtimes() map[model.ActorID]model.RuntimeKind {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make(map[model.ActorID]model.RuntimeKind, 2)
	for slot, kind := range e.cfg.Runtimes {
		result[slot] = kind
	}
	return result
}

// PrepareHead is non-consuming. File bytes must be staged and verified before
// ClaimPrepared starts the delivery lease. It rechecks the same membership on
// every wake and never returns a host-local attachment path or native session.
func (e *Engine) PrepareHead(ctx context.Context, a Auth, park bool) (*Prepared, error) {
	observed := false
	for {
		e.mu.Lock()
		b, err := e.auth(a, false)
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		if err = ctx.Err(); err != nil {
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
		if err = e.reapLocked(false); err != nil {
			e.mu.Unlock()
			return nil, err
		}
		busy := e.counts[a.Slot].Delivering > 0
		if ids := e.queued[a.Slot]; !busy && len(ids) > 0 {
			m := e.messages[ids[0]]
			if m.TargetGeneration == a.Generation {
				m = cloneMessage(m)
				data, _ := json.Marshal(m)
				p := &Prepared{Message: m, Digest: Digest(string(data))}
				e.mu.Unlock()
				return p, nil
			}
		}
		if park && len(e.queued[a.Slot]) == 0 && !busy && !e.replyExpectedLocked(a.Slot) {
			e.observeTurnEndLocked(a.Slot)
			e.mu.Unlock()
			return nil, nil
		}
		changed := e.changed
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
func (e *Engine) ClaimPrepared(ctx context.Context, a Auth, id, digest string, park bool) (*StructuredClaim, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, false)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if park && !b.ParkEnabled {
		return nil, nil
	}
	if err = e.reapLocked(false); err != nil {
		return nil, err
	}
	ids := e.queued[a.Slot]
	if e.counts[a.Slot].Delivering > 0 || len(ids) == 0 || ids[0] != id {
		return nil, nil
	}
	m := e.messages[id]
	data, _ := json.Marshal(cloneMessage(m))
	if m.TargetGeneration != a.Generation || !same(Digest(string(data)), digest) {
		return nil, errors.New("prepared inbox generation or content changed")
	}
	receipt, err := RandomID()
	if err != nil {
		return nil, err
	}
	m.State = "delivering"
	m.ClaimedAt = e.cfg.Now()
	m.UpdatedAt = m.ClaimedAt
	m.Receipt = receipt
	if err = e.append(EventMessage, a.Slot, messageFact{Message: m, Receipt: receipt}); err != nil {
		return nil, err
	}
	e.noteLANActivityLocked(a)
	return &StructuredClaim{ID: m.ID, Receipt: receipt, Message: cloneMessage(m)}, nil
}

// SendLANUser scopes idempotency and provenance to the admitted remote owner.
// Its authority is a human message in this Room, never host process control.
func (e *Engine) SendLANUser(a Auth, req SendRequest) (Message, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return Message{}, err
	}
	if a.MemberKey == "" || !req.To.ValidParticipant() {
		return Message{}, ErrAuth
	}
	req.author = "lan:" + a.MemberKey
	m, err := e.sendLocked(model.ActorUser, req.To, req, "lan-user/"+a.MemberKey+"/"+req.ID)
	return m, err
}

// SharedAttachment authorizes an object only while this generation is a
// member and the object is already explicitly present in shared Room history.
func (e *Engine) SharedAttachment(a Auth, id string) (model.Attachment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return model.Attachment{}, err
	}
	for _, key := range e.order {
		for _, v := range e.messages[key].Attachments {
			if v.ID == id {
				return v, nil
			}
		}
	}
	return model.Attachment{}, errors.New("attachment is not in shared Room history")
}

func (e *Engine) LANUserReceipt(a Auth, id string) (*Message, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return nil, err
	}
	if !validID(id) {
		return nil, errors.New("invalid client message ID")
	}
	if m, ok := e.messages[e.sends["lan-user/"+a.MemberKey+"/"+id]]; ok {
		m = cloneMessage(m)
		return &m, nil
	}
	return nil, nil
}

// A bound native owner may issue explicit invitation/accept/revoke commands.
// Its generation is checked at the same locked boundary as the durable effect;
// full local Management callers already carry owner authority and omit it.
func (e *Engine) authorizeLANOwnerLocked(owner []Auth) error {
	if len(owner) == 0 {
		return nil
	}
	if len(owner) != 1 {
		return ErrAuth
	}
	a := owner[0]
	if !e.cfg.SharedSlot.ValidParticipant() || a.Slot != model.OtherParticipant(e.cfg.SharedSlot) || a.MemberKey != "" {
		return ErrAuth
	}
	_, err := e.auth(a, false)
	return err
}
