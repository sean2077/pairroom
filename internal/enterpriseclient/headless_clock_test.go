package enterpriseclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
)

// headlessClockServer answers with an independent server wall clock. It reads
// the endpoint's own private pending candidate only to build matching receipts.
type headlessClockServer struct {
	mu           sync.Mutex
	srv          *httptest.Server
	c            *Client
	serverOffset time.Duration // server absolute clock minus local
	dropComplete bool
	receipt      *operationResponse // the one committed completion receipt
	pollScript   []string           // statuses per poll sequence
	lastResponse time.Time
	gaps         []time.Duration
	polls        int
	completes    int
}

func newHeadlessClockServer(t *testing.T, offset time.Duration, script []string) *headlessClockServer {
	p := &headlessClockServer{serverOffset: offset, pollScript: script}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.srv.Certificate().Raw}), 0600)
	var err error
	p.c, err = New(Config{Origin: p.srv.URL, DeploymentID: "clock", CAFile: ca, Directory: filepath.Join(t.TempDir(), "private"), OpenBrowser: func(string) error { return errors.New("no") }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.c.Close)
	return p
}

func (p *headlessClockServer) now() time.Time { return time.Now().UTC().Add(p.serverOffset) }

func (p *headlessClockServer) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(DeploymentHeader, "clock")
	if r.URL.Path == devicePath+"/deployment" {
		_ = json.NewEncoder(w).Encode(map[string]string{"deployment_id": "clock", "origin": p.srv.URL})
		return
	}
	s, _ := p.c.read()
	pd := s.Pending
	p.mu.Lock()
	defer p.mu.Unlock()
	expiry := p.now().Add(10 * time.Minute)
	if pd.ExpiresAt.IsZero() == false {
		expiry = pd.ExpiresAt
	}
	res := operationResponse{Outcome: "committed", TransportMode: "headless", RequestID: pd.RequestID, ExpiresAt: expiry}
	status := 200
	switch {
	case r.URL.Path == devicePath+"/enrollments":
		a, b := sha256.Sum256([]byte(pd.Next.Access)), sha256.Sum256([]byte(pd.Next.Refresh))
		res.Kind, res.Status, res.DeviceID = "device.enrollment.begin", "pending", pd.DeviceID
		res.VerificationURL = p.srv.URL + "/enterprise/device-enrollments/" + pd.RequestID
		res.CandidateFingerprint = enterpriseauth.HeadlessCandidateFingerprint(pd.Next.GrantID, a, b)
		res.NextPollAt = p.now().Add(5 * time.Second)
	case strings.HasSuffix(r.URL.Path, "/poll"):
		p.gaps = append(p.gaps, time.Since(p.lastResponse))
		var req headlessPollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		st := p.pollScript[p.polls]
		p.polls++
		res.Kind, res.Status, res.Sequence = "device.enrollment.headless.poll", st, req.Sequence
		if st != "approved" {
			res.NextPollAt = p.now().Add(5 * time.Second)
		}
		if st == "slow_down" {
			status = http.StatusTooManyRequests
		}
	case strings.HasSuffix(r.URL.Path, "/headless-complete"):
		p.completes++
		// The first completion commits one receipt on the server clock; every
		// recovery replays that same accepted receipt.
		if p.receipt == nil {
			receipt := res
			receipt.Kind, receipt.Status = "device.enrollment.headless.complete", "completed"
			receipt.DeviceID, receipt.UserID, receipt.GrantID = pd.DeviceID, "clock-user", pd.Next.GrantID
			receipt.ExpiresAt, receipt.RefreshExpiresAt = p.now().Add(15*time.Minute), p.now().Add(30*24*time.Hour)
			p.receipt = &receipt
		}
		res = *p.receipt
		if p.dropComplete {
			p.dropComplete = false
			panic(http.ErrAbortHandler)
		}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
	p.lastResponse = time.Now()
}

func headlessClockShow(HeadlessVerification) error { return nil }

func headlessClockPending(t *testing.T, c *Client) pending {
	t.Helper()
	s, err := c.read()
	if err != nil || s.Pending == nil || s.Active != nil {
		t.Fatal("headless journal was not retained as pending", err)
	}
	return *s.Pending
}

// Every poll, not only the first, follows at least five local monotonic
// seconds after the preceding confirmed response, including slow_down.
func TestHeadlessEveryPollWaitsMonotonicIntervalAfterEachResponse(t *testing.T) {
	s := newHeadlessClockServer(t, -30*time.Second, []string{"pending", "slow_down", "approved"})
	status, err := s.c.LoginHeadless(context.Background(), "clock", false, headlessClockShow)
	if err != nil || status.State != "active" || len(s.gaps) != 3 {
		t.Fatalf("state=%s polls=%d err=%v", status.State, len(s.gaps), err)
	}
	for i, gap := range s.gaps {
		if gap < 5*time.Second {
			t.Fatalf("poll %d followed its previous response after %s", i+1, gap)
		}
	}
}

// A Service clock modestly ahead neither stalls polling nor strands the
// committed receipt: after a lost response, explicit recovery activates the
// original candidate from the same accepted receipt.
func TestHeadlessServerClockAheadWithinAllowanceRecoversOriginalReceipt(t *testing.T) {
	for _, offset := range []time.Duration{90 * time.Second, 2 * time.Minute} {
		s := newHeadlessClockServer(t, offset, []string{"pending", "approved"})
		s.dropComplete = true
		started := time.Now()
		status, err := s.c.LoginHeadless(context.Background(), "clock", false, headlessClockShow)
		if err == nil || status.State != "pending_completion_unknown" || time.Since(started) > 20*time.Second {
			t.Fatalf("offset %s: lost completion state=%s err=%v elapsed=%s", offset, status.State, err, time.Since(started))
		}
		before := headlessClockPending(t, s.c)
		status, err = s.c.LoginHeadless(context.Background(), "", true, headlessClockShow)
		after, readErr := s.c.read()
		if err != nil || readErr != nil || status.State != "active" || s.completes != 2 || after.Active == nil {
			t.Fatalf("offset %s: recovery state=%s completes=%d err=%v", offset, status.State, s.completes, err)
		}
		if after.Active.Access != before.Next.Access || after.Active.GrantID != s.receipt.GrantID || !after.Active.ExpiresAt.Equal(s.receipt.ExpiresAt) || after.Proof != before.Proof || after.DeviceID != before.DeviceID {
			t.Fatalf("offset %s: recovery did not activate the original candidate from the original receipt", offset)
		}
	}
}

// A receipt still beyond the expiry ceilings stays unverifiable: unknown with
// the journal unchanged on every replay. Explicit recovery after the endpoint
// clock is corrected activates the same receipt without a new credential.
func TestHeadlessReceiptBeyondAllowanceStaysUnknownUntilClockCorrected(t *testing.T) {
	s := newHeadlessClockServer(t, 10*time.Minute, []string{"approved"})
	var operation *Error
	status, err := s.c.LoginHeadless(context.Background(), "clock", false, headlessClockShow)
	if !errors.As(err, &operation) || operation.Outcome != "unknown" || operation.Class != "receipt_mismatch" || status.State != "pending_completion_unknown" {
		t.Fatalf("receipt beyond allowance: state=%s err=%v", status.State, err)
	}
	original := headlessClockPending(t, s.c)
	status, err = s.c.LoginHeadless(context.Background(), "", true, headlessClockShow)
	if !errors.As(err, &operation) || operation.Class != "receipt_mismatch" || status.State != "pending_completion_unknown" || s.completes != 2 {
		t.Fatalf("recovery beyond allowance: state=%s completes=%d err=%v", status.State, s.completes, err)
	}
	if retained := headlessClockPending(t, s.c); retained.RequestID != original.RequestID || retained.Proof != original.Proof || retained.Next != original.Next || retained.ClientID != original.ClientID {
		t.Fatal("unverifiable receipt changed the retained enrollment journal")
	}
	corrected := receiptNow
	receiptNow = func() time.Time { return corrected().Add(10 * time.Minute) }
	t.Cleanup(func() { receiptNow = corrected })
	status, err = s.c.LoginHeadless(context.Background(), "", true, headlessClockShow)
	after, readErr := s.c.read()
	if err != nil || readErr != nil || status.State != "active" || s.completes != 3 || after.Active == nil || after.Active.Access != original.Next.Access || after.Active.GrantID != s.receipt.GrantID {
		t.Fatalf("corrected-clock recovery: state=%s completes=%d err=%v", status.State, s.completes, err)
	}
}

// The shared V1 completion, headless completion and refresh check widens only
// the two upper ceilings; expiry already past and refresh-before-access stay
// refused.
func TestReceiptExpiryCeilingsWidenOnlyByAllowance(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	restore := receiptNow
	receiptNow = func() time.Time { return now }
	t.Cleanup(func() { receiptNow = restore })
	access, refresh := 16*time.Minute+receiptClockTolerance, 31*24*time.Hour+receiptClockTolerance
	for _, c := range []struct {
		name            string
		expiry, refresh time.Duration
		valid           bool
	}{
		{"nominal", 15 * time.Minute, 30 * 24 * time.Hour, true},
		{"access ceiling", access, refresh, true},
		{"access beyond ceiling", access + time.Nanosecond, refresh, false},
		{"refresh beyond ceiling", 15 * time.Minute, refresh + time.Nanosecond, false},
		{"already expired", 0, 30 * 24 * time.Hour, false},
		{"refresh before access", 15 * time.Minute, 15*time.Minute - time.Nanosecond, false},
	} {
		if got := validExpiry(operationResponse{ExpiresAt: now.Add(c.expiry), RefreshExpiresAt: now.Add(c.refresh)}); got != c.valid {
			t.Errorf("%s: valid=%t, want %t", c.name, got, c.valid)
		}
	}
}
