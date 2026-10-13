package enterpriseauth

import (
	"errors"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
)

var ErrDeviceRequestInvalid = errors.New("enterprise device request invalid")

// ValidateDeviceCallback admits an exact client-owned numeric-loopback HTTP
// destination with an allocated port and independent random transaction state.
// No DNS name, network-facing interface, URL credential, query or redirect target
// is inferred or normalized. The accepted string must be frozen in the request.
func ValidateDeviceCallback(callbackURI, state string) error {
	if len(callbackURI) > 512 || !validSecret(state) || strings.ContainsAny(callbackURI, "\\\r\n\t") {
		return ErrDeviceRequestInvalid
	}
	u, err := url.Parse(callbackURI)
	if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || u.Port() == "" {
		return ErrDeviceRequestInvalid
	}
	address, err := netip.ParseAddr(u.Hostname())
	if err != nil || !address.IsLoopback() || address.Is4In6() || address.Zone() != "" || u.Hostname() != address.String() {
		return ErrDeviceRequestInvalid
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != u.Port() {
		return ErrDeviceRequestInvalid
	}
	if u.Path == "" || len(u.Path) > 256 || !strings.HasPrefix(u.Path, "/") || path.Clean(u.Path) != u.Path || u.EscapedPath() != u.Path || u.String() != callbackURI {
		return ErrDeviceRequestInvalid
	}
	return nil
}

// DeviceCompletionRedirect only constructs the browser's one-use completion
// redirect. It never contacts the callback and never includes an application,
// refresh or upstream provider token. The client must check its original state
// and exchange this code with its private enrollment proof.
func DeviceCompletionRedirect(callbackURI, state, completionCode string) (string, error) {
	if ValidateDeviceCallback(callbackURI, state) != nil || !validSecret(completionCode) {
		return "", ErrDeviceRequestInvalid
	}
	u, _ := url.Parse(callbackURI)
	u.RawQuery = url.Values{"completion_code": {completionCode}, "state": {state}}.Encode()
	return u.String(), nil
}
