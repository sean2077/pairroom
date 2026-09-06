package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

func TestIndependentPermissionProfilesAndNativeYOLO(t *testing.T) {
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
		for _, actor := range model.SlotActors() {
			t.Run(string(kind)+"/"+string(actor), func(t *testing.T) {
				original := Config{Actor: actor, Runtime: kind, Repo: "/repo", Model: "chosen-model", Provider: "chosen-provider", Effort: "high", AdditionalInstructions: "user instructions", SessionID: "exact-session"}
				inherited := PermissionConfig(original, model.PermissionConfigured)
				if inherited.PermissionMode != "" || inherited.ApprovalPolicy != "" || inherited.Sandbox != "" {
					t.Fatal("empty native overrides stopped inheriting")
				}
				yolo := PermissionConfig(original, model.PermissionYOLO)
				if yolo.Model != original.Model || yolo.Provider != original.Provider || yolo.SessionID != original.SessionID || yolo.AdditionalInstructions != original.AdditionalInstructions {
					t.Fatal("permissions changed unrelated selection")
				}
				restricted := PermissionConfig(original, model.PermissionReadOnly)
				switch kind {
				case model.RuntimeClaude:
					c := NewClaude(yolo, func(model.RuntimeEvent) {})
					if err := c.SetRole(context.Background(), model.RolePeer); err != nil || c.cfg.PermissionMode != "yolo" {
						t.Fatalf("Claude yolo=%+v err=%v", c.cfg, err)
					}
					if restricted.PermissionMode != "plan" {
						t.Fatal("Claude read-only does not use native plan")
					}
				case model.RuntimeCodex:
					c := NewCodex(yolo, func(model.RuntimeEvent) {})
					params := c.turnStartParams("thread", "body", model.AgentInput{Role: model.RolePeer, MessageID: "room-correlation"})
					if params["approvalPolicy"] != "never" || params["sandboxPolicy"].(map[string]any)["type"] != "dangerFullAccess" {
						t.Fatalf("Codex native YOLO=%+v", params)
					}
					if params["clientUserMessageId"] != "room-correlation" {
						t.Fatal("compact prompt removed transport correlation")
					}
					if restricted.ApprovalPolicy != "on-request" || restricted.Sandbox != "read-only" {
						t.Fatalf("Codex restriction=%+v", restricted)
					}
				case model.RuntimeGrok:
					g := NewGrok(yolo, func(model.RuntimeEvent) {})
					args := strings.Join(g.buildACPArgs(), " ")
					if !strings.Contains(args, "--always-approve") || !strings.Contains(args, "--sandbox off") {
						t.Fatalf("Grok native YOLO=%s", args)
					}
					if strings.Contains(args, original.AdditionalInstructions) {
						t.Fatal("instructions entered argv")
					}
					if restricted.PermissionMode != "plan" || restricted.Sandbox != "read-only" {
						t.Fatal("Grok read-only missing native boundaries")
					}
				}
			})
		}
	}
	// Explicit YOLO completes the sandbox override, but an explicit narrower
	// sandbox from creation still wins when restoring that configuration.
	cfg := PermissionConfig(Config{Actor: model.ActorCodex, Runtime: model.RuntimeCodex, ApprovalPolicy: "yolo"}, model.PermissionConfigured)
	if cfg.Sandbox != "danger-full-access" {
		t.Fatal("default Codex YOLO left a restrictive inherited sandbox")
	}
	cfg.Sandbox = "workspace-write"
	if PermissionConfig(cfg, model.PermissionConfigured).Sandbox != "workspace-write" {
		t.Fatal("overrode an explicit sandbox")
	}
}

func TestCollaborationInstructionsAreStableAndOutsideEnvelopes(t *testing.T) {
	d, _ := (model.Collaboration{}).ForCreation()
	for _, self := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
		for _, peer := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
			for _, actor := range model.SlotActors() {
				cfg := Config{Actor: actor, Runtime: self, PeerRuntime: peer, Collaboration: &d}
				got := collaborationPrompt(cfg)
				if strings.Count(got, d.Instructions) != 1 || !strings.Contains(got, "Your responsibility: "+d.Responsibility(actor)) {
					t.Fatalf("instructions=%s", got)
				}
				if len(got) > prompt.MaxBootstrapBytes {
					t.Fatalf("default native instructions=%d > %d", len(got), prompt.MaxBootstrapBytes)
				}
				if strings.Contains(got, "current_role") {
					t.Fatal("dynamic role remains in stable instructions")
				}
				if strings.Contains(prompt.Envelope(model.AgentInput{Text: "inspect"}), d.Instructions) {
					t.Fatal("mode repeated per turn")
				}
			}
		}
	}
	c, _ := (model.Collaboration{Mode: model.CollaborationCustom, Instructions: "Agent 2 plans. Agent 1 implements. Ask the user before deployment."}).ForCreation()
	got := collaborationPrompt(Config{Actor: model.ActorCodex, Collaboration: &c, AdditionalInstructions: "Keep project conventions."})
	if strings.Count(got, c.Instructions) != 1 || strings.Contains(got, d.Instructions) || !strings.Contains(got, "Keep project conventions.") {
		t.Fatal("custom mode lost user text or acquired default responsibilities")
	}
}
