package server

import (
	"testing"
	"time"
)

func TestSessionStoreExpiryAndSlidingTTL(t *testing.T) {
	now := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	store := newSessionStore()
	store.ttl = 10 * time.Minute
	store.now = func() time.Time { return now }
	value, err := store.create()
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	refreshed, ok := store.get(value.ID)
	if !ok || !refreshed.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("session was not refreshed: ok=%v expires=%s", ok, refreshed.ExpiresAt)
	}
	now = now.Add(11 * time.Minute)
	if _, ok := store.get(value.ID); ok {
		t.Fatal("expired session remained valid")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !constantTimeEqual("same", "same") {
		t.Fatal("equal values did not compare equal")
	}
	if constantTimeEqual("same", "different") || constantTimeEqual("same", "samp") {
		t.Fatal("different values compared equal")
	}
}
