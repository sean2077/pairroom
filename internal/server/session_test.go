package server

import (
	"testing"
)

func TestConstantTimeEqual(t *testing.T) {
	if !constantTimeEqual("same", "same") {
		t.Fatal("equal values did not compare equal")
	}
	if constantTimeEqual("same", "different") || constantTimeEqual("same", "samp") {
		t.Fatal("different values compared equal")
	}
}
