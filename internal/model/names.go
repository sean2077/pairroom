package model

import (
	"strings"
	"unicode"
)

// ShortRoomID is a display hint, never an identity or a lookup key.
func ShortRoomID(id string) string {
	runes := []rune(strings.TrimPrefix(id, "room-"))
	if len(runes) > 12 {
		runes = runes[len(runes)-12:]
	}
	return string(runes)
}

// TemporaryRoomName is generated once from the durable Room ID, not by a model.
// It remains unchanged until an explicit user rename.
func TemporaryRoomName(id string) string { return "Room-" + ShortRoomID(id) }

// NativeSessionName links a native session to its Room and stable participant.
// Grok's native title boundary accepts at most 100 Unicode scalars. Keep the
// handle and Room suffix intact even when the human-readable Room name is long.
func NativeSessionName(roomID, roomName string, actor ActorID, self, peer RuntimeKind) string {
	if roomID == "" || strings.TrimSpace(roomName) == "" || !actor.ValidParticipant() {
		return ""
	}
	runtimes := map[ActorID]RuntimeKind{actor: self.CanonicalForSlot(actor), OtherParticipant(actor): peer.CanonicalForSlot(OtherParticipant(actor))}
	for _, kind := range runtimes {
		if !kind.Valid() {
			return ""
		}
	}
	handle := ParticipantIdentityFor(actor, runtimes).MentionHandle
	clean := func(value string) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, value))
	}
	suffix := " · " + handle + " · " + clean(ShortRoomID(roomID))
	name := []rune(clean(roomName))
	limit := 100 - len([]rune(suffix))
	if len(name) > limit {
		name = append(name[:limit-1], '…')
	}
	return string(name) + suffix
}
