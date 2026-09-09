package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestRedactorPreservesJSONAndCoversEscapedSecrets(t *testing.T) {
	const secret = "fixture-\"key\\with\ncharacters"
	redactor := newSecretRedactor(map[string]string{"OPENAI_API_KEY": secret})
	encoded, err := json.Marshal(map[string]any{"text": secret, "nested": []any{map[string]string{secret: secret}}, "count": json.Number("9007199254740993")})
	if err != nil {
		t.Fatal(err)
	}
	result := redactor.raw(encoded)
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("redaction invalidated JSON: %s: %v", result, err)
	}
	var text string
	if err := json.Unmarshal(decoded["text"], &text); err != nil {
		t.Fatal(err)
	}
	if text != "[redacted]" {
		t.Fatalf("escaped credential was not removed: %q", text)
	}
	if string(decoded["count"]) != "9007199254740993" {
		t.Fatalf("integer precision lost: %s", decoded["count"])
	}
	if strings.Contains(string(result), `fixture-`) {
		t.Fatalf("nested/key credential leaked: %s", result)
	}
	unicodeSecret := newSecretRedactor(map[string]string{"TOKEN": "fixture-secret"})
	if result := unicodeSecret.raw(json.RawMessage(`{"text":"\u0066ixture-secret"}`)); !strings.Contains(string(result), "[redacted]") {
		t.Fatalf("JSON Unicode escape bypassed redaction: %s", result)
	}
}

func TestRedactorCoversDisplayMetadataAndOverlappingSecrets(t *testing.T) {
	redactor := newSecretRedactor(map[string]string{"TOKEN": "fixture-secret", "API_KEY": "fixture-secret-long"})
	if got := redactor.text("fixture-secret-long"); got != "[redacted]" {
		t.Fatalf("partial credential leaked: %q", got)
	}
	original := &model.RuntimeInfo{Version: "fixture-secret-long", Protocol: "fixture-secret-long", SessionName: "fixture-secret-long", SessionNameStatus: "fixture-secret-long", Provider: "fixture-secret-long", ProviderName: "fixture-secret-long"}
	event := redactor.event(model.RuntimeEvent{Runtime: original})
	data, _ := json.Marshal(event)
	if strings.Contains(string(data), "fixture-secret") {
		t.Fatalf("display metadata leaked a credential: %s", data)
	}
	if original.Version != "fixture-secret-long" {
		t.Fatal("redaction mutated adapter-owned metadata")
	}
	cause := errors.New("fixture-secret-long")
	err := redactor.err(cause)
	if !errors.Is(err, cause) || err.Error() != "[redacted]" {
		t.Fatalf("error identity/redaction lost: %v", err)
	}
}

func TestRedactorFailsClosedOnMalformedJSON(t *testing.T) {
	r := newSecretRedactor(map[string]string{"API_KEY": "fixture-secret"})
	for _, raw := range []string{`{"bad":"fixture-secret"`, `{"valid":true} {"other":"fixture-secret"}`} {
		result := r.raw(json.RawMessage(raw))
		if !json.Valid(result) || strings.Contains(string(result), "fixture-secret") {
			t.Fatalf("unsafe malformed diagnostic: %s", result)
		}
	}
}
