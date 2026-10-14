package enterpriseservice

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sean2077/pairroom/internal/enterpriseauth"
)

// Uses the frozen OPS08 scanner/closed metrics/log-capture helpers. During this
// isolated slice that source is supplied by a Go overlay, not copied into this tree.
func TestHeadlessDiagnosticRedactionHTTPPGOriginalCandidatesAndPollFault(t *testing.T) {
	f := newHTTPPGFixture(t)
	logs := &diagnosticLogBuffer{tlsLogged: make(chan struct{})}
	f.server.Config.ErrorLog = log.New(logs, "", 0)
	var mu sync.Mutex
	var tokens []string
	idp := f.idp.server.Config.Handler
	f.idp.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirror := &diagnosticMirror{ResponseWriter: w}
		idp.ServeHTTP(mirror, r)
		if r.URL.Path == "/token" {
			var reply struct {
				Access string `json:"access_token"`
				ID     string `json:"id_token"`
			}
			if json.Unmarshal(mirror.body.Bytes(), &reply) == nil {
				mu.Lock()
				tokens = append(tokens, reply.Access, reply.ID)
				mu.Unlock()
			}
		}
	})
	member, operator, native := f.browser("member-one"), f.browser("operator"), grantsPGNative(t, f)
	f.login(member)
	f.login(operator)
	m, a := f.session(member), f.session(operator)
	secret := func() string {
		value, err := enterpriseauth.NewBrowserSecret()
		if err != nil {
			t.Fatal("private candidate generation failed")
		}
		return value
	}
	proof, access, refresh, badProof := secret(), secret(), secret(), secret()
	code, err := enterpriseauth.NewHeadlessConfirmationCode()
	if err != nil {
		t.Fatal("private confirmation generation failed")
	}
	code = code[:4] + "-" + code[4:]
	canonical, _ := enterpriseauth.NormalizeHeadlessConfirmationCode(code)
	confirmation, _ := enterpriseauth.HeadlessConfirmationHash(code)
	ph, ah, rh := tokenHash(proof), tokenHash(access), tokenHash(refresh)
	const nativeMarker = "OPS08_HEADLESS_PRIVATE_NATIVE_SESSION"
	const pathMarker = `C:\OPS08_HEADLESS_PRIVATE_PATH\session`
	mu.Lock()
	captured := append([]string{}, tokens...)
	mu.Unlock()
	if len(captured) != 4 {
		t.Fatal("actual signed OIDC token positive control missing")
	}
	canaries := append([]string{proof, badProof, access, refresh, code, canonical, nativeMarker, pathMarker, f.cookie(member).Value, f.cookie(operator).Value, m.CSRF, a.CSRF}, captured...)
	for _, hash := range [][32]byte{ph, ah, rh, confirmation} {
		canaries = append(canaries, base64.RawURLEncoding.EncodeToString(hash[:]), hex.EncodeToString(hash[:]))
	}
	call := func(client *http.Client, method, path, bearer, csrf string, value any) (int, []byte) {
		t.Helper()
		var payload []byte
		if value != nil {
			var err error
			payload, err = json.Marshal(value)
			if err != nil {
				t.Fatal("request encoding failed")
			}
		}
		r, err := http.NewRequestWithContext(t.Context(), method, f.origin+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatal("request construction failed")
		}
		r.Header.Set(DeviceDeploymentHeader, f.cfg.DeploymentID)
		if value != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		if csrf != "" {
			r.Header.Set("Origin", f.origin)
			r.Header.Set(CSRFHeader, csrf)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal("actual public HTTP request failed")
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if err != nil {
			t.Fatal("actual public HTTP response failed")
		}
		diagnosticScan(t, path+" wire", string(data), canaries)
		for key, values := range response.Header {
			diagnosticScan(t, path+" header "+key, strings.Join(values, "\n"), canaries)
		}
		return response.StatusCode, data
	}
	check := func(status int, body []byte, want int, field, value string) {
		t.Helper()
		var result map[string]any
		if status != want || json.Unmarshal(body, &result) != nil || result[field] != value {
			t.Fatalf("public response contract mismatch: HTTP %d wanted %d field %s", status, want, field)
		}
	}
	id := "request-" + strings.Repeat("d", 32)
	device, grant := "ops08-headless-device", "ops08-headless-grant"
	begin := map[string]any{"request_id": id, "device_id": device, "label": "Headless privacy gate", "transport_mode": "headless", "proof_hash": grantsPGHash(proof), "confirmation_hash": base64.RawURLEncoding.EncodeToString(confirmation[:]), "credential": deviceCandidateRequest{GrantID: grant, AccessHash: grantsPGHash(access), RefreshHash: grantsPGHash(refresh)}}
	status, body := call(native, http.MethodPost, "/enterprise/device/v1/enrollments", "", "", begin)
	check(status, body, 200, "status", "pending")
	var begun headlessHTTPReceipt
	if json.Unmarshal(body, &begun) != nil || begun.CandidateFingerprint != enterpriseauth.HeadlessCandidateFingerprint(grant, ah, rh) || begun.VerificationURL != f.origin+"/enterprise/device-enrollments/"+id {
		t.Fatal("Begin did not freeze the original candidate metadata")
	}
	var bound bool
	if err := f.admin.QueryRow(t.Context(), `SELECT proof_hash=$2 AND access_hash=$3 AND refresh_hash=$4 AND confirmation_hash=$5 FROM device_enrollments WHERE request_id=$1`, id, ph[:], ah[:], rh[:], confirmation[:]).Scan(&bound); err != nil || !bound {
		t.Fatal("actual endpoint candidates/short code were not bound in PostgreSQL")
	}
	status, body = call(native, http.MethodGet, "/enterprise/device/v1/session", access, "", nil)
	check(status, body, 401, "error", "device_credential_invalid")
	path := "/enterprise/api/v1/device-enrollments/" + id
	status, body = call(native, http.MethodGet, path, "", "", nil)
	check(status, body, 401, "error", "session_invalid")
	status, _ = call(member, http.MethodGet, path, "", "", nil)
	if status != 200 {
		t.Fatal("own authenticated browser could not inspect frozen consent metadata")
	}
	wrong := "ABCD-EFGH"
	if wrong == code {
		wrong = "JKMN-PQRS"
	}
	canaries = append(canaries, wrong, strings.ReplaceAll(wrong, "-", ""))
	decision := map[string]string{"client_id": "ops08-wrong-code", "action": "approve", "confirmation_code": wrong}
	status, body = call(member, http.MethodPost, path+"/decision", "", m.CSRF, decision)
	check(status, body, 400, "disposition", "confirmation_rejected")
	status, body = call(member, http.MethodPost, path+"/decision", "", m.CSRF, decision)
	check(status, body, 400, "outcome", "replayed")
	var failures, grants int
	if err := f.admin.QueryRow(t.Context(), `SELECT confirmation_failures,(SELECT count(*) FROM device_grants) FROM device_enrollments WHERE request_id=$1`, id).Scan(&failures, &grants); err != nil || failures != 1 || grants != 0 {
		t.Fatal("wrong-code counter/replay issued a credential or double-counted")
	}
	status, body = call(member, http.MethodPost, path+"/decision", "", m.CSRF, map[string]string{"client_id": "ops08-right-code", "action": "approve", "confirmation_code": code})
	check(status, body, 200, "status", "approved")
	base := "/enterprise/device/v1/enrollments/" + id
	state := func() string {
		t.Helper()
		var result string
		if err := f.admin.QueryRow(t.Context(), `SELECT jsonb_build_object('enrollments',(SELECT jsonb_agg(to_jsonb(e) ORDER BY request_id) FROM device_enrollments e),'operations',(SELECT count(*) FROM operations),'audit',(SELECT count(*) FROM audit),'devices',(SELECT count(*) FROM devices),'grants',(SELECT count(*) FROM device_grants))::text`).Scan(&result); err != nil {
			t.Fatal("effect snapshot failed")
		}
		return result
	}
	beforeRefusals := state()
	status, body = call(native, http.MethodPost, base+"/poll", "", "", map[string]any{"client_proof": badProof, "sequence": 1})
	check(status, body, 403, "class", "authorization")
	status, body = call(native, http.MethodPost, base+"/poll", "", "", map[string]any{"sequence": 1})
	check(status, body, 400, "error", "invalid_device_request")
	status, body = call(native, http.MethodPost, base+"/poll", "", "", map[string]any{"client_proof": proof, "sequence": 0, "confirmation_code": code})
	check(status, body, 400, "error", "invalid_device_request")
	if state() != beforeRefusals {
		t.Fatal("wrong-proof, missing-proof or malformed poll changed durable effects")
	}
	status, body = call(native, http.MethodPost, base+"/poll", "", "", map[string]any{"client_proof": proof, "sequence": 1})
	var polled headlessHTTPReceipt
	if json.Unmarshal(body, &polled) != nil || polled.Sequence != 1 || polled.Outcome != "committed" || !(status == 429 && polled.Status == "slow_down" || status == 200 && polled.Status == "approved") {
		t.Fatal("original-proof poll did not preserve bounded public observation")
	}
	before := state()
	// Test-owned SQL fault at the real poll UPDATE, not a fabricated backend error.
	if _, err := f.admin.Exec(t.Context(), `CREATE FUNCTION ops08_headless_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION USING ERRCODE='23514', MESSAGE=TG_ARGV[0]||' '||TG_ARGV[1]||' '||TG_ARGV[2], DETAIL=TG_ARGV[3]||' '||TG_ARGV[4]||' '||TG_ARGV[5]; END $$`); err != nil {
		t.Fatal("private fault function creation failed")
	}
	args := []string{proof, code, nativeMarker, access, refresh, pathMarker}
	for i, value := range args {
		args[i] = "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	if _, err := f.admin.Exec(t.Context(), `CREATE TRIGGER aaa_ops08_headless_fault BEFORE UPDATE ON device_enrollments FOR EACH ROW EXECUTE FUNCTION ops08_headless_fault(`+strings.Join(args, ",")+`)`); err != nil {
		t.Fatal("private fault trigger creation failed")
	}
	_, directErr := f.admin.Exec(t.Context(), `UPDATE device_enrollments SET poll_sequence=poll_sequence WHERE request_id=$1`, id)
	var pgError *pgconn.PgError
	if !errors.As(directErr, &pgError) || pgError.Code != "23514" {
		t.Fatal("actual PostgreSQL fault positive control absent")
	}
	for _, value := range []string{proof, code, nativeMarker} {
		if !strings.Contains(pgError.Message, value) {
			t.Fatal("actual PostgreSQL MESSAGE canary missing")
		}
	}
	for _, value := range []string{access, refresh, pathMarker} {
		if !strings.Contains(pgError.Detail, value) {
			t.Fatal("actual PostgreSQL DETAIL canary missing")
		}
	}
	status, body = call(native, http.MethodPost, base+"/poll", "", "", map[string]any{"client_proof": proof, "sequence": 2})
	check(status, body, 409, "class", "constraint_violation")
	if state() != before {
		t.Fatal("failed poll leaked a partial counter, approval, credential, operation or audit effect")
	}
	status, body = call(native, http.MethodPost, base+"/resolve-poll", "", "", map[string]any{"client_proof": proof, "sequence": 2})
	check(status, body, 409, "outcome", "not_committed")
	if state() != before {
		t.Fatal("original failed poll Resolve changed effects")
	}
	if _, err := f.admin.Exec(t.Context(), `DROP TRIGGER aaa_ops08_headless_fault ON device_enrollments`); err != nil {
		t.Fatal("private fault trigger cleanup failed")
	}
	complete := map[string]string{"client_id": "ops08-original-complete", "client_proof": badProof, "confirmation_code": code}
	status, body = call(native, http.MethodPost, base+"/headless-complete", "", "", complete)
	check(status, body, 403, "class", "authorization")
	if state() != before {
		t.Fatal("wrong-proof completion changed durable effects")
	}
	complete["client_proof"] = proof
	status, body = call(native, http.MethodPost, base+"/headless-complete", "", "", complete)
	check(status, body, 200, "status", "completed")
	status, _ = call(native, http.MethodGet, "/enterprise/device/v1/session", access, "", nil)
	if status != 200 {
		t.Fatal("approved original candidate did not become an actually usable credential")
	}
	status, body = call(native, http.MethodPost, base+"/headless-complete", "", "", complete)
	check(status, body, 200, "outcome", "replayed")
	if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM device_grants WHERE grant_id=$1`, grant).Scan(&grants); err != nil || grants != 1 {
		t.Fatal("original completion replay minted another credential")
	}
	// A separate browser-denied request retains its own proof/candidates and never
	// becomes a credential. Codes alone cannot reverse that decision.
	deniedID := "request-" + strings.Repeat("e", 32)
	deniedProof, deniedAccess, deniedRefresh := secret(), secret(), secret()
	canaries = append(canaries, deniedProof, deniedAccess, deniedRefresh)
	for _, value := range []string{deniedProof, deniedAccess, deniedRefresh} {
		hash := tokenHash(value)
		canaries = append(canaries, base64.RawURLEncoding.EncodeToString(hash[:]), hex.EncodeToString(hash[:]))
	}
	deniedBegin := map[string]any{"request_id": deniedID, "device_id": "ops08-headless-denied-device", "label": "Denied privacy gate", "transport_mode": "headless", "proof_hash": grantsPGHash(deniedProof), "confirmation_hash": base64.RawURLEncoding.EncodeToString(confirmation[:]), "credential": deviceCandidateRequest{GrantID: "ops08-headless-denied-grant", AccessHash: grantsPGHash(deniedAccess), RefreshHash: grantsPGHash(deniedRefresh)}}
	status, body = call(native, http.MethodPost, "/enterprise/device/v1/enrollments", "", "", deniedBegin)
	check(status, body, 200, "status", "pending")
	status, body = call(member, http.MethodPost, "/enterprise/api/v1/device-enrollments/"+deniedID+"/decision", "", m.CSRF, map[string]string{"client_id": "ops08-deny", "action": "deny"})
	check(status, body, 200, "status", "denied")
	deniedBase := "/enterprise/device/v1/enrollments/" + deniedID
	status, body = call(native, http.MethodPost, deniedBase+"/poll", "", "", map[string]any{"client_proof": deniedProof, "sequence": 1})
	check(status, body, 200, "status", "denied")
	deniedBefore := state()
	status, body = call(native, http.MethodPost, deniedBase+"/headless-complete", "", "", map[string]string{"client_id": "ops08-denied-complete", "client_proof": deniedProof, "confirmation_code": code})
	check(status, body, 403, "class", "authorization")
	if state() != deniedBefore {
		t.Fatal("denied completion changed durable effects")
	}
	status, body = call(native, http.MethodGet, "/enterprise/device/v1/session", deniedAccess, "", nil)
	check(status, body, 401, "error", "device_credential_invalid")
	if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM device_grants`).Scan(&grants); err != nil || grants != 1 {
		t.Fatal("denied request minted an unapproved credential")
	}
	metadata := append(append([]string{}, canaries...), id, device, grant, deniedID, "ops08-headless-denied-device", "ops08-headless-denied-grant", f.cfg.DeploymentID, m.UserID, a.UserID)
	for _, client := range []*http.Client{member, operator} {
		status, body = call(client, http.MethodGet, administrationPrefix+"metrics", "", "", nil)
		if client == member {
			check(status, body, 403, "error", "administrator_required")
			continue
		}
		if status != 200 {
			t.Fatal("administrator metrics were unavailable")
		}
		diagnosticScan(t, "headless metrics metadata", string(body), metadata)
		diagnosticMetricContract(t, body)
	}
	status, body = call(operator, http.MethodGet, administrationPrefix+"diagnostics", "", "", nil)
	if status != 200 {
		t.Fatal("administrator diagnostics were unavailable")
	}
	diagnosticScan(t, "headless diagnostics metadata", string(body), metadata)
	var diagnostic map[string]json.RawMessage
	if json.Unmarshal(body, &diagnostic) != nil {
		t.Fatal("diagnostic output is not structured")
	}
	keys := make([]string, 0, len(diagnostic))
	for key := range diagnostic {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := []string{"concurrent_replay_limit", "concurrent_request_limit", "connection_limit", "draining", "replays_active", "request_duration_buckets", "request_duration_nanoseconds", "requests_active", "requests_completed_by_status_class", "requests_rejected", "resident_rooms", "version"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatal("headless changed the closed aggregate diagnostic key set")
	}
	origin, _ := url.Parse(f.origin)
	conn, err := net.DialTimeout("tcp", origin.Host, 2*time.Second)
	if err != nil {
		t.Fatal("actual TLS log trigger connection failed")
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = fmt.Fprintf(conn, "POST /%s/%s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nX-Client-Proof: %s\r\n\r\n", id, canonical, access, proof)
	_, _ = io.ReadAll(conn)
	_ = conn.Close()
	select {
	case <-logs.tlsLogged:
	case <-time.After(2 * time.Second):
		t.Fatal("actual standard TLS log event did not arrive")
	case <-t.Context().Done():
		t.Fatal("test ended before actual standard TLS log event")
	}
	diagnosticScan(t, "actual headless standard Server ErrorLog", logs.text(), metadata)
	t.Logf("positive controls: signed OIDC tokens=%d; protected private values/hashes=%d; real PgError MESSAGE/DETAIL canaries=6; wrong-code failures=1; issued original grants=%d; original poll2 Resolve=not_committed; actual TLS log bytes=%d", len(captured), len(canaries), grants, len(logs.text()))
}
