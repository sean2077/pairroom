package enterpriseservice

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
)

func TestHeadlessHTTPPGBeginHasExplicitModeAndNoCallback(t *testing.T) {
	f := newHTTPPGFixture(t)
	native := grantsPGNative(t, f)
	proof, _ := enterpriseauth.NewBrowserSecret()
	ph := tokenHash(proof)
	confirmation, err := enterpriseauth.HeadlessConfirmationHash("ABCD-EFGH")
	if err != nil {
		t.Fatal(err)
	}
	id := "request-" + strings.Repeat("a", 32)
	request := map[string]any{"request_id": id, "device_id": "headless-device", "label": "Headless endpoint", "transport_mode": "headless", "proof_hash": base64.RawURLEncoding.EncodeToString(ph[:]), "confirmation_hash": base64.RawURLEncoding.EncodeToString(confirmation[:]), "credential": candidateRequest(t, "headless-grant")}
	status, body := grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/enrollments", "", request)
	if status != http.StatusOK {
		t.Fatalf("headless Begin did not accept explicit transport: HTTP %d", status)
	}
	var result headlessHTTPReceipt
	if json.Unmarshal(body, &result) != nil || result.Outcome != "committed" || result.TransportMode != "headless" || result.NextPollAt.IsZero() || result.VerificationURL != f.origin+"/enterprise/device-enrollments/"+id {
		t.Fatal("headless Begin returned mismatched verification metadata")
	}
	for _, forbidden := range []string{proof, "ABCD-EFGH", "ABCDEFGH", "callback_uri", "completion_location"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatal("headless Begin exposed proof or callback material")
		}
	}
}

type headlessHTTPReceipt struct {
	Outcome, Kind, Status, Disposition string
	RequestID                          string    `json:"request_id"`
	DeviceID                           string    `json:"device_id"`
	GrantID                            string    `json:"grant_id"`
	TransportMode                      string    `json:"transport_mode"`
	VerificationURL                    string    `json:"verification_url"`
	CandidateFingerprint               string    `json:"candidate_fingerprint"`
	CompletionLocation                 string    `json:"completion_location"`
	Sequence                           uint64    `json:"sequence"`
	ExpiresAt                          time.Time `json:"expires_at"`
	NextPollAt                         time.Time `json:"next_poll_at"`
}

func TestHeadlessHTTPPGConsentCountersAndOriginalProof(t *testing.T) {
	f := newHTTPPGFixture(t)
	native, browser := grantsPGNative(t, f), f.browser("member-one")
	f.login(browser)
	session := f.session(browser)
	proof, _ := enterpriseauth.NewBrowserSecret()
	access, _ := enterpriseauth.NewBrowserSecret()
	refresh, _ := enterpriseauth.NewBrowserSecret()
	ph, ah, rh := tokenHash(proof), tokenHash(access), tokenHash(refresh)
	confirmation, _ := enterpriseauth.HeadlessConfirmationHash("ABCD-EFGH")
	id := "request-" + strings.Repeat("b", 32)
	begin := map[string]any{"request_id": id, "device_id": "headless-device", "label": "Headless endpoint", "transport_mode": "headless", "proof_hash": base64.RawURLEncoding.EncodeToString(ph[:]), "confirmation_hash": base64.RawURLEncoding.EncodeToString(confirmation[:]), "credential": deviceCandidateRequest{GrantID: "headless-grant", AccessHash: base64.RawURLEncoding.EncodeToString(ah[:]), RefreshHash: base64.RawURLEncoding.EncodeToString(rh[:])}}
	status, body := grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/enrollments", "", begin)
	if status != 200 {
		t.Fatalf("begin HTTP%d", status)
	}
	var begun headlessHTTPReceipt
	if json.Unmarshal(body, &begun) != nil || begun.CandidateFingerprint != enterpriseauth.HeadlessCandidateFingerprint("headless-grant", ah, rh) {
		t.Fatal("candidate comparison metadata changed")
	}
	path := "/enterprise/api/v1/device-enrollments/" + id
	status, body = f.get(browser, path)
	if status != 200 || strings.Contains(string(body), "confirmation_hash") || strings.Contains(string(body), "ABCDEFGH") {
		t.Fatal("consent metadata unsafe")
	}
	wrong := map[string]string{"client_id": "wrong-confirmation", "action": "approve", "confirmation_code": "0123-4567"}
	status, body = f.post(browser, path+"/decision", wrong, session.CSRF)
	var result headlessHTTPReceipt
	if status != 400 || json.Unmarshal(body, &result) != nil || result.Outcome != "committed" || result.Disposition != "confirmation_rejected" || result.Status != "pending" {
		t.Fatal("counter refusal was not a confirmed transaction")
	}
	status, body = f.post(browser, path+"/decision", wrong, session.CSRF)
	if status != 400 || json.Unmarshal(body, &result) != nil || result.Outcome != "replayed" {
		t.Fatal("same human attempt was counted twice")
	}
	var failures, grants int
	if err := f.admin.QueryRow(t.Context(), `SELECT confirmation_failures,(SELECT count(*) FROM device_grants) FROM device_enrollments WHERE request_id=$1`, id).Scan(&failures, &grants); err != nil || failures != 1 || grants != 0 {
		t.Fatal("counter/credential transaction incorrect")
	}
	status, body = f.post(browser, path+"/decision", map[string]string{"client_id": "correct-confirmation", "action": "approve", "confirmation_code": "ABCD-EFGH"}, session.CSRF)
	result = headlessHTTPReceipt{}
	if status != 200 || json.Unmarshal(body, &result) != nil || result.Status != "approved" || result.Kind != "device.enrollment.headless.decide" || result.CompletionLocation != "" {
		t.Fatal("headless browser approval returned wrong transport")
	}
	badProof, _ := enterpriseauth.NewBrowserSecret()
	base := "/enterprise/device/v1/enrollments/" + id
	status, _ = grantsPGCall(t, f, native, http.MethodPost, base+"/poll", "", map[string]any{"client_proof": badProof, "sequence": uint64(1)})
	if status != 403 {
		t.Fatalf("wrong proof disclosed poll state HTTP%d", status)
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, base+"/poll", "", map[string]any{"client_proof": proof, "sequence": uint64(1)})
	result = headlessHTTPReceipt{}
	if status != 429 || json.Unmarshal(body, &result) != nil || result.Outcome != "committed" || result.Status != "slow_down" || result.Disposition != "" || result.Sequence != 1 {
		t.Fatalf("rate write was misclassified: HTTP %d outcome=%s status=%s sequence=%d disposition=%s", status, result.Outcome, result.Status, result.Sequence, result.Disposition)
	}
	complete := map[string]string{"client_id": "headless-complete", "client_proof": badProof, "confirmation_code": "ABCD-EFGH"}
	status, _ = grantsPGCall(t, f, native, http.MethodPost, base+"/headless-complete", "", complete)
	if status != 403 {
		t.Fatal("browser code replaced original endpoint proof")
	}
	complete["client_proof"] = proof
	status, body = grantsPGCall(t, f, native, http.MethodPost, base+"/headless-complete", "", complete)
	result = headlessHTTPReceipt{}
	if status != 200 || json.Unmarshal(body, &result) != nil || result.Outcome != "committed" || result.Status != "completed" || result.GrantID != "headless-grant" {
		t.Fatal("original candidate completion failed")
	}
	for _, secret := range []string{proof, access, refresh, "ABCDEFGH", "ABCD-EFGH"} {
		if strings.Contains(string(body), secret) {
			t.Fatal("headless completion exposed private material")
		}
	}
	status, _ = grantsPGCall(t, f, native, http.MethodGet, "/enterprise/device/v1/session", access, nil)
	if status != 200 {
		t.Fatal("approved original candidate was unusable")
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, base+"/headless-complete", "", complete)
	result = headlessHTTPReceipt{}
	if status != 200 || json.Unmarshal(body, &result) != nil || result.Outcome != "replayed" {
		t.Fatal("completion did not preserve original receipt")
	}
	if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM device_grants`).Scan(&grants); err != nil || grants != 1 {
		t.Fatal("replay minted a second credential")
	}
}
