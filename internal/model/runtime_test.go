package model

import "testing"

func TestParticipantIdentitiesUseRuntimeHandlesAndStableDuplicateSuffixes(t *testing.T) {
	for _, test := range []struct {
		name       string
		runtimes   map[ActorID]RuntimeKind
		wantFirst  ParticipantIdentity
		wantSecond ParticipantIdentity
	}{
		{
			name:       "unique Claude and Codex",
			runtimes:   map[ActorID]RuntimeKind{ActorClaude: RuntimeClaude, ActorCodex: RuntimeCodex},
			wantFirst:  ParticipantIdentity{DisplayName: "Claude Code", MentionHandle: "@claude"},
			wantSecond: ParticipantIdentity{DisplayName: "Codex", MentionHandle: "@codex"},
		},
		{
			name:       "unique Grok and Claude",
			runtimes:   map[ActorID]RuntimeKind{ActorClaude: RuntimeGrok, ActorCodex: RuntimeClaude},
			wantFirst:  ParticipantIdentity{DisplayName: "Grok Build", MentionHandle: "@grok"},
			wantSecond: ParticipantIdentity{DisplayName: "Claude Code", MentionHandle: "@claude"},
		},
		{
			name:       "duplicate Claude",
			runtimes:   map[ActorID]RuntimeKind{ActorClaude: RuntimeClaude, ActorCodex: RuntimeClaude},
			wantFirst:  ParticipantIdentity{DisplayName: "Claude Code 0", MentionHandle: "@claude0"},
			wantSecond: ParticipantIdentity{DisplayName: "Claude Code 1", MentionHandle: "@claude1"},
		},
		{
			name:       "duplicate Codex",
			runtimes:   map[ActorID]RuntimeKind{ActorClaude: RuntimeCodex, ActorCodex: RuntimeCodex},
			wantFirst:  ParticipantIdentity{DisplayName: "Codex 0", MentionHandle: "@codex0"},
			wantSecond: ParticipantIdentity{DisplayName: "Codex 1", MentionHandle: "@codex1"},
		},
		{
			name:       "duplicate Grok",
			runtimes:   map[ActorID]RuntimeKind{ActorClaude: RuntimeGrok, ActorCodex: RuntimeGrok},
			wantFirst:  ParticipantIdentity{DisplayName: "Grok Build 0", MentionHandle: "@grok0"},
			wantSecond: ParticipantIdentity{DisplayName: "Grok Build 1", MentionHandle: "@grok1"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := ParticipantIdentities(test.runtimes)
			if got[ActorClaude] != test.wantFirst || got[ActorCodex] != test.wantSecond {
				t.Fatalf("identities = %#v, want %#v / %#v", got, test.wantFirst, test.wantSecond)
			}
		})
	}
}
