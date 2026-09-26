package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

type claudePending struct {
	input  model.AgentInput
	turnID string
}

func (c *ClaudeAdapter) StartTurn(ctx context.Context, input model.AgentInput) error {
	// PairRoom owns queued input. Serialize the native write so the accepted
	// turn retains exact correlation without creating a second adapter queue.
	c.submitMu.Lock()
	defer c.submitMu.Unlock()

	if err := c.Start(ctx); err != nil {
		return err
	}

	c.mu.Lock()
	protocolSent := c.protocolSent
	busy := c.state == model.StateWorking || c.state == model.StateWaiting || len(c.pending) > 0
	c.mu.Unlock()
	if busy {
		return errors.New("Claude Code already has an active turn")
	}

	text := prompt.Envelope(input)
	if !protocolSent {
		text = collaborationPrompt(c.cfg) + "\n\n" + text
	}
	content, err := claudeInputContent(text, input.Attachments)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"type": "user",
		// Claude's stream-json SDK input accepts an optional UUID. Use a real
		// RFC 4122 value rather than the room's human-readable message ID so the
		// native transcript remains valid across resume/replay operations.
		"uuid":       newUUID(),
		"session_id": c.SessionID(),
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
		"parent_tool_use_id": nil,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode claude input: %w", err)
	}

	entry := claudePending{input: input, turnID: model.NewID("claude-turn")}
	c.mu.Lock()
	c.pending = append(c.pending, entry)
	c.mu.Unlock()

	c.writeMu.Lock()
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		c.writeMu.Unlock()
		c.removePending(input.MessageID)
		return errors.New("claude stdin is not available")
	}
	_, err = stdin.Write(append(data, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		c.removePending(input.MessageID)
		c.setState(model.StateError, err.Error())
		return fmt.Errorf("send claude input: %w", err)
	}
	if !protocolSent {
		c.mu.Lock()
		c.protocolSent = true
		c.mu.Unlock()
	}

	c.emitTurnStarted(entry)
	c.emitInputState(entry, model.RuntimeInputProcessing, model.ProcessingWorking, "accepted by Claude Code")
	c.setState(model.StateWorking, "")
	return nil
}

func (c *ClaudeAdapter) Steer(context.Context, model.AgentInput) SteerOutcome {
	return SteerOutcome{State: SteerUnavailable, Detail: "Claude Code streaming input does not expose a verified same-turn steer contract"}
}

const (
	maxClaudeImageBytes      int64 = 5 << 20
	maxClaudeImagesPerInput        = 8
	maxClaudeTotalImageBytes int64 = 20 << 20
)

var claudeImageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// claudeInputContent produces the native Claude streaming-input shape. Text-only
// messages remain strings for the smallest wire representation; multimodal
// messages use standard text and base64 image content blocks. PairRoom reads
// only attachment-store files that were resolved by the room boundary.
func claudeInputContent(text string, attachments []model.AgentAttachment) (any, error) {
	if len(attachments) == 0 {
		return text, nil
	}
	if len(attachments) > maxClaudeImagesPerInput {
		return nil, fmt.Errorf("Claude input includes %d images; limit is %d", len(attachments), maxClaudeImagesPerInput)
	}
	// Anthropic recommends placing images before the text query when possible.
	blocks := make([]any, 0, len(attachments)+1)
	var total int64
	for _, value := range attachments {
		if value.Kind != "image" || !claudeImageMediaTypes[value.MediaType] {
			return nil, fmt.Errorf("attachment %q has unsupported Claude image type %q", value.Name, value.MediaType)
		}
		if strings.TrimSpace(value.Path) == "" {
			return nil, fmt.Errorf("attachment %q is missing its native image path", value.Name)
		}
		file, err := os.Open(value.Path)
		if err != nil {
			return nil, fmt.Errorf("open Claude image %q: %w", value.Name, err)
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("inspect Claude image %q: %w", value.Name, statErr)
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxClaudeImageBytes {
			_ = file.Close()
			return nil, fmt.Errorf("Claude image %q is not a regular image within the %d MiB limit", value.Name, maxClaudeImageBytes>>20)
		}
		if value.Size > 0 && info.Size() != value.Size {
			_ = file.Close()
			return nil, fmt.Errorf("Claude image %q changed after attachment validation", value.Name)
		}
		total += info.Size()
		if total > maxClaudeTotalImageBytes {
			_ = file.Close()
			return nil, fmt.Errorf("Claude images exceed the %d MiB total input limit", maxClaudeTotalImageBytes>>20)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, maxClaudeImageBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read Claude image %q: %w", value.Name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close Claude image %q: %w", value.Name, closeErr)
		}
		if int64(len(data)) != info.Size() {
			return nil, fmt.Errorf("Claude image %q changed while it was being read", value.Name)
		}
		blocks = append(blocks, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": value.MediaType,
				"data":       base64.StdEncoding.EncodeToString(data),
			},
		})
	}
	blocks = append(blocks, map[string]any{"type": "text", "text": text})
	return blocks, nil
}

func (c *ClaudeAdapter) emitTurnStarted(item claudePending) {
	e := runtimeEvent(c.cfg.Actor, model.RuntimeTurnStarted)
	e.TurnID = item.turnID
	e.CorrelationID = item.input.MessageID
	c.sink(e)
}

func (c *ClaudeAdapter) emitInputState(item claudePending, kind string, state model.ProcessingState, detail string) {
	e := runtimeEvent(c.cfg.Actor, kind)
	e.TurnID = item.turnID
	e.CorrelationID = item.input.MessageID
	e.Name = string(state)
	e.Text = detail
	c.sink(e)
}

func (c *ClaudeAdapter) removePending(messageID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, item := range c.pending {
		if item.input.MessageID == messageID {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return
		}
	}
}

func (c *ClaudeAdapter) currentPending() (claudePending, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return claudePending{}, false
	}
	return c.pending[0], true
}

func (c *ClaudeAdapter) popPending() (claudePending, bool, *claudePending) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return claudePending{}, false, nil
	}
	item := c.pending[0]
	c.pending = c.pending[1:]
	if len(c.pending) == 0 {
		return item, true, nil
	}
	next := c.pending[0]
	return item, true, &next
}

func (c *ClaudeAdapter) takePending() []claudePending {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := append([]claudePending(nil), c.pending...)
	c.pending = nil
	c.output.Reset()
	c.fallback = ""
	return items
}

func (c *ClaudeAdapter) cancelPending(kind, detail string) {
	for _, item := range c.takePending() {
		c.emitInputState(item, model.RuntimeInputCancelled, model.ProcessingCancelled, detail)
		completed := runtimeEvent(c.cfg.Actor, model.RuntimeTurnCompleted)
		completed.TurnID = item.turnID
		completed.CorrelationID = item.input.MessageID
		completed.Name = kind
		c.sink(completed)
	}
}
