package enterpriseservice

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func retiredIngressObjects(t *testing.T, f *httpPGFixture) [32]byte {
	t.Helper()
	var joined strings.Builder
	for _, table := range []string{"attachments", "attachment_usage", "attachment_references", "attachment_operations"} {
		var data string
		if err := f.admin.QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM `+table+` x`).Scan(&data); err != nil {
			t.Fatal("private attachment state snapshot unavailable")
		}
		joined.WriteString(table)
		joined.WriteString(data)
	}
	return sha256.Sum256([]byte(joined.String()))
}

func TestRetiredIngressHTTPPGSSEExportLANAndManagementCannotBypassAuthority(t *testing.T) {
	f := newObjectHTTPFixture(t)
	var certificateRequests atomic.Int64
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Retired-Certificate") == "1" {
			certificateRequests.Add(1)
			if r.TLS == nil || len(r.TLS.PeerCertificates) != 0 {
				t.Error("enterprise unexpectedly authenticated a configured LAN certificate")
			}
		}
		original.ServeHTTP(w, r)
	})
	owner, member := f.browser("member-one"), f.browser("member-two")
	f.login(owner)
	f.login(member)
	o, m := f.session(owner), f.session(member)
	const room = "retired-ingress-room"
	const bodyMarker = "RETIRED_INGRESS_PRIVATE_BODY"
	const quoteMarker = "RETIRED_INGRESS_PRIVATE_QUOTED_RESPONSE"
	data := []byte("RETIRED_INGRESS_PRIVATE_OBJECT_BYTES")
	f.createRoom(owner, o.CSRF, room)
	objectHTTPAdmit(t, f, owner, o.CSRF, room, m.UserID)
	native := grantsPGNative(t, f)
	device := grantsPGEnroll(t, f, native, owner, o.CSRF, "retired-ingress")
	binding := grantsPGBind(t, f, native, owner, o.CSRF, room, device, model.ActorSlot1, "retired-ingress-one")
	meta := objectHTTPFinish(t, f, native, room, "retired-verify", objectHTTPReserve(t, f, native, room, "retired-reserve", data, binding.token, ""), data, binding.token, "")
	ids, _ := model.NewEnterpriseAttachmentIDs([]string{meta.ID})
	status, body := grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+room+"/relay", binding.token, grantsPGRelayRequest{ClientID: "retired-source", BindingID: binding.bindingID, Generation: 1, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: bodyMarker, AttachmentIDs: ids, ExpectedPeerGeneration: 1}})
	var source grantsPGRelayReply
	if status != 200 || json.Unmarshal(body, &source) != nil || source.Result.Message == nil {
		t.Fatal("actual private source publication failed")
	}
	status, body = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/messages", map[string]any{"client_id": "retired-quoted", "to": "slot1", "text": quoteMarker, "quote_id": source.Result.Message.ID}, o.CSRF)
	if status != 200 || !bytes.Contains(body, []byte(bodyMarker)) || !bytes.Contains(body, []byte(meta.ID)) {
		t.Fatal("actual same-Room quoted evidence positive control failed")
	}
	status, body = f.get(member, "/enterprise/api/v1/rooms/"+room)
	if status != 200 || !bytes.Contains(body, []byte(bodyMarker)) || !bytes.Contains(body, []byte(quoteMarker)) {
		t.Fatal("admitted member full Room positive control failed")
	}
	content := "/enterprise/api/v1/rooms/" + room + "/attachments/" + meta.ID + "/content"
	status, body, _, err := objectHTTPRaw(t, f, member, http.MethodGet, content, nil, "", "", map[string]string{"Range": "bytes=0-6"})
	if err != nil || status != 206 || !bytes.Equal(body, data[:7]) {
		t.Fatal("admitted member actual referenced-object Range positive control failed")
	}
	// Production LAN identity and mutual-TLS verifier prove possession on their
	// own transport. This is not an enterprise-issued certificate or Room grant.
	host, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal("LAN host identity generation failed")
	}
	guest, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal("LAN client identity generation failed")
	}
	hostPin, _ := host.Fingerprint()
	guestPin, _ := guest.Fingerprint()
	possessed := make(chan bool, 1)
	lan := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		possessed <- r.TLS != nil && len(r.TLS.PeerCertificates) == 1 && lanshare.Fingerprint(r.TLS.PeerCertificates[0]) == guestPin
		w.WriteHeader(http.StatusNoContent)
	}))
	lan.TLS, err = lanshare.ServerTLS(host)
	if err != nil {
		t.Fatal("production LAN TLS configuration failed")
	}
	lan.StartTLS()
	t.Cleanup(lan.Close)
	lanClient, err := lanshare.NewClient(lanshare.Invite{Version: lanshare.Version, Endpoint: lan.URL, HostPin: hostPin, RoomID: "retained-local-room", InviteID: "retained-local-invite", ExpiresAt: time.Now().Add(time.Minute)}, guest)
	if err != nil {
		t.Fatal("production LAN client configuration failed")
	}
	t.Cleanup(lanClient.CloseIdleConnections)
	positive, err := lanClient.Get(lan.URL)
	if err != nil {
		t.Fatal("actual LAN mutual-TLS positive control failed")
	}
	_ = positive.Body.Close()
	if positive.StatusCode != 204 || !<-possessed {
		t.Fatal("LAN client did not prove possession of its exact key")
	}
	if f.server.TLS.ClientAuth != tls.NoClientCert {
		t.Fatal("fixture no longer represents the configured enterprise TLS ingress")
	}
	cert, err := guest.Certificate()
	if err != nil {
		t.Fatal("LAN credential unavailable")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.roots.Clone(), MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}}
	t.Cleanup(transport.CloseIdleConnections)
	certificateClient := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	before, objectsBefore := waitIngressDomain(t, f), retiredIngressObjects(t, f)
	type ingressCase struct {
		name, method, path string
		status             int
		code               string
		client             *http.Client
	}
	cases := []ingressCase{
		{"local-sse", http.MethodGet, "/api/v1/events", 404, "not_found", certificateClient},
		{"local-export", http.MethodGet, "/api/v1/export?format=json", 404, "not_found", certificateClient},
		{"local-room-sse", http.MethodGet, "/api/v1/rooms/" + room + "/events", 404, "not_found", owner},
		{"local-room-export", http.MethodGet, "/api/v1/rooms/" + room + "/export", 404, "not_found", member},
		{"enterprise-sse", http.MethodGet, "/enterprise/api/v1/rooms/" + room + "/events", 404, "not_found", owner},
		{"enterprise-export", http.MethodGet, "/enterprise/api/v1/rooms/" + room + "/export", 404, "not_found", member},
		{"enterprise-export-query", http.MethodGet, "/enterprise/api/v1/rooms/" + room + "/export?format=json", 400, "invalid_query", owner},
		{"enterprise-global-sse", http.MethodGet, "/enterprise/api/v1/events", 404, "not_found", owner},
		{"enterprise-export-no-session", http.MethodGet, "/enterprise/api/v1/rooms/" + room + "/export", 401, "session_invalid", certificateClient},
		{"local-management-setup", http.MethodPost, "/api/v1/projects/private-project/rooms", 404, "not_found", certificateClient},
		{"local-native-bind", http.MethodPost, "/api/v1/rooms/" + room + "/native-bindings/slot1", 404, "not_found", certificateClient},
		{"lan-history", http.MethodPost, "/lan/v1/rooms/" + room + "/history", 404, "not_found", certificateClient},
		{"lan-claim", http.MethodPost, "/lan/v1/rooms/" + room + "/claim", 404, "not_found", certificateClient},
		{"lan-join", http.MethodPost, "/lan/v1/rooms/" + room + "/join", 404, "not_found", certificateClient},
		{"native-sse", http.MethodGet, "/enterprise/device/v1/rooms/" + room + "/relay/events", 400, "invalid_relay_request", certificateClient},
		{"native-export", http.MethodGet, "/enterprise/device/v1/rooms/" + room + "/relay/export", 400, "invalid_relay_request", certificateClient},
		{"native-current-context-no-binding", http.MethodGet, "/enterprise/device/v1/rooms/" + room + "/relay/context", 400, "invalid_relay_request", certificateClient},
		{"native-current-context-malformed-binding", http.MethodGet, "/enterprise/device/v1/rooms/" + room + "/relay/context", 401, "binding_invalid", certificateClient},
		{"current-room-no-browser-session", http.MethodGet, "/enterprise/api/v1/rooms/" + room, 401, "session_invalid", certificateClient},
		{"legacy-object-path", http.MethodGet, "/api/v1/rooms/" + room + "/attachments/" + meta.ID + "/content", 404, "not_found", certificateClient},
	}
	protected := []string{bodyMarker, quoteMarker, string(data), meta.ID, source.Result.Message.ID, "Private Room"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := http.NewRequestWithContext(t.Context(), c.method, f.origin+c.path, strings.NewReader(`{"host_mode":"native","action":"approve","text":"candidate"}`))
			if err != nil {
				t.Fatal("public retired-ingress request construction failed")
			}
			r.Header.Set(DeviceDeploymentHeader, f.cfg.DeploymentID)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Forwarded-Client-Cert", strings.ReplaceAll(guest.CertificatePEM, "\n", ""))
			r.Header.Set("X-Client-Cert-Fingerprint", guestPin)
			r.Header.Set("X-PairRoom-User", o.UserID)
			r.Header.Set("X-PairRoom-Role", "owner")
			r.Header.Set("X-PairRoom-Room", room)
			r.Header.Set("X-Management-Token", "retained-setup-candidate")
			if c.name == "native-current-context-malformed-binding" {
				r.Header.Set("Authorization", "Bearer retained-setup-candidate")
			}
			if c.client == certificateClient {
				r.Header.Set("X-Test-Retired-Certificate", "1")
			}
			response, err := c.client.Do(r)
			if err != nil {
				t.Fatal("actual retired-ingress request failed")
			}
			data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			var reply struct {
				Error string `json:"error"`
			}
			if err != nil || response.StatusCode != c.status || json.Unmarshal(data, &reply) != nil || reply.Error != c.code {
				t.Fatalf("retired ingress contract: HTTP%d wanted%d", response.StatusCode, c.status)
			}
			for _, marker := range protected {
				if bytes.Contains(data, []byte(marker)) {
					t.Fatal("retired ingress disclosed retained history, object or quote evidence")
				}
			}
			if response.Header.Get("Content-Range") != "" || strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatal("retired ingress exposed a stream or content range")
			}
			if waitIngressDomain(t, f) != before || retiredIngressObjects(t, f) != objectsBefore {
				t.Fatal("retired ingress changed authorization, facts, operations, audit or objects")
			}
		})
	}
	if certificateRequests.Load() != 14 {
		t.Fatalf("configured-certificate request positive control count=%d", certificateRequests.Load())
	}
	t.Logf("retired entrypoints=%d; real LAN TLS key-possession=1; enterprise TLS client certificate requests/peer certificates=%d/0 (NoClientCert); actual authorized history/quote/Range positive controls=3; forbidden disclosures/effects=0", len(cases), certificateRequests.Load())
}
