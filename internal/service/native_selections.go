package service

import (
	"errors"
	"fmt"

	"github.com/sean2077/pairroom/internal/model"
)

// New Native Rooms persist vacant seats. Runtime is a binding-generation fact,
// while Agents is only a derived display projection after association.
func nativeUnboundSelections(sharing string, owner model.ActorID) map[model.ActorID]model.AgentSelection {
	selections := map[model.ActorID]model.AgentSelection{
		model.ActorSlot1: {AwaitingBinding: true},
		model.ActorSlot2: {AwaitingBinding: true},
	}
	if sharing == "lan" && owner.ValidParticipant() {
		selections[model.OtherParticipant(owner)] = model.AgentSelection{AwaitingPeer: true}
	}
	return selections
}

func nativeDynamicSelection(kind model.RuntimeKind, active, remote bool) model.AgentSelection {
	if active {
		return model.AgentSelection{Runtime: kind, Provider: model.NativeProviderRef()}
	}
	if remote {
		return model.AgentSelection{AwaitingPeer: true}
	}
	return model.AgentSelection{AwaitingBinding: true}
}

func validateRoomSelections(mode model.HostMode, sharing string, owner model.ActorID, agents map[model.ActorID]model.AgentSelection, dynamic bool) error {
	if !dynamic {
		for _, selection := range agents {
			if selection.AwaitingBinding {
				return errors.New("awaiting_binding requires a dynamic Native Room")
			}
		}
		return validateLANSelections(mode, sharing, owner, agents)
	}
	if mode != model.HostNative {
		return errors.New("dynamic_bindings requires Native hosting")
	}
	if sharing != "" && sharing != "lan" || sharing == "" && owner != "" || sharing == "lan" && !owner.ValidParticipant() {
		return errors.New("LAN sharing requires one local owner slot; local Rooms have no owner_slot")
	}
	if len(agents) != 2 {
		return errors.New("dynamic Native Rooms require exactly two seats")
	}
	for _, slot := range model.SlotActors() {
		selection, ok := agents[slot]
		if !ok {
			return fmt.Errorf("dynamic Native Room is missing %s", slot)
		}
		remote := sharing == "lan" && slot != owner
		if selection.AwaitingBinding || selection.AwaitingPeer {
			if selection != nativeDynamicSelection("", false, remote) {
				return errors.New("vacant Native seats cannot select a Runtime or execution configuration")
			}
			continue
		}
		if !selection.Runtime.Valid() || selection.Runtime != selection.Runtime.Canonical() || selection != nativeDynamicSelection(selection.Runtime, true, remote) {
			return errors.New("native Runtime is derived from its binding; configure execution settings in the native harness")
		}
	}
	return nil
}

func roomBindingRuntime(room Room, slot model.ActorID) model.RuntimeKind {
	if room.DynamicBindings {
		return room.Bindings[slot].Runtime
	}
	return room.Agents[slot].Runtime.CanonicalForSlot(slot)
}

func roomParticipantIdentities(room Room) map[model.ActorID]model.ParticipantIdentity {
	if !room.DynamicBindings {
		return model.ParticipantIdentities(selectionsRuntimeKinds(room.Agents))
	}
	identities := make(map[model.ActorID]model.ParticipantIdentity, 2)
	for _, slot := range model.SlotActors() {
		identities[slot] = model.NativeParticipantIdentityFor(slot)
	}
	return identities
}

func validRoomFormat(storeSchema, provisionSchema int) bool {
	return storeSchema == 12 && provisionSchema == 5 || storeSchema == 13 && provisionSchema == 6 || storeSchema == 14 && provisionSchema == 7
}

func validateProvisioningMode(schema int, mode model.HostMode, dynamic bool) error {
	if dynamic != (schema == 7 && mode == model.HostNative) {
		return errors.New("dynamic Native bindings require store 14/provisioning 7; legacy Rooms retain their original protocol")
	}
	return nil
}
