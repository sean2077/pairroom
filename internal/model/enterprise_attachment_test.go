package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnterpriseAttachmentIDsRejectTruncationAndAmbiguousOrder(t *testing.T) {
	const one = "att-111111111111111111111111"
	const two = "att-222222222222222222222222"
	for _, body := range []string{`["` + one + `","` + one + `"]`, `["","` + one + `"]`, `["../object"]`, `[` + strings.Repeat(`"`+one+`",`, 8) + `"` + two + `"]`} {
		var ids EnterpriseAttachmentIDs
		if json.Unmarshal([]byte(body), &ids) == nil {
			t.Fatalf("ambiguous selector accepted: %s", body)
		}
	}
	var ids EnterpriseAttachmentIDs
	if err := json.Unmarshal([]byte(`["`+one+`","`+two+`"]`), &ids); err != nil {
		t.Fatal(err)
	}
	if ids[0] != one || ids[1] != two || ids[2] != "" {
		t.Fatal("selector order changed")
	}
	type intent struct {
		Text string                  `json:"text"`
		IDs  EnterpriseAttachmentIDs `json:"attachment_ids,omitzero"`
	}
	encoded, err := json.Marshal(intent{Text: "original"})
	if err != nil || string(encoded) != `{"text":"original"}` {
		t.Fatal("empty selector changed prior text-only intent bytes")
	}
}
