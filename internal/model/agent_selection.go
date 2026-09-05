package model

import (
	"errors"
	"fmt"
	"strings"
)

// ProviderSource identifies where a Room resolves a provider configuration.
// Native delegates completely to the selected runtime's own user/global
// configuration. CCSwitch stores only a stable database reference; credentials
// are resolved into a child-process environment at activation time.
type ProviderSource string

const (
	ProviderNative   ProviderSource = "native"
	ProviderCCSwitch ProviderSource = "cc-switch"
)

func (s ProviderSource) Valid() bool {
	return s == ProviderNative || s == ProviderCCSwitch
}

// ProviderRef is safe to persist and return from APIs. It never contains a
// credential, endpoint, header, or raw CC Switch configuration document.
type ProviderRef struct {
	Source    ProviderSource `json:"source"`
	AppType   string         `json:"app_type,omitempty"`
	ProfileID string         `json:"profile_id,omitempty"`
}

func NativeProviderRef() ProviderRef { return ProviderRef{Source: ProviderNative} }

func (r ProviderRef) ValidateForRuntime(runtime RuntimeKind) error {
	if !r.Source.Valid() {
		return fmt.Errorf("invalid provider source %q", r.Source)
	}
	switch r.Source {
	case ProviderNative:
		if strings.TrimSpace(r.AppType) != "" || strings.TrimSpace(r.ProfileID) != "" {
			return errors.New("native provider reference must not include app_type or profile_id")
		}
	case ProviderCCSwitch:
		if strings.TrimSpace(r.AppType) == "" || strings.TrimSpace(r.ProfileID) == "" {
			return errors.New("cc-switch provider reference requires app_type and profile_id")
		}
		if normalizeCCSwitchAppType(r.AppType) != runtime.Canonical().ProviderAgentType() {
			return fmt.Errorf("cc-switch app_type %q does not match runtime %q", r.AppType, runtime.Canonical())
		}
	}
	return nil
}

func normalizeCCSwitchAppType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude", "claudecode", "claude-code", "claude_code":
		return "claudecode"
	case "codex":
		return "codex"
	case "grok", "grokbuild", "grok-build", "grok_build":
		return "grok"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

type OrdinaryReviewerPolicy string

const (
	// ReviewerEnforced keeps the ordinary Reviewer in PairRoom's read-only
	// native projection, irrespective of the selected runtime policy.
	ReviewerEnforced OrdinaryReviewerPolicy = "enforced"
	// ReviewerExplicit applies the explicitly selected native permission,
	// approval, and sandbox policy to ordinary Reviewer turns. The Room still
	// owns the Reviewer workspace boundary.
	ReviewerExplicit OrdinaryReviewerPolicy = "explicit"
)

func (p OrdinaryReviewerPolicy) Valid() bool {
	return p == ReviewerEnforced || p == ReviewerExplicit
}

// AgentSelection is the immutable, secret-free per-slot Room configuration.
// Historical ActorIDs remain slot identities; Runtime selects the vendor CLI.
type AgentSelection struct {
	Runtime                RuntimeKind            `json:"runtime"`
	Provider               ProviderRef            `json:"provider"`
	Model                  string                 `json:"model,omitempty"`
	Effort                 string                 `json:"effort,omitempty"`
	Instructions           string                 `json:"instructions,omitempty"`
	PermissionMode         string                 `json:"permission_mode,omitempty"`
	ApprovalPolicy         string                 `json:"approval_policy,omitempty"`
	Sandbox                string                 `json:"sandbox,omitempty"`
	OrdinaryReviewerPolicy OrdinaryReviewerPolicy `json:"ordinary_reviewer_policy"`
}

func (s AgentSelection) Normalized(actor ActorID) AgentSelection {
	s.Runtime = s.Runtime.CanonicalForSlot(actor)
	if s.Provider.Source == "" {
		s.Provider = NativeProviderRef()
	}
	s.Provider.AppType = strings.TrimSpace(s.Provider.AppType)
	s.Provider.ProfileID = strings.TrimSpace(s.Provider.ProfileID)
	s.Model = strings.TrimSpace(s.Model)
	s.Effort = strings.TrimSpace(s.Effort)
	s.Instructions = strings.TrimSpace(s.Instructions)
	s.PermissionMode = strings.TrimSpace(s.PermissionMode)
	s.ApprovalPolicy = strings.TrimSpace(s.ApprovalPolicy)
	s.Sandbox = strings.TrimSpace(s.Sandbox)
	if s.OrdinaryReviewerPolicy == "" {
		s.OrdinaryReviewerPolicy = ReviewerEnforced
	}
	return s
}

func (s AgentSelection) Validate(actor ActorID) error {
	if !actor.ValidParticipant() {
		return fmt.Errorf("invalid Agent slot %q", actor)
	}
	s = s.Normalized(actor)
	if !s.Runtime.Valid() {
		return fmt.Errorf("invalid runtime %q: use claude, codex, or grok", s.Runtime)
	}
	if err := s.Provider.ValidateForRuntime(s.Runtime); err != nil {
		return err
	}
	if !s.OrdinaryReviewerPolicy.Valid() {
		return fmt.Errorf("invalid ordinary_reviewer_policy %q", s.OrdinaryReviewerPolicy)
	}
	switch s.Runtime.Canonical() {
	case RuntimeClaude:
		if s.ApprovalPolicy != "" || s.Sandbox != "" {
			return errors.New("Claude Code selections support permission_mode but not approval_policy or sandbox")
		}
		if s.PermissionMode != "" && !oneOf(s.PermissionMode, "default", "manual", "acceptEdits", "plan", "auto", "dontAsk", "bypassPermissions", "bypass", "yolo", "always-approve") {
			return fmt.Errorf("invalid Claude Code permission_mode %q", s.PermissionMode)
		}
	case RuntimeCodex:
		if s.PermissionMode != "" {
			return errors.New("Codex selections support approval_policy and sandbox but not permission_mode")
		}
		if s.ApprovalPolicy != "" && !oneOf(s.ApprovalPolicy, "untrusted", "unless-trusted", "unlessTrusted", "on-failure", "on-request", "never", "yolo") {
			return fmt.Errorf("invalid Codex approval_policy %q", s.ApprovalPolicy)
		}
		if s.Sandbox != "" && !oneOf(s.Sandbox, "read-only", "workspace-write", "danger-full-access") {
			return fmt.Errorf("invalid Codex sandbox %q", s.Sandbox)
		}
	case RuntimeGrok:
		if s.ApprovalPolicy != "" {
			return errors.New("Grok Build selections support permission_mode and sandbox but not approval_policy")
		}
		if s.PermissionMode != "" && !oneOf(s.PermissionMode, "default", "ask", "acceptEdits", "plan", "auto", "dontAsk", "bypassPermissions", "always-approve", "yolo") {
			return fmt.Errorf("invalid Grok Build permission_mode %q", s.PermissionMode)
		}
		if s.Sandbox != "" && !oneOf(s.Sandbox, "read-only", "workspace", "strict", "off") {
			return fmt.Errorf("invalid Grok Build sandbox %q", s.Sandbox)
		}
	}
	if s.OrdinaryReviewerPolicy == ReviewerExplicit {
		explicit := s.PermissionMode != "" || s.ApprovalPolicy != "" || s.Sandbox != ""
		if !explicit {
			return errors.New("ordinary_reviewer_policy explicit requires an explicit Runtime permission, approval, or sandbox value")
		}
	}
	for label, value := range map[string]string{
		"model": s.Model, "effort": s.Effort, "instructions": s.Instructions,
		"permission_mode": s.PermissionMode, "approval_policy": s.ApprovalPolicy, "sandbox": s.Sandbox,
	} {
		if strings.ContainsAny(value, "\x00") {
			return fmt.Errorf("%s contains a NUL byte", label)
		}
	}
	if len(s.Model) > 512 || len(s.Effort) > 128 || len(s.PermissionMode) > 128 || len(s.ApprovalPolicy) > 128 || len(s.Sandbox) > 128 {
		return errors.New("Agent selection contains an oversized policy value")
	}
	if len(s.Instructions) > 64<<10 {
		return errors.New("Agent instructions exceed 65536 bytes")
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
