package enterpriseauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

const headlessAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewHeadlessConfirmationCode creates a 40-bit human confirmation value. It
// is never an endpoint proof or a credential and is not sufficient for pickup.
func NewHeadlessConfirmationCode() (string, error) {
	var random [5]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", ErrDeviceRequestInvalid
	}
	return base32.NewEncoding(headlessAlphabet).WithPadding(base32.NoPadding).EncodeToString(random[:]), nil
}

// NormalizeHeadlessConfirmationCode accepts only the exact uppercase alphabet
// in compact or XXXX-XXXX form. Ambiguous aliases and whitespace are rejected.
func NormalizeHeadlessConfirmationCode(value string) (string, error) {
	if len(value) == 9 && value[4] == '-' {
		value = value[:4] + value[5:]
	}
	if len(value) != 8 {
		return "", ErrDeviceRequestInvalid
	}
	for _, c := range value {
		if !strings.ContainsRune(headlessAlphabet, c) {
			return "", ErrDeviceRequestInvalid
		}
	}
	return value, nil
}

func HeadlessConfirmationHash(value string) ([32]byte, error) {
	code, err := NormalizeHeadlessConfirmationCode(value)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256([]byte("pairroom-device-confirmation-v1\x00" + code)), nil
}

// HeadlessCandidateFingerprint compares frozen candidates across the CLI and
// consent page without exposing their private access or refresh material.
func HeadlessCandidateFingerprint(grantID string, accessHash, refreshHash [32]byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte("pairroom-device-candidate-v1\x00" + grantID + "\x00"))
	_, _ = h.Write(accessHash[:])
	_, _ = h.Write(refreshHash[:])
	return hex.EncodeToString(h.Sum(nil)[:16])
}
