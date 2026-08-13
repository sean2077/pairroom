package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/sean2077/pairroom/internal/model"
)

type Agent struct {
	Command        string `json:"command"`
	Model          string `json:"model"`
	Effort         string `json:"effort,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	ApprovalPolicy string `json:"approval_policy,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
}

type File struct {
	Listen              string            `json:"listen"`
	RoomName            string            `json:"room_name,omitempty"`
	RoutingMode         model.RoutingMode `json:"routing_mode"`
	MaxAgentHops        int               `json:"max_agent_hops"`
	StallWarningSeconds int               `json:"stall_warning_seconds"`
	AutoStart           bool              `json:"auto_start"`
	Token               string            `json:"token,omitempty"`
	Claude              Agent             `json:"claude"`
	Codex               Agent             `json:"codex"`
}

func Defaults() File {
	return File{
		Listen:              "127.0.0.1:7332",
		RoomName:            "Claude × Codex",
		RoutingMode:         model.RoutingMentions,
		MaxAgentHops:        6,
		StallWarningSeconds: 300,
		AutoStart:           true,
		Claude:              Agent{Command: "claude", PermissionMode: "auto"},
		Codex:               Agent{Command: "codex", Effort: "high", ApprovalPolicy: "unlessTrusted", Sandbox: "workspaceWrite"},
	}
}

func Load(path string) (File, error) {
	cfg := Defaults()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return File{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return File{}, err
	}
	return cfg, nil
}

func (c File) Validate() error {
	if c.Listen == "" {
		return errors.New("listen address is required")
	}
	if !c.RoutingMode.Valid() {
		return fmt.Errorf("invalid routing mode %q", c.RoutingMode)
	}
	if c.MaxAgentHops < 1 || c.MaxAgentHops > 30 {
		return errors.New("max_agent_hops must be between 1 and 30")
	}
	if c.StallWarningSeconds != -1 && (c.StallWarningSeconds < 30 || c.StallWarningSeconds > 86400) {
		return errors.New("stall_warning_seconds must be -1 (disabled) or between 30 and 86400")
	}
	if c.Claude.Command == "" || c.Codex.Command == "" {
		return errors.New("both agent commands are required")
	}
	return nil
}
