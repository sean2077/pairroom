package enterpriseclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/privatefile"
)

type headlessWireFixture struct {
	t                 *testing.T
	server            *httptest.Server
	client            *Client
	mu                sync.Mutex
	posts             []string
	drop              string
	lastSequence      uint64
	overwritten       bool
	completeID        string
	expiry            time.Time
	browserCalls      int
	verification      []HeadlessVerification
	clockOffset       time.Duration
	clockGate         bool
	beginObserved     time.Time
	firstPollObserved time.Time
	clockViolations   int
}

func newHeadlessWireFixture(t *testing.T) *headlessWireFixture {
	t.Helper()
	f := &headlessWireFixture{t: t, expiry: time.Now().UTC().Add(10 * time.Minute)}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	f.client, err = New(Config{Origin: f.server.URL, DeploymentID: "headless-deployment", CAFile: ca, Directory: filepath.Join(t.TempDir(), "private"), OpenBrowser: func(string) error { f.browserCalls++; return errors.New("headless must not open browser") }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.client.Close)
	return f
}

func (f *headlessWireFixture) show(value HeadlessVerification) error {
	f.verification = append(f.verification, value)
	if strings.Contains(value.VerificationURL, value.ConfirmationCode) || !validHeadlessID(strings.TrimPrefix(value.VerificationURL, f.server.URL+"/enterprise/device-enrollments/")) {
		f.t.Fatal("unsafe verification URL")
	}
	return nil
}

func (f *headlessWireFixture) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(DeploymentHeader, "headless-deployment")
	if r.URL.Path == devicePath+"/deployment" {
		_ = json.NewEncoder(w).Encode(map[string]string{"deployment_id": "headless-deployment", "origin": f.server.URL})
		return
	}
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
		f.t.Error("headless endpoint mixed authorities")
		http.Error(w, "invalid", 400)
		return
	}
	s, err := f.client.read()
	if err != nil || s.Pending == nil || s.Pending.TransportMode != "headless" {
		f.t.Error("headless effect before private intent persisted")
		http.Error(w, "invalid", 500)
		return
	}
	p := s.Pending
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, r.URL.Path)
	result := operationResponse{Outcome: "committed", TransportMode: "headless", RequestID: p.RequestID, ExpiresAt: f.expiry}
	switch {
	case r.URL.Path == devicePath+"/enrollments":
		var request headlessBeginRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.RequestID != p.RequestID || request.ProofHash != verifier(p.Proof) || request.CallbackURI != "" || request.ClientState != "" || request.TransportMode != "headless" || request.Credential != p.Next.candidate() {
			f.t.Error("begin differs from durable frozen intent")
		}
		hash, _ := enterpriseauth.HeadlessConfirmationHash(string(p.ConfirmationCode))
		if request.ConfirmationHash != encodeHeadlessHash(hash) {
			f.t.Error("confirmation not frozen")
		}
		a, b := sha256.Sum256([]byte(p.Next.Access)), sha256.Sum256([]byte(p.Next.Refresh))
		result.Kind, result.Status, result.DeviceID = "device.enrollment.begin", "pending", p.DeviceID
		result.VerificationURL = f.server.URL + "/enterprise/device-enrollments/" + p.RequestID
		result.CandidateFingerprint = enterpriseauth.HeadlessCandidateFingerprint(p.Next.GrantID, a, b)
		// This wire fixture tests sequencing, not the SQL five-second policy.
		f.beginObserved = time.Now()
		result.NextPollAt = time.Now().UTC().Add(time.Millisecond)
		if f.clockGate {
			result.NextPollAt = time.Now().UTC().Add(f.clockOffset).Add(5 * time.Second)
		}
	case strings.HasSuffix(r.URL.Path, "/poll") || strings.HasSuffix(r.URL.Path, "/resolve-poll"):
		var request headlessPollRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.ClientProof != p.Proof || request.Sequence != p.PollSequence || p.PollIntentHash == "" {
			f.t.Error("poll differs from journaled observation")
		}
		if strings.HasSuffix(r.URL.Path, "/resolve-poll") {
			if f.overwritten {
				_ = json.NewEncoder(w).Encode(operationResponse{Outcome: "unknown", Class: "poll_observation_unavailable"})
				return
			}
			if request.Sequence != f.lastSequence {
				f.t.Error("recovery changed original poll sequence")
			}
			result.Outcome = "replayed"
		} else {
			f.lastSequence = request.Sequence
		}
		result.Kind, result.Status, result.Sequence = "device.enrollment.headless.poll", "approved", request.Sequence
		if f.firstPollObserved.IsZero() {
			f.firstPollObserved = time.Now()
		}
		if f.clockGate && time.Since(f.beginObserved) < 5*time.Second {
			f.clockViolations++
			result.Status, result.NextPollAt = "slow_down", f.beginObserved.Add(f.clockOffset).Add(5*time.Second)
			if f.clockViolations >= 5 {
				result.Status, result.Disposition = "invalidated", "polling_exhausted"
			}
			w.WriteHeader(http.StatusTooManyRequests)
		}
	case strings.HasSuffix(r.URL.Path, "/headless-complete"):
		var request headlessCompleteRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.ClientID != p.ClientID || request.ClientProof != p.Proof || request.ConfirmationCode != p.ConfirmationCode {
			f.t.Error("completion changed original proof/code/identity")
		}
		if f.completeID != "" && request.ClientID != f.completeID {
			f.t.Error("recovery allocated new completion")
		}
		if f.completeID != "" {
			result.Outcome = "replayed"
		}
		f.completeID = request.ClientID
		result.Kind, result.Status = "device.enrollment.headless.complete", "completed"
		result.DeviceID, result.UserID, result.GrantID = p.DeviceID, "headless-user", p.Next.GrantID
		result.ExpiresAt, result.RefreshExpiresAt = time.Now().UTC().Add(15*time.Minute), time.Now().UTC().Add(30*24*time.Hour)
	default:
		http.NotFound(w, r)
		return
	}
	if f.drop != "" && strings.HasSuffix(r.URL.Path, "/"+f.drop) {
		closeResponse(w)
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}

func TestHeadlessClockAheadStillWaitsForMonotonicPollingInterval(t *testing.T) {
	f := newHeadlessWireFixture(t)
	f.clockOffset = -30 * time.Second
	f.clockGate = true
	f.expiry = f.expiry.Add(f.clockOffset)
	status, err := f.client.LoginHeadless(context.Background(), "Server endpoint", false, f.show)
	if err != nil || status.State != "active" || f.firstPollObserved.Sub(f.beginObserved) < 5*time.Second || f.clockViolations != 0 {
		t.Fatalf("client used skewed absolute clock for polling: state=%s early=%d elapsed=%s error=%v", status.State, f.clockViolations, f.firstPollObserved.Sub(f.beginObserved), err)
	}
}

func TestHeadlessAdditionReadsLiteralV1LoopbackJournalAndOriginalHash(t *testing.T) {
	// Fixed pre-headless field order and SHA256, computed from the original V1
	// JSON contract independently of pending.intentHash. No new optional fields.
	const golden = `{"version":1,"origin":"https://service.example.test","deployment_id":"legacy-deployment","revision":1,"pending":{"kind":"enrollment","phase":"prepared","request_id":"request-legacy","client_id":"complete-legacy","device_id":"device-legacy","label":"Legacy endpoint","callback_uri":"http://127.0.0.1:49152/pairroom-device-complete","client_state":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI","proof":"AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM","next":{"grant_id":"grant-legacy","access":"BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ","refresh":"BQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQU","expires_at":"0001-01-01T00:00:00Z","refresh_expires_at":"0001-01-01T00:00:00Z"},"created_at":"2026-10-01T00:00:00Z","intent_hash":"4304fde77783770a718b541644575aa737ae9ccc3202c291f79860ecbfd79ca3"}}`
	c, err := New(Config{Origin: "https://service.example.test", DeploymentID: "legacy-deployment", Directory: filepath.Join(t.TempDir(), "private")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := privatefile.WriteJSON(filepath.Join(c.dir, "device.json"), json.RawMessage(golden)); err != nil {
		t.Fatal(err)
	}
	status, err := c.Status()
	if err != nil || status.State != "pending_prepared" || status.PendingOperationID != "complete-legacy" || status.DeviceID != "device-legacy" {
		t.Fatalf("literal V1 compatibility failed: %v", err)
	}
	s, err := c.read()
	if err != nil || s.Pending.TransportMode != "" || s.Pending.CallbackURI != "http://127.0.0.1:49152/pairroom-device-complete" || s.Pending.IntentHash != "4304fde77783770a718b541644575aa737ae9ccc3202c291f79860ecbfd79ca3" {
		t.Fatal("V1 intent was relabeled or rewritten")
	}
	invalid := strings.Replace(golden, `"phase":"prepared"`, `"phase":"poll_unknown"`, 1)
	if err := privatefile.WriteJSON(filepath.Join(c.dir, "device.json"), json.RawMessage(invalid)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(); !errors.Is(err, ErrPrivateState) {
		t.Fatal("headless-only phase was inferred for legacy loopback journal")
	}
}

func TestHeadlessConfirmedCounterErrorDoesNotClaimCredentialSuccessOrUncertainty(t *testing.T) {
	for _, outcome := range []string{"committed", "replayed"} {
		text := (&Error{Outcome: outcome, Class: "polling_exhausted"}).Error()
		if !strings.Contains(text, "counter transaction was confirmed") || !strings.Contains(text, "invalidated") || strings.Contains(text, "unknown") {
			t.Fatal("confirmed counter result was ambiguous")
		}
	}
}

func TestHeadlessLoginPersistsCandidatesAndNeverOpensBrowser(t *testing.T) {
	f := newHeadlessWireFixture(t)
	status, err := f.client.LoginHeadless(context.Background(), "Server endpoint", false, f.show)
	if err != nil || status.State != "active" || f.browserCalls != 0 || len(f.verification) != 1 {
		t.Fatalf("headless flow: %v", err)
	}
	s, err := f.client.read()
	if err != nil || s.Active == nil || s.Pending != nil {
		t.Fatal("headless activation missing")
	}
	encoded, _ := json.Marshal(status)
	for _, value := range []string{string(s.Proof), string(s.Active.Access), string(s.Active.Refresh), f.verification[0].ConfirmationCode, strings.ReplaceAll(f.verification[0].ConfirmationCode, "-", "")} {
		if strings.Contains(string(encoded), value) {
			t.Fatal("public status exposed headless material")
		}
	}
	if strings.Contains(fmt.Sprintf("%v %#v", f.verification[0], f.verification[0]), f.verification[0].ConfirmationCode) {
		t.Fatal("ordinary formatting exposed confirmation")
	}
}

func TestHeadlessUnknownPollRequiresOriginalExplicitResolve(t *testing.T) {
	f := newHeadlessWireFixture(t)
	f.drop = "poll"
	status, err := f.client.LoginHeadless(context.Background(), "Server endpoint", false, f.show)
	var operation *Error
	if !errors.As(err, &operation) || operation.Outcome != "unknown" || status.State != "pending_poll_unknown" {
		t.Fatalf("poll loss: %v", err)
	}
	before, _ := f.client.read()
	count := len(f.posts)
	if _, err = f.client.LoginHeadless(context.Background(), "replacement", false, f.show); !errors.Is(err, ErrPending) || len(f.posts) != count {
		t.Fatal("ordinary login retried uncertain poll")
	}
	if _, err = f.client.Login(context.Background(), "", true); !errors.Is(err, ErrPending) || len(f.posts) != count {
		t.Fatal("loopback recovery retargeted headless")
	}
	f.drop = ""
	status, err = f.client.LoginHeadless(context.Background(), "", true, f.show)
	if err != nil || status.State != "active" || !strings.HasSuffix(f.posts[count], "/resolve-poll") {
		t.Fatalf("original poll resolve: %v", err)
	}
	after, _ := f.client.read()
	if after.Active.GrantID != before.Pending.Next.GrantID || after.Proof != before.Pending.Proof || f.browserCalls != 0 {
		t.Fatal("recovery replaced endpoint candidate")
	}
}

func TestHeadlessOverwrittenUnknownPollStaysUnknownAndRetainsJournal(t *testing.T) {
	f := newHeadlessWireFixture(t)
	f.drop = "poll"
	_, _ = f.client.LoginHeadless(context.Background(), "Server endpoint", false, f.show)
	before, _ := f.client.read()
	count := len(f.posts)
	f.drop = ""
	f.overwritten = true
	status, err := f.client.LoginHeadless(context.Background(), "", true, f.show)
	var operation *Error
	if !errors.As(err, &operation) || operation.Outcome != "unknown" || operation.Class != "poll_observation_unavailable" || status.State != "pending_poll_unknown" {
		t.Fatalf("overwritten observation misclassified: %v", err)
	}
	after, _ := f.client.read()
	if after.Pending == nil || after.Pending.PollSequence != before.Pending.PollSequence || after.Pending.ClientID != before.Pending.ClientID || len(f.posts) != count+1 || !strings.HasSuffix(f.posts[count], "/resolve-poll") {
		t.Fatal("overwritten observation caused new effect")
	}
}

func TestHeadlessLostCompletionKeepsOriginalCandidateAndOneStableIdentity(t *testing.T) {
	f := newHeadlessWireFixture(t)
	f.drop = "headless-complete"
	status, err := f.client.LoginHeadless(context.Background(), "Server endpoint", false, f.show)
	var operation *Error
	if !errors.As(err, &operation) || operation.Outcome != "unknown" || status.State != "pending_completion_unknown" {
		t.Fatalf("completion loss: %v", err)
	}
	before, _ := f.client.read()
	count := len(f.posts)
	f.drop = ""
	status, err = f.client.LoginHeadless(context.Background(), "", true, f.show)
	if err != nil || status.State != "active" || len(f.posts) != count+1 || !strings.HasSuffix(f.posts[count], "/headless-complete") {
		t.Fatalf("original completion recovery: %v", err)
	}
	after, _ := f.client.read()
	if after.Active.Access != before.Pending.Next.Access || after.Active.GrantID != before.Pending.Next.GrantID || f.completeID != before.Pending.ClientID {
		t.Fatal("completion recovery replaced credentials")
	}
}
