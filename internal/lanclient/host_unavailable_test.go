package lanclient

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/relay"
)

// The host answering "this Room is unavailable right now" is not an
// authentication failure and not transport unreachability: the member must not
// conclude its admission was revoked.
func TestSafeErrorKeepsHostUnavailabilityOutOfAuthentication(t *testing.T) {
	err := safeError(&lanshare.Error{Status: http.StatusServiceUnavailable, Code: lanshare.HostUnavailableCode, Message: "untrusted peer text"})
	if errors.Is(err, relay.ErrAuth) || errors.Is(err, ErrTransportUnavailable) {
		t.Fatalf("host unavailability became an authentication or transport failure: %v", err)
	}
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != lanshare.HostUnavailableCode || failure.Status != http.StatusServiceUnavailable {
		t.Fatalf("host unavailability lost its code: %v", err)
	}
	if strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("remote text escaped the client boundary: %v", err)
	}
}
