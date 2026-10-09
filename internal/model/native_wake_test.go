package model

import "testing"

func TestNativeWakeTransportDoesNotAdmitUnknownValues(t *testing.T) {
	for _, transport := range []NativeWakeTransport{NativeWakeUnavailable, 3, 255} {
		if transport.Supported() {
			t.Fatalf("unknown transport %d permits an external wake", transport)
		}
	}
	for _, runtime := range []RuntimeKind{RuntimeClaude, RuntimeCodex} {
		if !runtime.NativeWakePolicy().Transport.Supported() {
			t.Fatalf("known runtime %q lost its wake transport", runtime)
		}
	}
}
