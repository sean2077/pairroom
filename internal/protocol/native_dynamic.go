package protocol

import (
	"fmt"

	"github.com/sean2077/pairroom/internal/model"
)

// NativeDynamicVersion belongs only to Rooms provisioned for binding-time
// Runtime selection. Existing Native Rooms retain NativeVersion.
const NativeDynamicVersion = "pairroom-protocol/v9"

func NativeDynamicBootstrap(actor model.ActorID, selfRuntime, peerRuntime model.RuntimeKind, peerGeneration uint64) string {
	if !actor.ValidParticipant() || !selfRuntime.Valid() || peerGeneration == 0 || !peerRuntime.Valid() && peerRuntime != model.RuntimeAwaitingBinding && peerRuntime != model.RuntimeAwaitingPeer {
		return ""
	}
	selfRuntime = selfRuntime.Canonical()
	self := model.NativeParticipantIdentityFor(actor)
	peer := model.NativeParticipantIdentityFor(model.OtherParticipant(actor))
	state := "awaiting binding"
	if peerRuntime.Valid() {
		state = peerRuntime.DisplayName()
	} else if peerRuntime == model.RuntimeAwaitingPeer {
		state = "awaiting owner admission"
	}
	stop := "Stop relays your full visible reply."
	if selfRuntime == model.RuntimeGemini {
		stop = "Gemini AfterAgent relays your full reply; BeforeTool supplies bind identity."
	} else if selfRuntime == model.RuntimeGrok {
		stop = "Grok clips hook text: send long replies with relay send/exchange; use relay wait for full input."
	}
	return fmt.Sprintf(`You: %s; peer: %s (%s), generation %d.
Human instructions and native/project permissions apply. PairRoom owns FIFO/audit, not processes; turns are advisory.
Mention %s only when another reply is needed. %s No peer handle ends relay; @user alone asks the human; peer wins over @user. Runtime names do not route.
Automatic replies retain the original peer generation. A replaced peer blocks stale publication; inspect relay peer, then send --peer-generation N. Empty slots reserve the next generation.
relay send targets the peer or --to @user; body mentions do not route. Use it for attachments. Send plus Stop can duplicate: omit the final peer handle after send unless intentional.
Use relay status only on uncertainty. Inspect unknown before explicit Retry. Never read or print relay credentials.
%s: pairroom protocol --host-mode native --actor %s`, self.MentionHandle, peer.MentionHandle, state, peerGeneration, peer.MentionHandle, stop, NativeDynamicVersion, actor)
}

func ResolveNativeDynamic(selection Selection) (Contract, error) {
	c, err := ResolveNative(selection)
	if err != nil {
		return Contract{}, err
	}
	c.Version = NativeDynamicVersion
	for i := range c.Rules {
		switch c.Rules[i].ID {
		case "input.envelope":
			c.Rules[i].Text = "Each [PairRoom message] is current input. Its from field preserves the original sender runtime/generation; its peer field supplies the current stable peer handle and generation. Runtime metadata never changes message authorship."
		case "delivery.peer":
			c.Rules[i].Text = "Only the exact other stable participant handle (@agent1 or @agent2) requests another Agent reply. The CLI binds that publication to the originating response's peer generation."
		}
	}
	c.Rules = append(c.Rules,
		Rule{ID: "native.binding-identity", Text: "The two stable participants use @agent1 and @agent2. Actual Runtime belongs to each binding generation; legacy runtime handles do not route in this Room."},
		Rule{ID: "native.peer-generation", Text: "Automatic replies stay pinned to the original peer context. Replacing the peer pauses automatic peer relay; inspecting relay peer does not resume it. After checking the current peer, a fresh explicit send --peer-generation N authorizes only that send. Missing or changed context never retargets a reply or Retry to a successor. An empty slot reserves its next admitted generation."},
	)
	return c, nil
}
