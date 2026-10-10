package lanshare

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLANInviteRejectsNetworkAndAuthorityAmbiguity(t *testing.T) {
	for _, endpoint := range []string{"http://192.168.1.2:8877", "https://example.com:8877", "https://0.0.0.0:8877", "https://8.8.8.8:8877", "https://127.0.0.1:8877/admin", "https://user:secret@127.0.0.1:8877", "https://127.0.0.1:8877?x=1", "https://127.0.0.1:0", "https://[::]:8877"} {
		if ValidateEndpoint(endpoint) == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	for _, endpoint := range []string{"https://192.168.1.2:8877", "https://127.0.0.1:8877", "https://[fd00::1]:8877"} {
		if err := ValidateEndpoint(endpoint); err != nil {
			t.Fatalf("valid numeric endpoint rejected: %s %v", endpoint, err)
		}
	}
	zone := EndpointForAddress("[fe80::1%en0]:8877")
	if zone != "https://[fe80::1%25en0]:8877" || ValidateEndpoint(zone) != nil {
		t.Fatalf("IPv6 interface zone lost during invitation encoding: %s", zone)
	}
	id, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := id.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	v := Invite{Version: Version, Endpoint: "https://127.0.0.1:8877", HostPin: pin, RoomID: "room_1", InviteID: "invite_1", ExpiresAt: time.Now().UTC().Add(time.Minute)}
	parsed, err := ParseInvite(EncodeInvite(v))
	if err != nil || parsed != v {
		t.Fatalf("invite roundtrip: %+v %v", parsed, err)
	}
	v.Version++
	if _, err := ParseInvite(EncodeInvite(v)); err == nil {
		t.Fatal("future invite version accepted")
	}
}
func TestLANPinnedMutualTLSAndResumptionVerifier(t *testing.T) {
	host, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	guest, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := host.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
			t.Error("client did not prove a private key")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	server.TLS, err = ServerTLS(host)
	if err != nil {
		t.Fatal(err)
	}
	server.StartTLS()
	defer server.Close()
	invite := Invite{Version: Version, Endpoint: server.URL, HostPin: pin, RoomID: "room", InviteID: "invite", ExpiresAt: time.Now().Add(time.Minute)}
	client, err := NewClient(invite, guest)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	wrong := invite
	wrong.HostPin = strings.Repeat("0", 64)
	bad, err := NewClient(wrong, guest)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.CloseIdleConnections()
	if response, err := bad.Get(server.URL); err == nil {
		response.Body.Close()
		t.Fatal("wrong host pin accepted")
	}
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("LAN inherited an environment proxy")
	}
	if err := client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatal("LAN followed an arbitrary redirect")
	}
	guestCert, err := guest.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	wrongLeaf, err := x509.ParseCertificate(guestCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.TLSClientConfig.VerifyConnection(tls.ConnectionState{DidResume: true, PeerCertificates: []*x509.Certificate{wrongLeaf}}); err == nil {
		t.Fatal("resumed handshake bypassed pin verification")
	}
	withoutCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // Negative fixture deliberately supplies no client identity.
	defer withoutCert.CloseIdleConnections()
	if response, err := withoutCert.Get(server.URL); err == nil {
		response.Body.Close()
		t.Fatal("server accepted client without private-key proof")
	}
}
