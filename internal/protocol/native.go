package protocol

import (
	"fmt"
	"github.com/sean2077/pairroom/internal/model"
)

const NativeVersion = "pairroom-protocol/v7"

func NativeBootstrap(actor model.ActorID, selfRuntime, peerRuntime model.RuntimeKind) string {
	peer := model.OtherParticipant(actor)
	ids := model.ParticipantIdentities(map[model.ActorID]model.RuntimeKind{actor: selfRuntime, peer: peerRuntime})
	return fmt.Sprintf(`You are %s (%s); peer: %s (%s).
[PairRoom message] names its sender. Native/project/permission rules remain authoritative; human instructions win. PairRoom owns FIFO and audit, not processes; turn ownership is advisory.
Mention %s only when another reply is needed. Stop relays your full visible reply. No peer handle ends relay; @user alone asks the human; peer wins when both appear.
relay send defaults to peer, or --to @user, ignoring body mentions; use it for attachments. After send, omit the final peer handle unless a second full reply is intentional. Both paths are never semantically deduplicated.
Check relay status next time after a peer-directed reply. Interruptions/pre-write crashes may be undetectably lost. Inspect unknown outcomes before explicit Retry. relay peer finds optional peer session metadata. Never read or print relay credentials.
%s: pairroom protocol --host-mode native --actor %s`, ids[actor].DisplayName, ids[actor].MentionHandle, ids[peer].DisplayName, ids[peer].MentionHandle, ids[peer].MentionHandle, NativeVersion, actor)
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
			c.Rules[i].Text = "Stop hooks publish the complete visible response at a response boundary only for relay or @user; no vendor transcript is parsed or mirrored."
		case "observability.inspector":
			c.Rules[i].Text = "The Room records relay, bindings, failures, publication gaps and lifecycle, not full native tool activity."
		}
	}
	c.Rules = append(c.Rules, Rule{ID: "native.explicit-send", Text: "relay send defaults to the peer inbox, or --to @user; body mentions do not route. Attachments use this path. Same-turn send plus peer-directed Stop creates visible duplicates; omit the final peer handle unless intentional."}, Rule{ID: "native.delivery", Text: "handed_off means CLI stdout was written, not model acceptance. Unknown outcomes are never replayed automatically; inspect before explicit Retry. Park only wakes during its bounded window; otherwise queue and nudge or wait after association."})
	return c, nil
}
