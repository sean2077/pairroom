// Package lanshare defines the deliberately small Native LAN transport. An
// invitation is public routing information; only owner-admitted certificate
// keys can read or write a Room.
package lanshare

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

const Version = 1
const MaxResponseBytes = 8 << 20

// HostUnavailableCode marks a host-side refusal the member cannot fix: the
// Room's runtime cannot be served right now, while the admission itself stays
// valid. It must not be reported as an authentication failure.
const HostUnavailableCode = "lan_host_unavailable"

// EndpointForAddress preserves a numeric listener address while escaping an
// IPv6 interface zone as required by a URL. The address itself still passes
// ValidateEndpoint before any connection or listener is admitted.
func EndpointForAddress(address string) string {
	return (&url.URL{Scheme: "https", Host: address}).String()
}

type Invite struct {
	Version   int       `json:"version"`
	Endpoint  string    `json:"endpoint"`
	HostPin   string    `json:"host_pin"`
	RoomID    string    `json:"room_id"`
	InviteID  string    `json:"invite_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func ValidID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// ValidateEndpoint does no DNS lookup and never accepts a callback, path,
// credential, public Internet target, wildcard, or ambient proxy destination.
func ValidateEndpoint(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New("LAN endpoint must be a numeric private HTTPS address with a port")
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return errors.New("LAN endpoint requires a numeric address and port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.IsUnspecified() || ip.IsMulticast() || !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return errors.New("LAN endpoint must use a numeric private or link-local address")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("LAN endpoint port is invalid")
	}
	return nil
}

func (v Invite) Validate() error {
	if v.Version != Version || !ValidID(v.RoomID) || !ValidID(v.InviteID) || !ValidFingerprint(v.HostPin) || v.ExpiresAt.IsZero() {
		return errors.New("invalid or unsupported PairRoom invitation")
	}
	return ValidateEndpoint(v.Endpoint)
}

func EncodeInvite(v Invite) string {
	b, _ := json.Marshal(v)
	return "pairroom://join/" + base64.RawURLEncoding.EncodeToString(b)
}
func ParseInvite(value string) (Invite, error) {
	var v Invite
	const prefix = "pairroom://join/"
	if !strings.HasPrefix(value, prefix) || len(value) > 4096 {
		return v, errors.New("invalid PairRoom invitation")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return v, errors.New("invalid PairRoom invitation")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF {
		return v, errors.New("invalid PairRoom invitation")
	}
	return v, v.Validate()
}

type JoinRequest struct {
	InviteID  string            `json:"invite_id"`
	RequestID string            `json:"request_id"`
	Runtime   model.RuntimeKind `json:"runtime"`
	Label     string            `json:"label,omitempty"`
}
type JoinStatusRequest struct {
	RequestID string `json:"request_id"`
}
type RoomInfo struct {
	RoomID        string                              `json:"room_id"`
	Name          string                              `json:"name"`
	Slot          model.ActorID                       `json:"slot"`
	Generation    uint64                              `json:"generation"`
	BindID        string                              `json:"bind_id"`
	Runtimes      map[model.ActorID]model.RuntimeKind `json:"runtimes"`
	Collaboration *model.Collaboration                `json:"collaboration"`
}
type JoinResponse struct {
	Status  string    `json:"status"`
	Receipt string    `json:"receipt"`
	Room    *RoomInfo `json:"room,omitempty"`
}
type Receipt struct {
	RoomID      string `json:"room_id"`
	RequestID   string `json:"request_id"`
	Fingerprint string `json:"fingerprint"`
}

func EncodeReceipt(v Receipt) string {
	b, _ := json.Marshal(v)
	return "pairroom-accept:" + base64.RawURLEncoding.EncodeToString(b)
}
func ParseReceipt(value string) (Receipt, error) {
	var v Receipt
	const prefix = "pairroom-accept:"
	if !strings.HasPrefix(value, prefix) || len(value) > 2048 {
		return v, errors.New("invalid join receipt")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return v, errors.New("invalid join receipt")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || !ValidID(v.RoomID) || !ValidID(v.RequestID) || !ValidFingerprint(v.Fingerprint) {
		return v, errors.New("invalid join receipt")
	}
	return v, nil
}

type Head struct {
	Message relay.Message `json:"message"`
	Digest  string        `json:"digest"`
}
type HeadRequest struct {
	Park           bool `json:"park,omitempty"`
	TimeoutSeconds int  `json:"timeout_seconds,omitempty"`
}
type HeadResponse struct {
	Head *Head `json:"head"`
}
type ClaimRequest struct {
	ID         string `json:"id"`
	Digest     string `json:"digest"`
	Generation uint64 `json:"generation"`
	Park       bool   `json:"park,omitempty"`
}
type Claim struct {
	ID      string        `json:"id"`
	Receipt string        `json:"receipt"`
	Message relay.Message `json:"message"`
}
type ClaimResponse struct {
	Claim *Claim `json:"claim"`
}

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return "LAN request failed: " + e.Message }
func SetMemberHeaders(r *http.Request, bindID string, generation uint64) {
	r.Header.Set("X-PairRoom-LAN-Bind", bindID)
	r.Header.Set("X-PairRoom-LAN-Generation", strconv.FormatUint(generation, 10))
}

// Call carries only the Room action. The caller owns its local credential
// journal; no TLS private material or native session metadata is serialized.
func Call(ctx context.Context, client *http.Client, invite Invite, action string, payload, result any) error {
	if err := invite.Validate(); err != nil {
		return err
	}
	if !ValidID(action) {
		return errors.New("invalid LAN action")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, invite.Endpoint+"/lan/v1/rooms/"+invite.RoomID+"/"+action, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("LAN Room is offline or peer identity failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > MaxResponseBytes {
		return errors.New("LAN response exceeds limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(data, &failure)
		if len(failure.Error) > 1024 {
			failure.Error = ""
		}
		if failure.Error == "" {
			failure.Error = resp.Status
		}
		return &Error{Status: resp.StatusCode, Code: failure.Code, Message: failure.Error}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("invalid LAN response")
	}
	return nil
}
