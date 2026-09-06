package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func configuredSessionName(cfg Config) string {
	// Provisioning validators deliberately have no Room ID. They must not rename
	// an existing native session before its Room binding has been committed.
	return model.NativeSessionName(cfg.RoomID, cfg.RoomName, cfg.Actor, cfg.Runtime, cfg.PeerRuntime)
}

// syncSessionName never starts a model Turn, writes vendor storage directly, or
// makes name synchronization a prerequisite for delivering a real user input.
// A bounded metadata failure remains visible without leaking raw vendor output.
func syncSessionName(parent context.Context, info model.RuntimeInfo, rename func(context.Context, string) error) model.RuntimeInfo {
	if info.SessionName == "" {
		return info
	}
	if parent.Err() != nil {
		info.SessionNameStatus = "failed"
		return info
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	err := rename(ctx, info.SessionName)
	info.SessionNameStatus = "synced"
	if err != nil {
		info.SessionNameStatus = "failed"
		var codexErr codexRPCError
		var grokErr grokRPCError
		if (errors.As(err, &codexErr) && codexErr.Code == -32601) || (errors.As(err, &grokErr) && grokErr.Code == -32601) {
			info.SessionNameStatus = "unsupported"
		}
	}
	return info
}

func (c *CodexAdapter) syncSessionName(ctx context.Context, info model.RuntimeInfo, threadID string) model.RuntimeInfo {
	return syncSessionName(ctx, info, func(ctx context.Context, name string) error {
		_, err := c.call(ctx, "thread/name/set", map[string]any{"threadId": threadID, "name": name})
		return err
	})
}

func (g *GrokAdapter) syncSessionName(ctx context.Context, info model.RuntimeInfo, sessionID string) model.RuntimeInfo {
	return syncSessionName(ctx, info, func(ctx context.Context, name string) error {
		params := map[string]any{"sessionId": sessionID, "title": name, "cwd": g.cfg.Repo}
		result, err := g.call(ctx, "x.ai/session/rename", params)
		var rpcErr grokRPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == -32601 {
			result, err = g.call(ctx, "_x.ai/session/rename", params)
		}
		if err != nil {
			return err
		}
		var receipt struct {
			Success bool `json:"success"`
		}
		if json.Unmarshal(result, &receipt) != nil || !receipt.Success {
			return errors.New("Grok did not acknowledge the session name")
		}
		return nil
	})
}
