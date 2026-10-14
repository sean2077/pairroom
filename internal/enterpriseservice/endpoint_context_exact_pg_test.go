package enterpriseservice

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/enterpriseclient"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

// A hardlinked test binary intercepts the platform's real browser-launch
// executable lookup. It cannot open a browser or run the suite recursively.
func init() {
	marker := os.Getenv("PAIRROOM_EXACT_BROWSER_SENTINEL")
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	if marker != "" && (name == "rundll32" || name == "open" || name == "xdg-open") {
		_ = os.WriteFile(marker, []byte("unexpected browser launch"), 0600)
		os.Exit(4)
	}
}

type exactSessionConfig struct {
	Endpoint                  endpointProcessConfig
	BrowserBin, BrowserMarker string
}
type exactSessionRequest struct {
	Args []string
	Hook string
	Exit bool
}
type exactSessionReply struct {
	PID           int
	Output, Error string
	ReadCalls     int
	EOF           bool
	ElapsedMS     int64
}
type exactHookInput struct {
	reader *strings.Reader
	calls  int
	eof    bool
}

func (r *exactHookInput) Read(p []byte) (int, error) {
	r.calls++
	n, e := r.reader.Read(p)
	if e == io.EOF {
		r.eof = true
	}
	return n, e
}

// Real independent OS sessions execute the public CLI entrypoint under distinct
// official metadata fixtures. No vendor model is started or acceptance claimed.
func TestEndpointContextExactSessionProcessHelper(t *testing.T) {
	path := os.Getenv("PAIRROOM_EXACT_SESSION_CONFIG")
	if path == "" {
		return
	}
	var cfg exactSessionConfig
	if err := privatefile.ReadJSON(path, 64<<10, &cfg); err != nil {
		t.Fatal("private session config unavailable")
	}
	relayclient.IsolateNativeCaller(t)
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData", "USERPROFILE"} {
		t.Setenv(key, cfg.Endpoint.Home)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(cfg.Endpoint.Home, "claude"))
	t.Setenv("CLAUDE_CODE_SESSION_ID", cfg.Endpoint.Session)
	t.Setenv("PAIRROOM_EXACT_BROWSER_SENTINEL", cfg.BrowserMarker)
	t.Setenv("PATH", cfg.BrowserBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	encode := json.NewEncoder(os.Stdout)
	_ = encode.Encode(exactSessionReply{PID: os.Getpid()})
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), 2<<20)
	for scan.Scan() {
		var req exactSessionRequest
		if json.Unmarshal(scan.Bytes(), &req) != nil {
			t.Fatal("invalid private session command")
		}
		if req.Exit {
			return
		}
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		input := &exactHookInput{reader: strings.NewReader(req.Hook)}
		var out, diagnostic bytes.Buffer
		started := time.Now()
		err := relayclient.Run(ctx, req.Args, input, &out, &diagnostic)
		cancel()
		reply := exactSessionReply{PID: os.Getpid(), Output: out.String(), ReadCalls: input.calls, EOF: input.eof, ElapsedMS: time.Since(started).Milliseconds()}
		if err != nil {
			reply.Error = err.Error()
		}
		if err := encode.Encode(reply); err != nil {
			t.Fatal(err)
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
}

type exactSession struct {
	cfg      exactSessionConfig
	cmd      *exec.Cmd
	in       *json.Encoder
	lines    chan exactSessionReply
	done     chan error
	pid      int
	finished bool
}

func exactStartSession(t *testing.T, cfg exactSessionConfig) *exactSession {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "session-private")
	if err := privatefile.Mkdir(directory); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "session.json")
	if err := privatefile.WriteJSON(path, cfg); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestEndpointContextExactSessionProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "PAIRROOM_EXACT_SESSION_CONFIG="+path)
	cmd.Dir = cfg.Endpoint.Workspace
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	s := &exactSession{cfg: cfg, cmd: cmd, in: json.NewEncoder(in), lines: make(chan exactSessionReply, 8), done: make(chan error, 1)}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { s.done <- cmd.Wait() }()
	go func() {
		defer close(s.lines)
		scan := bufio.NewScanner(out)
		scan.Buffer(make([]byte, 4096), 2<<20)
		for scan.Scan() {
			var reply exactSessionReply
			if strings.HasPrefix(scan.Text(), "{") && json.Unmarshal(scan.Bytes(), &reply) == nil {
				s.lines <- reply
			}
		}
	}()
	t.Cleanup(func() {
		if !s.finished {
			_ = s.cmd.Process.Kill()
			<-s.done
			s.finished = true
		}
	})
	ready := s.next(t)
	s.pid = ready.PID
	if s.pid != cmd.Process.Pid {
		t.Fatal("session PID report did not match actual process")
	}
	return s
}
func (s *exactSession) next(t *testing.T) exactSessionReply {
	t.Helper()
	select {
	case reply, ok := <-s.lines:
		if !ok {
			t.Fatal("session exited without response")
		}
		return reply
	case <-time.After(12 * time.Second):
		t.Fatal("session command exceeded bounded budget")
		return exactSessionReply{}
	}
}
func (s *exactSession) run(t *testing.T, hook string, args ...string) exactSessionReply {
	t.Helper()
	if err := s.in.Encode(exactSessionRequest{Args: args, Hook: hook}); err != nil {
		t.Fatal(err)
	}
	return s.next(t)
}
func (s *exactSession) success(t *testing.T, args ...string) string {
	t.Helper()
	r := s.run(t, "", args...)
	if r.Error != "" {
		t.Fatalf("public session command %s failed: %s", args[0], r.Error)
	}
	return r.Output
}
func exactStop(e endpointProcessConfig, text string) string {
	data, _ := json.Marshal(map[string]any{"hook_event_name": "Stop", "session_id": e.Session, "cwd": e.Workspace, "last_assistant_message": text, "stop_hook_active": false})
	return string(data)
}

type exactEndpointFixture struct {
	f                                  *httpPGFixture
	owner                              *http.Client
	csrf, room, ca                     string
	drop                               atomic.Bool
	home, root, directory, marker, bin string
}

func newExactEndpointFixture(t *testing.T) *exactEndpointFixture {
	t.Helper()
	f := newHTTPPGFixture(t)
	x := &exactEndpointFixture{f: f, room: "exact-context-room", home: t.TempDir(), marker: filepath.Join(t.TempDir(), "browser-launched")}
	// Install a real transport interruption before any browser/endpoint request.
	// The replacement listener keeps the exact original HTTPS authority/cert.
	certs := f.server.TLS.Certificates
	f.server.Close()
	u, _ := url.Parse(f.origin)
	listener, err := net.Listen("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/relay") {
			data, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
			r.Body = io.NopCloser(bytes.NewReader(data))
			var command struct {
				Intent struct {
					Kind string `json:"kind"`
				} `json:"intent"`
			}
			_ = json.Unmarshal(data, &command)
			if command.Intent.Kind == "report" && x.drop.Swap(false) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
		}
		f.handler.Load().ServeHTTP(w, r)
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.TLS = &tls.Config{Certificates: certs, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	f.server = server
	t.Cleanup(server.Close)
	x.owner = f.browser("member-one")
	f.login(x.owner)
	x.csrf = f.session(x.owner).CSRF
	f.createRoom(x.owner, x.csrf, x.room)
	x.ca = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(x.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certs[0].Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	x.directory = endpointDeviceLogin(t, f, x.owner, x.csrf, x.ca)
	x.root, err = filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "init", "-q", x.root).Run(); err != nil {
		t.Fatal(err)
	}
	x.bin = t.TempDir()
	name := "xdg-open"
	if runtime.GOOS == "windows" {
		name = "rundll32.exe"
	} else if runtime.GOOS == "darwin" {
		name = "open"
	}
	if err := os.Link(os.Args[0], filepath.Join(x.bin, name)); err != nil {
		t.Fatal("browser sentinel hardlink unavailable", err)
	}
	return x
}
func (x *exactEndpointFixture) session(t *testing.T, id string) *exactSession {
	t.Helper()
	e := endpointProcessConfig{Mode: "endpoint", Origin: x.f.origin, Deployment: x.f.cfg.DeploymentID, CA: x.ca, Workspace: x.root, DeviceDirectory: x.directory, Home: x.home, Session: id}
	return exactStartSession(t, exactSessionConfig{Endpoint: e, BrowserBin: x.bin, BrowserMarker: x.marker})
}
func (x *exactEndpointFixture) bindArgs(s *exactSession, slot int, replace bool, id string) []string {
	e := s.cfg.Endpoint
	a := []string{"bind", "--repo", e.Workspace, "--runtime", "claude", "--enterprise-service", e.Origin, "--deployment", e.Deployment, "--enterprise-ca", e.CA, "--enterprise-state-dir", e.DeviceDirectory, "--room", x.room, "--slot", fmt.Sprint(slot), "--id", id}
	if replace {
		a = append(a, "--replace")
	}
	return a
}
func (x *exactEndpointFixture) bind(t *testing.T, s *exactSession, slot int, replace bool, id string) {
	t.Helper()
	args := x.bindArgs(s, slot, replace, id)
	var pending struct {
		Status  string `json:"status"`
		Request string `json:"request_id"`
	}
	if json.Unmarshal([]byte(s.success(t, args...)), &pending) != nil || pending.Status != "awaiting_owner" || pending.Request == "" {
		t.Fatal("CLI binding skipped original Owner approval")
	}
	status, _ := x.f.post(x.owner, "/enterprise/api/v1/rooms/"+x.room+"/binding-requests/"+pending.Request+"/decision", map[string]string{"client_id": "approve-" + pending.Request, "action": "approve"}, x.csrf)
	if status != 200 {
		t.Fatal("actual Owner approval failed")
	}
	s.success(t, args...)
}
func (x *exactEndpointFixture) state(t *testing.T, slot int) (string, relayclient.State) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(x.root, ".pairroom", "rooms", "*", "slots", fmt.Sprintf("slot%d", slot), "state.json"))
	if err != nil || len(paths) != 1 {
		t.Fatal("actual slot state path not unique", err)
	}
	var state relayclient.State
	if err := privatefile.ReadJSON(paths[0], 2<<20, &state); err != nil {
		t.Fatal(err)
	}
	return paths[0], state
}
func exactPrivateMap(t *testing.T, path string) map[string]any {
	t.Helper()
	var record map[string]any
	if err := privatefile.ReadJSON(path, 2<<20, &record); err != nil {
		t.Fatal("private journal unreadable")
	}
	return record
}
func (x *exactEndpointFixture) counts(t *testing.T) [5]int64 {
	t.Helper()
	var c [5]int64
	if err := x.f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM room_events),(SELECT count(*) FROM binding_requests),(SELECT count(*) FROM binding_operations),(SELECT count(*) FROM relay_operations),(SELECT count(*) FROM audit)`).Scan(&c[0], &c[1], &c[2], &c[3], &c[4]); err != nil {
		t.Fatal(err)
	}
	return c
}

// This explicitly authorized, unique-schema clock fixture is not a public
// expiry mutation API. The immutable guard remains installed; transactional
// DDL disables only its named trigger while one already-approved grant's expiry
// is aged, then restores it before commit. All other graph bytes are checked.
func exactExpiredGrantClockFixture(t *testing.T, x *exactEndpointFixture, grantID string) time.Time {
	t.Helper()
	var schema, enabled string
	if err := x.f.admin.QueryRow(t.Context(), `SELECT current_schema(),tgenabled::text FROM pg_trigger WHERE tgrelid='binding_grants'::regclass AND tgname='binding_grants_identity_immutable'`).Scan(&schema, &enabled); err != nil || schema != x.f.schema || !strings.HasPrefix(schema, "sqltest_") || enabled != "O" {
		t.Fatal("clock fixture did not select its unique owned enabled trigger", err)
	}
	fingerprint := func() [32]byte {
		t.Helper()
		var data bytes.Buffer
		for _, table := range []string{"users", "browser_sessions", "devices", "device_grants", "device_families", "agents", "bindings", "room_slots", "slot_generations", "room_members", "room_events", "relay_operations", "binding_operations", "control_operations", "audit", "binding_grants"} {
			var encoded []byte
			query := `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb) FROM ` + pgx.Identifier{table}.Sanitize() + ` t`
			var err error
			if table == "binding_grants" {
				query = `SELECT COALESCE(jsonb_agg(CASE WHEN grant_id=$1 THEN to_jsonb(t)-'expires_at' ELSE to_jsonb(t) END ORDER BY grant_id),'[]'::jsonb) FROM binding_grants t`
				err = x.f.admin.QueryRow(t.Context(), query, grantID).Scan(&encoded)
			} else {
				err = x.f.admin.QueryRow(t.Context(), query).Scan(&encoded)
			}
			if err != nil {
				t.Fatal("clock fixture graph fingerprint failed", err)
			}
			_, _ = data.WriteString(table)
			_, _ = data.Write(encoded)
		}
		return sha256.Sum256(data.Bytes())
	}
	before := fingerprint()
	tx, err := x.f.admin.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) // DDL restoration also rolls back on every failure.
	if _, err := tx.Exec(t.Context(), `ALTER TABLE binding_grants DISABLE TRIGGER binding_grants_identity_immutable`); err != nil {
		t.Fatal(err)
	}
	var expired time.Time
	var past, valid bool
	if err := tx.QueryRow(t.Context(), `UPDATE binding_grants SET expires_at=GREATEST(created_at+interval '1 microsecond',clock_timestamp()-interval '1 millisecond') WHERE grant_id=$1 RETURNING expires_at,expires_at<clock_timestamp(),expires_at>created_at`, grantID).Scan(&expired, &past, &valid); err != nil || !past || !valid {
		t.Fatal("single grant clock fixture did not create a valid past expiry", err)
	}
	if _, err := tx.Exec(t.Context(), `ALTER TABLE binding_grants ENABLE TRIGGER binding_grants_identity_immutable`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fingerprint() != before {
		t.Fatal("clock fixture changed identity/token/Binding/fact/receipt/audit bytes")
	}
	if err := x.f.admin.QueryRow(t.Context(), `SELECT tgenabled::text FROM pg_trigger WHERE tgrelid='binding_grants'::regclass AND tgname='binding_grants_identity_immutable'`).Scan(&enabled); err != nil || enabled != "O" {
		t.Fatal("clock fixture left immutability guard disabled", err)
	}
	t.Log("DEV09 isolated clock fixture: exact named trigger restored; only one approved grant expiry aged; all remaining graph hashes unchanged")
	return expired
}

func TestEndpointContextExactSameDeviceTwoLiveSessionsLateStopAndExpiry(t *testing.T) {
	x := newExactEndpointFixture(t)
	a, b := x.session(t, "exact-native-a"), x.session(t, "exact-native-b")
	if a.pid == b.pid || a.cfg.Endpoint.Home != b.cfg.Endpoint.Home || a.cfg.Endpoint.DeviceDirectory != b.cfg.Endpoint.DeviceDirectory || a.cfg.Endpoint.Workspace != b.cfg.Endpoint.Workspace {
		t.Fatal("fixture did not provide same-machine/same-Device distinct live OS sessions")
	}
	a.success(t, "install", "--repo", x.root, "--runtime", "claude")
	x.bind(t, a, 1, false, "initial-a")
	x.bind(t, b, 2, false, "initial-b")
	a.success(t, "park", "--repo", x.root, "--enabled=false")
	b.success(t, "park", "--repo", x.root, "--enabled=false")
	pathA, stateA := x.state(t, 1)
	pathB, stateB := x.state(t, 2)
	if stateA.SessionID == stateB.SessionID || stateA.BindID == stateB.BindID || pathA == pathB || stateA.Enterprise.DeviceID != stateB.Enterprise.DeviceID || stateA.Enterprise.RoomID != stateB.Enterprise.RoomID || stateA.Enterprise.RecordID == stateB.Enterprise.RecordID {
		t.Fatal("same Device sessions mixed slot identity/recovery paths")
	}
	var deviceCount, agentCount, bindingCount int
	if err := x.f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM devices),(SELECT count(*) FROM agents),(SELECT count(*) FROM bindings WHERE retired_at IS NULL)`).Scan(&deviceCount, &agentCount, &bindingCount); err != nil || deviceCount != 1 || agentCount != 2 || bindingCount != 2 {
		t.Fatal("DEV07 fixture is not exactly one Device/two real session Bindings", err)
	}
	a.success(t, "status")
	b.success(t, "status")
	untouched, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatal(err)
	}
	const privateBody = "@agent2 PRIVATE_LATE_CONTEXT_BODY exact unforwarded Stop"
	x.drop.Store(true)
	stopped := a.run(t, exactStop(a.cfg.Endpoint, privateBody), "hook", "--runtime", "claude")
	if strings.Contains(stopped.Output, privateBody) {
		t.Fatal("uncertain Stop leaked private reply through hook stdout")
	}
	_, pending := x.state(t, 1)
	if pending.Pending == nil || pending.Pending.Text != privateBody || !pending.Pending.Unknown || pending.Pending.ExpectedPeerGeneration != 1 {
		t.Fatal("real interrupted Stop did not create original private WAL")
	}
	stillB, err := os.ReadFile(pathB)
	if err != nil || !bytes.Equal(stillB, untouched) {
		t.Fatal("one native session rewrote the other slot recovery file")
	}
	replacement := x.session(t, "exact-native-replacement")
	x.bind(t, replacement, 2, true, "replacement-b")
	_, fresh := x.state(t, 2)
	if fresh.Generation != 2 {
		t.Fatal("correct actual replacement did not advance generation")
	}
	r := a.run(t, "", "reconcile", "--repo", x.root)
	if strings.Contains(r.Output, privateBody) {
		t.Fatal("reconciliation disclosed private automatic body")
	}
	_, originalWAL := x.state(t, 1)
	originalRecord := exactPrivateMap(t, filepath.Join(x.directory, "bindings", stateA.Enterprise.RecordID, "binding.json"))
	originalGrant := originalRecord["grant"].(map[string]any)
	proofRequest := grantsPGRelayRequest{ClientID: "verify-original-report", BindingID: stateA.BindID, Generation: stateA.Generation, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationReport, ReportSeq: originalWAL.Pending.Seq, Text: privateBody, ExpectedPeerGeneration: 1}}
	verifyStatus, verifyBody := grantsPGCall(t, x.f, grantsPGNative(t, x.f), http.MethodPost, "/enterprise/device/v1/rooms/"+x.room+"/relay/resolve", originalGrant["token"].(string), proofRequest)
	if verifyStatus != http.StatusConflict || !bytes.Contains(verifyBody, []byte(`"outcome":"not_committed"`)) {
		var diagnostic struct {
			Outcome string `json:"outcome"`
			Class   string `json:"class"`
		}
		_ = json.Unmarshal(verifyBody, &diagnostic)
		t.Fatalf("public original report absence: status=%d outcome=%q class=%q", verifyStatus, diagnostic.Outcome, diagnostic.Class)
	}
	r = a.run(t, "", "reconcile", "--repo", x.root, "--resend")
	if strings.Contains(r.Output, privateBody) {
		t.Fatal("explicit original-sequence recovery disclosed private body")
	}
	assertPinned := func() {
		t.Helper()
		_, s := x.state(t, 1)
		if s.AutomaticPeerGeneration != 1 || !s.AutomaticPaused || s.Pending == nil || s.Pending.Text != privateBody || s.Pending.ExpectedPeerGeneration != 1 || s.Pending.Outcome != relay.PeerChangedCode || s.Pending.Unknown {
			outcome := "missing"
			expected := uint64(0)
			unknown := false
			bodyMatches := false
			if s.Pending != nil {
				outcome = s.Pending.Outcome
				expected = s.Pending.ExpectedPeerGeneration
				unknown = s.Pending.Unknown
				bodyMatches = s.Pending.Text == privateBody
			}
			t.Fatalf("late WAL gate: automatic=%d paused=%t outcome=%q expected=%d unknown=%t bodyMatches=%t reconcileError=%q", s.AutomaticPeerGeneration, s.AutomaticPaused, outcome, expected, unknown, bodyMatches, r.Error)
		}
		var encoded []byte
		if err := x.f.admin.QueryRow(t.Context(), `SELECT result_encoding FROM relay_operations WHERE room_id=$1 AND op_kind='report' AND binding_id=$2`, x.room, s.BindID).Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var result relay.EnterpriseOperationResult
		if json.Unmarshal(encoded, &result) != nil || result.Publication == nil || result.Publication.Outcome != relay.PeerChangedCode || result.Publication.Message != nil {
			t.Fatal("late Stop lacked accepted terminal peer_changed receipt")
		}
		var leaked int
		if err := x.f.admin.QueryRow(t.Context(), `SELECT count(*) FROM room_events WHERE room_id=$1 AND convert_from(event_encoding,'UTF8') LIKE $2`, x.room, "%PRIVATE_LATE_CONTEXT_BODY%").Scan(&leaked); err != nil || leaked != 0 {
			t.Fatal("private automatic body reached Room or successor", err)
		}
	}
	assertPinned()
	a.success(t, x.bindArgs(a, 1, false, "same-session-resume")...)
	assertPinned()
	// Fresh signed browser login and the supported SDK already-live login path
	// are explicit observations; do not force logout or fresh Device approval.
	x.f.login(x.owner)
	x.csrf = x.f.session(x.owner).CSRF
	sdk, err := enterpriseclient.New(enterpriseclient.Config{Origin: x.f.origin, DeploymentID: x.f.cfg.DeploymentID, Directory: x.directory, CAFile: x.ca, OpenBrowser: func(string) error { t.Fatal("already-live login attempted browser"); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sdk.Login(t.Context(), "same Device login", false)
	sdk.Close()
	if !errors.Is(err, enterpriseclient.ErrAlreadyLoggedIn) {
		t.Fatal("supported live Device login boundary changed", err)
	}
	assertPinned()
	a.success(t, "peer", "--repo", x.root)
	assertPinned()
	a.success(t, "send", "--repo", x.root, "--id", "one-explicit-new-peer", "--peer-generation", "2", "--text", "EXPLICIT_FRESH_CONTEXT_BODY deliberate")
	assertPinned()
	lateAgain := a.run(t, exactStop(a.cfg.Endpoint, "@agent2 PRIVATE_LATE_SECOND_BODY"), "hook", "--runtime", "claude")
	if strings.Contains(lateAgain.Output, "PRIVATE_LATE_SECOND_BODY") {
		t.Fatal("later hook forwarded private stale automatic text")
	}
	assertPinned()
	// A fresh stale request, not an accepted-operation replay, observes head 1
	// after the correct replacement established head 2.
	device := exactPrivateMap(t, filepath.Join(x.directory, "device.json"))
	active := device["active"].(map[string]any)
	native := grantsPGNative(t, x.f)
	source := enterpriseFixtureHash("stale-agent-source")
	status, data := grantsPGCall(t, x.f, native, http.MethodPost, "/enterprise/device/v1/agents", active["access"].(string), map[string]string{"client_id": "new-stale-agent", "source_key": source, "runtime": "claude"})
	if status != 200 {
		t.Fatal("fresh stale candidate Agent registration failed")
	}
	var agent struct {
		ID string `json:"agent_id"`
	}
	if json.Unmarshal(data, &agent) != nil {
		t.Fatal("Agent receipt invalid")
	}
	proof := enterpriseFixtureHash("stale-proof")
	token := enterpriseFixtureHash("stale-token")
	stale := bindingRequestBody{ClientID: "new-stale-observation", RequestID: "new-stale-request", AgentID: agent.ID, BindingID: "new-stale-binding", Slot: model.ActorSlot2, ObservedGeneration: 1, Replace: true, ProofHash: grantsPGHash(proof)}
	stale.Grant.GrantID = "new-stale-grant"
	stale.Grant.TokenHash = grantsPGHash(token)
	before := x.counts(t)
	status, data = grantsPGCall(t, x.f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+x.room+"/binding-requests", active["access"].(string), stale)
	if status < 400 || !bytes.Contains(data, []byte("invalid_control_state")) || x.counts(t) != before {
		t.Fatal("new stale observedGeneration request committed or was not explicitly refused")
	}
	// An actual expired active Binding grant is a clock-boundary fixture; no
	// authorization or business fact is fabricated. Its original WAL is kept.
	recordPath := filepath.Join(x.directory, "bindings", stateA.Enterprise.RecordID, "binding.json")
	record := exactPrivateMap(t, recordPath)
	grant := record["grant"].(map[string]any)
	activeStatus, _ := grantsPGCall(t, x.f, native, http.MethodGet, "/enterprise/device/v1/rooms/"+x.room+"/relay/context", grant["token"].(string), nil)
	if activeStatus != 200 {
		t.Fatal("original same bound grant was not active before clock fixture")
	}
	expired := exactExpiredGrantClockFixture(t, x, grant["id"].(string))
	expiredStatus, _ := grantsPGCall(t, x.f, native, http.MethodGet, "/enterprise/device/v1/rooms/"+x.room+"/relay/context", grant["token"].(string), nil)
	if expiredStatus < 400 {
		t.Fatal("actual same expired bound grant still authenticated")
	}
	grant["expires_at"] = expired
	if err := privatefile.WriteJSON(recordPath, record); err != nil {
		t.Fatal(err)
	}
	beforeState, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	const expiredBody = "@agent2 AFTER_EXPIRED_GRANT_DO_NOT_SEND"
	beforeEffects := x.counts(t)
	hookStarted := time.Now().UTC()
	expiredHook := a.run(t, exactStop(a.cfg.Endpoint, expiredBody), "hook", "--runtime", "claude")
	hookEnded := time.Now().UTC()
	if expiredHook.ElapsedMS >= 5000 || !expiredHook.EOF || strings.Contains(expiredHook.Output, "AFTER_EXPIRED_GRANT_DO_NOT_SEND") {
		t.Fatal("expired hook blocked finite stdin or emitted a private body")
	}
	if x.counts(t) != beforeEffects {
		t.Fatal("expired hook produced a Room, Binding, relay or audit effect")
	}
	afterState, err := os.ReadFile(pathA)
	var originalState, finalState relayclient.State
	var originalFields, finalFields map[string]any
	if err != nil || json.Unmarshal(beforeState, &originalState) != nil || json.Unmarshal(afterState, &finalState) != nil || json.Unmarshal(beforeState, &originalFields) != nil || json.Unmarshal(afterState, &finalFields) != nil {
		t.Fatal("expired hook state journal unreadable", err)
	}
	exactRetainExpiredHookEvidence(t, beforeState, afterState, originalState, finalState)
	// Stop saves its reply WAL before any HTTP (hook.go reservePublication), so
	// an expired grant cannot lose it. Behind the unresolved original head the
	// reply is held in order under the next sequence and the original automatic
	// context. Everything else in the journal, including the original head,
	// every earlier held reply, identity and context fence, is unchanged.
	for _, key := range []string{"held", "last_seq", "last_hook_at", "blocks"} {
		delete(originalFields, key)
		delete(finalFields, key)
	}
	if !reflect.DeepEqual(originalFields, finalFields) || finalState.Blocks != 0 {
		t.Fatal("expired hook changed journal identity, original head or context fence")
	}
	wantOutcome := ""
	if originalState.AutomaticScopeChanged {
		wantOutcome = "context_changed"
	}
	if originalState.Pending == nil || len(finalState.Held) != len(originalState.Held)+1 || !reflect.DeepEqual(originalState.Held, finalState.Held[:len(originalState.Held)]) || finalState.LastSeq != originalState.LastSeq+1 {
		t.Fatalf("expired hook did not append exactly one held reply behind the original prefix: originalHeld=%d finalHeld=%d originalSeq=%d finalSeq=%d", len(originalState.Held), len(finalState.Held), originalState.LastSeq, finalState.LastSeq)
	}
	added := finalState.Held[len(finalState.Held)-1]
	if added.Seq != finalState.LastSeq || added.Text != expiredBody || added.Unknown || added.ExpectedPeerGeneration != originalState.AutomaticPeerGeneration || added.Outcome != wantOutcome || added.At.Before(hookStarted.Add(-time.Second)) || added.At.After(hookEnded.Add(time.Second)) {
		t.Fatal("expired hook held reply is not this Stop body under the original automatic context")
	}
	if _, err := os.Stat(x.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("hook launched a browser")
	}
	unbound := x.session(t, "never-bound-official-session")
	unboundHook := unbound.run(t, exactStop(unbound.cfg.Endpoint, "@agent2 UNBOUND_BODY"), "hook", "--runtime", "claude")
	if unboundHook.Error != "" || strings.TrimSpace(unboundHook.Output) != "{}" || !unboundHook.EOF {
		t.Fatal("unbound hook was not inert and bounded")
	}
	t.Logf("DEV07/09 + ROOM04/05/08: same Device/Room live OS session PIDs %d/%d, independent journals; late actual Stop/WAL pinned after replacement/rebind/login/peer/explicit send; expired hook finite stdin and launcher0", a.pid, b.pid)
}

// Generate deterministic fixture secrets without putting any credential in argv.
// exactRetainExpiredHookEvidence keeps the actual before/after journals only in
// an explicitly selected owner-private directory and logs a body-free key diff.
func exactRetainExpiredHookEvidence(t *testing.T, before, after []byte, original, final relayclient.State) {
	t.Helper()
	var originalFields, finalFields map[string]json.RawMessage
	if json.Unmarshal(before, &originalFields) != nil || json.Unmarshal(after, &finalFields) != nil {
		t.Fatal("expired hook journal is not an object")
	}
	var changed []string
	for key, value := range finalFields {
		if !bytes.Equal(originalFields[key], value) {
			changed = append(changed, key)
		}
	}
	for key := range originalFields {
		if _, ok := finalFields[key]; !ok {
			changed = append(changed, key)
		}
	}
	slices.Sort(changed)
	t.Logf("DEV09 journal safe diff: changed=%v pendingEqual=%t held=%d->%d lastSeq=%d->%d lastConfirmed=%d->%d automatic=%d->%d paused=%t->%t", changed, reflect.DeepEqual(original.Pending, final.Pending), len(original.Held), len(final.Held), original.LastSeq, final.LastSeq, original.LastConfirmedSeq, final.LastConfirmedSeq, original.AutomaticPeerGeneration, final.AutomaticPeerGeneration, original.AutomaticPaused, final.AutomaticPaused)
	dir := os.Getenv("PAIRROOM_ENDPOINT_EXACT_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	if err := privatefile.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"dev09-before-state.json": before, "dev09-after-state.json": after} {
		if err := privatefile.WriteJSON(filepath.Join(dir, name), json.RawMessage(data)); err != nil {
			t.Fatal(err)
		}
	}
}

func enterpriseFixtureHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestEndpointContextExactLiveOtherCallbackCannotExchangeOriginalCode(t *testing.T) {
	f := newHTTPPGFixture(t)
	owner := f.browser("member-one")
	f.login(owner)
	csrf := f.session(owner).CSRF
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.TLS.Certificates[0].Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	locationA := make(chan string, 1)
	approve := func(verification string) (string, error) {
		id := strings.TrimPrefix(verification, f.origin+"/enterprise/device-enrollments/")
		status, data := f.post(owner, "/enterprise/api/v1/device-enrollments/"+id+"/decision", map[string]string{"client_id": "approve-" + id, "action": "approve"}, csrf)
		if status != 200 {
			return "", fmt.Errorf("approval status %d", status)
		}
		var reply struct {
			Location string `json:"completion_location"`
		}
		if json.Unmarshal(data, &reply) != nil {
			return "", errors.New("approval receipt invalid")
		}
		return reply.Location, nil
	}
	a, err := enterpriseclient.New(enterpriseclient.Config{Origin: f.origin, DeploymentID: f.cfg.DeploymentID, Directory: filepath.Join(t.TempDir(), "device-a"), CAFile: ca, OpenBrowser: func(v string) error {
		location, err := approve(v)
		if err == nil {
			locationA <- location
		}
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	doneA := make(chan error, 1)
	go func() { _, err := a.Login(ctx, "original endpoint A", false); doneA <- err }()
	var original string
	select {
	case original = <-locationA:
	case <-ctx.Done():
		t.Fatal("original live callback not ready")
	}
	originalURL, _ := url.Parse(original)
	codeA, stateA := originalURL.Query().Get("completion_code"), originalURL.Query().Get("state")
	var callbackB string
	b, err := enterpriseclient.New(enterpriseclient.Config{Origin: f.origin, DeploymentID: f.cfg.DeploymentID, Directory: filepath.Join(t.TempDir(), "device-b"), CAFile: ca, OpenBrowser: func(v string) error {
		location, err := approve(v)
		if err != nil {
			return err
		}
		callbackB = location
		other, _ := url.Parse(location)
		if other.Host == originalURL.Host {
			return errors.New("callbacks did not use distinct live loopback ports")
		}
		wrong := *other
		q := wrong.Query()
		q.Set("state", stateA)
		q.Set("completion_code", codeA)
		wrong.RawQuery = q.Encode()
		response, err := http.Get(wrong.String())
		if err != nil {
			return err
		}
		_ = response.Body.Close()
		if response.StatusCode != 400 {
			return errors.New("other live listener accepted wrong original nonce")
		}
		intercept := *other
		q = intercept.Query()
		q.Set("completion_code", codeA)
		intercept.RawQuery = q.Encode()
		go func() {
			response, err := http.Get(intercept.String())
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		}()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, err = b.Login(ctx, "other endpoint B", false)
	var failure *enterpriseclient.Error
	if !errors.As(err, &failure) || failure.Class != "authorization" || callbackB == "" {
		t.Fatal("other callback's own request/proof/candidate exchanged original code", err)
	}
	var grants int
	if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM device_grants`).Scan(&grants); err != nil || grants != 0 {
		t.Fatal("intercepted completion issued a long-term grant", err)
	}
	pendingB := exactPrivateMap(t, filepath.Join(b.Directory(), "device.json"))["pending"].(map[string]any)
	requestA := exactPrivateMap(t, filepath.Join(a.Directory(), "device.json"))["pending"].(map[string]any)
	status, _ := grantsPGCall(t, f, grantsPGNative(t, f), http.MethodPost, "/enterprise/device/v1/enrollments/"+requestA["request_id"].(string)+"/complete", "", deviceCompleteRequest{ClientID: "cross-request-proof", ClientProof: privateDeviceInput(pendingB["proof"].(string)), CompletionCode: privateDeviceInput(codeA)})
	if status < 400 {
		t.Fatal("other proof substituted for original endpoint proof")
	}
	response, err := http.Get(original)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("original callback did not remain legitimate")
	}
	select {
	case err := <-doneA:
		if err != nil {
			t.Fatal("original legitimate completion failed", err)
		}
	case <-ctx.Done():
		t.Fatal("original completion exceeded bound")
	}
	if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM device_grants`).Scan(&grants); err != nil || grants != 1 {
		t.Fatal("original completion did not issue exactly its own grant", err)
	}
	privateA := exactPrivateMap(t, filepath.Join(a.Directory(), "device.json"))
	access := privateA["active"].(map[string]any)["access"].(string)
	if bytes.Contains(body, []byte(access)) {
		t.Fatal("callback page exposed long-term token")
	}
	t.Log("DEV03: two actual live numeric-loopback ports; wrong nonce rejected, B-own proof/candidate + A-code denied, original A callback/proof completed once")
}

func TestEndpointContextExactSameIDChangedVerifiedAttachmentIDsConflictsWithoutEffect(t *testing.T) {
	f := newObjectHTTPFixture(t)
	owner := f.browser("member-one")
	f.login(owner)
	session := f.session(owner)
	const room = "exact-attachment-conflict"
	f.createRoom(owner, session.CSRF, room)
	native := grantsPGNative(t, f)
	device := grantsPGEnroll(t, f, native, owner, session.CSRF, "attachment-owner")
	binding := grantsPGBind(t, f, native, owner, session.CSRF, room, device, model.ActorSlot1, "attachment-source")
	dataA, dataB := []byte("verified attachment A"), []byte("verified attachment B")
	a := objectHTTPFinish(t, f, native, room, "verify-a", objectHTTPReserve(t, f, native, room, "reserve-a", dataA, binding.token, ""), dataA, binding.token, "")
	b := objectHTTPFinish(t, f, native, room, "verify-b", objectHTTPReserve(t, f, native, room, "reserve-b", dataB, binding.token, ""), dataB, binding.token, "")
	idsA, _ := model.NewEnterpriseAttachmentIDs([]string{a.ID})
	idsB, _ := model.NewEnterpriseAttachmentIDs([]string{b.ID})
	command := grantsPGRelayRequest{ClientID: "same-id-attachments", BindingID: binding.bindingID, Generation: binding.generation, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: model.ActorUser, Text: "same immutable body", AttachmentIDs: idsA}}
	path := "/enterprise/device/v1/rooms/" + room + "/relay"
	status, data := grantsPGCall(t, f, native, http.MethodPost, path, binding.token, command)
	if status != 200 {
		t.Fatal("original attachment publication failed")
	}
	var original struct {
		Result relay.EnterpriseOperationResult `json:"result"`
	}
	if json.Unmarshal(data, &original) != nil || original.Result.Message == nil {
		t.Fatal("original attachment receipt invalid")
	}
	counts := func() [4]int64 {
		var c [4]int64
		if err := f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM room_events WHERE room_id=$1),(SELECT count(*) FROM relay_operations WHERE room_id=$1),(SELECT count(*) FROM audit WHERE room_id=$1),(SELECT count(*) FROM attachment_references WHERE room_id=$1)`, room).Scan(&c[0], &c[1], &c[2], &c[3]); err != nil {
			t.Fatal(err)
		}
		return c
	}
	before := counts()
	command.Intent.AttachmentIDs = idsB
	status, data = grantsPGCall(t, f, native, http.MethodPost, path, binding.token, command)
	if status < 400 || !bytes.Contains(data, []byte("payload_conflict")) || counts() != before {
		t.Fatal("same ID changed AttachmentIDs created another body/fact/audit/reference")
	}
	command.Intent.AttachmentIDs = idsA
	status, data = grantsPGCall(t, f, native, http.MethodPost, path+"/resolve", binding.token, command)
	var resolved struct {
		Result relay.EnterpriseOperationResult `json:"result"`
	}
	if status != 200 || json.Unmarshal(data, &resolved) != nil || !reflect.DeepEqual(resolved.Result, original.Result) || counts() != before {
		t.Fatal("original attachment receipt/reference drifted after conflict")
	}
	var wrongReferences int
	if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM attachment_references WHERE room_id=$1 AND object_id=$2`, room, b.ID).Scan(&wrongReferences); err != nil || wrongReferences != 0 {
		t.Fatal("second attachment acquired forbidden message reference", err)
	}
	t.Log("MSG02: same client ID changed actual verified A->B AttachmentIDs refused; original canonical receipt/fact/audit/body/reference unchanged")
}

func TestEndpointContextExactAutomaticStopAfterReplacementRetainsPrivateTerminalWAL(t *testing.T) {
	x := newExactEndpointFixture(t)
	source, peer := x.session(t, "fresh-automatic-source"), x.session(t, "fresh-original-peer")
	source.success(t, "install", "--repo", x.root, "--runtime", "claude")
	x.bind(t, source, 1, false, "automatic-source")
	x.bind(t, peer, 2, false, "automatic-peer")
	replacement := x.session(t, "fresh-successor-peer")
	x.bind(t, replacement, 2, true, "automatic-successor")
	const body = "@agent2 PRIVATE_AUTOMATIC_AFTER_REPLACE full retained reply"
	out := source.run(t, exactStop(source.cfg.Endpoint, body), "hook", "--runtime", "claude")
	if strings.Contains(out.Output, body) {
		t.Fatal("late actual Stop exposed its private original reply")
	}
	_, state := x.state(t, 1)
	if state.AutomaticPeerGeneration != 1 || !state.AutomaticPaused || state.Pending == nil || state.Pending.Text != body || state.Pending.ExpectedPeerGeneration != 1 || state.Pending.Outcome != relay.PeerChangedCode || state.Pending.Unknown {
		t.Fatal("late actual automatic Stop did not retain terminal original WAL")
	}
	var encoding []byte
	if err := x.f.admin.QueryRow(t.Context(), `SELECT result_encoding FROM relay_operations WHERE room_id=$1 AND op_kind='report' AND binding_id=$2`, x.room, state.BindID).Scan(&encoding); err != nil {
		t.Fatal(err)
	}
	var result relay.EnterpriseOperationResult
	if json.Unmarshal(encoding, &result) != nil || result.Publication == nil || result.Publication.Outcome != relay.PeerChangedCode || result.Publication.Message != nil {
		t.Fatal("automatic late report did not accept a body-free peer_changed receipt")
	}
	var leaked int
	if err := x.f.admin.QueryRow(t.Context(), `SELECT count(*) FROM room_events WHERE room_id=$1 AND convert_from(event_encoding,'UTF8') LIKE '%PRIVATE_AUTOMATIC_AFTER_REPLACE%'`, x.room).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatal("old automatic Stop reached successor Room history", err)
	}
	t.Log("ROOM05: actual Stop first emitted after peer replacement, real TLS/PG terminal peer_changed accepted, original body private in endpoint WAL and absent from successor Room")
}
