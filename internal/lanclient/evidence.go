package lanclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/relay"
)

func validateAttachment(a model.Attachment) error {
	if !lanshare.ValidID(a.ID) || a.Size <= 0 || a.Size > attachment.MaxImageBytes || !lanshare.ValidFingerprint(a.SHA256) || a.Kind != "image" && a.Kind != "file" {
		return errors.New("invalid LAN attachment manifest")
	}
	return nil
}

func (c *Client) cachedEvidence(ctx context.Context, r record, expected model.Attachment) (model.Attachment, string, error) {
	if err := validateAttachment(expected); err != nil {
		return model.Attachment{}, "", err
	}
	dir := filepath.Join(c.dir, "evidence")
	if err := privatefile.Mkdir(dir); err != nil {
		return model.Attachment{}, "", err
	}
	unlock, err := privatelock.Lock(ctx, dir)
	if err != nil {
		return model.Attachment{}, "", err
	}
	defer unlock()
	media, err := attachment.Open(dir, r.Workspace)
	if err != nil {
		return model.Attachment{}, "", errors.New("LAN evidence cache is unavailable")
	}
	metadata, localPath, err := media.Resolve(expected.ID)
	if err == nil && metadata == expected {
		return metadata, localPath, nil
	}
	client, err := c.httpFor(r)
	if err != nil {
		return model.Attachment{}, "", safeError(err)
	}
	body, _ := json.Marshal(map[string]string{"id": expected.ID})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Invite.Endpoint+"/lan/v1/rooms/"+r.Invite.RoomID+"/download", bytes.NewReader(body))
	if err != nil {
		return model.Attachment{}, "", safeError(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return model.Attachment{}, "", safeError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return model.Attachment{}, "", safeError(&lanshare.Error{Status: response.StatusCode})
	}
	if response.ContentLength > expected.Size {
		return model.Attachment{}, "", errors.New("LAN evidence exceeds its manifest")
	}
	if _, err := media.ImportVerified(expected, io.LimitReader(response.Body, expected.Size+1)); err != nil {
		if errors.Is(err, attachment.ErrSharedQuota) {
			return model.Attachment{}, "", joinedRoomQuotaError{}
		}
		if errors.Is(err, attachment.ErrTemporaryQuota) {
			return model.Attachment{}, "", err
		}
		return model.Attachment{}, "", errors.New("LAN evidence failed content verification")
	}
	metadata, localPath, err = media.Resolve(expected.ID)
	if err != nil || metadata != expected {
		return model.Attachment{}, "", errors.New("LAN attachment does not match accepted message")
	}
	return metadata, localPath, nil
}

// joinedRoomQuotaError names this machine's verified-evidence cache as the
// exhausted store. It keeps the shared-quota identity for callers, but gives the
// guest an actionable local remedy instead of the host-only "start a new Room"
// advice, and it deliberately carries no private cache path. The hosting Room
// still owns the authoritative bytes and re-verifies every download, so removed
// files are simply fetched again.
type joinedRoomQuotaError struct{}

func (joinedRoomQuotaError) Error() string {
	return "the joined Room's verified-evidence cache on this machine reached the 100 MiB bound; inspect and remove cached evidence you no longer need, then retry (the hosting Room's own storage is separate)"
}

func (joinedRoomQuotaError) Is(target error) bool { return target == attachment.ErrSharedQuota }

// Download serves only a host-authorized attachment in this Room. A cache hit
// still performs the fresh remote ACL check; possession of an opaque ID never
// grants access to another Room's evidence. The returned path is private and
// verified on this machine, not a sender-provided filesystem path.
func (c *Client) Download(ctx context.Context, id string) (model.Attachment, string, error) {
	r, err := c.ownerRecord(ctx, false)
	if err != nil {
		return model.Attachment{}, "", err
	}
	if !lanshare.ValidID(id) {
		return model.Attachment{}, "", errors.New("invalid attachment ID")
	}
	var expected model.Attachment
	if err := c.call(ctx, r, "attachment", map[string]string{"id": id}, &expected); err != nil {
		return model.Attachment{}, "", safeError(err)
	}
	if expected.ID != id {
		return model.Attachment{}, "", errors.New("LAN attachment receipt mismatch")
	}
	return c.cachedEvidence(ctx, r, expected)
}

// Upload accepts one explicit local evidence stream. The source file path and
// the local authorization headers are not forwarded. A verified receipt must
// identify the same bytes before its ID can enter a send/publication.
func (c *Client) Upload(ctx context.Context, auth relay.Auth, kind, name string, reader io.Reader) (model.Attachment, error) {
	r, err := c.authenticated(ctx, auth, false)
	if err != nil {
		return model.Attachment{}, err
	}
	if kind == "" {
		kind = "image"
	}
	if kind != "image" && kind != "file" || reader == nil {
		return model.Attachment{}, errors.New("invalid evidence kind or stream")
	}
	data, err := io.ReadAll(io.LimitReader(reader, attachment.MaxImageBytes+1))
	if err != nil || len(data) == 0 || int64(len(data)) > attachment.MaxImageBytes {
		return model.Attachment{}, errors.New("attachment upload exceeds limit or is unreadable")
	}
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || name == "." || len(name) > 256 || strings.ContainsAny(name, "\r\n\x00") {
		return model.Attachment{}, errors.New("invalid attachment name")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", name)
	if err != nil {
		return model.Attachment{}, err
	}
	if _, err := part.Write(data); err != nil {
		return model.Attachment{}, err
	}
	if err := form.Close(); err != nil {
		return model.Attachment{}, err
	}
	client, err := c.httpFor(r)
	if err != nil {
		return model.Attachment{}, safeError(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Invite.Endpoint+"/lan/v1/rooms/"+r.Invite.RoomID+"/upload", &body)
	if err != nil {
		return model.Attachment{}, safeError(err)
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("X-PairRoom-Attachment-Kind", kind)
	response, err := client.Do(request)
	if err != nil {
		return model.Attachment{}, safeError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return model.Attachment{}, safeError(&lanshare.Error{Status: response.StatusCode})
	}
	var metadata model.Attachment
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<10))
	if decoder.Decode(&metadata) != nil || decoder.Decode(new(any)) != io.EOF || validateAttachment(metadata) != nil {
		return model.Attachment{}, errors.New("invalid LAN attachment receipt")
	}
	sum := sha256.Sum256(data)
	if metadata.Kind != kind || metadata.Size != int64(len(data)) || metadata.SHA256 != hex.EncodeToString(sum[:]) {
		return model.Attachment{}, errors.New("LAN attachment receipt does not match uploaded bytes")
	}
	return metadata, nil
}
