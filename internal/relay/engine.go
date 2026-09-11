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
	"github.com/sean2077/pairroom/internal/store"
)

type Config struct {
	RoomID   string
	Store    *store.JSONLStore
	Runtimes map[model.ActorID]model.RuntimeKind
	Media    *attachment.Store
	Now      func() time.Time
	Lease    time.Duration
	// CommitBinding serializes global native identity ownership with the active
	// Room writer. It must call appendFact exactly once or return an error.
	CommitBinding func(Binding, func() error) error
}

type Engine struct {
	mu         sync.Mutex
	cfg        Config
	bindings   map[model.ActorID]bindingFact
	seenBinds  map[string]bool
	messages   map[string]Message
	order      []string
	sends      map[string]string
	reports    map[string]Publication
	lastReport map[string]uint64
	audit      []Audit
	sequence   uint64
	changed    chan struct{}
	closed     bool
	draining   bool
	fatal      error
}

func Open(cfg Config) (*Engine, error) {
	if cfg.Store == nil || cfg.RoomID == "" {
		return nil, errors.New("native relay requires a published Room store")
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Lease <= 0 {
		cfg.Lease = DeliveryLease
	}
	e := &Engine{cfg: cfg, bindings: map[model.ActorID]bindingFact{}, seenBinds: map[string]bool{}, messages: map[string]Message{}, sends: map[string]string{}, reports: map[string]Publication{}, lastReport: map[string]uint64{}, changed: make(chan struct{})}
	events, err := cfg.Store.Load()
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		if ev.RoomID != cfg.RoomID {
			return nil, errors.New("native relay Room identity mismatch")
		}
		if err := e.apply(ev); err != nil {
			return nil, fmt.Errorf("native replay at event %d: %w", ev.Seq, err)
		}
	}
	// A previous writer cannot prove whether a claimed envelope reached stdout.
	// Preserve queued work; no delivery recovery is an automatic replay.
	for _, id := range e.order {
		m := e.messages[id]
		if m.State == "delivering" {
			m.State = "unknown"
			m.UpdatedAt = e.cfg.Now()
			if err := e.append(EventMessage, model.ActorSystem, messageFact{Message: m}); err != nil {
				return nil, err
			}
		}
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
func (e *Engine) signal() { close(e.changed); e.changed = make(chan struct{}) }
func (e *Engine) append(kind string, actor model.ActorID, payload any) error {
	if e.closed || e.fatal != nil {
		return ErrClosed
	}
	ev, err := model.NewEvent(e.cfg.RoomID, kind, actor, payload)
	if err != nil {
		return err
	}
	ev.CreatedAt = e.cfg.Now()
	if err := e.cfg.Store.Append(&ev); err != nil {
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
	return nil
}
func (e *Engine) apply(ev model.Event) error {
	e.sequence = ev.Seq
	detail := ""
	switch ev.Kind {
	case EventBinding:
		var b bindingFact
		if err := json.Unmarshal(ev.Data, &b); err != nil {
			return err
		}
		if !b.Slot.ValidParticipant() || b.Generation == 0 || !validID(b.BindID) || (b.Active && !validHash(b.CredentialHash)) {
			return errors.New("invalid native binding fact")
		}
		old := e.bindings[b.Slot]
		if b.Generation < old.Generation || (b.Generation == old.Generation && old.BindID != "" && b.BindID != old.BindID) {
			return errors.New("native binding generation regressed")
		}
		e.bindings[b.Slot] = b
		e.seenBinds[b.BindID] = true
		// Revocation is atomic with invalidating old-generation inbox work. Work
		// already handed off cannot be undone, nor does an empty inbox prove idle.
		for id, m := range e.messages {
			if m.To == b.Slot && (!b.Active || m.TargetGeneration != b.Generation) {
				if m.State == "queued" {
					m.State = "cancelled"
					m.UpdatedAt = ev.CreatedAt
					e.messages[id] = m
				}
				if m.State == "delivering" {
					m.State = "unknown"
					m.UpdatedAt = ev.CreatedAt
					e.messages[id] = m
				}
			}
		}
	case EventMessage:
		var fact messageFact
		if err := json.Unmarshal(ev.Data, &fact); err != nil {
			return err
		}
		if !validID(fact.ID) || !validMessageState(fact.State) {
			return errors.New("invalid native message fact")
		}
		fact.Message.Receipt = fact.Receipt
		e.putMessage(fact.Message)
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
	default:
		if strings.HasPrefix(ev.Kind, "native.") {
			return fmt.Errorf("unsupported native event %q", ev.Kind)
		}
		return nil
	}
	e.audit = append(e.audit, Audit{Seq: ev.Seq, Kind: ev.Kind, Actor: ev.Actor, At: ev.CreatedAt, Detail: detail})
	return nil
}
func validMessageState(s string) bool {
	switch s {
	case "queued", "delivering", "handed_off", "unknown", "cancelled", "human":
		return true
	}
	return false
}
func (e *Engine) putMessage(m Message) {
	if _, exists := e.messages[m.ID]; !exists {
		e.order = append(e.order, m.ID)
	}
	e.messages[m.ID] = m
}
func (e *Engine) commitBinding(b bindingFact) error {
	appendFact := func() error { return e.append(EventBinding, b.Slot, b) }
	if e.cfg.CommitBinding != nil {
		return e.cfg.CommitBinding(b.Binding, appendFact)
	}
	return appendFact()
}

func (e *Engine) Bind(slot model.ActorID, req BindRequest) (Binding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return Binding{}, err
	}
	if !slot.ValidParticipant() || !validID(req.BindID) || !validHash(req.CredentialHash) || !validHash(req.NonceHash) {
		return Binding{}, errors.New("invalid binding request")
	}
	old := e.bindings[slot]
	if old.Active {
		// Recovery of an in-flight bind is idempotent without consuming another
		// nonce/generation. An associated session must identify itself as well.
		if old.BindID == req.BindID && same(old.CredentialHash, req.CredentialHash) && (old.SessionID == "" || old.SessionID == req.SessionID) {
			return old.Binding, nil
		}
		if !req.Replace {
			return Binding{}, ErrOccupied
		}
	}
	if e.seenBinds[req.BindID] {
		return Binding{}, errors.New("revoked bind ID cannot be reused")
	}
	b := bindingFact{Binding: Binding{Slot: slot, BindID: req.BindID, Generation: old.Generation + 1, Active: true, ParkEnabled: true, LastActivity: e.cfg.Now()}, CredentialHash: req.CredentialHash, NonceHash: req.NonceHash}
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
	if !ok || !b.Active || a.BindID != b.BindID || a.Generation != b.Generation || !same(Digest(a.Secret), b.CredentialHash) {
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
func (e *Engine) Associate(a Auth, nonce, sessionID, transcript string) (Binding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, true)
	if err != nil {
		return Binding{}, err
	}
	if a.SessionID != sessionID || !validID(sessionID) || len(transcript) > 4096 || strings.ContainsAny(transcript, "\x00\r\n") {
		return Binding{}, errors.New("invalid official hook session metadata")
	}
	if b.SessionID != "" || b.NonceHash == "" || nonce == "" || !same(Digest(nonce), b.NonceHash) {
		return Binding{}, ErrNonce
	}
	b.SessionID = sessionID
	b.TranscriptPath = transcript
	b.NonceHash = ""
	if err := e.commitBinding(b); err != nil {
		return Binding{}, err
	}
	return b.Binding, nil
}
func (e *Engine) Unbind(slot model.ActorID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return err
	}
	b, ok := e.bindings[slot]
	if !ok || !b.Active {
		return nil
	}
	b.Active = false
	b.CredentialHash = ""
	b.NonceHash = ""
	b.SessionID = ""
	b.TranscriptPath = ""
	b.LastActivity = e.cfg.Now()
	return e.commitBinding(b)
}
func (e *Engine) Park(slot model.ActorID, enabled bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return err
	}
	b, ok := e.bindings[slot]
	if !ok || !b.Active {
		return ErrAuth
	}
	b.ParkEnabled = enabled
	return e.append(EventBinding, slot, b)
}
func (e *Engine) Peer(a Auth) (Binding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return Binding{}, err
	}
	return e.bindings[model.OtherParticipant(a.Slot)].Binding, nil
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
	state := "queued"
	var generation uint64
	if to == model.ActorUser {
		state = "human"
	} else {
		generation = e.generation(to)
	}
	return Message{ID: model.NewID("relay"), From: from, To: to, Text: text, State: state, TargetGeneration: generation, CreatedAt: now, UpdatedAt: now, Source: source}
}
func (e *Engine) Report(a Auth, seq uint64, text string) (Publication, error) {
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
		return clonePublication(existing), nil
	}
	last := e.lastReport[bindingKey(a.BindID, a.Generation)]
	if seq <= last {
		return Publication{}, errors.New("report_seq regressed; reconcile the original publication")
	}
	p := Publication{BindID: a.BindID, Generation: a.Generation, ReportSeq: seq}
	parsed := prompt.ParseMentions(text, a.Slot, e.cfg.Runtimes)
	var to model.ActorID
	if len(parsed.Targets) > 0 {
		to = model.OtherParticipant(a.Slot)
	} else if parsed.Human {
		to = model.ActorUser
	}
	if to != "" {
		m := e.makeMessage(a.Slot, to, text, "stop")
		p.Message = &m
	}
	kind := EventPublication
	if seq > last+1 {
		p.GapFrom = last + 1
		p.GapTo = seq - 1
		kind = EventPublicationGap
	}
	// Acceptance and optional enqueue share one durable append. A non-routed
	// boundary records only its idempotency receipt, never the private reply.
	if err := e.append(kind, a.Slot, p); err != nil {
		return Publication{}, err
	}
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
	return e.sendLocked(a.Slot, to, req, bindingKey(a.BindID, a.Generation)+"/send/"+req.ID)
}
func (e *Engine) SendUser(req SendRequest) (Message, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return Message{}, err
	}
	if !req.To.ValidParticipant() {
		return Message{}, errors.New("select a native target slot")
	}
	return e.sendLocked(model.ActorUser, req.To, req, "user/"+req.ID)
}
func (e *Engine) sendLocked(from, to model.ActorID, req SendRequest, key string) (Message, error) {
	if !validID(req.ID) {
		return Message{}, errors.New("client message ID is required")
	}
	if id, ok := e.sends[key]; ok {
		return cloneMessage(e.messages[id]), nil
	}
	if err := validateBody(req.Text); err != nil {
		return Message{}, err
	}
	if strings.TrimSpace(req.Text) == "" && len(req.AttachmentIDs) == 0 {
		return Message{}, errors.New("message text or attachments required")
	}
	m := e.makeMessage(from, to, req.Text, "send")
	if len(req.AttachmentIDs) > 0 {
		if e.cfg.Media == nil {
			return Message{}, errors.New("attachment store unavailable")
		}
		values, err := e.cfg.Media.ResolveMany(req.AttachmentIDs)
		if err != nil {
			return Message{}, err
		}
		m.Attachments = values
	}
	if req.QuoteID != "" {
		quote, ok := e.messages[req.QuoteID]
		if !ok {
			return Message{}, errors.New("quoted message is not in this Room")
		}
		m.Quote = &model.AgentQuote{FromHandle: e.handle(quote.From), Text: quote.Text}
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
		return Message{}, errors.New("too many message and quoted attachments")
	}
	var total int64
	for _, a := range m.Attachments {
		total += a.Size
	}
	if total > attachment.MaxTotalImageBytes {
		return Message{}, errors.New("message and quoted attachments exceed total image limit")
	}
	if _, err := e.envelope(m); err != nil {
		return Message{}, err
	}
	if err := e.append(EventMessage, from, messageFact{Message: m, ClientKey: key}); err != nil {
		return Message{}, err
	}
	return cloneMessage(m), nil
}
func (e *Engine) Failure(a Auth, category string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return err
	}
	// Do not echo vendor error_details/last_assistant_message: those may include
	// provider secrets or a partial response. Failure is visibility, not relay.
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
	return model.ParticipantIdentities(e.cfg.Runtimes)[actor].MentionHandle
}
func (e *Engine) envelope(m Message) (string, error) {
	input := model.AgentInput{From: m.From, To: m.To, FromHandle: e.handle(m.From), Text: m.Text, Quote: m.Quote}
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
func (e *Engine) Claim(ctx context.Context, a Auth, park bool) (*Claim, error) {
	for {
		e.mu.Lock()
		b, err := e.auth(a, false)
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			e.mu.Unlock()
			return nil, err
		}
		if park && !b.ParkEnabled {
			e.mu.Unlock()
			return nil, nil
		}
		if err := e.reapLocked(false); err != nil {
			e.mu.Unlock()
			return nil, err
		}
		busy := false
		var next *Message
		for _, id := range e.order {
			m := e.messages[id]
			if m.To != a.Slot || m.TargetGeneration != a.Generation {
				continue
			}
			if m.State == "delivering" {
				busy = true
				break
			}
			if m.State == "queued" && next == nil {
				v := m
				next = &v
			}
		}
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
			next.State = "delivering"
			next.ClaimedAt = e.cfg.Now()
			next.UpdatedAt = next.ClaimedAt
			next.Receipt = receipt
			if err := e.append(EventMessage, a.Slot, messageFact{Message: *next, Receipt: receipt}); err != nil {
				e.mu.Unlock()
				return nil, err
			}
			result := &Claim{ID: next.ID, Receipt: receipt, Envelope: envelope}
			e.mu.Unlock()
			return result, nil
		}
		changed := e.changed
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}
func (e *Engine) Ack(a Auth, id, receipt string) error {
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
	if !ok || m.To != a.Slot || m.TargetGeneration != a.Generation || receipt == "" || !same(m.Receipt, receipt) {
		return ErrAuth
	}
	if m.State == "handed_off" {
		return nil
	}
	if m.State != "delivering" {
		return errors.New("delivery outcome is unknown; inspect before explicit Retry")
	}
	m.State = "handed_off"
	m.UpdatedAt = e.cfg.Now()
	return e.append(EventMessage, a.Slot, messageFact{Message: m, Receipt: receipt})
}
func (e *Engine) Cancel(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return err
	}
	m, ok := e.messages[id]
	if !ok {
		return errors.New("message not found")
	}
	if m.State == "cancelled" {
		return nil
	}
	if m.State != "queued" {
		return errors.New("Cancel only removes queued native messages; it cannot stop native work")
	}
	m.State = "cancelled"
	m.UpdatedAt = e.cfg.Now()
	return e.append(EventMessage, model.ActorUser, messageFact{Message: m})
}
func (e *Engine) Retry(id string) (Message, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return Message{}, err
	}
	if err := e.reapLocked(false); err != nil {
		return Message{}, err
	}
	old, ok := e.messages[id]
	if !ok || old.State != "unknown" {
		return Message{}, errors.New("only unknown messages can be explicitly retried")
	}
	for _, m := range e.messages {
		if m.RetryOf == id && (m.State == "queued" || m.State == "delivering") {
			return Message{}, errors.New("a retry is already pending")
		}
	}
	m := e.makeMessage(old.From, old.To, old.Text, "retry")
	m.Attachments = append([]model.Attachment(nil), old.Attachments...)
	m.Quote = old.Quote
	m.RetryOf = id
	if _, err := e.envelope(m); err != nil {
		return Message{}, err
	}
	if err := e.append(EventMessage, model.ActorUser, messageFact{Message: m}); err != nil {
		return Message{}, err
	}
	return cloneMessage(m), nil
}
func (e *Engine) reapLocked(all bool) error {
	now := e.cfg.Now()
	for _, id := range e.order {
		m := e.messages[id]
		if m.State == "delivering" && (all || !now.Before(m.ClaimedAt.Add(e.cfg.Lease))) {
			m.State = "unknown"
			m.UpdatedAt = now
			if err := e.append(EventMessage, model.ActorSystem, messageFact{Message: m, Receipt: m.Receipt}); err != nil {
				return err
			}
		}
	}
	return nil
}
func (e *Engine) Reap() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	return e.reapLocked(false)
}
func (e *Engine) SetDraining(value bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.draining = value
	e.signal()
}
func (e *Engine) Busy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range e.messages {
		if m.State == "delivering" {
			return true
		}
	}
	return false
}
func (e *Engine) Close() error {
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
	s := Snapshot{HostMode: model.HostNative, RoomID: e.cfg.RoomID, Bindings: map[model.ActorID]Binding{}, Messages: make([]Message, 0, len(e.order)), Audit: append([]Audit(nil), e.audit...), Sequence: e.sequence, Notice: "handed_off means CLI stdout was written, not native acceptance. Interrupted replies and crashes before atomic publication may be undetectably lost. Park is bounded; queue and nudge/wait outside its window. Native work remains user-owned."}
	for slot, b := range e.bindings {
		s.Bindings[slot] = b.Binding
	}
	for _, id := range e.order {
		s.Messages = append(s.Messages, cloneMessage(e.messages[id]))
	}
	return s
}
func (e *Engine) WaitChanges(ctx context.Context, after uint64) error {
	e.mu.Lock()
	if e.sequence > after {
		e.mu.Unlock()
		return nil
	}
	changed := e.changed
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return ErrClosed
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	}
}
func cloneMessage(m Message) Message {
	m.Receipt = ""
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
	if !fact.Slot.ValidParticipant() || fact.Generation == 0 || !validID(fact.BindID) {
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
	if fact.SessionID != "" && fact.SessionID != a.SessionID {
		return ErrAuth
	}
	return nil
}
func (e *Engine) UnbindAs(a Auth) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, true)
	if err != nil {
		return err
	}
	b.Active = false
	b.CredentialHash = ""
	b.NonceHash = ""
	b.SessionID = ""
	b.TranscriptPath = ""
	b.LastActivity = e.cfg.Now()
	return e.commitBinding(b)
}
func (e *Engine) ParkAs(a Auth, enabled bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, false)
	if err != nil {
		return err
	}
	b.ParkEnabled = enabled
	return e.append(EventBinding, a.Slot, b)
}
func (e *Engine) AuthSnapshot(a Auth) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, true)
	if err != nil {
		return Snapshot{}, err
	}
	if b.SessionID == "" {
		return Snapshot{RoomID: e.cfg.RoomID, HostMode: model.HostNative, Bindings: map[model.ActorID]Binding{a.Slot: b.Binding}, Notice: "Awaiting nonce from an approved official Stop hook; no inbox access before association."}, nil
	}
	return e.snapshotLocked(), nil
}
