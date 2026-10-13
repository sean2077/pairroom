package enterpriseclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/prompt"
	"github.com/sean2077/pairroom/internal/relay"
)

var ErrUploadUnresolved = errors.New("enterprise upload bytes were not confirmed; repeat the original send to verify, or use --retry-upload only after an authoritative object_missing result")

type attachmentSpec struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type attachmentReceipt struct {
	Version   int              `json:"version"`
	Kind      string           `json:"kind"`
	RoomID    string           `json:"room_id,omitempty"`
	ObjectID  string           `json:"object_id,omitempty"`
	State     string           `json:"state,omitempty"`
	Metadata  model.Attachment `json:"metadata"`
	ExpiresAt time.Time        `json:"expires_at,omitempty"`
	BarrierID string           `json:"barrier_id,omitempty"`
}
type attachmentReply struct {
	Outcome string             `json:"outcome"`
	Class   string             `json:"class,omitempty"`
	Error   string             `json:"error,omitempty"`
	Receipt *attachmentReceipt `json:"receipt,omitempty"`
}
type uploadJournal struct {
	Version    int              `json:"version"`
	BindingID  string           `json:"binding_id"`
	Generation uint64           `json:"generation"`
	Key        string           `json:"key"`
	ReserveID  string           `json:"reserve_id"`
	VerifyID   string           `json:"verify_id"`
	Spec       attachmentSpec   `json:"spec"`
	ObjectID   string           `json:"object_id,omitempty"`
	Phase      string           `json:"phase"`
	Metadata   model.Attachment `json:"metadata"`
}

func (b *BindingClient) objectPath(r bindingRecord, id string) string {
	return devicePath + "/rooms/" + r.RoomID + "/attachments/" + id
}

// objectRequest allows only exact Room/object content paths. JSON operations
// retain their separate 2 MiB budget; effects are never retried by transport.
func (c *Client) objectRequest(ctx context.Context, method, path string, data []byte, bearer secret, limit int) (int, []byte, error) {
	parts := strings.Split(strings.TrimPrefix(path, devicePath+"/rooms/"), "/")
	if !strings.HasPrefix(path, devicePath+"/rooms/") || len(parts) != 4 || !validID(parts[0]) || parts[1] != "attachments" || !model.ValidEnterpriseAttachmentID(parts[2]) || parts[3] != "content" || strings.ContainsAny(path, "?#\\") || limit <= 0 || int64(limit) > attachment.MaxImageBytes || method != http.MethodGet && method != http.MethodPut || method == http.MethodGet && data != nil || method == http.MethodPut && (len(data) == 0 || int64(len(data)) > attachment.MaxImageBytes) {
		return 0, nil, ErrConfiguration
	}
	return c.requestEncoded(ctx, method, path, data, "application/octet-stream", bearer, limit)
}

func (b *BindingClient) attachmentOperation(ctx context.Context, r bindingRecord, path string, body any) (attachmentReply, error) {
	status, data, err := b.parent.request(ctx, http.MethodPost, path, body, r.Grant.Token, 64<<10)
	if err != nil {
		return attachmentReply{}, err
	}
	if status != http.StatusOK {
		return attachmentReply{}, wireFailure(status, data)
	}
	var reply attachmentReply
	if decodeWire(data, &reply) != nil {
		return reply, unknown("invalid_response")
	}
	if status == http.StatusOK && (reply.Outcome == "committed" || reply.Outcome == "replayed" || reply.Outcome == "not_committed") {
		return reply, nil
	}
	if reply.Receipt != nil {
		return reply, unknown("invalid_response")
	}
	if reply.Outcome == "not_committed" || reply.Outcome == "refused" || reply.Outcome == "conflict" || reply.Outcome == "unknown" {
		class := safeClass(reply.Class)
		if reply.Class == "object_missing" && reply.Outcome == "not_committed" {
			class = "object_missing"
		}
		return reply, &Error{Outcome: reply.Outcome, Class: class}
	}
	return reply, unknown("invalid_response")
}

// Upload is explicit foreground work. Its stable key is scoped to one send and
// attachment position. The original immutable bytes and operation keys survive
// interruption; an uncertain PUT is verified before any deliberate retry.
func (b *BindingClient) Upload(ctx context.Context, a relay.Auth, key, kind, name string, reader io.Reader, retry bool) (model.Attachment, error) {
	if !validID(key) || reader == nil {
		return model.Attachment{}, ErrConfiguration
	}
	data, err := io.ReadAll(io.LimitReader(reader, attachment.MaxImageBytes+1))
	if err != nil {
		return model.Attachment{}, errors.New("attachment could not be read")
	}
	meta, err := attachment.ValidateObjectBytes(name, kind, data)
	if err != nil {
		return model.Attachment{}, err
	}
	spec := attachmentSpec{Name: meta.Name, Kind: meta.Kind, Size: meta.Size, SHA256: meta.SHA256}
	unlock, err := b.lock(ctx)
	if err != nil {
		return model.Attachment{}, err
	}
	defer unlock()
	r, err := b.authenticated(ctx, a)
	if err != nil {
		return model.Attachment{}, err
	}
	if !r.Grant.ExpiresAt.After(time.Now()) {
		return model.Attachment{}, ErrBindingCredentialExpired
	}
	dir := filepath.Join(b.dir, "uploads")
	if privatefile.Mkdir(dir) != nil {
		return model.Attachment{}, ErrPrivateState
	}
	path := filepath.Join(dir, relay.Digest(key)+".json")
	bytesPath := filepath.Join(dir, relay.Digest(key)+".content")
	var journal uploadJournal
	err = privatefile.ReadJSON(path, 64<<10, &journal)
	if errors.Is(err, os.ErrNotExist) {
		entries, e := os.ReadDir(dir)
		if e != nil {
			return model.Attachment{}, ErrPrivateState
		}
		if len(entries) >= 4096+32 {
			return model.Attachment{}, ErrPending
		}
		pending := 0
		for _, entry := range entries {
			if entry.IsDir() {
				return model.Attachment{}, ErrPrivateState
			}
			if strings.HasSuffix(entry.Name(), ".content") {
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				return model.Attachment{}, ErrPrivateState
			}
			var retained uploadJournal
			if privatefile.ReadJSON(filepath.Join(dir, entry.Name()), 64<<10, &retained) != nil || retained.Version != 1 || retained.BindingID != r.BindID || retained.Generation != r.Generation {
				return model.Attachment{}, ErrPrivateState
			}
			if retained.Phase != "verified" {
				pending++
			}
		}
		if pending >= 32 {
			return model.Attachment{}, ErrPending
		}
		reserveID, err := newID("object-reserve")
		if err != nil {
			return model.Attachment{}, err
		}
		verifyID, err := newID("object-verify")
		if err != nil {
			return model.Attachment{}, err
		}
		journal = uploadJournal{Version: 1, BindingID: r.BindID, Generation: r.Generation, Key: key, ReserveID: reserveID, VerifyID: verifyID, Spec: spec, Phase: "prepared"}
	} else if err != nil {
		return model.Attachment{}, ErrPrivateState
	} else if journal.Version != 1 || journal.BindingID != r.BindID || journal.Generation != r.Generation || journal.Key != key || journal.Spec != spec || !validID(journal.ReserveID) || !validID(journal.VerifyID) {
		return model.Attachment{}, &Error{Outcome: "conflict", Class: "payload_conflict"}
	}
	save := func() error {
		if privatefile.WriteJSON(path, journal) != nil {
			return ErrPrivateState
		}
		return nil
	}
	if journal.Phase == "verified" {
		if !matchesObject(journal.Metadata, journal.ObjectID, spec) || journal.Metadata.MediaType != meta.MediaType || journal.Metadata.Width != meta.Width || journal.Metadata.Height != meta.Height {
			return model.Attachment{}, ErrPrivateState
		}
		if err := clearUploadBytes(bytesPath, data); err != nil {
			return model.Attachment{}, err
		}
		return journal.Metadata, nil
	}
	// Persist exact candidate bytes before any operation journal can authorize a
	// request. The small JSON never base64-encodes an up-to-5-MiB object.
	if err := retainUploadBytes(bytesPath, data, journal.Phase == "prepared"); err != nil {
		return model.Attachment{}, err
	}
	base := devicePath + "/rooms/" + r.RoomID + "/attachments"
	reserve := struct {
		ClientID string         `json:"client_id"`
		Spec     attachmentSpec `json:"spec"`
	}{journal.ReserveID, spec}
	if journal.Phase == "prepared" || journal.Phase == "reserve_unknown" {
		var reply attachmentReply
		if journal.Phase == "reserve_unknown" {
			reply, err = b.attachmentOperation(ctx, r, base+"/resolve-reservation", reserve)
			if err != nil {
				return model.Attachment{}, err
			}
		}
		if reply.Outcome == "" || reply.Outcome == "not_committed" {
			journal.Phase = "reserve_unknown"
			if err = save(); err != nil {
				return model.Attachment{}, err
			}
			reply, err = b.attachmentOperation(ctx, r, base, reserve)
			if err != nil {
				return model.Attachment{}, err
			}
		}
		x := reply.Receipt
		if x == nil || x.Version != 1 || x.Kind != "reserve" || x.RoomID != r.RoomID || !model.ValidEnterpriseAttachmentID(x.ObjectID) || x.State != "reserved" || !matchesObjectSpec(x.Metadata, spec) || x.Metadata.ID != x.ObjectID || x.ExpiresAt.IsZero() {
			return model.Attachment{}, unknown("invalid_response")
		}
		journal.ObjectID, journal.Phase = x.ObjectID, "reserved"
		if err = save(); err != nil {
			return model.Attachment{}, err
		}
	}
	if !model.ValidEnterpriseAttachmentID(journal.ObjectID) {
		return model.Attachment{}, ErrPrivateState
	}
	object := b.objectPath(r, journal.ObjectID)
	verify := struct {
		ClientID string `json:"client_id"`
	}{journal.VerifyID}
	accept := func(reply attachmentReply) (model.Attachment, error) {
		x := reply.Receipt
		if x == nil || x.Version != 1 || x.Kind != "verify" || x.RoomID != r.RoomID || x.ObjectID != journal.ObjectID || x.State != "verified" || !matchesObject(x.Metadata, journal.ObjectID, spec) || x.Metadata.MediaType != meta.MediaType || x.Metadata.Width != meta.Width || x.Metadata.Height != meta.Height {
			return model.Attachment{}, unknown("invalid_response")
		}
		journal.Metadata, journal.Phase = x.Metadata, "verified"
		if err := save(); err != nil {
			return model.Attachment{}, err
		}
		if err := clearUploadBytes(bytesPath, data); err != nil {
			return model.Attachment{}, err
		}
		return journal.Metadata, nil
	}
	if journal.Phase == "verify_unknown" {
		reply, e := b.attachmentOperation(ctx, r, object+"/resolve-verification", verify)
		if e != nil {
			return model.Attachment{}, e
		}
		if reply.Outcome != "not_committed" {
			return accept(reply)
		}
		journal.Phase = "upload_unknown"
		if err = save(); err != nil {
			return model.Attachment{}, err
		}
	}
	if journal.Phase == "upload_unknown" {
		journal.Phase = "verify_unknown"
		if err = save(); err != nil {
			return model.Attachment{}, err
		}
		reply, e := b.attachmentOperation(ctx, r, object+"/verify", verify)
		if e == nil {
			return accept(reply)
		}
		var failure *Error
		if !errors.As(e, &failure) || failure.Outcome != "not_committed" || failure.Class != "object_missing" {
			return model.Attachment{}, e
		}
		journal.Phase = "upload_unknown"
		if err = save(); err != nil {
			return model.Attachment{}, err
		}
		if !retry {
			return model.Attachment{}, ErrUploadUnresolved
		}
		journal.Phase = "reserved"
		if err = save(); err != nil {
			return model.Attachment{}, err
		}
	}
	if journal.Phase == "reserved" {
		journal.Phase = "upload_unknown"
		if err = save(); err != nil {
			return model.Attachment{}, err
		}
		status, response, e := b.parent.objectRequest(ctx, http.MethodPut, object+"/content", data, r.Grant.Token, 64<<10)
		if e != nil {
			return model.Attachment{}, e
		}
		if status != http.StatusOK {
			return model.Attachment{}, wireFailure(status, response)
		}
		var receipt struct {
			Status   string `json:"status"`
			ObjectID string `json:"object_id"`
			Size     int64  `json:"size"`
			SHA256   string `json:"sha256"`
		}
		if decodeWire(response, &receipt) != nil || receipt.Status != "uploaded" || receipt.ObjectID != journal.ObjectID || receipt.Size != spec.Size || receipt.SHA256 != spec.SHA256 {
			return model.Attachment{}, unknown("invalid_response")
		}
		journal.Phase = "uploaded"
		if err = save(); err != nil {
			return model.Attachment{}, err
		}
	}
	if journal.Phase != "uploaded" {
		return model.Attachment{}, ErrPrivateState
	}
	journal.Phase = "verify_unknown"
	if err = save(); err != nil {
		return model.Attachment{}, err
	}
	reply, err := b.attachmentOperation(ctx, r, object+"/verify", verify)
	if err != nil {
		return model.Attachment{}, err
	}
	return accept(reply)
}

func readUploadBytes(path string) ([]byte, error) {
	f, err := privatefile.Open(path, attachment.MaxImageBytes)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, attachment.MaxImageBytes+1))
	if err != nil || int64(len(data)) > attachment.MaxImageBytes {
		return nil, ErrPrivateState
	}
	return data, nil
}
func retainUploadBytes(path string, data []byte, create bool) error {
	original, err := readUploadBytes(path)
	if err == nil {
		if !bytes.Equal(original, data) {
			return &Error{Outcome: "conflict", Class: "payload_conflict"}
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) || !create {
		return ErrPrivateState
	}
	temp, err := privatefile.CreateTemp(filepath.Dir(path))
	if err != nil {
		return ErrPrivateState
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(data); err != nil {
		_ = temp.Close()
		return ErrPrivateState
	}
	if err = temp.Sync(); err != nil {
		_ = temp.Close()
		return ErrPrivateState
	}
	if err = temp.Close(); err != nil {
		return ErrPrivateState
	}
	if privatefile.Install(temp.Name(), path) != nil {
		return ErrPrivateState
	}
	return nil
}
func clearUploadBytes(path string, data []byte) error {
	original, err := readUploadBytes(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !bytes.Equal(original, data) {
		return ErrPrivateState
	}
	if os.Remove(path) != nil || privatefile.SyncDirectory(filepath.Dir(path)) != nil {
		return ErrPrivateState
	}
	return nil
}

func matchesObjectSpec(a model.Attachment, s attachmentSpec) bool {
	return a.Name == s.Name && a.Kind == s.Kind && a.Size == s.Size && a.SHA256 == s.SHA256
}
func matchesObject(a model.Attachment, id string, s attachmentSpec) bool {
	return a.ID == id && matchesObjectSpec(a, s) && validateObjectMetadata(a) == nil
}
func validateObjectMetadata(a model.Attachment) error {
	if !model.ValidEnterpriseAttachmentID(a.ID) || attachment.ValidateObjectSpec(a.Name, a.Kind, a.Size, a.SHA256) != nil || a.CreatedAt.IsZero() || len(a.Source) > 128 || strings.IndexFunc(a.Source, unicode.IsControl) >= 0 {
		return errors.New("invalid enterprise attachment manifest")
	}
	if a.Kind == "file" && (a.MediaType != "text/plain" || a.Width != 0 || a.Height != 0) || a.Kind == "image" && (a.MediaType != "image/png" && a.MediaType != "image/jpeg" && a.MediaType != "image/gif" && a.MediaType != "image/webp" || a.Width <= 0 || a.Height <= 0 || a.Width > attachment.MaxImageDimension || a.Height > attachment.MaxImageDimension || int64(a.Width)*int64(a.Height) > attachment.MaxImagePixels) {
		return errors.New("invalid enterprise attachment content metadata")
	}
	return nil
}
func sameObject(a, b model.Attachment) bool {
	a.CreatedAt, b.CreatedAt = a.CreatedAt.UTC(), b.CreatedAt.UTC()
	return a == b
}

func (b *BindingClient) prefetchAttachments(ctx context.Context, r bindingRecord, metadata []model.Attachment) ([]model.AgentAttachment, error) {
	if len(metadata) > attachment.MaxImagesPerMessage {
		return nil, errors.New("enterprise attachment count exceeds limit")
	}
	seen := map[string]bool{}
	var total int64
	for _, a := range metadata {
		if validateObjectMetadata(a) != nil || seen[a.ID] {
			return nil, errors.New("invalid enterprise attachment manifest")
		}
		seen[a.ID] = true
		total += a.Size
	}
	if total > attachment.MaxTotalImageBytes {
		return nil, errors.New("enterprise attachment total exceeds limit")
	}
	prepared := make([]model.AgentAttachment, 0, len(metadata))
	for _, a := range metadata {
		path, err := b.cachedAttachment(ctx, r, a)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, model.AgentAttachment{Attachment: a, Path: path})
	}
	return prepared, nil
}
func (b *BindingClient) cachedAttachment(ctx context.Context, r bindingRecord, expected model.Attachment) (string, error) {
	dir := filepath.Join(b.dir, "evidence")
	if privatefile.Mkdir(dir) != nil {
		return "", ErrPrivateState
	}
	cache := func(data []byte) (string, error) {
		unlock, err := privatelock.Lock(ctx, dir)
		if err != nil {
			return "", err
		}
		defer unlock()
		store, err := attachment.Open(dir, r.Workspace)
		if err != nil {
			return "", ErrPrivateState
		}
		if existing, path, err := store.Resolve(expected.ID); err == nil && sameObject(existing, expected) {
			return path, nil
		}
		if data == nil {
			return "", nil
		}
		if _, err := store.ImportVerified(expected, bytes.NewReader(data)); err != nil {
			return "", errors.New("enterprise evidence cache rejected content or capacity")
		}
		actual, path, err := store.Resolve(expected.ID)
		if err != nil || !sameObject(actual, expected) {
			return "", errors.New("enterprise evidence cache metadata mismatch")
		}
		return path, nil
	}
	path, err := cache(nil)
	if err != nil || path != "" {
		return path, err
	}
	status, data, err := b.parent.objectRequest(ctx, http.MethodGet, b.objectPath(r, expected.ID)+"/content", nil, r.Grant.Token, int(expected.Size))
	if err != nil {
		return "", err
	}
	if status != http.StatusOK || int64(len(data)) != expected.Size {
		return "", errors.New("enterprise evidence download was not authorized or complete")
	}
	return cache(data)
}

func enterpriseAttachmentEnvelope(r bindingRecord, claim *relay.StructuredClaim, prepared []model.AgentAttachment) (*relay.Claim, error) {
	if claim == nil || !validID(claim.ID) || claim.ID != claim.Message.ID || !validID(claim.Receipt) || claim.PeerGeneration == 0 || claim.Message.To != r.Slot || claim.Message.TargetGeneration != r.Generation || claim.Message.State != "queued" || len(prepared) != len(claim.Message.Attachments) {
		return nil, unknown("invalid_response")
	}
	m := claim.Message
	for i, a := range prepared {
		if !sameObject(a.Attachment, m.Attachments[i]) || !filepath.IsAbs(a.Path) {
			return nil, unknown("invalid_response")
		}
	}
	handle := "@user"
	if m.From.ValidParticipant() {
		if !m.FromRuntime.Valid() || m.FromGeneration == 0 {
			return nil, unknown("invalid_response")
		}
		handle = model.NativeParticipantIdentityFor(m.From).MentionHandle
	} else if m.From != model.ActorUser {
		return nil, unknown("invalid_response")
	} else if m.Author != "" && !relay.ValidEnterpriseHumanAuthor(m.Author) {
		return nil, unknown("invalid_response")
	}
	input := model.AgentInput{From: m.From, To: m.To, FromHandle: handle, Text: m.Text, Quote: m.Quote, FromRuntime: m.FromRuntime, FromGeneration: m.FromGeneration, PeerHandle: model.NativeParticipantIdentityFor(model.OtherParticipant(m.To)).MentionHandle, PeerGeneration: claim.PeerGeneration, Attachments: prepared}
	if m.From == model.ActorUser && relay.ValidEnterpriseHumanAuthor(m.Author) {
		input.Author = m.Author
	}
	if m.Review != nil {
		input.Text += m.Review.Envelope()
	}
	return &relay.Claim{ID: claim.ID, Receipt: claim.Receipt, PeerGeneration: claim.PeerGeneration, Envelope: prompt.Envelope(input)}, nil
}
