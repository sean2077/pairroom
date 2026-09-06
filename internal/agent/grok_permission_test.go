package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestGrokPermissionSelectionNeverWidensOrGuessesScope(t *testing.T) {
	once := grokPermissionOption{ID: "once", Kind: "allow_once"}
	always := grokPermissionOption{ID: "always", Kind: "allow_always"}
	reject := grokPermissionOption{ID: "reject", Kind: "reject_once"}
	for _, test := range []struct {
		name, decision, want string
		options              []grokPermissionOption
	}{
		{"once", "accept", "once", []grokPermissionOption{once, always}},
		{"no persistent fallback", "accept", "", []grokPermissionOption{always}},
		{"no once fallback", "acceptForSession", "", []grokPermissionOption{once}},
		{"legacy persistent choice", "acceptForSession", "always", []grokPermissionOption{once, always}},
		{"explicit once", "option:once", "once", []grokPermissionOption{once, always}},
		{"explicit persistent", "option:always", "always", []grokPermissionOption{once, always}},
		{"reject once", "decline", "reject", []grokPermissionOption{reject}},
		{"no persistent rejection fallback", "decline", "", []grokPermissionOption{{ID: "never", Kind: "reject_always"}}},
		{"ambiguous hint", "accept", "", []grokPermissionOption{once, {ID: "manual", Kind: "allow_once"}}},
		{"explicit disambiguation", "option:manual", "manual", []grokPermissionOption{once, {ID: "manual", Kind: "allow_once"}}},
		{"duplicate ID", "option:once", "", []grokPermissionOption{once, {ID: "once", Kind: "allow_always"}}},
		{"duplicate ID via hint", "accept", "", []grokPermissionOption{once, {ID: "once", Kind: "allow_always"}}},
		{"missing ID", "option:", "", []grokPermissionOption{once}},
		{"unknown decision", "approve", "", []grokPermissionOption{once}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := selectGrokPermissionOption(test.options, test.decision); got != test.want {
				t.Fatalf("decision %q: got %q, want %q", test.decision, got, test.want)
			}
		})
	}
}

func TestGrokInvalidPermissionLeavesNativeRequestAnswerable(t *testing.T) {
	writer := &testWriteCloser{}
	adapter := NewGrok(Config{Actor: model.ActorClaude}, func(model.RuntimeEvent) {})
	adapter.stdin = writer
	adapter.approvals["permission"] = grokPendingApproval{
		rawID: json.RawMessage(`"native-request"`), kind: "permission",
		options: []grokPermissionOption{{ID: "remember", Name: "Remember this choice", Kind: "allow_always"}},
	}
	if err := adapter.ResolveApproval(context.Background(), "permission", model.ApprovalResolution{Decision: "accept"}); err == nil {
		t.Fatal("allow-once must not broaden to allow-always")
	}
	if writer.Len() != 0 || len(adapter.approvals) != 1 {
		t.Fatal("invalid choice wrote or consumed the native request")
	}
	if err := adapter.ResolveApproval(context.Background(), "permission", model.ApprovalResolution{Decision: "option:remember"}); err != nil {
		t.Fatal(err)
	}
	var reply struct {
		ID     string `json:"id"`
		Result struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal(writer.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.ID != "native-request" || reply.Result.Outcome.Outcome != "selected" || reply.Result.Outcome.OptionID != "remember" {
		t.Fatalf("unexpected native reply: %s", writer.String())
	}
	if err := adapter.ResolveApproval(context.Background(), "permission", model.ApprovalResolution{Decision: "option:remember"}); err == nil {
		t.Fatal("consumed request accepted twice")
	}
	if strings.Count(writer.String(), "\n") != 1 {
		t.Fatal("wrote more than one native response")
	}
}

func TestGrokCancellationIsNotRememberedRejection(t *testing.T) {
	pending := grokPendingApproval{kind: "permission", options: []grokPermissionOption{
		{ID: "once", Kind: "reject_once"}, {ID: "never", Kind: "reject_always"},
	}}
	result, err := grokApprovalResult(pending, "cancel")
	encoded, _ := json.Marshal(result)
	if err != nil || string(encoded) != `{"outcome":{"outcome":"cancelled"}}` {
		t.Fatalf("cancel must use the ACP cancellation outcome: %s, %v", encoded, err)
	}
	pending.options = pending.options[1:]
	result, err = grokApprovalResult(pending, "decline")
	encoded, _ = json.Marshal(result)
	if err != nil || string(encoded) != `{"outcome":{"outcome":"cancelled"}}` {
		t.Fatalf("one rejection was remembered: %s, %v", encoded, err)
	}
	if _, err := grokApprovalResult(grokPendingApproval{kind: "plan"}, "garbage"); err == nil {
		t.Fatal("invalid plan decision silently cancelled the native request")
	}
}
