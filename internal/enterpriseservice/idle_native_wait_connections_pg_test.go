package enterpriseservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseclient"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

type idleWaitProcessConfig struct {
	Endpoint   endpointProcessConfig
	Cancelable bool
}

type idleWaitResponse struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *idleWaitResponse) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *idleWaitResponse) Write(p []byte) (int, error) {
	_, _ = w.body.Write(p)
	return w.ResponseWriter.Write(p)
}

// Executes the actual foreground CLI with official session metadata. Stdin is
// a test-owned caller cancellation signal, not a production endpoint/test hook.
func TestIdleNativeWaitProcessHelper(t *testing.T) {
	path := os.Getenv("PAIRROOM_IDLE_NATIVE_WAIT_CONFIG")
	if path == "" {
		return
	}
	var cfg idleWaitProcessConfig
	if privatefile.ReadJSON(path, 64<<10, &cfg) != nil {
		os.Exit(4)
	}
	relayclient.IsolateNativeCaller(t)
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData", "USERPROFILE"} {
		t.Setenv(key, cfg.Endpoint.Home)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(cfg.Endpoint.Home, "claude"))
	t.Setenv("CLAUDE_CODE_SESSION_ID", cfg.Endpoint.Session)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if cfg.Cancelable {
		go func() {
			var signal [1]byte
			if n, _ := os.Stdin.Read(signal[:]); n == 1 {
				cancel()
			}
		}()
	}
	var out, diagnostic bytes.Buffer
	err := relayclient.Run(ctx, cfg.Endpoint.Args, bytes.NewReader(nil), &out, &diagnostic)
	errorKind := "other"
	for name, known := range map[string]error{"relay_unknown": relay.ErrUnknown, "relay_auth": relay.ErrAuth, "private_state": enterpriseclient.ErrPrivateState, "no_session": enterpriseclient.ErrNoSession, "binding_inactive": enterpriseclient.ErrBindingInactive, "binding_expired": enterpriseclient.ErrBindingCredentialExpired, "delivery_unresolved": enterpriseclient.ErrDeliveryUnresolved} {
		if errors.Is(err, known) {
			errorKind = name
		}
	}
	if err != nil && err.Error() == "wait response missing claim; outcome uncertain, collection stopped" {
		errorKind = "missing_claim"
	}
	terminal := "unexpected"
	var failure *enterpriseclient.Error
	switch {
	case err == nil:
		terminal = "empty"
	case errors.Is(err, context.Canceled):
		terminal = "caller_canceled"
	case err != nil && err.Error() == "relay enterprise: enterprise operation refused (authorization)":
		terminal = "authorization_refused"
	case errors.As(err, &failure) && failure.Outcome == "refused" && failure.Class == "authorization":
		terminal = "authorization_refused"
	}
	// Never echo private stdout/stderr, native session or filesystem references.
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		Terminal       string `json:"terminal"`
		StdoutBytes    int    `json:"stdout_bytes"`
		ErrorType      string `json:"error_type"`
		CallerCanceled bool   `json:"caller_canceled"`
		AuthError      bool   `json:"auth_error"`
		ErrorKind      string `json:"error_kind"`
	}{terminal, out.Len(), fmt.Sprintf("%T", err), ctx.Err() == context.Canceled, errors.As(err, &failure), errorKind})
	if terminal == "unexpected" || out.Len() != 0 {
		os.Exit(3)
	}
	os.Exit(0)
}

type idleWaitProcess struct {
	cmd      *exec.Cmd
	done     chan error
	stdin    io.WriteCloser
	out      bytes.Buffer
	finished bool
}

func idleWaitStart(t *testing.T, cfg idleWaitProcessConfig) *idleWaitProcess {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private-wait")
	if privatefile.Mkdir(dir) != nil {
		t.Fatal("private wait directory unavailable")
	}
	path := filepath.Join(dir, "config.json")
	if privatefile.WriteJSON(path, cfg) != nil {
		t.Fatal("private wait configuration unavailable")
	}
	p := &idleWaitProcess{done: make(chan error, 1)}
	p.cmd = exec.Command(os.Args[0], "-test.run=^TestIdleNativeWaitProcessHelper$")
	p.cmd.Env = append(os.Environ(), "PAIRROOM_IDLE_NATIVE_WAIT_CONFIG="+path)
	p.cmd.Dir = cfg.Endpoint.Workspace
	p.cmd.Stdout = &p.out
	p.cmd.Stderr = io.Discard
	var err error
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil || p.cmd.Start() != nil {
		t.Fatal("actual wait process failed to start")
	}
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() {
		_ = p.stdin.Close()
		if !p.finished {
			_ = p.cmd.Process.Kill()
			<-p.done
			p.finished = true
		}
	})
	return p
}

func idleWaitSetupCommand(t *testing.T, cfg endpointProcessConfig, args ...string) []byte {
	t.Helper()
	cfg.Args = args
	p := endpointStart(t, cfg, false)
	select {
	case err := <-p.done:
		p.finished = true
		if err != nil {
			t.Fatal("actual endpoint setup command failed; private output withheld")
		}
		return append([]byte{}, p.stdout.Bytes()...)
	case <-time.After(20 * time.Second):
		t.Fatal("actual endpoint setup exceeded its budget")
		return nil
	}
}

func waitIngressDomain(t *testing.T, f *httpPGFixture) [32]byte {
	t.Helper()
	var joined strings.Builder
	for _, table := range []string{"rooms", "room_members", "bindings", "room_events", "operations", "relay_operations", "audit", "binding_operations", "binding_grants", "control_events", "control_operations", "device_grants"} {
		var data string
		if err := f.admin.QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM `+table+` x`).Scan(&data); err != nil {
			t.Fatal("private domain snapshot failed")
		}
		joined.WriteString(table)
		joined.WriteString(data)
	}
	return sha256.Sum256([]byte(joined.String()))
}

func TestIdleNativeWaitHTTPPGEightCLIProcessesKeepFixedConnections(t *testing.T) {
	f := newHTTPPGFixture(t)
	var mu sync.Mutex
	counts := map[string]int{}
	mutations := 0
	replayRefusals := 0
	activeHTTP, requestEpoch := 0, 0
	observe := false
	notify := make(chan struct{}, 1)
	original := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		observed := observe
		if observed {
			activeHTTP++
			requestEpoch++
		}
		mu.Unlock()
		response := &idleWaitResponse{ResponseWriter: w, status: http.StatusOK}
		original.ServeHTTP(response, r)
		mu.Lock()
		defer mu.Unlock()
		if !observed {
			return
		}
		activeHTTP--
		requestEpoch++
		for room := range counts {
			base := "/enterprise/device/v1/rooms/" + room + "/relay"
			if r.URL.Path == base+"/context" && r.Method == http.MethodGet && response.status == http.StatusOK {
				counts[room]++
				select {
				case notify <- struct{}{}:
				default:
				}
			}
			if r.URL.Path == base+"/context" && response.status == http.StatusTooManyRequests {
				var failure struct {
					Error string `json:"error"`
				}
				_ = json.Unmarshal(response.body.Bytes(), &failure)
				if failure.Error == "replay_capacity" {
					replayRefusals++
				}
			}
			if r.Method == http.MethodPost && (r.URL.Path == base || r.URL.Path == base+"/resolve") {
				mutations++
			}
		}
	})
	owner, member := f.browser("member-one"), f.browser("member-two")
	f.login(owner)
	f.login(member)
	o, m := f.session(owner), f.session(member)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.TLS.Certificates[0].Certificate[0]}), 0o600) != nil {
		t.Fatal("private fixture CA unavailable")
	}
	deviceDir := endpointDeviceLogin(t, f, member, m.CSRF, ca)
	const total = 8
	endpoints := make([]endpointProcessConfig, total)
	rooms := make([]string, total)
	for i := range endpoints {
		room := fmt.Sprintf("idle-wait-room-%d", i)
		rooms[i] = room
		f.createRoom(owner, o.CSRF, room)
		objectHTTPAdmit(t, f, owner, o.CSRF, room, m.UserID)
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal("fixture workspace unavailable")
		}
		if err := exec.Command("git", "init", "-q", root).Run(); err != nil {
			t.Fatal("fixture Git workspace unavailable")
		}
		e := endpointProcessConfig{Mode: "endpoint", Origin: f.origin, Deployment: f.cfg.DeploymentID, CA: ca, Workspace: root, DeviceDirectory: deviceDir, Home: t.TempDir(), Session: fmt.Sprintf("idle-official-session-%d", i)}
		idleWaitSetupCommand(t, e, "install", "--repo", root, "--runtime", "claude")
		bind := []string{"bind", "--repo", root, "--runtime", "claude", "--enterprise-service", e.Origin, "--deployment", e.Deployment, "--enterprise-ca", e.CA, "--enterprise-state-dir", deviceDir, "--room", room, "--slot", "1"}
		pending := idleWaitSetupCommand(t, e, bind...)
		var request struct {
			Status    string `json:"status"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(pending, &request) != nil || request.Status != "awaiting_owner" || request.RequestID == "" {
			t.Fatal("endpoint did not await exact Owner approval")
		}
		status, _ := f.post(owner, "/enterprise/api/v1/rooms/"+room+"/binding-requests/"+request.RequestID+"/decision", map[string]string{"client_id": "approve-" + request.RequestID, "action": "approve"}, o.CSRF)
		if status != 200 {
			t.Fatal("exact Owner approval failed")
		}
		ready := idleWaitSetupCommand(t, e, bind...)
		if bytes.Contains(ready, []byte(e.Session)) || bytes.Contains(ready, []byte(root)) {
			t.Fatal("private endpoint identity escaped foreground setup")
		}
		endpoints[i] = e
		counts[room] = 0
	}
	before := waitIngressDomain(t, f)
	var baseline int
	var baselinePIDs string
	if err := f.admin.QueryRow(t.Context(), `SELECT count(*),array_agg(pid ORDER BY pid)::text FROM pg_catalog.pg_stat_activity WHERE datname=current_database() AND backend_type='client backend'`).Scan(&baseline, &baselinePIDs); err != nil {
		t.Fatal("connection baseline unavailable")
	}
	mu.Lock()
	observe = true
	mu.Unlock()
	processes := make([]*idleWaitProcess, total)
	for i, e := range endpoints {
		timeout := "8"
		if i >= 6 {
			timeout = "15"
		}
		e.Args = []string{"wait", "--repo", e.Workspace, "--timeout", timeout}
		processes[i] = idleWaitStart(t, idleWaitProcessConfig{Endpoint: e, Cancelable: i == 6})
		// Start at a real completed-read boundary, avoiding an artificial burst
		// into the independent two-request replay admission cap. All eight
		// processes remain concurrent throughout the later observation window.
		started := time.NewTimer(3 * time.Second)
		for {
			mu.Lock()
			read := counts[rooms[i]] > 0
			mu.Unlock()
			if read {
				break
			}
			select {
			case <-notify:
			case <-started.C:
				t.Fatal("CLI did not reach its first actual context response")
			case <-processes[i].done:
				processes[i].finished = true
				t.Fatalf("CLI first read failed: %s", strings.TrimSpace(processes[i].out.String()))
			}
		}
		started.Stop()
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	sampler := time.NewTicker(20 * time.Millisecond)
	defer sampler.Stop()
	samples, maxConnections := 0, baseline
	for {
		mu.Lock()
		allRead := true
		for _, n := range counts {
			if n < 2 {
				allRead = false
			}
		}
		mu.Unlock()
		if allRead && samples >= 16 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("eight live CLI waits did not reach actual second context-read barrier")
		case <-notify:
		case <-sampler.C:
			for i, p := range processes {
				select {
				case <-p.done:
					p.finished = true
					mu.Lock()
					refusals := replayRefusals
					mu.Unlock()
					t.Fatalf("CLI wait %d ended before live connection sampling: %s; actual replay_capacity HTTP429 count=%d", i, strings.TrimSpace(p.out.String()), refusals)
				default:
				}
			}
			var connections, idleTX int
			var pids string
			mu.Lock()
			quietBefore, epoch := activeHTTP == 0, requestEpoch
			mu.Unlock()
			if err := f.admin.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER(WHERE state LIKE 'idle in transaction%'),array_agg(pid ORDER BY pid)::text FROM pg_catalog.pg_stat_activity WHERE datname=current_database() AND backend_type='client backend'`).Scan(&connections, &idleTX, &pids); err != nil {
				t.Fatal("actual wait connection sample unavailable")
			}
			if connections != baseline || pids != baselinePIDs {
				t.Fatalf("idle wait SQL sample: baseline=%d current=%d same-PIDs=%t idleTX=%d", baseline, connections, pids == baselinePIDs, idleTX)
			}
			mu.Lock()
			quiet := quietBefore && activeHTTP == 0 && requestEpoch == epoch
			mu.Unlock()
			// A normal multi-statement HTTP transaction can briefly be idle
			// between statements. Only a verified client-timer interval proves
			// whether waiting itself retains a SQL transaction.
			if quiet && idleTX != 0 {
				t.Fatal("client-side idle wait retained a SQL transaction outside any HTTP operation")
			}
			maxConnections = max(maxConnections, connections)
			if quiet {
				samples++
			}
		}
	}
	if waitIngressDomain(t, f) != before {
		t.Fatal("empty waits wrote Room/control/credential/claim effects")
	}
	if _, err := processes[6].stdin.Write([]byte{1}); err != nil {
		t.Fatal("real caller cancellation signal failed")
	}
	// Current membership removal runs the normal typed mutation. Snapshot after
	// it separates its intended retirement/audit from forbidden wait effects.
	status, body := f.get(owner, "/enterprise/api/v1/rooms/"+rooms[7])
	var roomView struct {
		Revision uint64 `json:"revision"`
	}
	if status != 200 || json.Unmarshal(body, &roomView) != nil {
		t.Fatal("remove revision unavailable")
	}
	status, _ = f.post(owner, "/enterprise/api/v1/rooms/"+rooms[7]+"/memberships", map[string]any{"client_id": "remove-live-wait-member", "expected_revision": roomView.Revision, "intent": map[string]string{"action": "remove", "target_user_id": m.UserID}}, o.CSRF)
	if status != 200 {
		t.Fatal("typed live wait member removal failed")
	}
	afterRemoval := waitIngressDomain(t, f)
	for i, p := range processes {
		select {
		case err := <-p.done:
			p.finished = true
			if err != nil {
				t.Fatalf("actual wait process %d terminal unexpected: %s; private output withheld", i, strings.TrimSpace(p.out.String()))
			}
		case <-time.After(10 * time.Second):
			t.Fatal("actual wait did not stop within finite/caller/revocation budget")
		}
		var result struct {
			Terminal    string `json:"terminal"`
			StdoutBytes int    `json:"stdout_bytes"`
		}
		want := "empty"
		if i == 6 {
			want = "caller_canceled"
		}
		if i == 7 {
			want = "authorization_refused"
		}
		if json.Unmarshal(p.out.Bytes(), &result) != nil || result.Terminal != want || result.StdoutBytes != 0 {
			t.Fatal("wait terminal classification or no-envelope stdout contract failed")
		}
	}
	if waitIngressDomain(t, f) != afterRemoval {
		t.Fatal("wait exit changed durable domain after authorized removal")
	}
	mu.Lock()
	posts := mutations
	refusals := replayRefusals
	reads := 0
	for _, n := range counts {
		reads += n
	}
	mu.Unlock()
	if posts != 0 {
		t.Fatal("empty wait issued claim, recovery or other relay mutation requests")
	}
	journals, err := filepath.Glob(filepath.Join(deviceDir, "bindings", "*", "delivery.json"))
	if err != nil || len(journals) != 0 {
		t.Fatal("empty waits created a private claim/delivery journal")
	}
	t.Logf("actual CLI wait processes=%d; context reads=%d; live quiet SQL samples=%d; baseline/max client backends=%d/%d; idle-in-transaction while HTTP inactive=0; replay refusals=%d; relay POSTs=0; delivery journals=0; finite/canceled/revoked terminals=6/1/1", total, reads, samples, baseline, maxConnections, refusals)
}
