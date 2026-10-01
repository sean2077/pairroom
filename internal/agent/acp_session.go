package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

func (g *ACPAdapter) ensureSession(ctx context.Context) error {
	if g.cfg.Runtime == model.RuntimeGemini {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, geminiSetupTimeout)
		defer cancel()
	}
	g.mu.Lock()
	if g.sessionOpened {
		g.mu.Unlock()
		return nil
	}
	required := strings.TrimSpace(g.sessionID)
	repo := g.cfg.Repo
	access := g.access
	capabilities := g.capabilities
	g.mu.Unlock()

	loaded := required != "" && (strings.TrimSpace(g.cfg.SessionID) != "" || g.sessionEngaged)
	if !loaded {
		required = ""
	}
	if loaded {
		if !capabilities.loadSession {
			return fmt.Errorf("load exact ACP session %q: runtime did not advertise session/load", required)
		}
		params := map[string]any{"sessionId": required, "cwd": repo, "mcpServers": []any{}}
		if g.cfg.Runtime == model.RuntimeGrok {
			params["_meta"] = map[string]any{"noReplay": true, "startupHints": grokStartupHints()}
		}
		result, err := g.call(ctx, "session/load", params)
		if err != nil {
			return fmt.Errorf("load exact ACP session %q: %w", required, err)
		}
		var response struct {
			SessionID string `json:"sessionId"`
			Modes     struct {
				Current string `json:"currentModeId"`
			} `json:"modes"`
		}
		if err := json.Unmarshal(result, &response); err != nil {
			return fmt.Errorf("decode ACP session/load: %w", err)
		}
		if strings.TrimSpace(response.SessionID) != "" && strings.TrimSpace(response.SessionID) != required {
			return fmt.Errorf("ACP session/load returned %q instead of required session %q", response.SessionID, required)
		}
		g.mu.Lock()
		g.nativeMode = response.Modes.Current
		g.mu.Unlock()
	} else {
		params := map[string]any{"cwd": repo, "mcpServers": []any{}}
		if g.cfg.Runtime == model.RuntimeGrok {
			params["_meta"] = map[string]any{"rules": collaborationPrompt(g.cfg), "sessionKind": "headless", "startupHints": grokStartupHints()}
		}
		result, err := g.call(ctx, "session/new", params)
		if err != nil {
			return fmt.Errorf("create ACP session: %w", err)
		}
		var response struct {
			SessionID string `json:"sessionId"`
			Modes     struct {
				Current string `json:"currentModeId"`
			} `json:"modes"`
		}
		if err := json.Unmarshal(result, &response); err != nil || strings.TrimSpace(response.SessionID) == "" {
			if err == nil {
				err = errors.New("missing sessionId")
			}
			return fmt.Errorf("decode ACP session: %w", err)
		}
		required = strings.TrimSpace(response.SessionID)
		g.mu.Lock()
		g.nativeMode = response.Modes.Current
		g.mu.Unlock()
	}

	// Retain a newly allocated ID across a recoverable setup failure, but do not
	// publish or mark the session open until its role mode is accepted.
	g.mu.Lock()
	if g.sessionOpened {
		g.mu.Unlock()
		return nil
	}
	g.sessionID = required
	g.mu.Unlock()
	if err := g.applyAccessMode(ctx, required, access); err != nil {
		return err
	}
	g.mu.Lock()
	if g.sessionOpened {
		g.mu.Unlock()
		return nil
	}
	g.sessionOpened = true
	g.bootstrapPending = loaded || g.cfg.Runtime == model.RuntimeGemini
	info := g.runtimeInfo
	g.mu.Unlock()
	if info.SessionName != "" && g.cfg.Runtime == model.RuntimeGrok {
		info = g.syncSessionName(ctx, info, required)
		g.mu.Lock()
		g.runtimeInfo = info
		g.mu.Unlock()
		emitRuntimeInfo(g.sink, g.cfg.Actor, info)
	}

	session := runtimeEvent(g.cfg.Actor, model.RuntimeSession)
	session.SessionID = required
	g.sink(session)
	return nil
}

func grokStartupHints() map[string]any {
	return map[string]any{"nonInteractive": true, "skipGitStatus": true, "skipProjectLayout": true}
}
