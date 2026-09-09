package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

// RedactingFactory wraps a native adapter at the process boundary. Provider
// credentials are intentionally present in Config.Env so the child process can
// authenticate, but a hostile/misconfigured Runtime may echo them in stderr,
// JSON telemetry, or an error. The wrapper removes those exact values before
// they reach the Room Event Log, RuntimeInfo, SSE, or an HTTP error response.
// It does not mutate the environment passed to the child.
func RedactingFactory(factory Factory) Factory {
	return func(cfg Config, sink EventSink) Adapter {
		redactor := newSecretRedactor(cfg.Env)
		wrappedSink := func(event model.RuntimeEvent) { sink(redactor.event(event)) }
		return &redactingAdapter{inner: factory(cfg, wrappedSink), redactor: redactor}
	}
}

type secretRedactor struct {
	values   []string
	replacer *strings.Replacer
}

func newSecretRedactor(env map[string]string) *secretRedactor {
	seen := make(map[string]struct{}, len(env))
	values := make([]string, 0, len(env))
	for key, value := range env {
		if !credentialEnvKey(key) {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 16<<10 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
	pairs := make([]string, 0, len(values)*2)
	for _, value := range values {
		pairs = append(pairs, value, "[redacted]")
	}
	return &secretRedactor{values: values, replacer: strings.NewReplacer(pairs...)}
}

func credentialEnvKey(value string) bool {
	name := strings.ToUpper(strings.TrimSpace(value))
	if name == "TOKEN" || name == "SECRET" || name == "PASSWORD" || name == "PASSWD" || name == "API_KEY" || name == "AUTH" ||
		name == "ANTHROPIC_AUTH_TOKEN" || name == "ANTHROPIC_API_KEY" || name == "OPENAI_API_KEY" {
		return true
	}
	return strings.HasSuffix(name, "_API_KEY") || strings.HasSuffix(name, "_AUTH_TOKEN") ||
		strings.HasSuffix(name, "_ACCESS_TOKEN") || strings.HasSuffix(name, "_SECRET") ||
		strings.HasSuffix(name, "_TOKEN") || strings.Contains(name, "_PASSWORD") ||
		strings.Contains(name, "_PASSWD") || (strings.HasPrefix(name, "PAIRROOM_CC_SWITCH_") && strings.Contains(name, "KEY"))
}

func (r *secretRedactor) text(value string) string {
	if r == nil || r.replacer == nil {
		return value
	}
	return r.replacer.Replace(value)
}

func (r *secretRedactor) raw(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	if r == nil || len(r.values) == 0 {
		return value
	}
	// Redact decoded strings, not serialized bytes: escaping quotes, backslashes
	// or Unicode must neither bypass the boundary nor corrupt the JSON payload.
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return json.RawMessage(`null`)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return json.RawMessage(`null`)
	}
	redacted, err := json.Marshal(r.jsonValue(decoded))
	if err != nil {
		return json.RawMessage(`null`)
	}
	return redacted
}

func (r *secretRedactor) jsonValue(value any) any {
	switch value := value.(type) {
	case string:
		return r.text(value)
	case []any:
		for index := range value {
			value[index] = r.jsonValue(value[index])
		}
		return value
	case map[string]any:
		clean := make(map[string]any, len(value))
		for key, item := range value {
			clean[r.text(key)] = r.jsonValue(item)
		}
		return clean
	default:
		return value
	}
}

func (r *secretRedactor) event(event model.RuntimeEvent) model.RuntimeEvent {
	event.Text = r.text(event.Text)
	event.Name = r.text(event.Name)
	event.SessionID = r.text(event.SessionID)
	event.Data = r.raw(event.Data)
	if event.Runtime != nil {
		info := *event.Runtime
		info.SessionName = r.text(info.SessionName)
		info.SessionNameStatus = r.text(info.SessionNameStatus)
		info.Version = r.text(info.Version)
		info.Protocol = r.text(info.Protocol)
		info.Command = r.text(info.Command)
		info.Path = r.text(info.Path)
		info.Provider = r.text(info.Provider)
		info.ProviderName = r.text(info.ProviderName)
		info.Model = r.text(info.Model)
		info.Effort = r.text(info.Effort)
		info.PermissionMode = r.text(info.PermissionMode)
		info.ApprovalPolicy = r.text(info.ApprovalPolicy)
		info.Sandbox = r.text(info.Sandbox)
		info.Capabilities = redactStrings(r, info.Capabilities)
		info.Warnings = redactStrings(r, info.Warnings)
		info.Data = r.raw(info.Data)
		event.Runtime = &info
	}
	if event.Approval != nil {
		approval := *event.Approval
		approval.Kind = r.text(approval.Kind)
		approval.Title = r.text(approval.Title)
		approval.Decision = r.text(approval.Decision)
		approval.Detail = r.raw(approval.Detail)
		event.Approval = &approval
	}
	return event
}

func redactStrings(r *secretRedactor, values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = r.text(value)
	}
	return result
}

func (r *secretRedactor) err(err error) error {
	if err == nil {
		return nil
	}
	// Preserve errors.Is/As semantics while ensuring callers that serialize
	// Error() never receive the original credential-bearing detail.
	return &redactedError{message: r.text(err.Error()), cause: err}
}

type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

type redactingAdapter struct {
	inner    Adapter
	redactor *secretRedactor
}

func (a *redactingAdapter) Actor() model.ActorID    { return a.inner.Actor() }
func (a *redactingAdapter) State() model.AgentState { return a.inner.State() }
func (a *redactingAdapter) SessionID() string       { return a.redactor.text(a.inner.SessionID()) }
func (a *redactingAdapter) Start(ctx context.Context) error {
	return a.redactor.err(a.inner.Start(ctx))
}
func (a *redactingAdapter) StartTurn(ctx context.Context, input model.AgentInput) error {
	return a.redactor.err(a.inner.StartTurn(ctx, input))
}
func (a *redactingAdapter) Steer(ctx context.Context, input model.AgentInput) SteerOutcome {
	outcome := a.inner.Steer(ctx, input)
	outcome.Detail = a.redactor.text(outcome.Detail)
	return outcome
}
func (a *redactingAdapter) Interrupt(ctx context.Context) error {
	return a.redactor.err(a.inner.Interrupt(ctx))
}
func (a *redactingAdapter) Stop(ctx context.Context) error { return a.redactor.err(a.inner.Stop(ctx)) }
func (a *redactingAdapter) ResolveApproval(ctx context.Context, id string, resolution model.ApprovalResolution) error {
	return a.redactor.err(a.inner.ResolveApproval(ctx, id, resolution))
}
func (a *redactingAdapter) SetRole(ctx context.Context, role model.ParticipantRole) error {
	return a.redactor.err(a.inner.SetRole(ctx, role))
}
func (a *redactingAdapter) SetWorkspace(ctx context.Context, workspace string) error {
	return a.redactor.err(a.inner.SetWorkspace(ctx, workspace))
}

// Keep the compiler honest if Adapter grows a method without this boundary
// being updated.
var _ Adapter = (*redactingAdapter)(nil)
