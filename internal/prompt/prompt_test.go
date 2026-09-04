package prompt

import (
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
)

func TestMentionsUseRuntimeHandlesOnly(t *testing.T) {
	runtimes := map[model.ActorID]model.RuntimeKind{
		model.ActorClaude: model.RuntimeGrok,
		model.ActorCodex:  model.RuntimeCodex,
	}
	tests := []struct {
		name    string
		text    string
		sender  model.ActorID
		want    []model.ActorID
		human   bool
		removed int
	}{
		{name: "actual runtimes", text: "@GROK review with @codex", sender: model.ActorUser, want: []model.ActorID{model.ActorClaude, model.ActorCodex}},
		{name: "self ignored", text: "@grok note to self then @codex", sender: model.ActorClaude, want: []model.ActorID{model.ActorCodex}},
		{name: "human wins recorded separately", text: "@codex and @user", sender: model.ActorClaude, want: []model.ActorID{model.ActorCodex}, human: true},
		{name: "legacy aliases removed", text: "@peer @human @all @agent1 @agent2", sender: model.ActorClaude, removed: 5},
		{name: "email ignored", text: "mail a@codex.dev, me+tag@codex.dev, or @codex@example.com", sender: model.ActorUser},
		{name: "urls ignored", text: "see https://example.test/@codex, ssh://host/@codex, example.test/@codex, localhost/@codex, or 127.0.0.1:7332/@codex", sender: model.ActorUser},
		{name: "inline code ignored", text: "write `@codex` and ``@codex`` literally", sender: model.ActorUser},
		{name: "fenced code ignored", text: "```text\n@codex\n```\n~~~text\n@codex\n~~~", sender: model.ActorUser},
		{name: "indented code ignored", text: "example:\n    @codex", sender: model.ActorUser},
		{name: "quoted indented code ignored", text: ">     @codex", sender: model.ActorUser},
		{name: "escaped and doubled handles ignored", text: `\@codex @@codex`, sender: model.ActorUser},
		{name: "unicode word prefix ignored", text: "中文@codex", sender: model.ActorUser},
		{name: "non exact suffix ignored", text: "@codex-build @codex.dev @codex界 @codexé", sender: model.ActorUser},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseMentions(tt.text, tt.sender, runtimes)
			if len(got.Targets) != len(tt.want) {
				t.Fatalf("targets = %v, want %v", got.Targets, tt.want)
			}
			for index := range tt.want {
				if got.Targets[index] != tt.want[index] {
					t.Fatalf("targets = %v, want %v", got.Targets, tt.want)
				}
			}
			if got.Human != tt.human {
				t.Fatalf("human = %v, want %v", got.Human, tt.human)
			}
			if len(got.RemovedAliases) != tt.removed {
				t.Fatalf("removed aliases = %v, want %d", got.RemovedAliases, tt.removed)
			}
		})
	}
}

func TestDuplicateRuntimeRequiresSuffixedHandle(t *testing.T) {
	for _, runtimeKind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
		t.Run(string(runtimeKind), func(t *testing.T) {
			runtimes := map[model.ActorID]model.RuntimeKind{
				model.ActorClaude: runtimeKind,
				model.ActorCodex:  runtimeKind,
			}
			base := "@" + string(runtimeKind)
			ambiguous := ParseMentions("ask "+base, model.ActorUser, runtimes)
			if len(ambiguous.Targets) != 0 || len(ambiguous.Ambiguous) != 1 || ambiguous.Ambiguous[0] != base {
				t.Fatalf("ambiguous result = %+v", ambiguous)
			}
			resolved := ParseMentions(base+"1 review", model.ActorUser, runtimes)
			if len(resolved.Targets) != 1 || resolved.Targets[0] != model.ActorCodex {
				t.Fatalf("resolved result = %+v", resolved)
			}
		})
	}
}

func TestMentionsHumanOnlyRecognizesUser(t *testing.T) {
	if !MentionsHuman("Need @user to choose") {
		t.Fatal("expected @user to be detected")
	}
	for _, text := range []string{"Need @human", "`@user`", "https://example.test/@user"} {
		if MentionsHuman(text) {
			t.Fatalf("must not detect %q", text)
		}
	}
}

func TestBootstrapPromptUsesVersionedContractAndStaysCompact(t *testing.T) {
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		got := BootstrapPromptWithRuntime(actor, actorRuntime(actor), actorRuntime(model.OtherParticipant(actor)))
		if len([]byte(got)) > MaxBootstrapBytes {
			t.Fatalf("%s bootstrap = %d bytes, budget = %d:\n%s", actor, len([]byte(got)), MaxBootstrapBytes, got)
		}
		for _, fragment := range []string{protocol.Version, "pairroom protocol --actor " + string(actor), "current_role", "single active turn", "@user", "No fixed relay packet"} {
			if !strings.Contains(got, fragment) {
				t.Fatalf("%s bootstrap missing %q:\n%s", actor, fragment, got)
			}
		}
		for _, fragment := range []string{"HANDOFF", "PAIRROOM:", "workflow", "@peer", "@human"} {
			if strings.Contains(got, fragment) {
				t.Fatalf("%s bootstrap contains removed prose %q:\n%s", actor, fragment, got)
			}
		}
	}
}

func actorRuntime(actor model.ActorID) model.RuntimeKind {
	if actor == model.ActorCodex {
		return model.RuntimeCodex
	}
	return model.RuntimeClaude
}

func TestEnvelopeCarriesOnlyDynamicTurnContext(t *testing.T) {
	input := model.AgentInput{
		MessageID: "msg-0123456789abcdef01234567", ThreadID: "thread-0123456789abcdef01234567",
		From: model.ActorClaude, To: model.ActorCodex,
		FromHandle: "@claude", SelfHandle: "@codex", PeerHandle: "@claude",
		Text: "Inspect the race", ReplyTo: "msg-0123456789abcdef01234567",
		Role: model.RoleReviewer, Intent: model.IntentQueue,
	}
	got := Envelope(input)
	for _, fragment := range []string{
		"protocol: " + protocol.Version, "message_id: msg-0123456789abcdef01234567",
		"thread_id: thread-0123456789abcdef01234567", "from_handle: @claude",
		"self_handle: @codex", "peer_handle: @claude", "current_role: reviewer", "Inspect the race",
	} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("Envelope() missing %q:\n%s", fragment, got)
		}
	}
	for _, fragment := range []string{"hop:", "remaining_agent_hops", "workflow", "delivery_intent", "HANDOFF", "PAIRROOM:"} {
		if strings.Contains(got, fragment) {
			t.Fatalf("Envelope() contains removed field %q:\n%s", fragment, got)
		}
	}
	overhead := len([]byte(got)) - len([]byte(input.Text))
	if overhead > MaxEnvelopeOverheadBytes {
		t.Fatalf("Envelope() overhead = %d bytes, budget = %d:\n%s", overhead, MaxEnvelopeOverheadBytes, got)
	}
}
