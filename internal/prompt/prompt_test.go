package prompt

import (
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
)

func TestMentions(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		sender model.ActorID
		want   []model.ActorID
	}{
		{name: "explicit", text: "@Claude review with @codex", sender: model.ActorUser, want: []model.ActorID{model.ActorClaude, model.ActorCodex}},
		{name: "peer", text: "@peer please verify", sender: model.ActorClaude, want: []model.ActorID{model.ActorCodex}},
		{name: "all deduplicates", text: "@all and @claude", sender: model.ActorUser, want: []model.ActorID{model.ActorClaude, model.ActorCodex}},
		{name: "email is not mention", text: "mail a@claude.dev", sender: model.ActorUser, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Mentions(tt.text, tt.sender)
			if len(got) != len(tt.want) {
				t.Fatalf("Mentions() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("Mentions() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestMentionsHuman(t *testing.T) {
	if !MentionsHuman("Need @human to choose") {
		t.Fatal("expected @human to be detected")
	}
	if MentionsHuman("human-readable output") {
		t.Fatal("plain prose must not be treated as a mention")
	}
}

func TestBootstrapPromptUsesVersionedContractAndStaysCompact(t *testing.T) {
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		got := SystemPrompt(actor, "room-identity-must-not-leak", "/repo/identity/must/not/leak")
		if len([]byte(got)) > MaxBootstrapBytes {
			t.Fatalf("%s bootstrap = %d bytes, budget = %d:\n%s", actor, len([]byte(got)), MaxBootstrapBytes, got)
		}
		for _, fragment := range []string{protocol.Version, "pairroom protocol --actor " + string(actor), "current_role", "single active turn", "[PAIRROOM:HANDOFF]", "[PAIRROOM:NEXT]", "[PAIRROOM:DONE]"} {
			if !strings.Contains(got, fragment) {
				t.Fatalf("%s bootstrap missing %q:\n%s", actor, fragment, got)
			}
		}
		for _, fragment := range []string{"PairRoom rules:", "room-identity-must-not-leak", "/repo/identity/must/not/leak"} {
			if strings.Contains(got, fragment) {
				t.Fatalf("%s bootstrap contains unstable or legacy prose %q:\n%s", actor, fragment, got)
			}
		}
	}
}

func TestEnvelopeCarriesOnlyRuntimeAndRoutingContext(t *testing.T) {
	input := model.AgentInput{
		// PairRoom generates 24-hex-digit IDs with msg- and thread- prefixes.
		// Exercise the production-sized optional scalar fields in the fixed
		// envelope budget; attachment metadata remains dynamic content.
		MessageID: "msg-0123456789abcdef01234567", ThreadID: "thread-0123456789abcdef01234567", Hop: 2,
		From: model.ActorClaude, To: model.ActorCodex,
		Text: "Inspect the race", ReplyTo: "msg-0123456789abcdef01234567",
		Role: model.RoleReviewer, RoutingMode: model.RoutingRoundtable, MaxHops: 6, Intent: model.IntentSupersede,
		WorkflowID: "workflow-0123456789abcdef01234567", WorkflowStage: 2, WorkflowMode: model.WorkflowAudit,
	}
	got := Envelope(input)
	for _, fragment := range []string{
		"protocol: " + protocol.Version,
		"message_id: msg-0123456789abcdef01234567", "thread_id: thread-0123456789abcdef01234567", "from: claude", "to: codex",
		"reply_to: msg-0123456789abcdef01234567", "current_role: reviewer", "delivery_intent: supersede", "turn_policy: turns",
		"workflow_id: workflow-0123456789abcdef01234567", "workflow_stage: 3", "workflow_mode: audit",
		"remaining_agent_hops: 4", "Inspect the race",
	} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("Envelope() missing %q:\n%s", fragment, got)
		}
	}
	for _, fragment := range []string{"role_rule:", "Do not modify files", "[PAIRROOM:CONSENSUS]", "Keep the shared-room answer"} {
		if strings.Contains(got, fragment) {
			t.Fatalf("Envelope() repeated stable contract prose %q:\n%s", fragment, got)
		}
	}
	overhead := len([]byte(got)) - len([]byte(input.Text))
	if overhead > MaxEnvelopeOverheadBytes {
		t.Fatalf("Envelope() overhead = %d bytes, budget = %d:\n%s", overhead, MaxEnvelopeOverheadBytes, got)
	}
}
