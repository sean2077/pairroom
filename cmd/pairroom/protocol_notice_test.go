package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/protocol"
)

// An older release's Embedded bootstrap prints `pairroom protocol --actor <slot>`
// without a host mode, and this release defaults to native. The assumed contract
// must be named instead of handing an Embedded Room the Native rules silently.
func TestProtocolDefaultDisclosesTheAssumedHostMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := writeProtocol([]string{"--actor", "slot1"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{protocol.NativeVersion, "--host-mode was not given", "--host-mode embedded"} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Fatalf("assumed host mode was not disclosed (%q):\n%s", fragment, stdout.String())
		}
	}
	stdout.Reset()
	if err := writeProtocol([]string{"--host-mode", "native", "--actor", "slot1"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "--host-mode was not given") {
		t.Fatalf("explicit host mode carried the default notice:\n%s", stdout.String())
	}
}
