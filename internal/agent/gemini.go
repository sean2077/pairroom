package agent

import (
	"errors"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

var geminiSetupTimeout = 20 * time.Second

const geminiResumeWarning = "cannot safely resume Gemini CLI ACP: no supported replay-completion boundary; the existing session is preserved. Continue in Gemini's native CLI"

var errGeminiExactResume = errors.New(geminiResumeWarning)

// NewGemini uses Gemini CLI's official ACP transport. Authentication remains
// native: session/new reuses Gemini settings and credentials. Exact resume is
// blocked until Gemini offers a reliable boundary for session/load replay.
// In particular, do not call authenticate: that RPC rewrites user settings.
func NewGemini(cfg Config, sink EventSink) *ACPAdapter {
	cfg.Runtime = model.RuntimeGemini
	return newACP(cfg, sink)
}

func GeminiFactory(cfg Config, sink EventSink) Adapter {
	actor := factoryActor(cfg, model.ActorSlot1)
	cfg.Runtime = model.RuntimeGemini
	return newHumanInputAdapter(cfg, actor, sink, func(innerSink EventSink) Adapter { return NewGemini(cfg, innerSink) })
}

func (g *ACPAdapter) acpProtocol() string {
	return string(g.cfg.Runtime) + "-acp-v1"
}

func geminiACPArgs(cfg Config, probe ProbeResult) []string {
	args := append([]string(nil), cfg.CommandArgs...)
	flag := "--acp"
	if !probe.SupportedFlags[flag] {
		flag = "--experimental-acp"
	}
	args = append(args, flag)
	if value := strings.TrimSpace(cfg.Model); value != "" {
		args = append(args, "--model", value)
	}
	if value := strings.TrimSpace(cfg.PermissionMode); value != "" {
		args = append(args, "--approval-mode", value)
	}
	// Gemini's sandbox option is boolean, not Grok's named policy. An empty
	// override preserves native inheritance; off is an explicit per-slot choice.
	switch cfg.Sandbox {
	case "on":
		args = append(args, "--sandbox=true")
	case "off":
		args = append(args, "--sandbox=false")
	}
	return args
}

func geminiACPEnv(env []string, cfg Config) []string {
	// Gemini gives GEMINI_SANDBOX precedence over --sandbox and native settings.
	// Project explicit slot choices into the child only; an empty choice retains
	// the native environment, including a selected sandbox implementation.
	switch cfg.Sandbox {
	case "on":
		return mergeRuntimeEnv(env, map[string]string{"GEMINI_SANDBOX": "true"})
	case "off":
		return mergeRuntimeEnv(env, map[string]string{"GEMINI_SANDBOX": "false"})
	default:
		return env
	}
}
