package relay

import (
	"errors"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
)

func initializeDynamicRuntimes(cfg *Config) {
	if !cfg.DynamicBindings {
		return
	}
	cfg.Runtimes = make(map[model.ActorID]model.RuntimeKind, 2)
	for _, slot := range model.SlotActors() {
		cfg.Runtimes[slot] = vacantRuntime(slot, cfg.SharedSlot)
	}
}

func vacantRuntime(slot, shared model.ActorID) model.RuntimeKind {
	if slot == shared {
		return model.RuntimeAwaitingPeer
	}
	return model.RuntimeAwaitingBinding
}

func (e *Engine) ProtocolVersion() string {
	if e.cfg.DynamicBindings {
		return protocol.NativeDynamicVersion
	}
	return protocol.NativeVersion
}

// BootstrapContext returns the peer generation represented by this exact
// bootstrap, under the same lock. It is context provenance, not permission to
// retag an already-produced response after a peer replacement.
func (e *Engine) BootstrapContext(slot model.ActorID) (string, uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bootstrapContextLocked(slot)
}

// BootstrapContextForBinding produces a coherent response for a previously
// accepted binding. Replacing or revoking that binding before this read must
// not return its successor's bootstrap with the original credentials.
// The caller already obtained b from a trusted Engine operation; this is an
// identity consistency check, not a transport authentication substitute.
func (e *Engine) BootstrapContextForBinding(b Binding) (string, uint64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.available(); err != nil {
		return "", 0, err
	}
	current := e.bindings[b.Slot]
	if !b.Slot.ValidParticipant() || !b.Active || !current.Active || b.BindID != current.BindID || b.Generation != current.Generation || b.Runtime != current.Runtime || b.SessionID != current.SessionID || b.RemoteKey != current.RemoteKey {
		return "", 0, ErrAuth
	}
	text, generation := e.bootstrapContextLocked(b.Slot)
	return text, generation, nil
}

func (e *Engine) bootstrapContextLocked(slot model.ActorID) (string, uint64) {
	if !slot.ValidParticipant() {
		return "", 0
	}
	peer := model.OtherParticipant(slot)
	if e.cfg.DynamicBindings {
		generation := e.generation(peer)
		return protocol.NativeDynamicBootstrap(slot, e.cfg.Runtimes[slot], e.cfg.Runtimes[peer], generation), generation
	}
	return protocol.NativeBootstrap(slot, e.cfg.Runtimes[slot], e.cfg.Runtimes[peer]), 0
}

func (e *Engine) Bootstrap(slot model.ActorID) string {
	text, _ := e.BootstrapContext(slot)
	return text
}

// PeerGeneration uses the observer's slot. An empty peer reserves the next
// binding generation; it never denotes a wildcard successor.
func (e *Engine) PeerGeneration(observer model.ActorID) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.peerGenerationLocked(observer)
}

func (e *Engine) peerGenerationLocked(observer model.ActorID) uint64 {
	if !e.cfg.DynamicBindings || !observer.ValidParticipant() {
		return 0
	}
	return e.generation(model.OtherParticipant(observer))
}

func (e *Engine) targetGenerationsLocked() map[model.ActorID]uint64 {
	if !e.cfg.DynamicBindings {
		return nil
	}
	return map[model.ActorID]uint64{
		model.ActorSlot1: e.generation(model.ActorSlot1),
		model.ActorSlot2: e.generation(model.ActorSlot2),
	}
}

func (e *Engine) bindingRuntimeLocked(slot model.ActorID) model.RuntimeKind {
	if e.cfg.DynamicBindings {
		return e.bindings[slot].Runtime
	}
	return e.cfg.Runtimes[slot].Canonical()
}

// resolveBindSlotLocked resolves retries before looking for a vacancy. The
// caller keeps the lock through commitBinding, so two auto binds cannot choose
// the same slot and a retry cannot accidentally occupy the second slot.
func (e *Engine) resolveBindSlotLocked(slot model.ActorID, req BindRequest) (model.ActorID, error) {
	if !e.cfg.DynamicBindings {
		return slot, nil
	}
	if err := e.healthy(); err != nil {
		return "", err
	}
	if !validID(req.BindID) || !validHash(req.CredentialHash) || !validID(req.SessionID) || !req.Runtime.Valid() {
		return "", errors.New("dynamic Native binding requires the actual runtime and official session identity")
	}
	for _, candidate := range model.SlotActors() {
		b := e.bindings[candidate]
		if b.BindID != req.BindID {
			continue
		}
		if !b.Active || b.RemoteKey != "" || slot != "" && slot != candidate || b.Runtime != req.Runtime.Canonical() || b.SessionID != req.SessionID || !same(b.CredentialHash, req.CredentialHash) {
			return "", ErrAuth
		}
		return candidate, nil
	}
	if e.seenBinds[req.BindID] {
		return "", errors.New("revoked bind ID cannot be reused")
	}
	if slot != "" {
		return slot, nil
	}
	if req.Replace {
		return "", errors.New("explicit replacement requires its original slot")
	}
	for _, candidate := range model.SlotActors() {
		if candidate != e.cfg.SharedSlot && !e.bindings[candidate].Active {
			return candidate, nil
		}
	}
	return "", ErrOccupied
}

func (e *Engine) validateDynamicBinding(b, old bindingFact) error {
	if !e.cfg.DynamicBindings {
		return nil
	}
	if !b.Runtime.Valid() || b.Runtime != b.Runtime.Canonical() || b.Active && b.RemoteKey == "" && !validID(b.SessionID) {
		return errors.New("dynamic Native binding lacks its actual runtime/session identity")
	}
	if b.Generation == old.Generation {
		// Local unbind erases credentials and native session references. That
		// terminal erasure is the only identity change allowed within a
		// generation; Runtime remains as historical attribution.
		erasing := old.Active && !b.Active && b.RemoteKey == "" && b.SessionID == "" && b.CredentialHash == "" && b.TranscriptPath == ""
		if b.Runtime != old.Runtime || b.RemoteKey != old.RemoteKey || !old.Active && b.Active || !erasing && (b.SessionID != old.SessionID || b.CredentialHash != old.CredentialHash) {
			return errors.New("dynamic Native binding identity changed within a generation")
		}
	}
	for slot, other := range e.bindings {
		if slot != b.Slot && b.Active && other.Active && b.RemoteKey == "" && other.RemoteKey == "" && b.Runtime == other.Runtime && b.SessionID == other.SessionID {
			return errors.New("this runtime/session is already bound to the other slot")
		}
	}
	return nil
}

func (e *Engine) validateDynamicMessage(m Message) error {
	if !e.cfg.DynamicBindings {
		return nil
	}
	if !m.From.ValidParticipant() && m.From != model.ActorUser || !m.To.ValidParticipant() && m.To != model.ActorUser {
		return errors.New("invalid dynamic Native message participants")
	}
	if m.To.ValidParticipant() && m.TargetGeneration == 0 || m.To == model.ActorUser && m.TargetGeneration != 0 {
		return errors.New("dynamic Native message lacks its fixed target generation")
	}
	if m.From.ValidParticipant() {
		if !m.FromRuntime.Valid() || m.FromRuntime != m.FromRuntime.Canonical() || m.FromGeneration == 0 {
			return errors.New("dynamic Native message lacks its source runtime/generation")
		}
	} else if m.FromRuntime != "" || m.FromGeneration != 0 {
		return errors.New("human message cannot claim a native source binding")
	}
	if old, ok := e.messages[m.ID]; ok {
		if m.From != old.From || m.To != old.To || m.FromRuntime != old.FromRuntime || m.FromGeneration != old.FromGeneration || m.TargetGeneration != old.TargetGeneration {
			return errors.New("dynamic Native message identity changed")
		}
		return nil
	}
	if m.RetryOf != "" {
		old, ok := e.messages[m.RetryOf]
		if !ok || m.From != old.From || m.To != old.To || m.FromRuntime != old.FromRuntime || m.FromGeneration != old.FromGeneration || m.TargetGeneration != old.TargetGeneration {
			return errors.New("retry changed its original Native source or target")
		}
		return nil
	}
	if m.From.ValidParticipant() {
		b := e.bindings[m.From]
		if !b.Active || m.FromRuntime != b.Runtime || m.FromGeneration != b.Generation {
			return errors.New("message source does not match its active Native binding")
		}
	}
	return nil
}
