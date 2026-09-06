package model

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRoomNamesKeepIdentitySuffixAndRuntimeHandles(t *testing.T) {
	id := "room-0123456789abcdef01234567"
	if got := TemporaryRoomName(id); got != "Room-cdef01234567" {
		t.Fatalf("temporary name = %q", got)
	}
	for _, tc := range []struct {
		actor      ActorID
		self, peer RuntimeKind
		handle     string
	}{
		{ActorClaude, RuntimeClaude, RuntimeCodex, "@claude"},
		{ActorCodex, RuntimeCodex, RuntimeClaude, "@codex"},
		{ActorClaude, RuntimeGrok, RuntimeGrok, "@grok0"},
		{ActorCodex, RuntimeGrok, RuntimeGrok, "@grok1"},
	} {
		name := NativeSessionName(id, "规划项目", tc.actor, tc.self, tc.peer)
		if name != "规划项目 · "+tc.handle+" · cdef01234567" {
			t.Fatalf("name = %q", name)
		}
	}
	a := NativeSessionName(id, "Same name", ActorClaude, RuntimeClaude, RuntimeCodex)
	b := NativeSessionName("room-fedcba987654321000000000", "Same name", ActorClaude, RuntimeClaude, RuntimeCodex)
	if a == b {
		t.Fatal("different Rooms have indistinguishable names")
	}
	for _, invalid := range []string{"", "  "} {
		if NativeSessionName(id, invalid, ActorClaude, "", "") != "" {
			t.Fatal("blank name not suppressed")
		}
	}
	if NativeSessionName("", "Validation", ActorClaude, "", "") != "" {
		t.Fatal("validation renamed an unowned session")
	}
	if NativeSessionName(id, "Name", ActorUser, "", "") != "" {
		t.Fatal("user is not a native participant")
	}
}

func TestNativeSessionNameUnicodeAndLimit(t *testing.T) {
	name := NativeSessionName("room-0123456789abcdef01234567", strings.Repeat("磁🧪", 100), ActorCodex, RuntimeCodex, RuntimeCodex)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) != 100 || !strings.HasSuffix(name, "… · @codex1 · cdef01234567") {
		t.Fatalf("invalid/broken/truncated identity: %q (%d runes)", name, utf8.RuneCountInString(name))
	}
	if NativeSessionName("room-valid", "Name", ActorClaude, RuntimeKind(strings.Repeat("x", 120)), "") != "" {
		t.Fatal("unknown runtime generated an unbounded title")
	}
	legacy := NativeSessionName("room-"+strings.Repeat("界", 20), "Old\nname\x1b", ActorClaude, "", "")
	if !utf8.ValidString(legacy) || strings.ContainsAny(legacy, "\n\x1b") || !strings.HasSuffix(legacy, strings.Repeat("界", 12)) {
		t.Fatalf("unsafe legacy projection %q", legacy)
	}
}
