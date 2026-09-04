package prompt

import (
	"regexp"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

var mentionPattern = regexp.MustCompile(`(?i)(?:^|[^[:alnum:]_])@(agent1|agent2|claude|codex|grok|all|peer|human|user)\b`)

// Mentions returns user-addressable agent targets found in text. @peer is
// resolved relative to the sender and @human deliberately returns no agent
// target. In an Agent final, an explicit peer mention is a handoff signal; the
// Room Engine still requires HANDOFF + NEXT when continuation is implicit.
func Mentions(text string, sender model.ActorID) []model.ActorID {
	return MentionsWithRuntimes(text, sender, nil)
}

func MentionsWithRuntimes(text string, sender model.ActorID, runtimes map[model.ActorID]model.RuntimeKind) []model.ActorID {
	matches := mentionPattern.FindAllStringSubmatch(text, -1)
	var targets []model.ActorID
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		switch strings.ToLower(match[1]) {
		case "agent1", "claude":
			if resolved := runtimeSlots(runtimes, model.RuntimeClaude); len(resolved) == 1 && strings.ToLower(match[1]) == "claude" {
				targets = append(targets, resolved...)
			} else {
				targets = append(targets, model.ActorClaude)
			}
		case "agent2", "codex":
			if resolved := runtimeSlots(runtimes, model.RuntimeCodex); len(resolved) == 1 && strings.ToLower(match[1]) == "codex" {
				targets = append(targets, resolved...)
			} else {
				targets = append(targets, model.ActorCodex)
			}
		case "grok":
			targets = append(targets, runtimeSlots(runtimes, model.RuntimeGrok)...)
		case "all":
			targets = append(targets, model.ActorClaude, model.ActorCodex)
		case "peer":
			if peer := model.OtherParticipant(sender); peer.ValidParticipant() {
				targets = append(targets, peer)
			}
		}
	}
	return model.NormalizeActors(targets)
}

func runtimeSlots(runtimes map[model.ActorID]model.RuntimeKind, want model.RuntimeKind) []model.ActorID {
	if len(runtimes) == 0 {
		return nil
	}
	var slots []model.ActorID
	for _, actor := range model.SlotActors() {
		if runtimes[actor].CanonicalForSlot(actor) == want {
			slots = append(slots, actor)
		}
	}
	return slots
}

func MentionsHuman(text string) bool {
	for _, match := range mentionPattern.FindAllStringSubmatch(text, -1) {
		if len(match) >= 2 {
			name := strings.ToLower(match[1])
			if name == "human" || name == "user" {
				return true
			}
		}
	}
	return false
}
