package attachment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

// ValidObjectID shares the opaque ID grammar with Local verified imports.
func ValidObjectID(id string) bool { return attachmentIDPattern.MatchString(id) }

// ValidateObjectSpec validates display metadata without opening a Local Store.
func ValidateObjectSpec(name, kind string, size int64, digest string) error {
	if name == "" || len(name) > 256 || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\") || strings.IndexFunc(name, unicode.IsControl) >= 0 || size <= 0 || size > MaxImageBytes || (kind != "file" && kind != "image") {
		return errors.New("invalid attachment specification")
	}
	b, err := hex.DecodeString(digest)
	if err != nil || len(b) != 32 || hex.EncodeToString(b) != digest {
		return errors.New("invalid attachment digest")
	}
	return nil
}

// ValidateObjectBytes derives content metadata with the existing image and text
// validators. Caller-provided MIME or dimensions are never authoritative.
func ValidateObjectBytes(name, kind string, data []byte) (model.Attachment, error) {
	digest := sha256.Sum256(data)
	meta := model.Attachment{Name: name, Kind: kind, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}
	if err := ValidateObjectSpec(name, kind, meta.Size, meta.SHA256); err != nil {
		return model.Attachment{}, err
	}
	if kind == "file" {
		if err := validateEvidence(data); err != nil {
			return model.Attachment{}, err
		}
		meta.MediaType = "text/plain"
		return meta, nil
	}
	meta.MediaType = canonicalImageType(http.DetectContentType(data))
	if meta.MediaType == "" {
		return model.Attachment{}, errors.New("unsupported attachment image type")
	}
	var err error
	meta.Width, meta.Height, err = decodeImageMetadata(bytes.NewReader(data), meta.MediaType, meta.Size)
	return meta, err
}
