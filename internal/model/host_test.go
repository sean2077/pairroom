package model

import "testing"

func TestHostModeCreationDefaultsToNativeOnly(t *testing.T) {
	for _, tc := range []struct {
		input HostMode
		want  HostMode
	}{
		{"", HostNative},
		{HostNative, HostNative},
		{HostEmbedded, HostEmbedded},
		{"unknown", "unknown"},
	} {
		if got := tc.input.ForCreation(); got != tc.want {
			t.Errorf("ForCreation(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
	if HostMode("").Valid() {
		t.Fatal("empty persisted host mode must remain invalid")
	}
}
