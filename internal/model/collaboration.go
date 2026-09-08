package model

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Collaboration is selected once, stored with room.created, and projected at
// the native instruction layer. It is not a scheduler or a permission grant.
type Collaboration struct {
	Version      int    `json:"version"`
	Mode         string `json:"mode"`
	Instructions string `json:"instructions"`
}

const (
	CollaborationVersion              = 2
	CollaborationDefault              = "default"
	CollaborationCustom               = "custom"
	MaxCollaborationInstructionsBytes = 16 << 10
	DefaultCollaborationInstructions  = "Resolve the human's request accurately with the least necessary coordination. For simple, low-risk tasks, the addressed Agent executes, verifies, and answers directly, without delegation or peer review. Otherwise, Agent 1 (Lead) focuses on planning, decisions, and review; Agent 2 (Executor) implements, verifies, and contributes technical feedback. Involve the peer when complexity, uncertainty, or risk warrants it, not to complete a fixed sequence. These are defaults, not tool permissions. Follow newer human instructions."
	// Frozen for existing version-1 Rooms; never replace their stored policy.
	defaultCollaborationInstructionsV1 = "Agent 1 is Lead: own planning, technical decisions, and final review. Delegate implementation and routine verification to Agent 2; inspect the evidence and request concrete corrections when needed. Agent 2 is Executor: implement, test, and report evidence, risks, and unresolved issues; challenge or supplement the Lead's plan when warranted. Prefer completing useful work over repeated debate or acknowledgements. Scale planning and review to the task; do not create ceremonial turns. These responsibilities do not grant or restrict tools. Follow newer human instructions."
)

// ForCreation accepts only the two public modes. Default prose is versioned and
// persisted, not silently regenerated on activation after an application update.
// Omitted versions select the current default; explicit supported versions round-trip.
func (c Collaboration) ForCreation() (Collaboration, error) {
	if c.Version == 0 {
		c.Version = CollaborationVersion
	}
	if c.Version != 1 && c.Version != CollaborationVersion {
		return Collaboration{}, fmt.Errorf("unsupported collaboration version %d", c.Version)
	}
	c.Mode = strings.TrimSpace(c.Mode)
	if c.Mode == "" {
		c.Mode = CollaborationDefault
	}
	c.Instructions = strings.TrimSpace(c.Instructions)
	if c.Mode == CollaborationDefault {
		instructions := defaultCollaborationInstructions(c.Version)
		if c.Instructions != "" && c.Instructions != instructions {
			return Collaboration{}, errors.New("default mode has fixed instructions; choose custom for natural-language collaboration rules")
		}
		c.Instructions = instructions
	}
	return c, c.Validate()
}

func (c Collaboration) Validate() error {
	if c.Version != 1 && c.Version != CollaborationVersion {
		return fmt.Errorf("unsupported collaboration version %d", c.Version)
	}
	if c.Mode != CollaborationDefault && c.Mode != CollaborationCustom {
		return fmt.Errorf("invalid collaboration mode %q: use default or custom", c.Mode)
	}
	if strings.TrimSpace(c.Instructions) == "" {
		return errors.New("custom collaboration instructions must not be empty")
	}
	if !utf8.ValidString(c.Instructions) || strings.ContainsRune(c.Instructions, '\x00') {
		return errors.New("collaboration instructions must be UTF-8 without NUL bytes")
	}
	if len(c.Instructions) > MaxCollaborationInstructionsBytes {
		return fmt.Errorf("collaboration instructions exceed %d bytes", MaxCollaborationInstructionsBytes)
	}
	if c.Mode == CollaborationDefault && c.Instructions != defaultCollaborationInstructions(c.Version) {
		return errors.New("default collaboration instructions do not match their version")
	}
	return nil
}

// defaultCollaborationInstructions is called only after version validation.
func defaultCollaborationInstructions(version int) string {
	if version == 1 {
		return defaultCollaborationInstructionsV1
	}
	return DefaultCollaborationInstructions
}

func CloneCollaboration(c *Collaboration) *Collaboration {
	if c == nil {
		return nil
	}
	copy := *c
	return &copy
}

func (c Collaboration) Responsibility(actor ActorID) string {
	if c.Mode == CollaborationDefault {
		if actor == ActorClaude {
			return "lead"
		}
		return "executor"
	}
	return "participant"
}

// PermissionProfile is independent of collaboration responsibility. Configured
// restores the immutable Room selection; an empty selection inherits the CLI.
type PermissionProfile string

const (
	PermissionConfigured PermissionProfile = "configured"
	PermissionReadOnly   PermissionProfile = "read-only"
	PermissionYOLO       PermissionProfile = "yolo"
)

func (p PermissionProfile) Valid() bool {
	return p == PermissionConfigured || p == PermissionReadOnly || p == PermissionYOLO
}
