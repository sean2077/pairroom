package enterpriseservice

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type diagnosticLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
	tlsLogged chan struct{}
	tlsOnce   sync.Once
}

func (b *diagnosticLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.Buffer.Write(p)
	if b.tlsLogged != nil && bytes.Contains(p[:n], []byte("TLS handshake error")) {
		b.tlsOnce.Do(func() { close(b.tlsLogged) })
	}
	return n, err
}
func (b *diagnosticLogBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

type diagnosticMirror struct {
	http.ResponseWriter
	body bytes.Buffer
}

func (w *diagnosticMirror) Write(p []byte) (int, error) {
	_, _ = w.body.Write(p)
	return w.ResponseWriter.Write(p)
}

func diagnosticScan(t *testing.T, label, text string, canaries []string) {
	t.Helper()
	for i, canary := range canaries {
		encoded, _ := json.Marshal(canary)
		escaped := string(encoded[1 : len(encoded)-1])
		if canary != "" && (strings.Contains(text, canary) || strings.Contains(text, escaped)) {
			t.Fatalf("%s leaked protected canary index %d", label, i)
		}
	}
}

func diagnosticMetricContract(t *testing.T, body []byte) {
	t.Helper()
	allowed := map[string]string{
		"pairroom_http_requests_active": "", "pairroom_http_request_limit": "", "pairroom_http_requests_rejected_total": "", "pairroom_resident_rooms": "",
		"pairroom_replays_active": "", "pairroom_replay_limit": "", "pairroom_connection_limit": "",
		"pairroom_http_requests_completed_total": "status_class", "pairroom_http_request_duration_seconds_bucket": "le",
		"pairroom_http_request_duration_seconds_count": "", "pairroom_http_request_duration_seconds_sum": "",
	}
	status := map[string]bool{"0": false, "1": false, "2": false, "3": false, "4": false, "5": false}
	buckets := map[string]bool{"0.01": false, "0.05": false, "0.1": false, "0.5": false, "1": false, "5": false, "30": false, "+Inf": false}
	seen := map[string]bool{}
	pattern := regexp.MustCompile(`^([a-z_]+)(?:\{([a-z_]+)="([^"]+)"\})? ([0-9]+(?:\.[0-9]+)?(?:e[+-]?[0-9]+)?)$`)
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		match := pattern.FindStringSubmatch(line)
		if match == nil {
			t.Fatal("metric sample is not in closed scalar/one-label syntax")
		}
		want, exists := allowed[match[1]]
		if !exists || match[2] != want {
			t.Fatal("metric introduced dynamic name or label key")
		}
		seen[match[1]] = true
		if want == "status_class" {
			if _, ok := status[match[3]]; !ok || status[match[3]] {
				t.Fatal("metric status label is not fixed unique 0..5")
			}
			status[match[3]] = true
		}
		if want == "le" {
			if _, ok := buckets[match[3]]; !ok || buckets[match[3]] {
				t.Fatal("metric duration label is not a fixed bucket")
			}
			buckets[match[3]] = true
		}
	}
	if len(seen) != len(allowed) {
		t.Fatal("closed metric set incomplete")
	}
	for _, present := range status {
		if !present {
			t.Fatal("fixed status class missing")
		}
	}
	for _, present := range buckets {
		if !present {
			t.Fatal("fixed bucket missing")
		}
	}
}

func TestDiagnosticRedactionHTTPPGCanaryDataFaultsAndRealServerLogs(t *testing.T) {
	f := newObjectHTTPFixture(t)
	serverLogs := &diagnosticLogBuffer{tlsLogged: make(chan struct{})}
	// Capture the actual standard net/http logger used by the Service; no
	// product logger or handler behavior is replaced to manufacture redaction.
	f.server.Config.ErrorLog = log.New(serverLogs, "", 0)
	var tokensMu sync.Mutex
	var identityTokens []string
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
				tokensMu.Lock()
				identityTokens = append(identityTokens, reply.Access, reply.ID)
				tokensMu.Unlock()
			}
		}
	})
	owner, operator := f.browser("member-one"), f.browser("operator")
	f.login(owner)
	f.login(operator)
	o, a := f.session(owner), f.session(operator)
	const room = "ops08-room-id-canary"
	const clientID = "ops08-client-id-canary"
	const bodyCanary = "OPS08_PRIVATE_BODY_CANARY"
	const nativeCanary = "OPS08_NATIVE_SESSION_CANARY"
	const pathCanary = `C:\OPS08_PRIVATE_ENDPOINT_PATH\session`
	const secretCanary = "OPS08_PRIVATE_SECRET_CANARY"
	f.createRoom(owner, o.CSRF, room)
	text := bodyCanary + " " + nativeCanary + " " + pathCanary
	status, body := f.post(owner, "/enterprise/api/v1/rooms/"+room+"/messages", map[string]any{"client_id": clientID, "to": "slot1", "text": text}, o.CSRF)
	if status != 200 || !bytes.Contains(body, []byte(bodyCanary)) {
		t.Fatal("authorized business content canary was not actually published")
	}
	status, body = f.get(owner, "/enterprise/api/v1/rooms/"+room)
	if status != 200 || !bytes.Contains(body, []byte(bodyCanary)) {
		t.Fatal("authorized history positive control did not contain its deliberate content")
	}
	native := grantsPGNative(t, f)
	device := grantsPGEnroll(t, f, native, owner, o.CSRF, "ops08-real-device")
	tokensMu.Lock()
	captured := append([]string{}, identityTokens...)
	tokensMu.Unlock()
	if len(captured) < 4 {
		t.Fatal("real signed identity-token positive control absent")
	}
	canaries := append([]string{bodyCanary, nativeCanary, pathCanary, secretCanary, room, clientID, o.UserID, a.UserID, f.cfg.DeploymentID, f.cookie(owner).Value, f.cookie(operator).Value, o.CSRF, a.CSRF, device.access, device.refresh}, captured...)
	scopeCanaries := make([]string, 0, len(canaries))
	for _, canary := range canaries {
		if canary != o.UserID && canary != a.UserID {
			scopeCanaries = append(scopeCanaries, canary)
		}
	}
	publicScopes := map[*http.Client][2]string{}
	for _, client := range []*http.Client{owner, operator} {
		issued, err := f.store.AuthenticateSession(t.Context(), tokenHash(f.cookie(client).Value))
		if err != nil {
			t.Fatal("issued public scope reference unavailable")
		}
		publicScopes[client] = [2]string{issued.Auth.UserID, issued.Auth.SessionID}
	}
	read := func(client *http.Client, path string, want int) []byte {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, f.origin+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("diagnostic HTTP request failed")
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil || response.StatusCode != want {
			t.Fatalf("diagnostic response %s: status=%d", path, response.StatusCode)
		}
		diagnosticScan(t, path, string(data), canaries)
		// Existing X-PairRoom-User/Session are authentication scope references,
		// not metrics labels or native identity. Issued bearer/cookie/proof
		// values must nevertheless never appear on these diagnostic responses.
		for name, values := range response.Header {
			if name == "X-Pairroom-User" || name == "X-Pairroom-Session" {
				diagnosticScan(t, path+" public scope header "+name, strings.Join(values, "\n"), scopeCanaries)
				index := 0
				if name == "X-Pairroom-Session" {
					index = 1
				}
				if len(values) != 1 || values[0] != publicScopes[client][index] {
					t.Fatal("diagnostic scope header does not match exact issued public reference")
				}
			} else {
				diagnosticScan(t, path+" header "+name, strings.Join(values, "\n"), canaries)
			}
		}
		if len(response.Cookies()) != 0 {
			t.Fatal("diagnostics unexpectedly issued credential cookies")
		}
		return data
	}
	metrics := read(operator, administrationPrefix+"metrics", 200)
	diagnosticMetricContract(t, metrics)
	diagnostics := read(operator, administrationPrefix+"diagnostics", 200)
	var value map[string]json.RawMessage
	if json.Unmarshal(diagnostics, &value) != nil {
		t.Fatal("diagnostic JSON invalid")
	}
	keys := []string{}
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	wantKeys := []string{"concurrent_replay_limit", "concurrent_request_limit", "connection_limit", "draining", "request_duration_buckets", "replays_active", "request_duration_nanoseconds", "requests_active", "requests_completed_by_status_class", "requests_rejected", "resident_rooms", "version"}
	sort.Strings(wantKeys)
	if strings.Join(keys, ",") != strings.Join(wantKeys, ",") {
		t.Fatal("diagnostics escaped closed aggregate key set")
	}
	read(owner, administrationPrefix+"metrics", 403)
	read(owner, administrationPrefix+"diagnostics", 403)
	read(native, administrationPrefix+"metrics", 401)
	read(operator, "/enterprise/health/live", 200)
	read(operator, "/enterprise/health/ready", 200)
	for _, absent := range []struct {
		path   string
		status int
	}{{"/enterprise/api/v1/administration/trace", 400}, {"/enterprise/api/v1/administration/diagnostics/download", 400}, {"/enterprise/debug/pprof", 404}} {
		read(operator, absent.path, absent.status)
	}
	status, body = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/messages", map[string]any{"client_id": "bad-private-native-input", "to": "slot1", "text": bodyCanary, "native_session": nativeCanary, "workspace": pathCanary, "token": secretCanary}, o.CSRF)
	if status != 400 {
		t.Fatal("raw native/secret extra fields were not rejected")
	}
	diagnosticScan(t, "invalid native shape", string(body), canaries)
	var before [4]int64
	counts := func() [4]int64 {
		t.Helper()
		var out [4]int64
		if err := f.admin.QueryRow(t.Context(), `SELECT current_seq,revision,(SELECT count(*) FROM operations),(SELECT count(*) FROM audit) FROM rooms WHERE room_id=$1`, room).Scan(&out[0], &out[1], &out[2], &out[3]); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before = counts()
	// Force a real PostgreSQL error carrying every private category in its
	// MESSAGE/DETAIL, rather than only supplying a fake Go error to the adapter.
	function := `CREATE FUNCTION ops08_private_fault() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN RAISE EXCEPTION USING ERRCODE='23514', MESSAGE='` + bodyCanary + ` ` + nativeCanary + `', DETAIL='` + secretCanary + ` ` + pathCanary + `'; END $body$`
	if _, err := f.admin.Exec(t.Context(), function); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(t.Context(), `CREATE TRIGGER ops08_private_fault BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION ops08_private_fault()`); err != nil {
		t.Fatal(err)
	}
	_, faultErr := f.admin.Exec(t.Context(), `INSERT INTO audit(deployment_id,room_id,user_id,actor_kind,kind,outcome) VALUES($1,$2,$3,'user','ops08-trigger-positive','accepted')`, f.cfg.DeploymentID, room, o.UserID)
	var pgFault *pgconn.PgError
	if !errors.As(faultErr, &pgFault) || !strings.Contains(pgFault.Message, bodyCanary) || !strings.Contains(pgFault.Message, nativeCanary) || !strings.Contains(pgFault.Detail, secretCanary) || !strings.Contains(pgFault.Detail, pathCanary) {
		t.Fatal("real PostgreSQL canary MESSAGE/DETAIL positive control absent")
	}
	status, body = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/messages", map[string]any{"client_id": "failed-canary-mutation", "to": "slot1", "text": text}, o.CSRF)
	diagnosticScan(t, "PG fault wire", string(body), canaries)
	if status != 409 || !bytes.Contains(body, []byte(`"constraint_violation"`)) || counts() != before {
		t.Fatal("real dependency fault was not generic definite rollback")
	}
	read(operator, administrationPrefix+"metrics", 200)
	read(operator, administrationPrefix+"diagnostics", 200)
	f.idp.online.Store(false)
	f.provider.Close()
	f.installHandler(f.store)
	if f.provider.Ready() {
		t.Fatal("IdP outage did not reach actual Service provider-unavailable state")
	}
	response, err := operator.Get(f.origin + "/enterprise/auth/login")
	if err != nil {
		t.Fatal("IdP fault request failed")
	}
	fault, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != 503 || !bytes.Contains(fault, []byte("login_unavailable")) {
		t.Fatal("offline IdP did not expose a degraded generic outcome")
	}
	diagnosticScan(t, "IdP offline wire", string(fault), canaries)
	read(operator, administrationPrefix+"metrics", 200)
	read(operator, administrationPrefix+"diagnostics", 200)
	f.idp.online.Store(true)
	// Plain HTTP to the real TLS listener produces an actual nonempty standard
	// Server ErrorLog without invoking any panic or logging private request data.
	origin, _ := url.Parse(f.origin)
	conn, err := net.DialTimeout("tcp", origin.Host, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = fmt.Fprintf(conn, "GET /%s/%s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nX-Native-Session: %s\r\n\r\n", bodyCanary, room, secretCanary, nativeCanary)
	_, _ = io.ReadAll(conn)
	_ = conn.Close()
	select {
	case <-serverLogs.tlsLogged:
	case <-time.After(2 * time.Second):
		t.Fatal("actual standard Server ErrorLog TLS event did not arrive")
	case <-t.Context().Done():
		t.Fatal("test context ended before actual standard Server ErrorLog TLS event")
	}
	logs := serverLogs.text()
	if !strings.Contains(logs, "TLS handshake error") {
		t.Fatal("actual standard Server ErrorLog positive control was empty")
	}
	diagnosticScan(t, "actual standard Server ErrorLog", logs, canaries)
	var pid uint32
	if err = f.admin.QueryRow(t.Context(), `SELECT lease_pid FROM ownership WHERE id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = f.admin.Exec(t.Context(), `SELECT pg_terminate_backend($1,5000)`, int32(pid)); err != nil {
		t.Fatal(err)
	}
	read(operator, "/enterprise/health/ready", 503)
	read(operator, "/enterprise/health/live", 200)
	read(operator, administrationPrefix+"metrics", 503)
	read(operator, administrationPrefix+"diagnostics", 503)
	diagnosticScan(t, "final actual standard Server ErrorLog", serverLogs.text(), canaries)
	t.Logf("positive controls: real identity tokens=%d; protected values checked=%d; actual standard Server ErrorLog bytes=%d; closed metric names=11/status labels=6/duration labels=8; diagnostic keys=12", len(captured), len(canaries), len(serverLogs.text()))
}
