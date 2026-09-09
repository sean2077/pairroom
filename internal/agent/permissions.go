package agent

import "github.com/sean2077/pairroom/internal/model"

// PermissionConfig projects a human-selected profile without changing model,
// provider, extra instructions, or the immutable collaboration responsibility.
func PermissionConfig(cfg Config, profile model.PermissionProfile) Config {
	kind := cfg.Runtime.CanonicalForSlot(cfg.Actor)
	switch profile {
	case model.PermissionReadOnly:
		switch kind {
		case model.RuntimeClaude:
			cfg.PermissionMode, cfg.ApprovalPolicy, cfg.Sandbox = "plan", "", ""
		case model.RuntimeCodex:
			cfg.PermissionMode, cfg.ApprovalPolicy, cfg.Sandbox = "", "on-request", "read-only"
		case model.RuntimeGrok:
			cfg.PermissionMode, cfg.ApprovalPolicy, cfg.Sandbox = "plan", "", "read-only"
		}
	case model.PermissionYOLO:
		switch kind {
		case model.RuntimeClaude:
			cfg.PermissionMode, cfg.ApprovalPolicy, cfg.Sandbox = "yolo", "", ""
		case model.RuntimeCodex:
			cfg.PermissionMode, cfg.ApprovalPolicy, cfg.Sandbox = "", "yolo", "danger-full-access"
		case model.RuntimeGrok:
			cfg.PermissionMode, cfg.ApprovalPolicy, cfg.Sandbox = "yolo", "", "off"
		}
	}
	// YOLO is an explicit Room override, not an inference from an empty native
	// setting. Preserve a separately selected sandbox when restoring configuration.
	if kind == model.RuntimeCodex && cfg.ApprovalPolicy == "yolo" && cfg.Sandbox == "" {
		cfg.Sandbox = "danger-full-access"
	}
	if kind == model.RuntimeGrok && cfg.PermissionMode == "yolo" && cfg.Sandbox == "" {
		cfg.Sandbox = "off"
	}
	return cfg
}
