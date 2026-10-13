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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/enterpriseclient"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"golang.org/x/oauth2"
)

// This fixture runs the real Handler/Store and CLI entrypoint in distinct OS
// processes, with TLS, signed synthetic OIDC, PostgreSQL and private files.
// It does not launch a vendor model or certify model acceptance of stdout.
type endpointProcessConfig struct {
	Mode, Origin, Deployment, Schema, Issuer, CA, Key, Control string
	Workspace, DeviceDirectory, Home, Session, BeforeStdout    string
	Args                                                       []string
}

type endpointStoppedWriter struct{ marker string }

func (w endpointStoppedWriter) Write([]byte) (int, error) {
	if err := os.WriteFile(w.marker, []byte("claim persisted; stdout not released"), 0o600); err != nil {
		return 0, err
	}
	// Parent terminates this actual process at the durable-claim/stdout gap.
	select {}
}

func TestEnterpriseEndpointProcessHelper(t *testing.T) {
	path := os.Getenv("PAIRROOM_ENDPOINT_PROCESS_CONFIG")
	if path == "" {
		return
	}
	var cfg endpointProcessConfig
	if err := privatefile.ReadJSON(path, 64<<10, &cfg); err != nil {
		t.Fatal("private process configuration unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if cfg.Mode == "endpoint" {
		relayclient.IsolateNativeCaller(t)
		for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData", "USERPROFILE"} {
			t.Setenv(key, cfg.Home)
		}
		t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(cfg.Home, "claude"))
		t.Setenv("CLAUDE_CODE_SESSION_ID", cfg.Session)
		var out io.Writer = os.Stdout
		if cfg.BeforeStdout != "" {
			out = endpointStoppedWriter{cfg.BeforeStdout}
		}
		if err := relayclient.Run(ctx, cfg.Args, os.Stdin, out, os.Stderr); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		os.Exit(0)
	}
	if cfg.Mode != "service" {
		t.Fatal("unsupported process role")
	}
	store, err := sqlstore.Open(ctx, sqlstore.Config{DSN: os.Getenv("PAIRROOM_SQLSTORE_TEST_DSN"), Schema: cfg.Schema, DeploymentID: cfg.Deployment, RequireSessionAuth: true})
	if err != nil {
		t.Fatal("process Store unavailable")
	}
	defer store.Close()
	if err := store.CheckIdentityConfiguration(ctx, "org-http", "fixture-connection", cfg.Issuer); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(cfg.CA)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid process CA")
	}
	provider, err := enterpriseauth.NewRecoveringProvider(ctx, enterpriseauth.ProviderConfig{ConnectionID: "fixture-connection", Issuer: cfg.Issuer, ClientID: "fixture-client", ClientSecret: "fixture-client-secret", RedirectURL: cfg.Origin + "/enterprise/auth/callback", AuthStyle: oauth2.AuthStyleInHeader, SigningAlgorithms: []string{"ES256"}, AllowedReturnPaths: []string{"/enterprise/"}, UseUserInfo: true, Outbound: enterpriseauth.OutboundPolicy{AllowedOrigins: []string{cfg.Issuer}, RootCAs: roots, Timeout: 3 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	transactions, err := enterpriseauth.NewMemoryStore(16)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{DeploymentID: cfg.Deployment, PublicOrigin: cfg.Origin}, provider, transactions, store)
	if err != nil {
		t.Fatal(err)
	}
	var faultMu sync.Mutex
	wrapper := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Test-only response-loss fault, after the real SQL-backed Handler has
		// completed. The fault never changes its outcome or retries the effect.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/relay") {
			data, readErr := io.ReadAll(io.LimitReader(r.Body, 2<<20))
			if readErr != nil {
				http.Error(w, "fixture read", 500)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			var request struct {
				Intent struct {
					Kind string `json:"kind"`
				} `json:"intent"`
			}
			_ = json.Unmarshal(data, &request)
			faultMu.Lock()
			armed, _ := os.ReadFile(cfg.Control)
			drop := string(armed) == request.Intent.Kind && len(armed) > 0
			if drop {
				_ = os.WriteFile(cfg.Control, nil, 0o600)
			}
			faultMu.Unlock()
			if drop {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				if recorder.Code != 200 {
					t.Errorf("fault never reached accepted operation: %d", recorder.Code)
				}
				conn, _, hijackErr := w.(http.Hijacker).Hijack()
				if hijackErr == nil {
					_ = conn.Close()
				}
				return
			}
		}
		handler.ServeHTTP(w, r)
	})
	u, _ := url.Parse(cfg.Origin)
	listener, err := net.Listen("tcp", "127.0.0.1:"+u.Port())
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: wrapper, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	if err := server.ServeTLS(listener, cfg.CA, cfg.Key); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

type endpointRunningProcess struct {
	command        *exec.Cmd
	done           chan error
	stdout, stderr bytes.Buffer
	finished       bool
}

func endpointStart(t *testing.T, cfg endpointProcessConfig, ready bool) *endpointRunningProcess {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "private-process")
	if err := privatefile.Mkdir(directory); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "process.json")
	if err := privatefile.WriteJSON(path, cfg); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestEnterpriseEndpointProcessHelper$")
	command.Env = append(os.Environ(), "PAIRROOM_ENDPOINT_PROCESS_CONFIG="+path)
	if cfg.Workspace != "" {
		command.Dir = cfg.Workspace
	}
	p := &endpointRunningProcess{command: command, done: make(chan error, 1)}
	command.Stderr = &p.stderr
	if ready {
		pipe, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		line := make(chan string, 1)
		go func() {
			reader := bufio.NewReader(pipe)
			first, _ := reader.ReadString('\n')
			line <- first
			_, _ = io.Copy(&p.stdout, reader)
		}()
		go func() { p.done <- command.Wait() }()
		select {
		case first := <-line:
			if strings.TrimSpace(first) != "ready" {
				_ = command.Process.Kill()
				<-p.done
				t.Fatalf("service child not ready: %s %s", first, p.stderr.String())
			}
		case <-time.After(20 * time.Second):
			_ = command.Process.Kill()
			<-p.done
			t.Fatal("service child startup exceeded budget")
		}
	} else {
		command.Stdout = &p.stdout
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { p.done <- command.Wait() }()
	}
	t.Cleanup(func() {
		if !p.finished {
			_ = p.command.Process.Kill()
			<-p.done
			p.finished = true
		}
	})
	return p
}

func (p *endpointRunningProcess) wait(t *testing.T, wantSuccess bool) string {
	t.Helper()
	select {
	case err := <-p.done:
		p.finished = true
		if (err == nil) != wantSuccess {
			t.Fatalf("endpoint result success=%t, error=%v, stderr=%s, stdout=%s", wantSuccess, err, p.stderr.String(), p.stdout.String())
		}
		return p.stdout.String()
	case <-time.After(25 * time.Second):
		_ = p.command.Process.Kill()
		<-p.done
		p.finished = true
		t.Fatal("endpoint process exceeded budget")
		return ""
	}
}

func (p *endpointRunningProcess) kill(t *testing.T) {
	t.Helper()
	if err := p.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		p.finished = true
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit")
	}
}

func endpointRun(t *testing.T, cfg endpointProcessConfig, args ...string) string {
	t.Helper()
	cfg.Args = args
	return endpointStart(t, cfg, false).wait(t, true)
}

func endpointDeviceLogin(t *testing.T, f *httpPGFixture, browser *http.Client, csrf, ca string) string {
	t.Helper()
	completed := make(chan error, 1)
	client, err := enterpriseclient.New(enterpriseclient.Config{Origin: f.origin, DeploymentID: f.cfg.DeploymentID, Directory: filepath.Join(t.TempDir(), "device"), CAFile: ca, OpenBrowser: func(verification string) error {
		requestID := strings.TrimPrefix(verification, f.origin+"/enterprise/device-enrollments/")
		status, body := f.post(browser, "/enterprise/api/v1/device-enrollments/"+requestID+"/decision", map[string]string{"client_id": "approve-" + requestID, "action": "approve"}, csrf)
		if status != 200 {
			return fmt.Errorf("browser approval status %d", status)
		}
		var response struct {
			Location string `json:"completion_location"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return err
		}
		go func() {
			client := &http.Client{Timeout: 10 * time.Second}
			r, err := client.Get(response.Location)
			if err == nil {
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
				if r.StatusCode != 200 {
					err = fmt.Errorf("callback status %d", r.StatusCode)
				}
			}
			completed <- err
		}()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := client.Login(ctx, "process endpoint", false); err != nil {
		t.Fatal(err)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	return client.Directory()
}

func TestEnterpriseEndpointProcessesSurviveServiceKillWithoutReprintingClaims(t *testing.T) {
	f := newHTTPPGFixture(t)
	owner, member := f.browser("member-one"), f.browser("member-two")
	f.login(owner)
	f.login(member)
	ownerSession, memberSession := f.session(owner), f.session(member)
	const room = "room-process-endpoints"
	f.createRoom(owner, ownerSession.CSRF, room)
	status, body := f.post(owner, "/enterprise/api/v1/rooms/"+room+"/memberships", map[string]any{"client_id": "admit-peer", "expected_revision": 1, "intent": map[string]any{"target_user_id": memberSession.UserID, "action": "admit", "role": "member"}}, ownerSession.CSRF)
	if status != 200 {
		t.Fatalf("admit process peer: %d %s", status, body)
	}
	cert := f.server.TLS.Certificates[0]
	ca, key := filepath.Join(t.TempDir(), "ca.pem"), filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		t.Fatal(err)
	}
	dirs := []string{endpointDeviceLogin(t, f, owner, ownerSession.CSRF, ca), endpointDeviceLogin(t, f, member, memberSession.CSRF, ca)}
	endpoints := make([]endpointProcessConfig, 2)
	for i := range endpoints {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
			t.Fatalf("fixture git init: %v %s", err, output)
		}
		endpoints[i] = endpointProcessConfig{Mode: "endpoint", Origin: f.origin, Deployment: f.cfg.DeploymentID, CA: ca, Workspace: root, DeviceDirectory: dirs[i], Home: t.TempDir(), Session: fmt.Sprintf("native-process-session-%d", i+1)}
		endpointRun(t, endpoints[i], "install", "--repo", root, "--runtime", "claude")
	}
	f.server.Close()
	f.provider.Close()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	serviceConfig := endpointProcessConfig{Mode: "service", Origin: f.origin, Deployment: f.cfg.DeploymentID, Schema: f.schema, Issuer: f.idp.server.URL, CA: ca, Key: key, Control: filepath.Join(t.TempDir(), "drop-response")}
	service := endpointStart(t, serviceConfig, true)
	bindArgs := func(e endpointProcessConfig, slot int) []string {
		return []string{"bind", "--repo", e.Workspace, "--runtime", "claude", "--enterprise-service", e.Origin, "--deployment", e.Deployment, "--enterprise-ca", e.CA, "--enterprise-state-dir", e.DeviceDirectory, "--room", room, "--slot", fmt.Sprint(slot)}
	}
	for i, e := range endpoints {
		pending := endpointRun(t, e, bindArgs(e, i+1)...)
		var request struct {
			Status    string `json:"status"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal([]byte(pending), &request); err != nil || request.Status != "awaiting_owner" || request.RequestID == "" {
			t.Fatalf("endpoint bypassed Owner approval: %s", pending)
		}
		status, body := f.post(owner, "/enterprise/api/v1/rooms/"+room+"/binding-requests/"+request.RequestID+"/decision", map[string]string{"client_id": "approve-" + request.RequestID, "action": "approve"}, ownerSession.CSRF)
		if status != 200 {
			t.Fatalf("approve endpoint: %d %s", status, body)
		}
		ready := endpointRun(t, e, bindArgs(e, i+1)...)
		if strings.Contains(ready, e.Session) {
			t.Fatal("native session leaked through bind stdout")
		}
	}
	send := func(id, text string) {
		t.Helper()
		endpointRun(t, endpoints[0], "send", "--repo", endpoints[0].Workspace, "--id", id, "--text", text, "--peer-generation", "1")
	}
	wait := func() string {
		t.Helper()
		return endpointRun(t, endpoints[1], "wait", "--repo", endpoints[1].Workspace, "--timeout", "1")
	}
	send("first-process-send", "first full process envelope")
	if !strings.Contains(wait(), "first full process envelope") {
		t.Fatal("two same-Runtime processes did not relay")
	}
	send("kill-before-output", "uncertain process envelope")
	stoppedConfig := endpoints[1]
	stoppedConfig.BeforeStdout = filepath.Join(t.TempDir(), "before-stdout")
	stoppedConfig.Args = []string{"wait", "--repo", stoppedConfig.Workspace, "--timeout", "10"}
	stopped := endpointStart(t, stoppedConfig, false)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(stoppedConfig.BeforeStdout); err == nil {
			break
		}
		if time.Now().After(deadline) {
			stopped.kill(t)
			t.Fatalf("endpoint never reached pre-stdout gap: %s", stopped.stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopped.kill(t)
	if stopped.stdout.Len() != 0 {
		t.Fatal("fault released envelope before process termination")
	}
	var beforeEpoch int64
	if err := f.admin.QueryRow(context.Background(), `SELECT epoch FROM ownership`).Scan(&beforeEpoch); err != nil {
		t.Fatal(err)
	}
	service.kill(t)
	service = endpointStart(t, serviceConfig, true)
	var afterEpoch int64
	if err := f.admin.QueryRow(context.Background(), `SELECT epoch FROM ownership`).Scan(&afterEpoch); err != nil || afterEpoch <= beforeEpoch {
		t.Fatal("actual service process restart did not advance ownership")
	}
	if f.session(owner).CSRF != ownerSession.CSRF {
		t.Fatal("ordinary process restart replaced persistent browser login")
	}
	var recovered int
	if err := f.admin.QueryRow(context.Background(), `SELECT count(*) FROM room_events WHERE room_id=$1 AND kind='native.message.updated' AND origin_kind='operator' AND origin_user IS NULL`, room).Scan(&recovered); err != nil || recovered != 1 {
		t.Fatalf("startup did not durably recover exactly the interrupted delivery: count=%d err=%v", recovered, err)
	}
	resolution := endpointRun(t, endpoints[1], "reconcile", "--repo", endpoints[1].Workspace)
	if strings.Contains(resolution, "uncertain process envelope") {
		t.Fatal("new endpoint process reprinted unknown claim during reconciliation")
	}
	if strings.Contains(wait(), "uncertain process envelope") {
		t.Fatal("service restart automatically redelivered unknown claim")
	}
	send("post-restart", "new work after service recovery")
	if !strings.Contains(wait(), "new work after service recovery") {
		t.Fatal("persisted credentials did not resume after restart")
	}
	if err := os.WriteFile(serviceConfig.Control, []byte("ack"), 0o600); err != nil {
		t.Fatal(err)
	}
	send("lost-ack", "printed once before lost ACK response")
	lostACK := endpoints[1]
	lostACK.Args = []string{"wait", "--repo", lostACK.Workspace, "--timeout", "1"}
	printed := endpointStart(t, lostACK, false).wait(t, false)
	if strings.Count(printed, "printed once before lost ACK response") != 1 {
		t.Fatal("ACK-loss fixture did not first write one full envelope")
	}
	resolution = endpointRun(t, endpoints[1], "reconcile", "--repo", endpoints[1].Workspace)
	if strings.Contains(resolution, "printed once before lost ACK response") {
		t.Fatal("ACK receipt reconciliation reprinted stdout")
	}
	// Same original publication ID still resolves once, across both process
	// boundaries and the ownership change; bodies are not the deduplication key.
	send("post-restart", "new work after service recovery")
	var accepted int
	if err := f.admin.QueryRow(context.Background(), `SELECT count(*) FROM relay_operations WHERE identifier='post-restart'`).Scan(&accepted); err != nil || accepted != 1 {
		t.Fatalf("publication duplicated: count=%d err=%v", accepted, err)
	}
	readRoom := func() (uint64, relay.Snapshot) {
		t.Helper()
		status, body := f.get(owner, "/enterprise/api/v1/rooms/"+room)
		var view struct {
			Revision uint64 `json:"revision"`
			relay.Snapshot
		}
		if status != 200 || json.Unmarshal(body, &view) != nil || view.Revision == 0 {
			t.Fatal("process history unavailable")
		}
		return view.Revision, view.Snapshot
	}
	revision, view := readRoom()
	var interrupted string
	for _, message := range view.Messages {
		if message.Text == "uncertain process envelope" && message.State == "unknown" {
			interrupted = message.ID
		}
	}
	if interrupted == "" {
		t.Fatal("interrupted delivery was lost from authorized history")
	}
	retry := map[string]any{"client_id": "explicit-inspected-retry", "expected_revision": revision, "intent": map[string]string{"kind": "retry", "message_id": interrupted}}
	status, _ = f.post(member, "/enterprise/api/v1/rooms/"+room+"/queue", retry, memberSession.CSRF)
	if status != http.StatusForbidden {
		t.Fatal("ordinary Member acquired Owner queue authority")
	}
	status, body = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/queue", retry, ownerSession.CSRF)
	if status != 200 || bytes.Contains(body, []byte("uncertain process envelope")) {
		t.Fatalf("explicit Owner retry failed or leaked body: %d", status)
	}
	status, _ = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/resolve-queue", retry, ownerSession.CSRF)
	if status != 200 {
		t.Fatal("explicit queue receipt did not resolve")
	}
	if !strings.Contains(wait(), "uncertain process envelope") {
		t.Fatal("explicit inspected retry was not delivered")
	}
	revision, _ = readRoom()
	status, _ = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/memberships", map[string]any{"client_id": "process-member-revoke", "expected_revision": revision, "intent": map[string]string{"action": "remove", "target_user_id": memberSession.UserID}}, ownerSession.CSRF)
	if status != 200 {
		t.Fatal("live process membership revocation failed")
	}
	denied := endpoints[1]
	denied.Args = []string{"status", "--repo", denied.Workspace}
	endpointStart(t, denied, false).wait(t, false)
	revision, _ = readRoom()
	status, _ = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/memberships", map[string]any{"client_id": "process-member-readmit", "expected_revision": revision, "intent": map[string]string{"action": "admit", "target_user_id": memberSession.UserID, "role": "member"}}, ownerSession.CSRF)
	if status != 200 {
		t.Fatal("member readmission failed")
	}
	endpointStart(t, denied, false).wait(t, false)
	endpointRun(t, endpoints[0], "status", "--repo", endpoints[0].Workspace)
	service.kill(t)
}
