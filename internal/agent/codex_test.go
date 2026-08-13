package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCodexApprovalResult(t *testing.T) {
	command := pendingApproval{method: "item/commandExecution/requestApproval"}
	got, err := codexApprovalResult(command, "acceptForSession")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]any{"decision": "acceptForSession"}) {
		t.Fatalf("unexpected command response: %#v", got)
	}

	permission := pendingApproval{
		method: "item/permissions/requestApproval",
		params: json.RawMessage(`{"permissions":{"fileSystem":{"write":["/repo"]},"network":{"enabled":true}}}`),
	}
	got, err = codexApprovalResult(permission, "accept")
	if err != nil {
		t.Fatal(err)
	}
	if got["scope"] != "turn" {
		t.Fatalf("unexpected turn scope: %#v", got)
	}
	permissions, ok := got["permissions"].(map[string]any)
	if !ok || permissions["fileSystem"] == nil || permissions["network"] == nil {
		t.Fatalf("requested permission profile was not preserved: %#v", got)
	}

	got, err = codexApprovalResult(permission, "acceptForSession")
	if err != nil {
		t.Fatal(err)
	}
	if got["scope"] != "session" {
		t.Fatalf("unexpected session scope: %#v", got)
	}

	got, err = codexApprovalResult(permission, "decline")
	if err != nil {
		t.Fatal(err)
	}
	denied, ok := got["permissions"].(map[string]any)
	if !ok || len(denied) != 0 {
		t.Fatalf("decline must grant an empty subset: %#v", got)
	}
}

func TestCodexApprovalResultRejectsMalformedPermissionRequest(t *testing.T) {
	_, err := codexApprovalResult(pendingApproval{
		method: "item/permissions/requestApproval",
		params: json.RawMessage(`{"permissions":`),
	}, "accept")
	if err == nil {
		t.Fatal("expected malformed request to fail closed")
	}
}

func TestParseCodexRequestID(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`42`), json.RawMessage(`"42"`)} {
		got, err := ParseCodexRequestID(raw)
		if err != nil || got != 42 {
			t.Fatalf("ParseCodexRequestID(%s) = %d, %v", raw, got, err)
		}
	}
}
