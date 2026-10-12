package prompt

import (
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestDynamicNativeOnlyRoutesExactStablePeerHandles(t *testing.T) {
	for _, tc := range []struct {
		text  string
		peer  bool
		human bool
	}{
		{"@agent2 please inspect", true, false},
		{"@AgEnT2 and @user", true, true},
		{"@agent1 is my own handle", false, false},
		{"@codex @claude @grok @gemini", false, false},
		{"@peer @lead @executor", false, false},
		{"`@agent2` is literal", false, false},
		{"```text\n@agent2\n```", false, false},
		{"https://example.com/@agent2", false, false},
		{"name@agent2.example", false, false},
		{"@agent2-extra @agent20 @agent2.dev", false, false},
		{"@user decision", false, true},
	} {
		parsed := ParseNativeMentions(tc.text, model.ActorSlot1)
		if (len(parsed.Targets) == 1) != tc.peer || parsed.Human != tc.human || tc.peer && parsed.Targets[0] != model.ActorSlot2 {
			t.Fatalf("stable routing %q = %+v", tc.text, parsed)
		}
	}
	legacy := ParseMentions("@agent2", model.ActorSlot1, map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex})
	if len(legacy.Targets) != 0 || len(legacy.RemovedAliases) != 1 {
		t.Fatal("stable handles leaked into the old protocol")
	}
}

func TestDynamicEnvelopeKeepsPeerContextWithinBudget(t *testing.T) {
	input := model.AgentInput{FromHandle: "@agent1", FromRuntime: model.RuntimeGemini, FromGeneration: 12, PeerHandle: "@agent1", PeerGeneration: 13, Text: "complete message"}
	text := Envelope(input)
	if len(text)-len(input.Text) > MaxEnvelopeOverheadBytes {
		t.Fatalf("generation metadata exceeded envelope budget: %d", len(text)-len(input.Text))
	}
}
