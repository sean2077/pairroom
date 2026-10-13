package enterpriseservice

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
)

func TestDeviceTransportNeverAcceptsBrowserAuthorityOrUnpinnedDeployment(t *testing.T) {
	secret, _ := enterpriseauth.NewBrowserSecret()
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "https://service.example.test/enterprise/device/v1/refresh", nil)
		r.Header.Set(DeviceDeploymentHeader, "deployment-1")
		r.Header.Set("Authorization", "Bearer "+secret)
		return r
	}
	r := request()
	if !deviceEnvelope(r, "deployment-1", true) {
		t.Fatal("pinned native request refused")
	}
	if hash, ok := deviceBearerHash(r); !ok || hash != tokenHash(secret) {
		t.Fatal("native bearer did not produce only its verifier")
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "browser_session", Value: "private-cookie"}) },
		func(r *http.Request) { r.Header.Set("Cookie", "") },
		func(r *http.Request) { r.Header.Set("Origin", "https://service.example.test") },
		func(r *http.Request) { r.Header.Set(CSRFHeader, "private-csrf") },
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") },
		func(r *http.Request) { r.Header.Set(DeviceDeploymentHeader, "deployment-2") },
		func(r *http.Request) { r.Header.Add(DeviceDeploymentHeader, "deployment-1") },
		func(r *http.Request) { r.URL.RawQuery = "token=private-token" },
		func(r *http.Request) { r.Method = http.MethodGet },
		func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+secret) },
	} {
		r := request()
		mutate(r)
		if deviceEnvelope(r, "deployment-1", true) {
			t.Fatal("browser/ambiguous/unpinned native request accepted")
		}
	}
	if deviceEnvelope(request(), "deployment-1", false) {
		t.Fatal("initial enrollment silently used an application bearer")
	}
}

func TestDeviceBearerAndVerifierDecodingAreCanonicalAndSecretFree(t *testing.T) {
	secret, _ := enterpriseauth.NewBrowserSecret()
	for _, value := range []string{"", secret, "Basic " + secret, "Bearer  " + secret, "Bearer " + secret + " ", "Bearer private-invalid-secret", "Bearer " + secret + "," + secret} {
		r := httptest.NewRequest(http.MethodPost, "https://service.example.test/", nil)
		r.Header.Set("Authorization", value)
		if hash, ok := deviceBearerHash(r); ok || hash != ([32]byte{}) {
			t.Fatal("noncanonical credential produced a verifier")
		}
	}
	hash := tokenHash(secret)
	wire := base64.RawURLEncoding.EncodeToString(hash[:])
	decoded, ok := deviceVerifierHash(wire)
	if !ok || decoded != hash {
		t.Fatal("precomputed token hash was hashed again")
	}
	if _, ok := deviceVerifierHash(base64.RawURLEncoding.EncodeToString(make([]byte, 32))); ok {
		t.Fatal("empty verifier admitted")
	}
	for _, label := range []string{"", " leading", "trailing ", "private\x00label", strings.Repeat("a", 129)} {
		if validDeviceLabel(label) {
			t.Fatal("invalid display label admitted")
		}
	}
	if !validDeviceLabel("Developer laptop") || !validDeviceLabel("研发终端") {
		t.Fatal("display label incorrectly used as identity")
	}
}
