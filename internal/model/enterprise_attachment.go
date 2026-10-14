package model

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// EnterpriseAttachmentIDs is a bounded, ordered, comparable publication
// selector. Its decoder rejects overflow instead of Go's fixed-array decoder
// silently discarding a ninth item from an idempotency-bearing payload.
type EnterpriseAttachmentIDs [8]string

func ValidEnterpriseAttachmentID(id string) bool {
	if len(id) != 28 || !strings.HasPrefix(id, "att-") {
		return false
	}
	bytes, err := hex.DecodeString(id[4:])
	return err == nil && len(bytes) == 12 && hex.EncodeToString(bytes) == id[4:]
}

func NewEnterpriseAttachmentIDs(values []string) (EnterpriseAttachmentIDs, error) {
	var ids EnterpriseAttachmentIDs
	if len(values) > len(ids) {
		return ids, errors.New("too many enterprise attachments")
	}
	copy(ids[:], values)
	if err := ids.Validate(); err != nil {
		return EnterpriseAttachmentIDs{}, err
	}
	return ids, nil
}

func (ids EnterpriseAttachmentIDs) Validate() error {
	empty := false
	for i, id := range ids {
		if id == "" {
			empty = true
			continue
		}
		if empty || !ValidEnterpriseAttachmentID(id) {
			return errors.New("invalid enterprise attachment selector")
		}
		for _, prior := range ids[:i] {
			if prior == id {
				return errors.New("duplicate enterprise attachment selector")
			}
		}
	}
	return nil
}

func (ids EnterpriseAttachmentIDs) Slice() []string {
	var values []string
	for _, id := range ids {
		if id == "" {
			break
		}
		values = append(values, id)
	}
	return values
}

func (ids EnterpriseAttachmentIDs) MarshalJSON() ([]byte, error) {
	if err := ids.Validate(); err != nil {
		return nil, err
	}
	values := ids.Slice()
	if values == nil {
		values = []string{}
	}
	return json.Marshal(values)
}

func (ids *EnterpriseAttachmentIDs) UnmarshalJSON(data []byte) error {
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	parsed, err := NewEnterpriseAttachmentIDs(values)
	if err != nil {
		return err
	}
	*ids = parsed
	return nil
}
