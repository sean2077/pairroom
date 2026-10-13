package enterpriseservice

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"golang.org/x/oauth2"
)

type archiveReplaceProcessConfig struct {
	endpointProcessConfig
	Marker, Requests string
}

type archiveSessionHostConfig struct {
	endpointProcessConfig
	ExpectedBindingID string
}

// This child is a real SQL-backed Service. Only the response boundary is
// intercepted: the production Handler must commit before the marker exists.
func TestArchiveReplaceServiceProcessHelper(t *testing.T) {
	path := os.Getenv("PAIRROOM_ARCHIVE_REPLACE_PROCESS_CONFIG")
	if path == "" {
		return
	}
	var cfg archiveReplaceProcessConfig
	if err := privatefile.ReadJSON(path, 64<<10, &cfg); err != nil {
		t.Fatal("private helper config unavailable")
	}
	ctx := context.Background()
	s, err := sqlstore.Open(ctx, sqlstore.Config{DSN: os.Getenv("PAIRROOM_SQLSTORE_TEST_DSN"), Schema: cfg.Schema, DeploymentID: cfg.Deployment, RequireSessionAuth: true})
	if err != nil {
		t.Fatal("Service Store unavailable")
	}
	defer s.Close()
	if err := s.CheckIdentityConfiguration(ctx, "org-http", "fixture-connection", cfg.Issuer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(cfg.CA)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid CA")
	}
	p, err := enterpriseauth.NewRecoveringProvider(ctx, enterpriseauth.ProviderConfig{ConnectionID: "fixture-connection", Issuer: cfg.Issuer, ClientID: "fixture-client", ClientSecret: "fixture-client-secret", RedirectURL: cfg.Origin + "/enterprise/auth/callback", AuthStyle: oauth2.AuthStyleInHeader, SigningAlgorithms: []string{"ES256"}, AllowedReturnPaths: []string{"/enterprise/"}, UseUserInfo: true, Outbound: enterpriseauth.OutboundPolicy{AllowedOrigins: []string{cfg.Issuer}, RootCAs: roots, Timeout: 3 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	transactions, err := enterpriseauth.NewMemoryStore(16)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{DeploymentID: cfg.Deployment, PublicOrigin: cfg.Origin}, p, transactions, s)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	wrapper := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/binding-requests/") {
			action := filepath.Base(r.URL.Path)
			if r.Method == http.MethodGet {
				action = "read-request"
			}
			mu.Lock()
			log, e := os.OpenFile(cfg.Requests, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if e == nil {
				_, e = fmt.Fprintln(log, action)
				_ = log.Close()
			}
			armed, _ := os.ReadFile(cfg.Control)
			drop := action == "complete" && string(armed) == "complete"
			if drop {
				_ = os.WriteFile(cfg.Control, nil, 0o600)
			}
			mu.Unlock()
			if e != nil {
				http.Error(w, "fixture log unavailable", 500)
				return
			}
			if drop {
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, r)
				if recorder.Code != 200 {
					http.Error(w, "fault did not reach commit", 500)
					return
				}
				if err := os.WriteFile(cfg.Marker, []byte("committed; response withheld"), 0o600); err != nil {
					http.Error(w, "fixture marker unavailable", 500)
					return
				}
				select {} // Parent must kill the actual Service, not a proxy.
			}
		}
		h.ServeHTTP(w, r)
	})
	u, _ := url.Parse(cfg.Origin)
	l, err := net.Listen("tcp", "127.0.0.1:"+u.Port())
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: wrapper, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	if err := server.ServeTLS(l, cfg.CA, cfg.Key); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

func archiveReplaceStart(t *testing.T, cfg archiveReplaceProcessConfig) *endpointRunningProcess {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "service")
	if err := privatefile.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := privatefile.WriteJSON(path, cfg); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestArchiveReplaceServiceProcessHelper$")
	cmd.Env = append(os.Environ(), "PAIRROOM_ARCHIVE_REPLACE_PROCESS_CONFIG="+path)
	p := &endpointRunningProcess{command: cmd, done: make(chan error, 1)}
	cmd.Stderr = &p.stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan bool, 1)
	go func() {
		reader := bufio.NewReader(out)
		line, _ := reader.ReadString('\n')
		ready <- strings.TrimSpace(line) == "ready"
		_, _ = io.Copy(&p.stdout, reader)
	}()
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if !p.finished {
			p.kill(t)
		}
	})
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("Service not ready")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Service startup exceeded budget")
	}
	return p
}

// This is deliberately a controlled session host, not a vendor process or a
// relay wait. It drives real CLI status from the bound Workspace/Home and
// official session metadata. Private CLI output never enters the IPC stream.
func TestArchiveNativeSessionHostProcessHelper(t *testing.T) {
	path := os.Getenv("PAIRROOM_ARCHIVE_SESSION_HOST")
	if path == "" {
		return
	}
	var cfg archiveSessionHostConfig
	if err := privatefile.ReadJSON(path, 64<<10, &cfg); err != nil {
		t.Fatal("session host config unavailable")
	}
	relayclient.IsolateNativeCaller(t)
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData", "USERPROFILE"} {
		t.Setenv(key, cfg.Home)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(cfg.Home, "claude"))
	t.Setenv("CLAUDE_CODE_SESSION_ID", cfg.Session)
	_, journal := processBindingJournal(t, cfg.DeviceDirectory, false)
	if journal["bind_id"] != cfg.ExpectedBindingID || journal["session_hash"] != relay.Digest("claude\x00"+cfg.Session) || journal["workspace"] != cfg.Workspace {
		t.Fatal("session host official source differs from frozen Binding")
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var privateOut, privateErr bytes.Buffer
		err := relayclient.Run(ctx, []string{"status", "--repo", cfg.Workspace}, strings.NewReader(""), &privateOut, &privateErr)
		cancel()
		var wire struct {
			Relay relay.Summary `json:"relay"`
			Local struct {
				Workspace string `json:"binding_workspace"`
			} `json:"local"`
		}
		if err != nil || json.Unmarshal(privateOut.Bytes(), &wire) != nil || !wire.Relay.Bindings["slot1"].Associated || wire.Relay.Bindings["slot1"].Generation != 1 || wire.Relay.Bindings["slot1"].Runtime != "claude" || wire.Local.Workspace != cfg.Workspace {
			_, _ = fmt.Fprintln(os.Stdout, "status-failed")
			continue
		}
		_, _ = fmt.Fprintln(os.Stdout, scanner.Text())
	}
	os.Exit(0)
}

func archiveSessionHost(t *testing.T, cfg endpointProcessConfig) func(string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "session-host")
	if err := privatefile.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	_, journal := processBindingJournal(t, cfg.DeviceDirectory, false)
	if err := privatefile.WriteJSON(path, archiveSessionHostConfig{cfg, journal["bind_id"].(string)}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestArchiveNativeSessionHostProcessHelper$")
	cmd.Env = append(os.Environ(), "PAIRROOM_ARCHIVE_SESSION_HOST="+path)
	cmd.Dir = cfg.Workspace
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Process.Kill(); <-done })
	return func(nonce string) {
		t.Helper()
		if cmd.Process.Pid != pid {
			t.Fatal("session host PID changed")
		}
		select {
		case <-done:
			t.Fatal("session host exited")
		default:
		}
		if _, err := fmt.Fprintln(in, nonce); err != nil {
			t.Fatal(err)
		}
		select {
		case value := <-lines:
			if value != nonce {
				t.Fatal("session IPC nonce mismatch")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("session host did not echo")
		}
	}
}

type archiveReplaceFixture struct {
	f          *httpPGFixture
	owner      *http.Client
	csrf, room string
	endpoints  [2]endpointProcessConfig
	cfg        archiveReplaceProcessConfig
	service    *endpointRunningProcess
}

func newArchiveReplaceFixture(t *testing.T) *archiveReplaceFixture {
	t.Helper()
	f := newHTTPPGFixture(t)
	owner := f.browser("member-one")
	f.login(owner)
	session := f.session(owner)
	room := "room-archive-replace-process"
	f.createRoom(owner, session.CSRF, room)
	cert := f.server.TLS.Certificates[0]
	ca, key := filepath.Join(t.TempDir(), "ca.pem"), filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyData, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyData}), 0o600); err != nil {
		t.Fatal(err)
	}
	x := &archiveReplaceFixture{f: f, owner: owner, csrf: session.CSRF, room: room}
	for i := range x.endpoints {
		dir := endpointDeviceLogin(t, f, owner, session.CSRF, ca)
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("git", "init", "-q", root).Run(); err != nil {
			t.Fatal(err)
		}
		x.endpoints[i] = endpointProcessConfig{Mode: "endpoint", Origin: f.origin, Deployment: f.cfg.DeploymentID, CA: ca, Workspace: root, DeviceDirectory: dir, Home: t.TempDir(), Session: fmt.Sprintf("archive-replace-session-%d", i)}
		endpointRun(t, x.endpoints[i], "install", "--repo", root, "--runtime", "claude")
	}
	f.server.Close()
	f.provider.Close()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	x.cfg = archiveReplaceProcessConfig{endpointProcessConfig: endpointProcessConfig{Mode: "service", Origin: f.origin, Deployment: f.cfg.DeploymentID, Schema: f.schema, Issuer: f.idp.server.URL, CA: ca, Key: key, Control: filepath.Join(t.TempDir(), "control")}, Marker: filepath.Join(t.TempDir(), "marker"), Requests: filepath.Join(t.TempDir(), "requests")}
	x.service = archiveReplaceStart(t, x.cfg)
	for i, e := range x.endpoints {
		x.bind(t, e, i+1, false, "initial")
	}
	return x
}

func (x *archiveReplaceFixture) args(e endpointProcessConfig, slot int, replace bool, id string) []string {
	a := []string{"bind", "--repo", e.Workspace, "--runtime", "claude", "--enterprise-service", e.Origin, "--deployment", e.Deployment, "--enterprise-ca", e.CA, "--enterprise-state-dir", e.DeviceDirectory, "--room", x.room, "--slot", fmt.Sprint(slot), "--id", id}
	if replace {
		a = append(a, "--replace")
	}
	return a
}
func (x *archiveReplaceFixture) bind(t *testing.T, e endpointProcessConfig, slot int, replace bool, id string) string {
	t.Helper()
	args := x.args(e, slot, replace, id)
	pending := endpointRun(t, e, args...)
	var request struct{ Status, RequestID string }
	var wire struct {
		Status    string `json:"status"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal([]byte(pending), &wire) != nil {
		t.Fatal("invalid bind response")
	}
	request.Status, request.RequestID = wire.Status, wire.RequestID
	if request.Status != "awaiting_owner" || request.RequestID == "" {
		t.Fatal("binding bypassed Owner approval")
	}
	status, _ := x.f.post(x.owner, "/enterprise/api/v1/rooms/"+x.room+"/binding-requests/"+request.RequestID+"/decision", map[string]string{"client_id": "approve-" + request.RequestID, "action": "approve"}, x.csrf)
	if status != 200 {
		t.Fatalf("Owner approval status=%d", status)
	}
	endpointRun(t, e, args...)
	return request.RequestID
}

func (x *archiveReplaceFixture) revision(t *testing.T) uint64 {
	t.Helper()
	var revision uint64
	if err := x.f.admin.QueryRow(context.Background(), `SELECT revision FROM rooms WHERE room_id=$1`, x.room).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}
func (x *archiveReplaceFixture) counts(t *testing.T) [4]int64 {
	t.Helper()
	var counts [4]int64
	for i, table := range []string{"room_events", "relay_operations", "binding_operations", "audit"} {
		if err := x.f.admin.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&counts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}
func (x *archiveReplaceFixture) restart(t *testing.T) {
	t.Helper()
	type owner struct {
		epoch    int64
		instance string
		backend  int
		start    time.Time
	}
	read := func() owner {
		var value owner
		if err := x.f.admin.QueryRow(context.Background(), `SELECT epoch,instance_id,lease_pid,lease_backend_start FROM ownership`).Scan(&value.epoch, &value.instance, &value.backend, &value.start); err != nil {
			t.Fatal("ownership observation unavailable")
		}
		return value
	}
	before := read()
	old := x.service
	oldPID := old.command.Process.Pid
	x.service.kill(t)
	if !old.finished || old.command.ProcessState == nil || old.command.ProcessState.Success() {
		t.Fatal("old Service lacks actual abnormal terminal process state")
	}
	x.service = archiveReplaceStart(t, x.cfg)
	after := read()
	if after.epoch != before.epoch+1 || after.instance == before.instance || after.instance == "" || after.backend == before.backend && after.start.Equal(before.start) || x.service.command.Process.Pid == oldPID {
		t.Fatal("Service restart did not replace OS process, instance and lease identity at exact next epoch")
	}
}

func processBindingJournal(t *testing.T, directory string, replace bool) (string, map[string]any) {
	t.Helper()
	var selected string
	var journal map[string]any
	err := filepath.WalkDir(filepath.Join(directory, "bindings"), func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Name() != "binding.json" {
			return nil
		}
		var value map[string]any
		if err := privatefile.ReadJSON(path, 64<<10, &value); err != nil {
			return err
		}
		if value["replace"] == replace {
			selected, journal = path, value
		}
		return nil
	})
	if err != nil || selected == "" {
		t.Fatal("original private binding journal unavailable")
	}
	return selected, journal
}

func TestEnterpriseArchiveActualServiceKillKeepsNativeSessionHostAndBindings(t *testing.T) {
	x := newArchiveReplaceFixture(t)
	echo := archiveSessionHost(t, x.endpoints[0])
	echo("before-archive-7d41")
	e := x.endpoints[0]
	endpointRun(t, e, "send", "--repo", e.Workspace, "--id", "before-archive-send", "--text", "retained archive history", "--peer-generation", "1")
	_, journal := processBindingJournal(t, e.DeviceDirectory, false)
	status, _ := x.f.post(x.owner, "/enterprise/api/v1/catalog/commands", map[string]any{"client_id": "archive-process-room", "expected_revision": x.revision(t), "intent": map[string]string{"kind": "room.archive", "room_id": x.room}}, x.csrf)
	if status != 200 {
		t.Fatalf("typed archive status=%d", status)
	}
	echo("after-archive-f09a")
	x.restart(t)
	echo("after-service-kill-58ba")
	before := x.counts(t)
	denied := e
	denied.Args = []string{"send", "--repo", e.Workspace, "--id", "forbidden-after-archive", "--text", "must not persist", "--peer-generation", "1"}
	endpointStart(t, denied, false).wait(t, false)
	grant := journal["grant"].(map[string]any)
	native := grantsPGNative(t, x.f)
	request := grantsPGRelayRequest{ClientID: "narrow-after-archive", BindingID: journal["bind_id"].(string), Generation: uint64(journal["generation"].(float64)), Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: "slot2", Text: "must not persist", ExpectedPeerGeneration: 1}}
	status, _ = grantsPGCall(t, x.f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+x.room+"/relay", grant["token"].(string), request)
	if status != 403 {
		t.Fatalf("archived narrow grant new send status=%d", status)
	}
	if x.counts(t) != before {
		t.Fatal("archive refusal produced business fact, receipt or audit")
	}
	status, history := x.f.get(x.owner, "/enterprise/api/v1/rooms/"+x.room)
	if status != 200 || !bytes.Contains(history, []byte("retained archive history")) {
		t.Fatal("archive lost authorized retained history")
	}
	var active int
	if err := x.f.admin.QueryRow(context.Background(), `SELECT count(*) FROM bindings b JOIN agents a ON a.agent_id=b.agent_id WHERE b.room_id=$1 AND b.retired_at IS NULL AND a.retired_at IS NULL`, x.room).Scan(&active); err != nil || active != 2 {
		t.Fatal("archive released original Binding/Agent reservations")
	}
	status, _ = x.f.post(x.owner, "/enterprise/api/v1/catalog/commands", map[string]any{"client_id": "restore-process-room", "expected_revision": x.revision(t), "intent": map[string]string{"kind": "room.restore", "room_id": x.room}}, x.csrf)
	if status != 200 {
		t.Fatalf("typed restore status=%d", status)
	}
	endpointRun(t, e, "send", "--repo", e.Workspace, "--id", "after-restore-send", "--text", "explicit work after restore", "--peer-generation", "1")
	got := endpointRun(t, x.endpoints[1], "wait", "--repo", x.endpoints[1].Workspace, "--timeout", "1")
	if !strings.Contains(got, "retained archive history") {
		t.Fatal("restore did not preserve original queued work")
	}
	got = endpointRun(t, x.endpoints[1], "wait", "--repo", x.endpoints[1].Workspace, "--timeout", "1")
	if !strings.Contains(got, "explicit work after restore") {
		t.Fatal("original valid Binding did not resume after typed restore")
	}
	echo("after-restore-a9c5")
}

func TestEnterpriseReplacementActualServiceKillResolvesOriginalCompletionFirst(t *testing.T) {
	x := newArchiveReplaceFixture(t)
	e := x.endpoints[0]
	_, oldJournal := processBindingJournal(t, e.DeviceDirectory, false)
	e.Session = "explicit-replacement-session"
	args := x.args(e, 1, true, "replace-process-attempt")
	pending := endpointRun(t, e, args...)
	var request struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal([]byte(pending), &request) != nil || request.RequestID == "" {
		t.Fatal("replacement request missing")
	}
	status, _ := x.f.post(x.owner, "/enterprise/api/v1/rooms/"+x.room+"/binding-requests/"+request.RequestID+"/decision", map[string]string{"client_id": "approve-replacement", "action": "approve"}, x.csrf)
	if status != 200 {
		t.Fatal("replacement approval failed")
	}
	path, original := processBindingJournal(t, e.DeviceDirectory, true)
	if err := os.WriteFile(x.cfg.Control, []byte("complete"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.Args = args
	cli := endpointStart(t, e, false)
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(x.cfg.Marker); err == nil {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("replacement never reached committed response gap")
		case <-ticker.C:
		}
	}
	var state struct {
		Generation                            int
		Retired, NewValid, Receipts, Consumed int
	}
	err := x.f.admin.QueryRow(context.Background(), `SELECT (SELECT current_generation FROM room_slots WHERE room_id=$1 AND slot='slot1'),(SELECT count(*) FROM binding_grants g JOIN bindings b ON b.binding_id=g.binding_id WHERE b.room_id=$1 AND b.slot='slot1' AND b.generation=1 AND g.revoked_at IS NOT NULL),(SELECT count(*) FROM binding_grants g JOIN bindings b ON b.binding_id=g.binding_id WHERE b.room_id=$1 AND b.slot='slot1' AND b.generation=2 AND g.revoked_at IS NULL AND b.retired_at IS NULL),(SELECT count(*) FROM binding_operations WHERE room_id=$1 AND op_kind='complete'),(SELECT count(*) FROM binding_requests WHERE request_id=$2 AND status='consumed')`, x.room, request.RequestID).Scan(&state.Generation, &state.Retired, &state.NewValid, &state.Receipts, &state.Consumed)
	if err != nil || state.Generation != 2 || state.Retired != 1 || state.NewValid != 1 || state.Receipts != 3 || state.Consumed != 1 {
		t.Fatalf("committed replacement authority shape invalid: %+v error=%v", state, err)
	}
	native := grantsPGNative(t, x.f)
	contextPath := "/enterprise/device/v1/rooms/" + x.room + "/relay/context"
	status, _ = grantsPGCall(t, x.f, native, http.MethodGet, contextPath, oldJournal["grant"].(map[string]any)["token"].(string), nil)
	if status != 403 {
		t.Fatalf("old generation credential still accepted before Service kill: %d", status)
	}
	status, body := grantsPGCall(t, x.f, native, http.MethodGet, contextPath, original["grant"].(map[string]any)["token"].(string), nil)
	var live struct {
		Binding struct {
			Generation uint64 `json:"generation"`
		} `json:"binding"`
	}
	if status != 200 || json.Unmarshal(body, &live) != nil || live.Binding.Generation != 2 {
		t.Fatal("new candidate grant was not genuinely usable at committed response gap")
	}
	var audits int
	if err := x.f.admin.QueryRow(context.Background(), `SELECT count(*) FROM audit WHERE room_id=$1 AND kind='binding.complete' AND outcome='accepted'`, x.room).Scan(&audits); err != nil || audits != 3 {
		t.Fatal("replacement commit lacks exactly one accepted completion audit")
	}
	committedCounts := x.counts(t)
	var lastAudit int64
	if err := x.f.admin.QueryRow(context.Background(), `SELECT COALESCE(max(audit_seq),0) FROM audit`).Scan(&lastAudit); err != nil {
		t.Fatal(err)
	}
	x.restart(t)
	cli.wait(t, false)
	// Startup deliberately appends operator recovery audits even when no
	// delivery needs classification. Freeze the post-startup baseline so the
	// original completion Resolve must still produce zero business effects.
	postRestartCounts := x.counts(t)
	for i := 0; i < 3; i++ {
		if postRestartCounts[i] != committedCounts[i] {
			t.Fatal("restart changed committed replacement facts or receipts")
		}
	}
	if postRestartCounts[3] != committedCounts[3]+2 {
		t.Fatal("restart recovery audit count differs from room plus deployment recovery")
	}
	var roomAudits, deploymentAudits, roomRecoveries, deploymentRecoveries int
	if err := x.f.admin.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM audit WHERE audit_seq>$1 AND actor_kind='operator' AND user_id IS NULL AND outcome='accepted' AND kind='delivery.recovery.room' AND room_id=$2),
		(SELECT count(*) FROM audit WHERE audit_seq>$1 AND actor_kind='operator' AND user_id IS NULL AND outcome='accepted' AND kind='delivery.recovery.complete' AND room_id IS NULL),
		(SELECT count(*) FROM startup_room_recoveries WHERE ownership_epoch=(SELECT epoch FROM ownership) AND room_id=$2 AND reaped_count=0),
		(SELECT count(*) FROM startup_recovery_runs WHERE ownership_epoch=(SELECT epoch FROM ownership) AND room_count=1 AND reaped_count=0)`, lastAudit, x.room).Scan(&roomAudits, &deploymentAudits, &roomRecoveries, &deploymentRecoveries); err != nil || roomAudits != 1 || deploymentAudits != 1 || roomRecoveries != 1 || deploymentRecoveries != 1 {
		t.Fatal("restart delta lacks unique typed room/deployment recovery audits and current-epoch markers")
	}
	var unknown map[string]any
	if err := privatefile.ReadJSON(path, 64<<10, &unknown); err != nil || unknown["phase"] != "completion_unknown" {
		t.Fatal("CLI did not retain original unknown completion")
	}
	for _, key := range []string{"request_id", "proof", "grant", "completion_operation_id", "observed_generation", "bind_id", "agent_id"} {
		if !reflect.DeepEqual(original[key], unknown[key]) {
			t.Fatalf("private immutable %s changed at kill", key)
		}
	}
	if err := os.WriteFile(x.cfg.Requests, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	endpointRun(t, e, args...)
	requests, err := os.ReadFile(x.cfg.Requests)
	if err != nil || strings.TrimSpace(string(requests)) != "resolve-completion" {
		t.Fatal("recovery contacted completion or approval before original Resolve")
	}
	if x.counts(t) != postRestartCounts {
		t.Fatal("recovery duplicated accepted Binding fact, receipt or audit")
	}
	var recovered map[string]any
	if err := privatefile.ReadJSON(path, 64<<10, &recovered); err != nil || recovered["phase"] != "active" || recovered["generation"] != float64(2) {
		t.Fatal("original completion did not recover generation two")
	}
	for _, key := range []string{"request_id", "proof", "completion_operation_id", "bind_id", "agent_id"} {
		if !reflect.DeepEqual(unknown[key], recovered[key]) {
			t.Fatalf("recovery replaced original %s", key)
		}
	}
	endpointRun(t, e, "send", "--repo", e.Workspace, "--id", "new-generation-send", "--text", "generation two real relay", "--peer-generation", "1")
	got := endpointRun(t, x.endpoints[1], "wait", "--repo", x.endpoints[1].Workspace, "--timeout", "1")
	if !strings.Contains(got, "generation two real relay") {
		t.Fatal("recovered generation two could not relay")
	}
}
