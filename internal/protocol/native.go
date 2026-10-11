package protocol

import (
	"fmt"
	"github.com/sean2077/pairroom/internal/model"
)

const NativeVersion = "pairroom-protocol/v8"

func NativeBootstrap(actor model.ActorID, selfRuntime, peerRuntime model.RuntimeKind) string {
	if peerRuntime == model.RuntimeAwaitingPeer {
		return fmt.Sprintf(`You occupy %s in a Native LAN Room. The remote peer is awaiting owner admission; no peer mention handle exists yet. Use explicit relay send to queue shared work, or relay peer to inspect the admitted runtime. Human instructions and native/project permissions apply. PairRoom owns FIFO/audit, not processes. Share only intended Room content and files; do not print credentials. Inspect unknown delivery before explicit Retry. After admission, the next Room envelope identifies the peer. %s: pairroom protocol --host-mode native --actor %s`, actor, NativeVersion, actor)
	}
	peer := model.OtherParticipant(actor)
	ids := model.ParticipantIdentities(map[model.ActorID]model.RuntimeKind{actor: selfRuntime, peer: peerRuntime})
	stop := "Stop relays your full visible reply."
	if selfRuntime == model.RuntimeGemini {
		stop = "Gemini AfterAgent relays your full visible reply; bind discovery uses its approved BeforeTool hook."
	}
	if selfRuntime == model.RuntimeGrok {
		stop = "Grok clips hook text: prefer relay send/exchange for long replies. Hook blocks prompt relay wait; collect full input with that tool."
	}
	return fmt.Sprintf(`You: %s (%s); peer: %s (%s).
[PairRoom message] names the sender. Human instructions win; native/project permissions apply. PairRoom owns FIFO/audit, not processes; turns are advisory.
Mention %s only when another reply is needed. %s No peer handle ends relay; @user alone asks the human; peer wins over @user.
relay send targets the peer or --to @user; body mentions do not route. Use relay send for attachments. Send plus Stop can duplicate: omit the final peer handle after send unless intentional.
Use relay status on uncertainty, not after every reply. Inspect unknown before explicit Retry; pre-publication loss may be undetectable. Never read or print relay credentials.
%s: pairroom protocol --host-mode native --actor %s`, ids[actor].DisplayName, ids[actor].MentionHandle, ids[peer].DisplayName, ids[peer].MentionHandle, ids[peer].MentionHandle, stop, NativeVersion, actor)
}

func ResolveNative(selection Selection) (Contract, error) {
	c, err := Resolve(selection)
	if err != nil {
		return Contract{}, err
	}
	c.Version = NativeVersion
	for i := range c.Rules {
		switch c.Rules[i].ID {
		case "delivery.single-turn":
			c.Rules[i].Text = "Native hosting does not own processes. Single Owner Turn is advisory; per-slot durable FIFO, binding uniqueness and append-only audit are enforced."
		case "output.verbatim":
			c.Rules[i].Text = "Stop hooks publish complete visible responses for relay or @user; clipped Grok replies require explicit send/exchange. No vendor transcript is parsed or mirrored."
		case "observability.inspector":
			c.Rules[i].Text = "The Room records relay, bindings, failures, publication gaps and lifecycle, not full native tool activity."
		}
	}
	c.Rules = append(c.Rules, Rule{ID: "native.explicit-send", Text: "relay send defaults to the peer inbox, or --to @user; body mentions do not route. Attachments use this path. Same-turn send plus peer-directed Stop creates visible duplicates; omit the final peer handle unless intentional."}, Rule{ID: "native.delivery", Text: "handed_off means CLI stdout was written, not model acceptance. Unknown outcomes are never replayed automatically; inspect before explicit Retry. Park only wakes during its bounded window; otherwise queue and nudge or wait after association."})
	return c, nil
}
