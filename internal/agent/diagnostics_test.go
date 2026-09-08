package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

type diagnosticAdapter struct {
	Adapter
	start func(context.Context) error
	turn  func(context.Context, model.AgentInput) error
	stop  func(context.Context) error
}

func (a diagnosticAdapter) Start(ctx context.Context) error { return a.start(ctx) }
func (a diagnosticAdapter) StartTurn(ctx context.Context, input model.AgentInput) error {
	return a.turn(ctx, input)
}
func (a diagnosticAdapter) Stop(ctx context.Context) error { return a.stop(ctx) }

func TestRuntimeDiagnosticEvidenceAndCleanup(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git required for disposable worktree")
	}
	for _, test := range []struct{ name, code string }{
		{"success", "responded"}, {"deltas", "responded"}, {"wrong-response", "unexpected_response"},
		{"wrong-turn", "unexpected_response"}, {"approval", "interaction_required"}, {"tool", "interaction_required"},
		{"auth", "authentication_failed"}, {"quota", "quota_or_rate_limit"}, {"startup", "startup_failed"},
		{"cancel", "cancelled"}, {"overflow", "output_limit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			realRepo := t.TempDir()
			var workspace string
			stopped := false
			cfg := Config{Actor: model.ActorClaude, Runtime: model.RuntimeCodex, Repo: realRepo, DataDir: realRepo,
				SessionID: "existing-private-session", RequireExactSession: true, RoomID: "private-room", RoomName: "private-name",
				AdditionalInstructions: "write production files", Model: "selected-model", Effort: "high", Env: map[string]string{"API_KEY": "secret-fixture"},
				PermissionMode: "yolo", Sandbox: "danger-full-access", ApprovalPolicy: "yolo"}
			factory := func(actual Config, sink EventSink) Adapter {
				workspace = actual.Repo
				if actual.Repo == realRepo || actual.DataDir == realRepo || actual.SessionID != "" || actual.RequireExactSession || actual.RoomID != "" || actual.RoomName != "" || actual.AdditionalInstructions != "" || actual.Collaboration != nil {
					t.Fatalf("diagnostic reused private Room state: %#v", actual)
				}
				if actual.Model != cfg.Model || actual.Effort != cfg.Effort || actual.Runtime != cfg.Runtime || actual.Env["API_KEY"] != "secret-fixture" {
					t.Fatal("explicit runtime selection was lost")
				}
				if actual.Sandbox != "read-only" || actual.ApprovalPolicy != "never" || actual.PermissionMode != "plan" {
					t.Fatal("YOLO inherited")
				}
				return diagnosticAdapter{
					start: func(context.Context) error {
						if _, err := os.Stat(actual.Repo + string(os.PathSeparator) + ".git"); err != nil {
							t.Fatal(err)
						}
						if test.name == "startup" {
							return errors.New("secret-fixture startup failed in /private/path")
						}
						return nil
					},
					turn: func(_ context.Context, input model.AgentInput) error {
						if input.Role != model.RoleReviewer {
							t.Fatal("diagnostic input must be read-only")
						}
						id, marker := input.MessageID, "PAIRROOM_CHECK_"+input.MessageID
						if !strings.Contains(input.Text, marker) {
							t.Fatal("nonce missing from input")
						}
						send := func(kind, text, correlation string) {
							sink(model.RuntimeEvent{Agent: actual.Actor, Kind: kind, Text: text, CorrelationID: correlation})
						}
						switch test.name {
						case "success":
							send(model.RuntimeFinal, marker, id)
						case "deltas":
							send(model.RuntimeTextDelta, "PAIRROOM_CHECK_", id)
							send(model.RuntimeTextDelta, id, id)
						case "wrong-response":
							send(model.RuntimeFinal, "secret-fixture", id)
						case "wrong-turn":
							send(model.RuntimeFinal, marker, "other-turn")
							send(model.RuntimeInputCompleted, "", "other-turn")
						case "approval":
							send(model.RuntimeApprovalRequested, "", id)
						case "tool":
							send(model.RuntimeToolStarted, "", id)
						case "auth":
							return errors.New("401 unauthorized secret-fixture")
						case "quota":
							send(model.RuntimeInputFailed, "429 quota secret-fixture", id)
						case "cancel":
							cancel()
							return ctx.Err()
						case "overflow":
							send(model.RuntimeFinal, strings.Repeat("X", 65537), id)
						}
						send(model.RuntimeInputCompleted, "", id)
						return nil
					},
					stop: func(context.Context) error { stopped = true; return nil },
				}
			}
			checks := checkRuntime(ctx, cfg, factory)
			found := false
			for _, check := range checks {
				found = found || check.Code == test.code
			}
			if !found || !stopped {
				t.Fatalf("checks=%+v stopped=%v", checks, stopped)
			}
			if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary workspace remains: %v", err)
			}
			if _, err := os.Stat(realRepo); err != nil {
				t.Fatalf("original workspace changed: %v", err)
			}
			data, _ := json.Marshal(checks)
			for _, private := range []string{"secret-fixture", "existing-private-session", "private-room", "private-name", "selected-model", realRepo, workspace} {
				if strings.Contains(string(data), private) {
					t.Fatalf("report leaks %q", private)
				}
			}
		})
	}
}

func TestRuntimeDiagnosticClassification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if diagnosticErrorCode(ctx, "401 secret", "fallback") != "timeout" {
		t.Fatal("deadline must take precedence")
	}
	for _, test := range []struct{ text, code string }{
		{"unknown model secret", "model_unavailable"}, {"tls connection refused secret", "network_failed"},
		{"an unrecognized private error", "fallback"},
	} {
		if actual := diagnosticErrorCode(context.Background(), test.text, "fallback"); actual != test.code {
			t.Fatalf("got %s want %s", actual, test.code)
		}
	}
}

func TestInstallationDiagnosticDoesNotLeakRawOutput(t *testing.T) {
	check := CheckInstallation(context.Background(), Config{Command: "/private/secret-fixture/no-such-cli", Runtime: model.RuntimeGrok})
	data, _ := json.Marshal(check)
	if check.Status != "fail" || check.Code != "cli_unavailable" || strings.Contains(string(data), "secret-fixture") || strings.Contains(string(data), "/private") {
		t.Fatalf("unsafe installation report: %s", data)
	}
}

func TestProbeOutputBounded(t *testing.T) {
	buffer := &probeOutput{}
	input := []byte(strings.Repeat("x", 1<<20))
	n, err := buffer.Write(input)
	if err != nil || n != len(input) || len(buffer.String()) != 256<<10 || !buffer.overflow {
		t.Fatalf("capture not bounded: n=%d err=%v", n, err)
	}
}
