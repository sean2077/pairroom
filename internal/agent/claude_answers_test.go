package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestClaudeQuestionAnswersMatchTheCompleteNativeRequest(t *testing.T) {
	for _, test := range []struct {
		name    string
		input   string
		answers map[string]string
	}{
		{"partial", `{"questions":[{"question":"First?"},{"question":"Second?"}]}`, map[string]string{"First?": "answer"}},
		{"unknown", `{"questions":[{"question":"First?"}]}`, map[string]string{"First?": "answer", "Other?": "extra"}},
		{"blank", `{"questions":[{"question":"First?"}]}`, map[string]string{"First?": " \n "}},
		{"empty list", `{"questions":[]}`, map[string]string{"First?": "answer"}},
		{"missing text", `{"questions":[{"header":"First?"}]}`, map[string]string{"First?": "answer"}},
		{"duplicate identity", `{"questions":[{"question":"First?"},{"question":"First?"}]}`, map[string]string{"First?": "answer"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := &testWriteCloser{}
			adapter := NewClaude(Config{}, func(model.RuntimeEvent) {})
			adapter.stdin = writer
			adapter.approvals["question"] = claudeApprovalRequest{requestID: "native", toolName: "AskUserQuestion", input: json.RawMessage(test.input)}
			if err := adapter.ResolveApproval(context.Background(), "question", model.ApprovalResolution{Decision: "accept", Answers: test.answers}); err == nil {
				t.Fatal("invalid or incomplete answers crossed the native boundary")
			}
			if writer.Len() != 0 || len(adapter.approvals) != 1 {
				t.Fatal("failed validation consumed the pending question")
			}
			if err := adapter.ResolveApproval(context.Background(), "question", model.ApprovalResolution{Decision: "decline"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
