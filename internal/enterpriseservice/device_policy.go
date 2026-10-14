package enterpriseservice

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const DeviceDeploymentHeader = "X-PairRoom-Deployment"

// deviceEnvelope separates native transport from browser-cookie authority.
// The parent dispatcher also checks the configured HTTPS/Host/proxy policy;
// each typed endpoint decides which one credential/proof kind it accepts.
func deviceEnvelope(r *http.Request, deployment string, authenticated bool) bool {
	return r.Method == http.MethodPost && deviceNativeHeaders(r, deployment, authenticated)
}

func deviceReadEnvelope(r *http.Request, deployment string, authenticated bool) bool {
	return r.Method == http.MethodGet && deviceNativeHeaders(r, deployment, authenticated)
}

func deviceNativeHeaders(r *http.Request, deployment string, authenticated bool) bool {
	if !deviceNoBrowserHeaders(r) {
		return false
	}
	if len(r.Header.Values(DeviceDeploymentHeader)) != 1 || r.Header.Get(DeviceDeploymentHeader) != deployment {
		return false
	}
	if authenticated {
		return len(r.Header.Values("Authorization")) == 1
	}
	return len(r.Header.Values("Authorization")) == 0
}

func deviceNoBrowserHeaders(r *http.Request) bool {
	if r.URL.RawQuery != "" || r.URL.ForceQuery || len(r.Header.Values("Cookie")) != 0 || len(r.Header.Values("Origin")) != 0 || len(r.Header.Values(CSRFHeader)) != 0 {
		return false
	}
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "sec-fetch-") {
			return false
		}
	}
	return true
}

// deviceBearerHash returns only a verifier. Callers never forward the original
// Authorization header or use another credential class as a fallback.
func deviceBearerHash(r *http.Request) ([32]byte, bool) {
	if len(r.Header.Values("Authorization")) != 1 {
		return [32]byte{}, false
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || !validOpaqueSecret(token) {
		return [32]byte{}, false
	}
	return tokenHash(token), true
}

func deviceVerifierHash(encoded string) ([32]byte, bool) {
	if !validOpaqueSecret(encoded) {
		return [32]byte{}, false
	}
	bytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return [32]byte{}, false
	}
	var hash [sha256.Size]byte
	copy(hash[:], bytes)
	return hash, hash != ([32]byte{})
}

func validDeviceLabel(label string) bool {
	return label != "" && len(label) <= 128 && utf8.ValidString(label) && strings.TrimSpace(label) == label && !strings.ContainsFunc(label, unicode.IsControl)
}
