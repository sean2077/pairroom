package relay

import (
	"errors"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// enterpriseBindingIdentity is deliberately private and absent from the
// Local/LAN binding codec. No fake native SessionID, credential or RemoteKey
// substitutes for an enterprise Agent identity.
type enterpriseBindingIdentity struct {
	agentID       string
	retiredAt     time.Time
	retiredReason string
}

func (e *Engine) applyEnterpriseBinding(event model.Event) error {
	if !e.enterprise || !e.cfg.DynamicBindings || e.cfg.SharedSlot != "" {
		return errors.New("enterprise Binding fact requires an enterprise Native Room")
	}
	identity, err := model.DecodeEnterpriseBindingUpdate(event)
	if err != nil {
		return err
	}
	if identity.RoomID != e.cfg.RoomID {
		return errors.New("enterprise Binding belongs to another Room")
	}
	binding := bindingFact{Binding: Binding{Slot: identity.Slot, Runtime: identity.Runtime, BindID: identity.BindID, Generation: identity.Generation, Active: identity.Active, ParkEnabled: identity.ParkEnabled},
		enterprise: &enterpriseBindingIdentity{agentID: identity.AgentID, retiredReason: identity.RetiredReason}}
	if identity.RetiredAt != nil {
		binding.enterprise.retiredAt = *identity.RetiredAt
	}
	old := e.bindings[binding.Slot]
	if old.BindID != "" && old.enterprise == nil {
		return errors.New("enterprise Binding cannot replace a Local or LAN identity")
	}
	if err := e.validateBindingGenerationLocked(binding, old); err != nil {
		return err
	}
	if binding.Generation == old.Generation {
		if old.enterprise == nil || binding.Runtime != old.Runtime || binding.enterprise.agentID != old.enterprise.agentID || !old.Active && binding.Active {
			return errors.New("enterprise Binding identity changed within a generation")
		}
		if !old.Active && (!binding.enterprise.retiredAt.Equal(old.enterprise.retiredAt) || binding.enterprise.retiredReason != old.enterprise.retiredReason) {
			return errors.New("enterprise Binding retirement is immutable")
		}
	}
	for slot, other := range e.bindings {
		if slot == binding.Slot || !binding.Active || !other.Active {
			continue
		}
		if other.enterprise == nil || binding.enterprise.agentID == other.enterprise.agentID {
			return errors.New("enterprise Agent is already bound to another slot or the Room mixes binding authorities")
		}
	}
	return e.applyBindingProjection(event, binding)
}
