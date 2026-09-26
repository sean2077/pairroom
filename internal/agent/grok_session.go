package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

func (g *GrokAdapter) ensureSession(ctx context.Context) error {
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
			return fmt.Errorf("load exact Grok session %q: runtime did not advertise session/load", required)
		}
		result, err := g.call(ctx, "session/load", map[string]any{
			"sessionId": required, "cwd": repo, "mcpServers": []any{},
			"_meta": map[string]any{"noReplay": true, "startupHints": grokStartupHints()},
		})
		if err != nil {
			return fmt.Errorf("load exact Grok session %q: %w", required, err)
		}
		var response struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(result, &response); err == nil && strings.TrimSpace(response.SessionID) != "" && strings.TrimSpace(response.SessionID) != required {
			return fmt.Errorf("Grok session/load returned %q instead of required session %q", response.SessionID, required)
		}
	} else {
		result, err := g.call(ctx, "session/new", map[string]any{
			"cwd": repo, "mcpServers": []any{},
			"_meta": map[string]any{"rules": collaborationPrompt(g.cfg), "sessionKind": "headless", "startupHints": grokStartupHints()},
		})
		if err != nil {
			return fmt.Errorf("create Grok session: %w", err)
		}
		var response struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(result, &response); err != nil || strings.TrimSpace(response.SessionID) == "" {
			if err == nil {
				err = errors.New("missing sessionId")
			}
			return fmt.Errorf("decode Grok session: %w", err)
		}
		required = strings.TrimSpace(response.SessionID)
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
	g.bootstrapPending = loaded
	info := g.runtimeInfo
	g.mu.Unlock()
	if info.SessionName != "" {
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
