package prompt

import (
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
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

func TestEnvelopePreservesRuntimeAndRoutingContext(t *testing.T) {
	input := model.AgentInput{
		MessageID: "m1", ThreadID: "t1", Hop: 2,
		From: model.ActorClaude, To: model.ActorCodex,
		Text: "Inspect the race", ReplyTo: "m0",
		Role: model.RoleReviewer, RoutingMode: model.RoutingRoundtable, MaxHops: 6,
	}
	got := Envelope(input)
	for _, fragment := range []string{
		"message_id: m1", "thread_id: t1", "from: Claude Code", "to: Codex",
		"reply_to: m0", "current_role: reviewer", "remaining_agent_hops: 4",
		"Do not modify files", "[PAIRROOM:CONSENSUS]", "Inspect the race",
	} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("Envelope() missing %q:\n%s", fragment, got)
		}
	}
}
