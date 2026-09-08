package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/execx"
	"github.com/sean2077/pairroom/internal/model"
)

// DiagnosticCheck contains evidence categories, never process output, paths,
// credentials, model text, command arguments, or native session identities.
// A successful installation check is deliberately not a runtime-ready claim.
type DiagnosticCheck struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	Code       string            `json:"code"`
	Runtime    model.RuntimeKind `json:"runtime,omitempty"`
	Actor      model.ActorID     `json:"actor,omitempty"`
	Version    string            `json:"version,omitempty"`
	DurationMS int64             `json:"duration_ms"`
}

func CheckInstallation(ctx context.Context, cfg Config) DiagnosticCheck {
	start := time.Now()
	check := DiagnosticCheck{ID: "installation", Runtime: cfg.Runtime.CanonicalForSlot(cfg.Actor), Actor: cfg.Actor, Status: "pass", Code: "installed"}
	probe, err := ProbeRuntime(ctx, cfg)
	if err != nil {
		check.Status, check.Code = "fail", "cli_unavailable"
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			check.Code = "timeout"
		}
	} else {
		// Only numeric version components are allowed into support reports.
		if match := semanticVersionPattern.FindStringSubmatch(probe.Version); len(match) >= 4 {
			check.Version = strings.Join(match[1:4], ".")
		}
	}
	check.DurationMS = time.Since(start).Milliseconds()
	return check
}

// CheckRuntime starts a fresh native session in a disposable Git worktree and
// checks a nonce-bearing model response through the real adapter. It does not
// resume a Room, inherit its instructions, or change its workspace. Native user
// configuration (including hooks/MCP) still applies: this is an explicit,
// potentially billable operation, not a security sandbox or a passive probe.
func CheckRuntime(ctx context.Context, cfg Config) []DiagnosticCheck {
	return checkRuntime(ctx, cfg, SlotFactory(false, cfg.Runtime))
}

func checkRuntime(parent context.Context, cfg Config, factory Factory) (checks []DiagnosticCheck) {
	ctx, cancel := context.WithTimeout(parent, 75*time.Second)
	defer cancel()
	start := time.Now()
	checks = []DiagnosticCheck{
		{ID: "startup", Runtime: cfg.Runtime, Actor: cfg.Actor, Status: "fail", Code: "startup_failed"},
		{ID: "response", Runtime: cfg.Runtime, Actor: cfg.Actor, Status: "skipped", Code: "not_checked"},
	}
	finish := func(index int, status, code string) []DiagnosticCheck {
		checks[index].Status, checks[index].Code = status, code
		checks[index].DurationMS = time.Since(start).Milliseconds()
		return checks
	}
	root, err := os.MkdirTemp("", "pairroom-runtime-check-")
	if err != nil {
		return finish(0, "fail", "workspace_unavailable")
	}
	defer func() {
		if err := os.RemoveAll(root); err != nil {
			checks = append(checks, DiagnosticCheck{ID: "cleanup", Runtime: cfg.Runtime, Actor: cfg.Actor, Status: "warn", Code: "cleanup_failed"})
		}
	}()
	cfg.Repo, cfg.DataDir = filepath.Join(root, "workspace"), filepath.Join(root, "data")
	for _, dir := range []string{cfg.Repo, cfg.DataDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return finish(0, "fail", "workspace_unavailable")
		}
	}
	git := exec.CommandContext(ctx, "git", "-c", "init.templateDir=", "init", "--template=", cfg.Repo)
	execx.NoConsole(git)
	git.Stdout, git.Stderr = io.Discard, io.Discard
	git.WaitDelay = time.Second
	if err := git.Run(); err != nil {
		return finish(0, "fail", diagnosticErrorCode(ctx, "", "workspace_unavailable"))
	}
	cfg.SessionID, cfg.RoomID, cfg.RoomName = "", "", ""
	cfg.RequireExactSession = false
	cfg.Collaboration, cfg.LegacyRole, cfg.AdditionalInstructions = nil, "", ""
	cfg.PermissionMode, cfg.ApprovalPolicy, cfg.Sandbox = "plan", "never", "read-only"
	cfg.OrdinaryReviewerPolicy = ""
	cfg.SystemPrompt = "This is a connectivity diagnostic. Reply with the exact requested text only. Do not use tools, read files, run commands, or ask questions."
	id := model.NewID("diagnostic")
	marker := "PAIRROOM_CHECK_" + id
	events := make(chan model.RuntimeEvent, 64)
	overflow := make(chan struct{}, 1)
	adapter := factory(cfg, func(event model.RuntimeEvent) {
		// Bound both memory and callback work even for a noisy/broken runtime.
		if len(event.Text) > 64<<10 || len(event.CorrelationID) > 256 {
			select {
			case overflow <- struct{}{}:
			default:
			}
			return
		}
		switch event.Kind {
		case model.RuntimeFinal, model.RuntimeTextDelta, model.RuntimeInputCompleted,
			model.RuntimeInputFailed, model.RuntimeInputCancelled, model.RuntimeError,
			model.RuntimeApprovalRequested, model.RuntimeToolStarted:
			select {
			case events <- model.RuntimeEvent{Kind: event.Kind, Text: event.Text, CorrelationID: event.CorrelationID}:
			default:
				select {
				case overflow <- struct{}{}:
				default:
				}
			}
		}
	})
	// Start may partially spawn before failing. Stop is mandatory on every path.
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		if err := adapter.Stop(stopCtx); err != nil {
			checks = append(checks, DiagnosticCheck{ID: "cleanup", Runtime: cfg.Runtime, Actor: cfg.Actor, Status: "warn", Code: "cleanup_failed"})
		}
	}()
	if err := adapter.Start(ctx); err != nil {
		return finish(0, "fail", diagnosticErrorCode(ctx, err.Error(), "startup_failed"))
	}
	finish(0, "pass", "started")
	start = time.Now()
	input := model.AgentInput{MessageID: id, From: model.ActorUser, To: cfg.Actor, Role: model.RoleReviewer, Text: "Reply with exactly " + marker + ". Do not use any tools."}
	if err := adapter.StartTurn(ctx, input); err != nil {
		return finish(1, "fail", diagnosticErrorCode(ctx, err.Error(), "response_failed"))
	}
	var text strings.Builder
	matched := false
	for {
		select {
		case <-ctx.Done():
			return finish(1, "fail", diagnosticErrorCode(ctx, "", "response_failed"))
		case <-overflow:
			return finish(1, "fail", "output_limit")
		case event := <-events:
			// A dropped event must not be mistaken for a completed response.
			select {
			case <-overflow:
				return finish(1, "fail", "output_limit")
			default:
			}
			if event.Kind == model.RuntimeApprovalRequested || event.Kind == model.RuntimeToolStarted {
				return finish(1, "fail", "interaction_required")
			}
			if event.Kind == model.RuntimeError {
				return finish(1, "fail", diagnosticErrorCode(ctx, event.Text, "response_failed"))
			}
			if event.CorrelationID != id {
				continue
			}
			switch event.Kind {
			case model.RuntimeFinal, model.RuntimeTextDelta:
				if text.Len()+len(event.Text) > 64<<10 {
					return finish(1, "fail", "output_limit")
				}
				if event.Kind == model.RuntimeFinal {
					matched = strings.TrimSpace(event.Text) == marker
				} else {
					text.WriteString(event.Text)
				}
			case model.RuntimeInputCompleted:
				if matched || strings.TrimSpace(text.String()) == marker {
					return finish(1, "pass", "responded")
				}
				return finish(1, "fail", "unexpected_response")
			case model.RuntimeInputFailed, model.RuntimeInputCancelled:
				return finish(1, "fail", diagnosticErrorCode(ctx, event.Text, "response_failed"))
			}
		}
	}
}

// Classify locally and discard raw errors; redaction by guessing secret values
// is not sufficient for a shareable report. Categories are hints, not diagnoses
// of a provider's internal state.
func diagnosticErrorCode(ctx context.Context, text, fallback string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	if ctx.Err() != nil {
		return "cancelled"
	}
	text = strings.ToLower(text)
	for _, hint := range []struct {
		terms []string
		code  string
	}{
		{[]string{"401", "403", "unauthorized", "authentication", "not logged in", "api key"}, "authentication_failed"},
		{[]string{"429", "rate limit", "quota", "credit", "billing"}, "quota_or_rate_limit"},
		{[]string{"model not found", "unknown model", "invalid model"}, "model_unavailable"},
		{[]string{"connection refused", "no such host", "network", "tls", "connection reset"}, "network_failed"},
	} {
		for _, term := range hint.terms {
			if strings.Contains(text, term) {
				return hint.code
			}
		}
	}
	return fallback
}
