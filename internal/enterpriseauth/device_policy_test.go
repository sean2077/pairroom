package enterpriseauth

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestDeviceCallbackPinsCanonicalNumericLoopbackAndState(t *testing.T) {
	state, _ := NewBrowserSecret()
	for _, callback := range []string{"http://127.0.0.1:49152/pairroom/callback", "http://[::1]:49152/pairroom/callback", "http://127.0.0.2:12345/callback"} {
		if err := ValidateDeviceCallback(callback, state); err != nil {
			t.Fatalf("numeric loopback refused: %s", callback)
		}
	}
	for _, callback := range []string{
		"http://localhost:49152/callback", "https://127.0.0.1:49152/callback", "http://127.0.0.1/callback", "http://127.0.0.1:0/callback", "http://127.0.0.1:65536/callback", "http://127.0.0.1:049152/callback",
		"http://0.0.0.0:49152/callback", "http://192.168.1.2:49152/callback", "http://[::]:49152/callback", "http://[::ffff:127.0.0.1]:49152/callback", "http://[0:0:0:0:0:0:0:1]:49152/callback", "http://[::1%25private]:49152/callback",
		"http://user:private-password@127.0.0.1:49152/callback", "http://127.0.0.1:49152/callback?token=private-token", "http://127.0.0.1:49152/callback?", "http://127.0.0.1:49152/callback#fragment",
		"http://127.0.0.1:49152", "http://127.0.0.1:49152//callback", "http://127.0.0.1:49152/a/../callback", "http://127.0.0.1:49152/callback/", "http://127.0.0.1:49152/%63allback", "http://127.0.0.1:49152/call%2fback", "http://127.0.0.1:49152/call\\back", "http://127.0.0.1:49152/" + strings.Repeat("x", 300),
	} {
		t.Run(callback, func(t *testing.T) {
			err := ValidateDeviceCallback(callback, state)
			if !errors.Is(err, ErrDeviceRequestInvalid) {
				t.Fatal("unsafe callback accepted")
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("callback validation leaked provided secrets")
			}
		})
	}
	for _, invalid := range []string{"", "short", state + "=", strings.Repeat("A", 42), strings.Repeat("A", 44)} {
		if !errors.Is(ValidateDeviceCallback("http://127.0.0.1:49152/callback", invalid), ErrDeviceRequestInvalid) {
			t.Fatal("invalid state accepted")
		}
	}
}

func TestDeviceCompletionLocationContainsOnlyFrozenCallbackCodeAndState(t *testing.T) {
	state, _ := NewBrowserSecret()
	code, _ := NewBrowserSecret()
	callback := "http://[::1]:49152/pairroom/callback"
	location, err := DeviceCompletionRedirect(callback, state, code)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	u.RawQuery = ""
	if u.String() != callback || len(query) != 2 || query.Get("state") != state || query.Get("completion_code") != code {
		t.Fatal("completion redirect changed destination or exposed unexpected fields")
	}
	if location, err := DeviceCompletionRedirect(callback, state, "private-invalid-code"); !errors.Is(err, ErrDeviceRequestInvalid) || location != "" || strings.Contains(err.Error(), "private-invalid-code") {
		t.Fatal("invalid completion code leaked or emitted a Location")
	}
}
