package enterpriseservice

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

// These cuts exercise real TLS HTTP, signed synthetic OIDC tokens and PostgreSQL.
// Middleware is installed before browser requests; no live handler is mutated.
func TestHTTPPGSameSubjectChangedEmailAndIssuerConfigurationFailClosed(t *testing.T) {
	f := newHTTPPGFixture(t)
	var email atomic.Value
	email.Store("before@example.test")
	f.idp.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" && r.URL.Path != "/userinfo" {
			f.idp.serve(w, r)
			return
		}
		rec := httptest.NewRecorder()
		f.idp.serve(rec, r)
		var value map[string]any
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &value) != nil {
			w.WriteHeader(500)
			return
		}
		if r.URL.Path == "/token" {
			raw, ok := value["id_token"].(string)
			if !ok {
				w.WriteHeader(500)
				return
			}
			parts := strings.Split(raw, ".")
			if len(parts) != 3 {
				w.WriteHeader(500)
				return
			}
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			var claims map[string]any
			if err != nil || json.Unmarshal(payload, &claims) != nil {
				w.WriteHeader(500)
				return
			}
			claims["email"] = email.Load().(string)
			payload, err = json.Marshal(claims)
			if err != nil {
				w.WriteHeader(500)
				return
			}
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: f.idp.key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture-signing-key"))
			if err != nil {
				w.WriteHeader(500)
				return
			}
			signed, err := signer.Sign(payload)
			if err != nil {
				w.WriteHeader(500)
				return
			}
			value["id_token"], err = signed.CompactSerialize()
			if err != nil {
				w.WriteHeader(500)
				return
			}
		} else {
			value["email"] = email.Load().(string)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	})
	first := f.browser("member-one")
	f.login(first)
	original := f.session(first)
	var identityID, issuer, subject, userID string
	if err := f.admin.QueryRow(t.Context(), `SELECT identity_id,issuer,subject,user_id FROM external_identities WHERE subject='member-one'`).Scan(&identityID, &issuer, &subject, &userID); err != nil {
		t.Fatal(err)
	}
	email.Store("after@example.test")
	second := f.browser("member-one")
	f.login(second)
	if f.session(second).UserID != original.UserID || userID != original.UserID {
		t.Fatal("email change replaced the stable User")
	}
	before := exactIdentityGraph(t, f)
	changedIssuer := f.idp.server.URL + "/different-issuer"
	identity := func(subject string) enterpriseauth.VerifiedIdentity {
		return enterpriseauth.VerifiedIdentity{ConnectionID: "fixture-connection", Issuer: changedIssuer, Subject: subject}
	}
	result, _ := f.store.InitializeIdentity(t.Context(), sqlstore.IdentityConfig{Connections: []sqlstore.TrustConnectionConfig{{ConnectionID: "fixture-connection", Issuer: changedIssuer}}, Bootstrap: identity("operator"), Admitted: []enterpriseauth.VerifiedIdentity{identity("member-one"), identity("member-two")}})
	if result.Outcome == sqlstore.OutcomeCommitted || result.Outcome == sqlstore.OutcomeReplayed {
		t.Fatal("existing connection issuer configuration was accepted")
	}
	if err := f.store.CheckIdentityConfiguration(t.Context(), "org-http", "fixture-connection", changedIssuer); err == nil {
		t.Fatal("changed issuer passed startup identity check")
	}
	if err := f.store.CheckIdentityConfiguration(t.Context(), "org-http", "fixture-connection", f.idp.server.URL); err != nil {
		t.Fatal("original configuration no longer accepted")
	}
	if !bytes.Equal(before, exactIdentityGraph(t, f)) {
		t.Fatal("refused issuer change mutated identity/session/control facts")
	}
	var afterID, afterIssuer, afterSubject, afterUser string
	if err := f.admin.QueryRow(t.Context(), `SELECT identity_id,issuer,subject,user_id FROM external_identities WHERE subject='member-one'`).Scan(&afterID, &afterIssuer, &afterSubject, &afterUser); err != nil {
		t.Fatal(err)
	}
	if afterID != identityID || afterIssuer != issuer || afterSubject != subject || afterUser != userID {
		t.Fatal("identity tuple migrated silently")
	}
	var users, identities, sessions int
	if err := f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM external_identities),(SELECT count(*) FROM browser_sessions)`).Scan(&users, &identities, &sessions); err != nil || users != 1 || identities != 1 || sessions != 2 {
		t.Fatalf("changed-email logins produced incorrect identity counts %d/%d/%d", users, identities, sessions)
	}
	if f.session(first).UserID != original.UserID || f.session(second).UserID != original.UserID {
		t.Fatal("issuer refusal invalidated original application sessions")
	}
}

// Snapshot the whole relevant durable graph, not only the session count.
func exactIdentityGraph(t *testing.T, f *httpPGFixture) []byte {
	t.Helper()
	var raw []byte
	err := f.admin.QueryRow(t.Context(), `SELECT jsonb_build_object('users',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY user_id),'[]'::jsonb) FROM users x),'identities',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY identity_id),'[]'::jsonb) FROM external_identities x),'sessions',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY session_id),'[]'::jsonb) FROM browser_sessions x),'connections',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY connection_id),'[]'::jsonb) FROM trust_connections x),'audit',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY audit_seq),'[]'::jsonb) FROM audit x),'control',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY seq),'[]'::jsonb) FROM control_events x))`).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestHTTPPGTokenEndpointFailuresCreateNoApplicationIdentityOrSession(t *testing.T) {
	for _, mode := range []string{"disconnect", "deadline", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			f := newHTTPPGFixture(t)
			if mode == "deadline" {
				f.provider.Close()
				f.providerConfig.Outbound.Timeout = 100 * time.Millisecond
				f.installHandler(f.store)
			}
			entered := make(chan struct{}, 4)
			release := make(chan struct{})
			defer close(release)
			f.idp.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/token" {
					f.idp.serve(w, r)
					return
				}
				entered <- struct{}{}
				switch mode {
				case "disconnect":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
				case "deadline":
					select {
					case <-r.Context().Done():
					case <-release:
					}
				case "malformed":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id_token":`))
				}
			})
			before := exactIdentityGraph(t, f)
			browser := f.browser("member-one")
			status, _ := f.get(browser, "/enterprise/auth/login")
			if status != 401 && status != 503 {
				t.Fatalf("failed token exchange HTTP%d", status)
			}
			select {
			case <-entered:
			default:
				t.Fatal("token failure injection did not reach actual exchange")
			}
			if f.cookie(browser) != nil {
				t.Fatal("failed exchange issued application session cookie")
			}
			if !bytes.Equal(before, exactIdentityGraph(t, f)) {
				t.Fatal("failed exchange changed durable identity/session/audit/control graph")
			}
			status, _ = f.get(browser, "/enterprise/api/v1/session")
			if status != 401 {
				t.Fatal("failed login could authenticate application session")
			}
		})
	}
}

func TestHTTPPGIndependentApprovedAndDeniedDeviceConsent(t *testing.T) {
	for _, action := range []string{"approve", "deny"} {
		t.Run(action, func(t *testing.T) {
			f := newHTTPPGFixture(t)
			browser := f.browser("member-one")
			f.login(browser)
			session := f.session(browser)
			native := grantsPGNative(t, f)
			proof, _ := enterpriseauth.NewBrowserSecret()
			access, _ := enterpriseauth.NewBrowserSecret()
			refresh, _ := enterpriseauth.NewBrowserSecret()
			ph, ah, rh := tokenHash(proof), tokenHash(access), tokenHash(refresh)
			ch, _ := enterpriseauth.HeadlessConfirmationHash("ABCD-EFGH")
			id := "request-" + strings.Repeat("e", 32)
			begin := map[string]any{"request_id": id, "device_id": "exact-consent-device", "label": "Exact consent", "transport_mode": "headless", "proof_hash": base64.RawURLEncoding.EncodeToString(ph[:]), "confirmation_hash": base64.RawURLEncoding.EncodeToString(ch[:]), "credential": deviceCandidateRequest{GrantID: "exact-consent-grant", AccessHash: base64.RawURLEncoding.EncodeToString(ah[:]), RefreshHash: base64.RawURLEncoding.EncodeToString(rh[:])}}
			status, _ := grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/enrollments", "", begin)
			if status != 200 {
				t.Fatal("actual endpoint enrollment failed")
			}
			path := "/enterprise/api/v1/device-enrollments/" + id
			status, body := f.get(browser, path)
			if status != 200 {
				t.Fatal("browser could not inspect independent enrollment")
			}
			for _, secret := range []string{proof, access, refresh} {
				if bytes.Contains(body, []byte(secret)) {
					t.Fatal("consent disclosed endpoint credential")
				}
			}
			decisionRequest := map[string]string{"client_id": "exact-consent-" + action, "action": action}
			if action == "approve" {
				decisionRequest["confirmation_code"] = "ABCD-EFGH"
			}
			status, body = f.post(browser, path+"/decision", decisionRequest, session.CSRF)
			var decision headlessHTTPReceipt
			if status != 200 || json.Unmarshal(body, &decision) != nil || decision.Status != map[string]string{"approve": "approved", "deny": "denied"}[action] {
				t.Fatalf("browser decision failed HTTP%d status=%s", status, decision.Status)
			}
			status, body = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/enrollments/"+id+"/headless-complete", "", map[string]string{"client_id": "exact-complete", "client_proof": proof, "confirmation_code": "ABCD-EFGH"})
			want := 0
			if action == "approve" {
				want = 1
				if status != 200 {
					t.Fatal("approved original completion failed")
				}
			} else if status != 403 {
				t.Fatalf("denied original completion HTTP%d", status)
			}
			for _, secret := range []string{proof, access, refresh} {
				if bytes.Contains(body, []byte(secret)) {
					t.Fatal("completion disclosed endpoint credential")
				}
			}
			var devices, families, grants int
			if err := f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM devices),(SELECT count(*) FROM device_families),(SELECT count(*) FROM device_grants)`).Scan(&devices, &families, &grants); err != nil || devices != want || families != want || grants != want {
				t.Fatalf("decision minted wrong authority %d/%d/%d want%d", devices, families, grants, want)
			}
			status, _ = grantsPGCall(t, f, native, http.MethodGet, "/enterprise/device/v1/session", access, nil)
			if (want == 1 && status != 200) || (want == 0 && status != 401) {
				t.Fatalf("candidate access after %s HTTP%d", action, status)
			}
		})
	}
}
