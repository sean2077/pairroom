package prompt

import (
	"regexp"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

var mentionPattern = regexp.MustCompile(`(?i)(?:^|[^[:alnum:]_])@(claude|codex|all|peer|human|user)\b`)

// Mentions returns agent targets explicitly requested by text. @peer is
// resolved relative to the sender. @human deliberately returns no agent target.
func Mentions(text string, sender model.ActorID) []model.ActorID {
	matches := mentionPattern.FindAllStringSubmatch(text, -1)
	var targets []model.ActorID
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		switch strings.ToLower(match[1]) {
		case "claude":
			targets = append(targets, model.ActorClaude)
		case "codex":
			targets = append(targets, model.ActorCodex)
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
