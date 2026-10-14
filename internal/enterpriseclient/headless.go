package enterpriseclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
)

// HeadlessVerification is shown only by an explicit foreground login. The
// code is a human confirmation, never a bearer credential or endpoint proof.
type HeadlessVerification struct {
	VerificationURL, DeviceID, CandidateFingerprint string
	ConfirmationCode                                string `json:"-"`
	ExpiresAt                                       time.Time
}

func (HeadlessVerification) String() string {
	return "enterprise headless verification <code redacted>"
}
func (HeadlessVerification) GoString() string {
	return "enterprise headless verification <code redacted>"
}

type headlessBeginRequest struct {
	beginRequest
	TransportMode    string `json:"transport_mode"`
	ConfirmationHash string `json:"confirmation_hash"`
}
type headlessPollRequest struct {
	ClientProof secret `json:"client_proof"`
	Sequence    uint64 `json:"sequence"`
}
type headlessCompleteRequest struct {
	ClientID         string `json:"client_id"`
	ClientProof      secret `json:"client_proof"`
	ConfirmationCode secret `json:"confirmation_code"`
}

func validHeadlessID(id string) bool {
	if len(id) != 40 || !strings.HasPrefix(id, "request-") {
		return false
	}
	data, err := hex.DecodeString(id[8:])
	return err == nil && len(data) == 16 && hex.EncodeToString(data) == id[8:]
}

func newHeadlessID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("enterprise random source unavailable")
	}
	return "request-" + hex.EncodeToString(random[:]), nil
}

func encodeHeadlessHash(hash [32]byte) string {
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// headlessPost preserves confirmed counter writes even when HTTP reports a
// refused human confirmation or rate limit. It never treats those writes as a
// grant, a non-commit, or an uncertain credential issuance.
func (c *Client) headlessPost(ctx context.Context, path string, body any) (operationResponse, error) {
	status, data, err := c.request(ctx, http.MethodPost, path, body, "", 64<<10)
	if err != nil {
		return operationResponse{}, err
	}
	var result operationResponse
	if decodeBounded(strings.NewReader(string(data)), &result) != nil {
		return operationResponse{}, unknown("invalid_response")
	}
	if result.confirmed() && (status == http.StatusOK || status == http.StatusTooManyRequests || status == http.StatusBadRequest) {
		if result.TransportMode != "headless" || result.Sequence > 120 || result.CompletionLocation != "" {
			return operationResponse{}, unknown("receipt_mismatch")
		}
		switch result.Disposition {
		case "", "confirmation_rejected", "verification_exhausted", "polling_exhausted":
		default:
			return operationResponse{}, unknown("receipt_mismatch")
		}
		if status == http.StatusBadRequest && result.Disposition != "confirmation_rejected" || status == http.StatusTooManyRequests && result.Status != "slow_down" && result.Disposition != "verification_exhausted" && result.Disposition != "polling_exhausted" {
			return operationResponse{}, unknown("receipt_mismatch")
		}
		return result, nil
	}
	if result.Outcome == "not_committed" || result.Outcome == "refused" || result.Outcome == "conflict" || result.Outcome == "unknown" {
		return result, &Error{Outcome: result.Outcome, Class: safeClass(result.Class)}
	}
	return operationResponse{}, unknown("operation_unconfirmed")
}

func (c *Client) headlessFailure(s *state, err error, phase string) (Status, error) {
	var operation *Error
	if errors.As(err, &operation) && operation.Outcome == "not_committed" {
		s.Pending.Phase = strings.TrimSuffix(phase, "_unknown") + "_not_committed"
	} else {
		s.Pending.Phase = phase
	}
	if saveErr := c.save(s); saveErr != nil {
		return s.status(), saveErr
	}
	return s.status(), err
}

func waitHeadlessPoll(ctx context.Context) error {
	// The server's absolute clock may differ from this endpoint's clock. Every
	// new observation waits at least five monotonic seconds after its preceding
	// confirmed response, including slow_down. NextPollAt is metadata, not a
	// local permission to bypass the server's persisted rate fence.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// LoginHeadless owns no listener or browser. Only this explicit foreground
// call may poll; an unknown result preserves the exact saved sequence and
// exits, requiring the user to explicitly recover that observation.
func (c *Client) LoginHeadless(parent context.Context, label string, recover bool, show func(HeadlessVerification) error) (Status, error) {
	if show == nil {
		return Status{}, ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	unlock, err := c.lock(ctx)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	s, err := c.read()
	if err != nil {
		return Status{}, err
	}
	if recover {
		if s.Pending == nil || s.Pending.Kind != "enrollment" || s.Pending.TransportMode != "headless" {
			return s.status(), ErrPending
		}
	} else {
		if s.Pending != nil {
			if !s.LoggedOut || s.LogoutAt.IsZero() {
				return s.status(), ErrPending
			}
			if len(s.Retained) >= maxRetainedOperations {
				return s.status(), ErrRetainedLimit
			}
			s.Retained = append(s.Retained, retainedOperation{Origin: s.Origin, DeploymentID: s.DeploymentID, RetainedAt: time.Now().UTC(), LogoutAt: s.LogoutAt, Resolution: "unresolved", Operation: *s.Pending})
			s.Pending = nil
		}
		if s.Active != nil && !s.LoggedOut {
			return s.status(), ErrAlreadyLoggedIn
		}
		if label == "" || len(label) > 128 || !utf8.ValidString(label) || strings.TrimSpace(label) != label || strings.ContainsFunc(label, unicode.IsControl) {
			return s.status(), ErrConfiguration
		}
		proof, err := newSecret()
		if err != nil {
			return s.status(), err
		}
		code, err := enterpriseauth.NewHeadlessConfirmationCode()
		if err != nil {
			return s.status(), err
		}
		next, err := newCredential()
		if err != nil {
			return s.status(), err
		}
		deviceID, err := newID("device")
		if err != nil {
			return s.status(), err
		}
		requestID, err := newHeadlessID()
		if err != nil {
			return s.status(), err
		}
		clientID, err := newID("complete")
		if err != nil {
			return s.status(), err
		}
		p := &pending{Kind: "enrollment", TransportMode: "headless", Phase: "prepared", RequestID: requestID, ClientID: clientID, DeviceID: deviceID, Label: label, Proof: proof, ConfirmationCode: secret(code), Next: next, CreatedAt: time.Now().UTC()}
		p.IntentHash = p.intentHash(c.origin, c.deployment)
		s.Pending, s.LoggedOut, s.LogoutAt = p, false, time.Time{}
		if err := c.save(&s); err != nil {
			return s.status(), err
		}
	}
	p := s.Pending
	if p.Phase == "terminal" {
		return s.status(), &Error{Outcome: "refused", Class: "invalid_control_state"}
	}
	if p.Phase == "prepared" || p.Phase == "begin_unknown" || p.Phase == "begin_not_committed" {
		confirmation, _ := enterpriseauth.HeadlessConfirmationHash(string(p.ConfirmationCode))
		request := headlessBeginRequest{beginRequest: beginRequest{RequestID: p.RequestID, DeviceID: p.DeviceID, Label: p.Label, ProofHash: verifier(p.Proof), Credential: p.Next.candidate()}, TransportMode: "headless", ConfirmationHash: encodeHeadlessHash(confirmation)}
		result, err := c.headlessPost(ctx, devicePath+"/enrollments", request)
		if err != nil {
			return c.headlessFailure(&s, err, "begin_unknown")
		}
		access, refresh := sha256.Sum256([]byte(p.Next.Access)), sha256.Sum256([]byte(p.Next.Refresh))
		expected := enterpriseauth.HeadlessCandidateFingerprint(p.Next.GrantID, access, refresh)
		if result.Kind != "device.enrollment.begin" || result.RequestID != p.RequestID || result.DeviceID != p.DeviceID || result.Status != "pending" || result.Disposition != "" || result.Sequence != 0 || result.VerificationURL != c.origin+"/enterprise/device-enrollments/"+p.RequestID || result.CandidateFingerprint != expected || !result.ExpiresAt.After(time.Now()) || result.NextPollAt.IsZero() || result.NextPollAt.After(result.ExpiresAt) {
			return c.headlessFailure(&s, unknown("receipt_mismatch"), "begin_unknown")
		}
		p.ExpiresAt, p.NextPollAt, p.Phase = result.ExpiresAt.UTC(), result.NextPollAt.UTC(), "awaiting_approval"
		if err := c.save(&s); err != nil {
			return s.status(), err
		}
	}
	access, refresh := sha256.Sum256([]byte(p.Next.Access)), sha256.Sum256([]byte(p.Next.Refresh))
	code := string(p.ConfirmationCode)
	if err := show(HeadlessVerification{VerificationURL: c.origin + "/enterprise/device-enrollments/" + p.RequestID, DeviceID: p.DeviceID, CandidateFingerprint: enterpriseauth.HeadlessCandidateFingerprint(p.Next.GrantID, access, refresh), ConfirmationCode: code[:4] + "-" + code[4:], ExpiresAt: p.ExpiresAt}); err != nil {
		return s.status(), errors.New("headless verification could not be displayed; original enrollment is retained")
	}
	for p.Phase != "completion_ready" && p.Phase != "completion_unknown" && p.Phase != "completion_not_committed" {
		resolving := p.Phase == "poll_unknown"
		if !resolving && p.Phase != "poll_ready" && p.Phase != "poll_not_committed" {
			if p.PollSequence >= 120 {
				return s.status(), &Error{Outcome: "refused", Class: "polling_exhausted"}
			}
			if err := waitHeadlessPoll(ctx); err != nil {
				return s.status(), err
			}
			p.PollSequence++
			p.PollIntentHash = p.pollIntentHash(c.origin, c.deployment)
			p.Phase = "poll_ready"
			if err := c.save(&s); err != nil {
				return s.status(), err
			}
		}
		path := devicePath + "/enrollments/" + p.RequestID + "/poll"
		if resolving {
			path = devicePath + "/enrollments/" + p.RequestID + "/resolve-poll"
		}
		result, err := c.headlessPost(ctx, path, headlessPollRequest{ClientProof: p.Proof, Sequence: p.PollSequence})
		if err != nil {
			return c.headlessFailure(&s, err, "poll_unknown")
		}
		if result.Kind != "device.enrollment.headless.poll" || result.RequestID != p.RequestID || result.Sequence != p.PollSequence || !result.ExpiresAt.Equal(p.ExpiresAt) || result.UserID != "" || result.GrantID != "" || result.DeviceID != "" {
			return c.headlessFailure(&s, unknown("receipt_mismatch"), "poll_unknown")
		}
		if result.Disposition != "" || result.Status == "denied" || result.Status == "invalidated" || result.Status == "expired" {
			p.Phase = "terminal"
			if err := c.save(&s); err != nil {
				return s.status(), err
			}
			class := "invalid_control_state"
			if result.Status == "expired" {
				class = "enrollment_expired"
			}
			if result.Disposition != "" {
				class = safeClass(result.Disposition)
			}
			return s.status(), &Error{Outcome: string(result.Outcome), Class: class}
		}
		if result.Status == "approved" {
			p.Phase = "completion_ready"
		} else if result.Status == "pending" || result.Status == "slow_down" {
			if result.NextPollAt.IsZero() || result.NextPollAt.After(p.ExpiresAt) {
				return c.headlessFailure(&s, unknown("receipt_mismatch"), "poll_unknown")
			}
			p.NextPollAt, p.Phase = result.NextPollAt.UTC(), "poll_confirmed"
		} else {
			return c.headlessFailure(&s, unknown("receipt_mismatch"), "poll_unknown")
		}
		if err := c.save(&s); err != nil {
			return s.status(), err
		}
	}
	result, err := c.headlessPost(ctx, devicePath+"/enrollments/"+p.RequestID+"/headless-complete", headlessCompleteRequest{ClientID: p.ClientID, ClientProof: p.Proof, ConfirmationCode: p.ConfirmationCode})
	if err != nil {
		return c.headlessFailure(&s, err, "completion_unknown")
	}
	if result.Kind != "device.enrollment.headless.complete" || result.RequestID != p.RequestID || result.DeviceID != p.DeviceID || result.GrantID != p.Next.GrantID || !validID(result.UserID) || result.Status != "completed" || result.Disposition != "" || !validExpiry(result) {
		return c.headlessFailure(&s, unknown("receipt_mismatch"), "completion_unknown")
	}
	return c.activate(&s, result)
}
