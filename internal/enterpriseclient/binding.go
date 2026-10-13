package enterpriseclient

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/relay"
)

const bindingStateVersion = 1
const maxBindingStateBytes = 2 << 20

var ErrBindingInactive = errors.New("enterprise Binding is inactive; inspect the original association")
var ErrBindingCredentialExpired = errors.New("enterprise Binding credential expired; renew it explicitly in the foreground with the current Device login")
var ErrDeliveryUnresolved = errors.New("enterprise delivery outcome is unresolved; reconcile its original operation without printing the envelope again")
var ErrBindingRequestExpired = errors.New("enterprise Binding request expired; explicitly abandon its local attempt before requesting another admission")
var ErrBindingRequestDenied = errors.New("enterprise Binding request was denied; explicitly abandon its local attempt before requesting another admission")

// BindingOptions is local setup input. Official native identity and workspace
// never cross HTTPS: only a random stable source key identifies the Agent.
type BindingOptions struct {
	RecordID, RoomID, Workspace string
	Auth                        relay.Auth
	ObservedGeneration          uint64
	Replace, ParkEnabled        bool
}

type BindingMetadata struct {
	RecordID, Origin, DeploymentID, UserID, DeviceID, AgentID, RoomID, Workspace string
	Slot                                                                         model.ActorID
	Runtime                                                                      model.RuntimeKind
	BindID                                                                       string
	Generation                                                                   uint64
	SessionHash, CredentialHash                                                  string
	Status                                                                       string
	RequestID                                                                    string
	ObservedGeneration                                                           uint64
	Replace, ParkEnabled                                                         bool
	ExpiresAt                                                                    time.Time
	RemotePhase                                                                  string
}

type BindingContext struct {
	RoomID           string                               `json:"room_id"`
	UserID           string                               `json:"user_id"`
	Role             string                               `json:"role"`
	OwnerUserID      string                               `json:"owner_user_id"`
	OwnershipPending bool                                 `json:"ownership_pending"`
	Slots            map[model.ActorID]BindingSlotContext `json:"slots"`
}
type BindingSlotContext struct {
	Slot           model.ActorID     `json:"slot"`
	HeadGeneration uint64            `json:"head_generation"`
	Vacant         bool              `json:"vacant"`
	BindingID      string            `json:"binding_id,omitempty"`
	AgentID        string            `json:"agent_id,omitempty"`
	Runtime        model.RuntimeKind `json:"runtime,omitempty"`
}
type BindingRequestInfo struct {
	RequestID          string            `json:"request_id"`
	RoomID             string            `json:"room_id"`
	Slot               model.ActorID     `json:"slot"`
	ObservedGeneration uint64            `json:"observed_generation"`
	Replace            bool              `json:"replace"`
	AgentID            string            `json:"agent_id"`
	Runtime            model.RuntimeKind `json:"runtime"`
	BindingID          string            `json:"binding_id"`
	UserID             string            `json:"user_id"`
	DeviceID           string            `json:"device_id"`
	ParkEnabled        bool              `json:"park_enabled"`
	Status             string            `json:"status"`
	RequestedAt        time.Time         `json:"requested_at"`
	ExpiresAt          time.Time         `json:"expires_at"`
	ApprovedBy         string            `json:"approved_by,omitempty"`
}
type bindingReceipt struct {
	Version   int                      `json:"version"`
	Kind      string                   `json:"kind"`
	Request   *BindingRequestInfo      `json:"request,omitempty"`
	Binding   *model.EnterpriseBinding `json:"binding,omitempty"`
	GrantID   string                   `json:"grant_id,omitempty"`
	ExpiresAt time.Time                `json:"expires_at,omitzero"`
	Seq       uint64                   `json:"seq,omitempty"`
	Revision  uint64                   `json:"revision,omitempty"`
}
type bindingReply struct {
	Outcome string          `json:"outcome"`
	Class   string          `json:"class,omitempty"`
	Error   string          `json:"error,omitempty"`
	Receipt *bindingReceipt `json:"receipt,omitempty"`
}
type bindingGrant struct {
	ID        string    `json:"id"`
	Token     secret    `json:"token"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}
type bindingRenewal struct {
	ClientID string       `json:"client_id"`
	Phase    string       `json:"phase"`
	Grant    bindingGrant `json:"grant"`
}
type bindingRecord struct {
	Version               int               `json:"version"`
	RecordID              string            `json:"record_id"`
	Origin                string            `json:"origin"`
	DeploymentID          string            `json:"deployment_id"`
	UserID                string            `json:"user_id"`
	DeviceID              string            `json:"device_id"`
	AgentID               string            `json:"agent_id,omitempty"`
	RoomID                string            `json:"room_id"`
	Slot                  model.ActorID     `json:"slot"`
	Runtime               model.RuntimeKind `json:"runtime"`
	Workspace             string            `json:"workspace"`
	BindID                string            `json:"bind_id"`
	Generation            uint64            `json:"generation,omitempty"`
	SessionHash           string            `json:"session_hash"`
	CredentialHash        string            `json:"credential_hash"`
	SourceKey             secret            `json:"source_key"`
	AgentOperationID      string            `json:"agent_operation_id"`
	RequestID             string            `json:"request_id"`
	RequestOperationID    string            `json:"request_operation_id"`
	CompletionOperationID string            `json:"completion_operation_id"`
	ObservedGeneration    uint64            `json:"observed_generation"`
	Replace               bool              `json:"replace"`
	ParkEnabled           bool              `json:"park_enabled"`
	Phase                 string            `json:"phase"`
	Proof                 secret            `json:"proof"`
	Grant                 bindingGrant      `json:"grant"`
	Renewal               *bindingRenewal   `json:"renewal,omitempty"`
	DetachedAt            time.Time         `json:"detached_at,omitzero"`
}

type BindingClient struct {
	parent  *Client
	dir, id string
}

func (c *Client) Directory() string { return c.dir }
func nativeSessionHash(a relay.Auth) string {
	return relay.Digest(string(a.Runtime) + "\x00" + a.SessionID)
}
func (r bindingRecord) metadata() BindingMetadata {
	status := r.Phase
	if !r.DetachedAt.IsZero() {
		status = "locally_detached"
	}
	return BindingMetadata{RecordID: r.RecordID, Origin: r.Origin, DeploymentID: r.DeploymentID, UserID: r.UserID, DeviceID: r.DeviceID, AgentID: r.AgentID, RoomID: r.RoomID, Workspace: r.Workspace, Slot: r.Slot, Runtime: r.Runtime, BindID: r.BindID, Generation: r.Generation, SessionHash: r.SessionHash, CredentialHash: r.CredentialHash, Status: status, RemotePhase: r.Phase, RequestID: r.RequestID, ObservedGeneration: r.ObservedGeneration, Replace: r.Replace, ParkEnabled: r.ParkEnabled, ExpiresAt: r.Grant.ExpiresAt}
}

func (c *Client) OpenBinding(recordID string) (*BindingClient, error) {
	if !validID(recordID) {
		return nil, ErrPrivateState
	}
	dir := filepath.Join(c.dir, "bindings", recordID)
	if err := privatefile.CheckDirectory(dir); err != nil {
		return nil, ErrPrivateState
	}
	b := &BindingClient{parent: c, dir: dir, id: recordID}
	if _, err := b.read(); err != nil {
		return nil, err
	}
	return b, nil
}
func (b *BindingClient) Close() {}
func (b *BindingClient) read() (bindingRecord, error) {
	var r bindingRecord
	if err := privatefile.ReadJSON(filepath.Join(b.dir, "binding.json"), maxBindingStateBytes, &r); err != nil {
		return r, ErrPrivateState
	}
	if r.Version != bindingStateVersion || r.RecordID != b.id || r.Origin != b.parent.origin || r.DeploymentID != b.parent.deployment || !validID(r.UserID) || !validID(r.DeviceID) || !validID(r.RoomID) || !validID(r.BindID) || !r.Slot.ValidParticipant() || !r.Runtime.Valid() || r.Runtime != r.Runtime.Canonical() || !filepath.IsAbs(r.Workspace) || !validBindingHash(r.SessionHash) || !validBindingHash(r.CredentialHash) || !validSecret(r.SourceKey) || !validSecret(r.Proof) || !validSecret(r.Grant.Token) || !validID(r.Grant.ID) || r.ObservedGeneration == 0 || !validID(r.AgentOperationID) || !validID(r.RequestID) || !validID(r.RequestOperationID) || !validID(r.CompletionOperationID) {
		return r, ErrPrivateState
	}
	switch r.Phase {
	case "prepared", "agent_unknown", "request_prepared", "request_unknown", "awaiting_owner", "completion_prepared", "completion_unknown", "active", "inactive", "denied", "expired":
	default:
		return r, ErrPrivateState
	}
	if r.Phase == "active" && (r.Generation == 0 || !validID(r.AgentID) || r.Grant.ExpiresAt.IsZero()) {
		return r, ErrPrivateState
	}
	if r.Renewal != nil && (!validID(r.Renewal.ClientID) || !validID(r.Renewal.Grant.ID) || !validSecret(r.Renewal.Grant.Token) || (r.Renewal.Phase != "prepared" && r.Renewal.Phase != "unknown")) {
		return r, ErrPrivateState
	}
	return r, nil
}

func validBindingHash(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 32 && hex.EncodeToString(data) == value
}

type localAgentSource struct {
	Version                                int `json:"version"`
	Origin, DeploymentID, UserID, DeviceID string
	Runtime                                model.RuntimeKind
	SessionHash                            string
	SourceKey                              secret
}

// This mapping survives explicit Binding replacement and Room retirement.
// Its file key is local only; the server receives an independent random key,
// never a hash of an official vendor session identifier.
func (c *Client) agentSource(r bindingRecord) (secret, error) {
	dir := filepath.Join(c.dir, "agents")
	if err := privatefile.Mkdir(dir); err != nil {
		return "", ErrPrivateState
	}
	path := filepath.Join(dir, relay.Digest(r.UserID+"\x00"+r.DeviceID+"\x00"+string(r.Runtime)+"\x00"+r.SessionHash)+".json")
	var source localAgentSource
	if err := privatefile.ReadJSON(path, 4096, &source); err == nil {
		if source.Version != 1 || source.Origin != c.origin || source.DeploymentID != c.deployment || source.UserID != r.UserID || source.DeviceID != r.DeviceID || source.Runtime != r.Runtime || source.SessionHash != r.SessionHash || !validSecret(source.SourceKey) {
			return "", ErrPrivateState
		}
		return source.SourceKey, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", ErrPrivateState
	}
	value, err := newSecret()
	if err != nil {
		return "", err
	}
	source = localAgentSource{Version: 1, Origin: c.origin, DeploymentID: c.deployment, UserID: r.UserID, DeviceID: r.DeviceID, Runtime: r.Runtime, SessionHash: r.SessionHash, SourceKey: value}
	if err := privatefile.WriteJSON(path, source); err != nil {
		return "", ErrPrivateState
	}
	return value, nil
}
func (b *BindingClient) save(r bindingRecord) error {
	data, err := json.Marshal(r)
	if err != nil || len(data)+1 > maxBindingStateBytes {
		return ErrPrivateState
	}
	if err := privatefile.WriteJSON(filepath.Join(b.dir, "binding.json"), r); err != nil {
		return ErrPrivateState
	}
	return nil
}
func (b *BindingClient) lock(ctx context.Context) (func(), error) {
	if err := privatefile.CheckDirectory(b.dir); err != nil {
		return nil, ErrPrivateState
	}
	return privatelock.Lock(ctx, b.dir)
}
func (b *BindingClient) currentDevice(r bindingRecord, access bool) (state, error) {
	s, err := b.parent.read()
	if err != nil {
		return s, err
	}
	if s.LoggedOut || s.Active == nil || s.UserID != r.UserID || s.DeviceID != r.DeviceID {
		return s, ErrNoSession
	}
	if access && !s.Active.ExpiresAt.After(time.Now()) {
		return s, ErrNoSession
	}
	return s, nil
}
func (b *BindingClient) authenticated(ctx context.Context, a relay.Auth) (bindingRecord, error) {
	r, err := b.read()
	if err != nil {
		return r, err
	}
	if _, err := b.currentDevice(r, false); err != nil {
		return r, err
	}
	if r.Phase != "active" || !r.DetachedAt.IsZero() {
		return r, ErrBindingInactive
	}
	if a.BindID != r.BindID || a.Generation != r.Generation || a.Slot != r.Slot || a.Runtime != r.Runtime || nativeSessionHash(a) != r.SessionHash || subtle.ConstantTimeCompare([]byte(relay.Digest(a.Secret)), []byte(r.CredentialHash)) != 1 {
		return r, relay.ErrAuth
	}
	identities, err := nativeidentity.Open()
	if err != nil {
		return r, err
	}
	if err := identities.Check(ctx, nativeidentity.Claim{Runtime: r.Runtime, SessionID: a.SessionID, Association: nativeidentity.Enterprise(r.Origin, r.DeploymentID, r.RoomID), BindID: r.BindID, Generation: r.Generation}); err != nil {
		// A private identity-lock wait can exhaust this caller's context
		// without disproving its identity. Preserve only those causes; actual
		// reservation/credential mismatches remain authentication failures.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return r, err
		}
		return r, relay.ErrAuth
	}
	return r, nil
}
func (b *BindingClient) Metadata(ctx context.Context) (BindingMetadata, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return BindingMetadata{}, err
	}
	defer unlock()
	r, err := b.read()
	if err != nil {
		return BindingMetadata{}, err
	}
	if _, err := b.currentDevice(r, false); err != nil {
		return BindingMetadata{}, err
	}
	return r.metadata(), nil
}

// LocalMetadata supports explicit offline retirement of an old login's
// association. It is never a discovery or network-authorization substitute.
func (b *BindingClient) LocalMetadata(ctx context.Context) (BindingMetadata, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return BindingMetadata{}, err
	}
	defer unlock()
	r, err := b.read()
	if err != nil {
		return BindingMetadata{}, err
	}
	return r.metadata(), nil
}

func decodeWire(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		return unknown("invalid_response")
	}
	return nil
}
func wireFailure(status int, data []byte) error {
	var f struct {
		Outcome  string `json:"outcome"`
		Class    string `json:"class"`
		Error    string `json:"error"`
		Seq      uint64 `json:"seq,omitempty"`
		Revision uint64 `json:"revision,omitempty"`
	}
	if decodeWire(data, &f) != nil {
		return unknown("invalid_response")
	}
	if f.Outcome == "not_committed" || f.Outcome == "refused" || f.Outcome == "conflict" || f.Outcome == "unknown" {
		return &Error{Outcome: f.Outcome, Class: safeClass(f.Class)}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &Error{Outcome: "refused", Class: "authorization"}
	}
	return unknown("operation_unconfirmed")
}
func (c *Client) bindingWire(ctx context.Context, path string, body any, bearer secret) (bindingReply, error) {
	status, data, err := c.request(ctx, http.MethodPost, path, body, bearer, 64<<10)
	if err != nil {
		return bindingReply{}, err
	}
	if status != http.StatusOK {
		return bindingReply{}, wireFailure(status, data)
	}
	var reply bindingReply
	if err := decodeWire(data, &reply); err != nil {
		return reply, err
	}
	if reply.Outcome != "committed" && reply.Outcome != "replayed" && reply.Outcome != "not_committed" {
		return bindingReply{}, wireFailure(status, data)
	}
	if reply.Outcome == "not_committed" && reply.Receipt != nil {
		return bindingReply{}, unknown("invalid_response")
	}
	return reply, nil
}

func (c *Client) ReadBindingContext(ctx context.Context, roomID string) (BindingContext, error) {
	if !validID(roomID) {
		return BindingContext{}, ErrConfiguration
	}
	s, err := c.read()
	if err != nil {
		return BindingContext{}, err
	}
	if s.LoggedOut || s.Active == nil || !s.Active.ExpiresAt.After(time.Now()) {
		return BindingContext{}, ErrNoSession
	}
	status, data, err := c.request(ctx, http.MethodGet, devicePath+"/rooms/"+roomID+"/binding-context", nil, s.Active.Access, 64<<10)
	if err != nil {
		return BindingContext{}, err
	}
	if status != http.StatusOK {
		return BindingContext{}, wireFailure(status, data)
	}
	var view BindingContext
	if err := decodeWire(data, &view); err != nil {
		return view, err
	}
	if view.RoomID != roomID || view.UserID != s.UserID || (view.Role != "member" && view.Role != "owner") || !validID(view.OwnerUserID) || len(view.Slots) != 2 {
		return BindingContext{}, unknown("invalid_response")
	}
	for _, slot := range model.SlotActors() {
		entry, ok := view.Slots[slot]
		if !ok || entry.Slot != slot || entry.HeadGeneration == 0 || entry.Vacant && (entry.BindingID != "" || entry.AgentID != "" || entry.Runtime != "") || !entry.Vacant && (!validID(entry.BindingID) || !validID(entry.AgentID) || !entry.Runtime.Valid() || entry.Runtime != entry.Runtime.Canonical()) {
			return BindingContext{}, unknown("invalid_response")
		}
	}
	current, err := c.read()
	if err != nil || current.LoggedOut || current.Active == nil || current.UserID != s.UserID || current.DeviceID != s.DeviceID {
		return BindingContext{}, ErrNoSession
	}
	return view, nil
}

func newBindingGrant() (bindingGrant, error) {
	id, err := newID("binding-grant")
	if err != nil {
		return bindingGrant{}, err
	}
	token, err := newSecret()
	return bindingGrant{ID: id, Token: token}, err
}

// PrepareBinding is durable and local only. The caller has already reserved
// its native identity before invoking it; ResumeBinding is the explicit effect.
func (c *Client) PrepareBinding(ctx context.Context, o BindingOptions) (*BindingClient, error) {
	if !validID(o.RecordID) || !validID(o.RoomID) || !validID(o.Auth.BindID) || len(o.Auth.BindID) > 128 || !o.Auth.Slot.ValidParticipant() || !o.Auth.Runtime.Valid() || o.Auth.Runtime != o.Auth.Runtime.Canonical() || o.Auth.SessionID == "" || o.Auth.Secret == "" || !filepath.IsAbs(o.Workspace) || o.ObservedGeneration == 0 {
		return nil, ErrConfiguration
	}
	unlock, err := c.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s, err := c.read()
	if err != nil {
		return nil, err
	}
	if s.Active == nil || s.LoggedOut || !s.Active.ExpiresAt.After(time.Now()) {
		return nil, ErrNoSession
	}
	for _, dir := range []string{filepath.Join(c.dir, "bindings"), filepath.Join(c.dir, "bindings", o.RecordID)} {
		if err := privatefile.Mkdir(dir); err != nil {
			return nil, ErrPrivateState
		}
	}
	b := &BindingClient{parent: c, dir: filepath.Join(c.dir, "bindings", o.RecordID), id: o.RecordID}
	if _, err := os.Lstat(filepath.Join(b.dir, "binding.json")); err == nil {
		r, err := b.read()
		if err != nil {
			return nil, err
		}
		if r.UserID != s.UserID || r.DeviceID != s.DeviceID || r.RoomID != o.RoomID || r.Workspace != o.Workspace || r.Slot != o.Auth.Slot || r.BindID != o.Auth.BindID || r.Runtime != o.Auth.Runtime || r.SessionHash != nativeSessionHash(o.Auth) || r.CredentialHash != relay.Digest(o.Auth.Secret) || r.ObservedGeneration != o.ObservedGeneration || r.Replace != o.Replace || r.ParkEnabled != o.ParkEnabled {
			return nil, ErrPrivateState
		}
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, ErrPrivateState
	}
	r := bindingRecord{Version: bindingStateVersion, RecordID: o.RecordID, Origin: c.origin, DeploymentID: c.deployment, UserID: s.UserID, DeviceID: s.DeviceID, RoomID: o.RoomID, Slot: o.Auth.Slot, Runtime: o.Auth.Runtime, Workspace: o.Workspace, BindID: o.Auth.BindID, SessionHash: nativeSessionHash(o.Auth), CredentialHash: relay.Digest(o.Auth.Secret), ObservedGeneration: o.ObservedGeneration, Replace: o.Replace, ParkEnabled: o.ParkEnabled, Phase: "prepared"}
	r.SourceKey, err = c.agentSource(r)
	if err != nil {
		return nil, err
	}
	for _, target := range []*secret{&r.Proof} {
		value, err := newSecret()
		if err != nil {
			return nil, err
		}
		*target = value
	}
	for _, target := range []*string{&r.AgentOperationID, &r.RequestID, &r.RequestOperationID, &r.CompletionOperationID} {
		value, err := newID("binding-op")
		if err != nil {
			return nil, err
		}
		*target = value
	}
	r.Grant, err = newBindingGrant()
	if err != nil {
		return nil, err
	}
	if err := b.save(r); err != nil {
		return nil, err
	}
	return b, nil
}

type bindingRequestPayload struct {
	ClientID           string              `json:"client_id"`
	RequestID          string              `json:"request_id"`
	AgentID            string              `json:"agent_id"`
	BindingID          string              `json:"binding_id"`
	Slot               model.ActorID       `json:"slot"`
	ObservedGeneration uint64              `json:"observed_generation"`
	Replace            bool                `json:"replace"`
	ParkEnabled        bool                `json:"park_enabled"`
	ProofHash          string              `json:"proof_hash"`
	Grant              bindingGrantPayload `json:"grant"`
}
type bindingGrantPayload struct {
	GrantID   string `json:"grant_id"`
	TokenHash string `json:"token_hash"`
}

func (r bindingRecord) requestPayload() bindingRequestPayload {
	return bindingRequestPayload{r.RequestOperationID, r.RequestID, r.AgentID, r.BindID, r.Slot, r.ObservedGeneration, r.Replace, r.ParkEnabled, verifier(r.Proof), bindingGrantPayload{r.Grant.ID, verifier(r.Grant.Token)}}
}
func (r bindingRecord) requestPath() string {
	return devicePath + "/rooms/" + r.RoomID + "/binding-requests/" + r.RequestID
}
func (r bindingRecord) matchesRequest(i *BindingRequestInfo) bool {
	return i != nil && i.RequestID == r.RequestID && i.RoomID == r.RoomID && i.Slot == r.Slot && i.ObservedGeneration == r.ObservedGeneration && i.Replace == r.Replace && i.AgentID == r.AgentID && i.BindingID == r.BindID && i.Runtime == r.Runtime && i.UserID == r.UserID && i.DeviceID == r.DeviceID && i.ParkEnabled == r.ParkEnabled && !i.RequestedAt.IsZero() && i.ExpiresAt.After(i.RequestedAt)
}

func (b *BindingClient) acceptCompletion(r *bindingRecord, reply bindingReply) error {
	x := reply.Receipt
	if (reply.Outcome != "committed" && reply.Outcome != "replayed") || x == nil || x.Version != 1 || x.Kind != "complete" || !r.matchesRequest(x.Request) || x.Request.Status != "consumed" || x.Binding == nil || x.GrantID != r.Grant.ID || x.ExpiresAt.IsZero() || x.Seq == 0 || x.Revision == 0 {
		return unknown("invalid_response")
	}
	i := x.Binding
	if !i.Active || i.Version != 1 || i.RoomID != r.RoomID || i.Slot != r.Slot || i.BindID != r.BindID || i.AgentID != r.AgentID || i.Runtime != r.Runtime || i.Generation < r.ObservedGeneration || i.Generation > r.ObservedGeneration+1 || r.Replace && i.Generation != r.ObservedGeneration+1 || i.ParkEnabled != r.ParkEnabled || i.RetiredAt != nil || i.RetiredReason != "" {
		return unknown("invalid_response")
	}
	r.Generation, r.Grant.ExpiresAt, r.Phase = i.Generation, x.ExpiresAt, "active"
	return b.save(*r)
}

// ResumeBinding is explicitly foreground. It does not open a browser or poll
// for approval; an awaiting_owner result supplies a safe public approval URL.
// Every uncertain request is queried by its original fixed intent first.
func (b *BindingClient) ResumeBinding(ctx context.Context) (BindingMetadata, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return BindingMetadata{}, err
	}
	defer unlock()
	r, err := b.read()
	if err != nil {
		return BindingMetadata{}, err
	}
	if !r.DetachedAt.IsZero() {
		return r.metadata(), ErrBindingInactive
	}
	if r.Phase == "denied" {
		return r.metadata(), ErrBindingRequestDenied
	}
	if r.Phase == "expired" {
		return r.metadata(), ErrBindingRequestExpired
	}
	if _, err := b.currentDevice(r, false); err != nil {
		return BindingMetadata{}, err
	}
	if r.Phase == "active" {
		return r.metadata(), nil
	}
	if r.Phase == "inactive" {
		return r.metadata(), ErrBindingInactive
	}
	s, err := b.currentDevice(r, true)
	if err != nil {
		return r.metadata(), err
	}
	if r.AgentID == "" {
		r.Phase = "agent_unknown"
		if err := b.save(r); err != nil {
			return r.metadata(), err
		}
		// RegisterAgent is a canonical metadata operation keyed by this opaque
		// source. Only another explicit Resume invokes it after a lost result.
		reply, err := b.parent.post(ctx, devicePath+"/agents", struct {
			ClientID  string            `json:"client_id"`
			SourceKey string            `json:"source_key"`
			Runtime   model.RuntimeKind `json:"runtime"`
		}{r.AgentOperationID, string(r.SourceKey), r.Runtime}, s.Active.Access)
		if err != nil {
			return r.metadata(), err
		}
		if !reply.confirmed() || reply.Kind != "agent.register" || !validID(reply.AgentID) || reply.DeviceID != "" && reply.DeviceID != r.DeviceID || reply.UserID != "" && reply.UserID != r.UserID {
			return r.metadata(), unknown("invalid_response")
		}
		r.AgentID, r.Phase = reply.AgentID, "request_prepared"
		if err := b.save(r); err != nil {
			return r.metadata(), err
		}
	}
	if r.Phase == "request_prepared" || r.Phase == "request_unknown" {
		var reply bindingReply
		if r.Phase == "request_unknown" {
			reply, err = b.parent.bindingWire(ctx, r.requestPath()+"/resolve-request", r.requestPayload(), s.Active.Access)
			if err != nil {
				return r.metadata(), err
			}
		}
		if reply.Outcome == "" || reply.Outcome == "not_committed" {
			r.Phase = "request_unknown"
			if err := b.save(r); err != nil {
				return r.metadata(), err
			}
			reply, err = b.parent.bindingWire(ctx, devicePath+"/rooms/"+r.RoomID+"/binding-requests", r.requestPayload(), s.Active.Access)
			if err != nil {
				return r.metadata(), err
			}
		}
		if reply.Receipt == nil || reply.Receipt.Version != 1 || reply.Receipt.Kind != "request" || !r.matchesRequest(reply.Receipt.Request) || reply.Receipt.Request.Status != "pending" || reply.Receipt.Binding != nil || reply.Receipt.GrantID != "" {
			return r.metadata(), unknown("invalid_response")
		}
		r.Phase = "awaiting_owner"
		if err := b.save(r); err != nil {
			return r.metadata(), err
		}
	}
	completion := struct {
		ClientID string `json:"client_id"`
		Proof    secret `json:"client_proof"`
	}{r.CompletionOperationID, r.Proof}
	if r.Phase == "completion_unknown" {
		reply, err := b.parent.bindingWire(ctx, r.requestPath()+"/resolve-completion", completion, s.Active.Access)
		if err != nil {
			return r.metadata(), err
		}
		if reply.Outcome != "not_committed" {
			err = b.acceptCompletion(&r, reply)
			return r.metadata(), err
		}
		r.Phase = "awaiting_owner"
		if err := b.save(r); err != nil {
			return r.metadata(), err
		}
	}
	status, data, err := b.parent.request(ctx, http.MethodGet, r.requestPath(), nil, s.Active.Access, 64<<10)
	if err != nil {
		return r.metadata(), err
	}
	if status != http.StatusOK {
		return r.metadata(), wireFailure(status, data)
	}
	var view struct {
		Request *BindingRequestInfo `json:"request"`
	}
	if err := decodeWire(data, &view); err != nil {
		return r.metadata(), err
	}
	if !r.matchesRequest(view.Request) {
		return r.metadata(), unknown("invalid_response")
	}
	if view.Request.Status == "denied" {
		r.Phase = "denied"
		if err := b.save(r); err != nil {
			return r.metadata(), err
		}
		return r.metadata(), ErrBindingRequestDenied
	}
	if (view.Request.Status == "pending" || view.Request.Status == "approved") && !view.Request.ExpiresAt.After(time.Now()) {
		r.Phase = "expired"
		if err := b.save(r); err != nil {
			return r.metadata(), err
		}
		return r.metadata(), ErrBindingRequestExpired
	}
	if view.Request.Status == "pending" {
		return r.metadata(), nil
	}
	if view.Request.Status != "approved" && view.Request.Status != "consumed" {
		return r.metadata(), unknown("invalid_response")
	}
	if view.Request.Status == "consumed" {
		reply, err := b.parent.bindingWire(ctx, r.requestPath()+"/resolve-completion", completion, s.Active.Access)
		if err != nil {
			return r.metadata(), err
		}
		err = b.acceptCompletion(&r, reply)
		return r.metadata(), err
	}
	r.Phase = "completion_unknown"
	if err := b.save(r); err != nil {
		return r.metadata(), err
	}
	reply, err := b.parent.bindingWire(ctx, r.requestPath()+"/complete", completion, s.Active.Access)
	if err != nil {
		return r.metadata(), err
	}
	if _, err := b.currentDevice(r, false); err != nil {
		return r.metadata(), err
	}
	err = b.acceptCompletion(&r, reply)
	return r.metadata(), err
}

func (b *BindingClient) ApprovalURL(ctx context.Context) (string, error) {
	m, err := b.Metadata(ctx)
	if err != nil {
		return "", err
	}
	return m.Origin + "/enterprise/binding-requests/" + m.RoomID + "/" + m.RequestID, nil
}

// RenewCredential is an explicit foreground effect. Hooks only use the saved
// grant and retain pending work when it expires; they never invoke renewal.
func (b *BindingClient) RenewCredential(ctx context.Context, a relay.Auth) (BindingMetadata, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return BindingMetadata{}, err
	}
	defer unlock()
	r, err := b.authenticated(ctx, a)
	if err != nil {
		return BindingMetadata{}, err
	}
	s, err := b.currentDevice(r, true)
	if err != nil {
		return r.metadata(), err
	}
	if r.Renewal == nil {
		id, err := newID("renew")
		if err != nil {
			return r.metadata(), err
		}
		grant, err := newBindingGrant()
		if err != nil {
			return r.metadata(), err
		}
		r.Renewal = &bindingRenewal{ClientID: id, Phase: "prepared", Grant: grant}
		if err := b.save(r); err != nil {
			return r.metadata(), err
		}
	}
	p := r.Renewal
	body := struct {
		ClientID   string              `json:"client_id"`
		Generation uint64              `json:"generation"`
		Grant      bindingGrantPayload `json:"grant"`
	}{p.ClientID, r.Generation, bindingGrantPayload{p.Grant.ID, verifier(p.Grant.Token)}}
	path := devicePath + "/rooms/" + r.RoomID + "/bindings/" + r.BindID
	var reply bindingReply
	if p.Phase == "unknown" {
		reply, err = b.parent.bindingWire(ctx, path+"/resolve-renewal", body, s.Active.Access)
		if err != nil {
			return r.metadata(), err
		}
	}
	if reply.Outcome == "" || reply.Outcome == "not_committed" {
		r.Renewal.Phase = "unknown"
		if err := b.save(r); err != nil {
			return r.metadata(), err
		}
		reply, err = b.parent.bindingWire(ctx, path+"/renew", body, s.Active.Access)
		if err != nil {
			return r.metadata(), err
		}
	}
	x := reply.Receipt
	if x == nil || x.Version != 1 || x.Kind != "renew" || x.Request != nil || x.Binding == nil || x.Binding.RoomID != r.RoomID || x.Binding.BindID != r.BindID || x.Binding.Generation != r.Generation || x.Binding.Slot != r.Slot || x.Binding.AgentID != r.AgentID || x.Binding.Runtime != r.Runtime || !x.Binding.Active || x.Binding.RetiredAt != nil || x.Binding.RetiredReason != "" || x.GrantID != p.Grant.ID || x.ExpiresAt.IsZero() {
		return r.metadata(), unknown("invalid_response")
	}
	if _, err := b.currentDevice(r, false); err != nil {
		return r.metadata(), err
	}
	r.Grant, r.Renewal = p.Grant, nil
	r.Grant.ExpiresAt = x.ExpiresAt
	if err := b.save(r); err != nil {
		return r.metadata(), err
	}
	return r.metadata(), nil
}

// DetachLocal is the explicit offline escape. It retains every private journal
// and sends no request. A later Device login never revives this record.
func (b *BindingClient) DetachLocal(ctx context.Context, a relay.Auth) error {
	unlock, err := b.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := b.read()
	if err != nil {
		return err
	}
	if r.BindID != a.BindID || r.Generation != a.Generation || r.Slot != a.Slot || r.Runtime != a.Runtime || r.SessionHash != nativeSessionHash(a) || subtle.ConstantTimeCompare([]byte(r.CredentialHash), []byte(relay.Digest(a.Secret))) != 1 {
		return relay.ErrAuth
	}
	// Local retirement is separate from the last known remote operation phase.
	// In particular, completion_unknown must survive an offline detach verbatim.
	if r.DetachedAt.IsZero() {
		r.DetachedAt = time.Now().UTC()
	}
	if err := b.save(r); err != nil {
		return err
	}
	identities, err := nativeidentity.Open()
	if err != nil {
		return err
	}
	claim := nativeidentity.Claim{Runtime: r.Runtime, SessionID: a.SessionID, Association: nativeidentity.Enterprise(r.Origin, r.DeploymentID, r.RoomID), BindID: r.BindID, Generation: r.Generation}
	if _, err := identities.ReleaseIfHeld(ctx, claim); err != nil {
		return err
	}
	if claim.Generation != 0 {
		// Completion may have been saved immediately before a crash, while the
		// native identity index still holds this exact pending generation zero.
		claim.Generation = 0
		if _, err := identities.ReleaseIfHeld(ctx, claim); err != nil {
			return err
		}
	}
	return nil
}
