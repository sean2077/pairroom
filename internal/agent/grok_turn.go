package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

type grokTurn struct {
	turnID string
	inputs []model.AgentInput
	final  strings.Builder
}

func (g *GrokAdapter) StartTurn(ctx context.Context, input model.AgentInput) error {
	g.submitMu.Lock()
	defer g.submitMu.Unlock()
	g.mu.Lock()
	state := g.state
	running := g.cmd != nil && g.stdin != nil
	g.mu.Unlock()
	if !running || state == model.StateStopped || state == model.StateError {
		if err := g.Start(ctx); err != nil {
			return err
		}
	}
	if err := g.ensureSession(ctx); err != nil {
		return err
	}

	g.mu.Lock()
	if g.turn != nil {
		g.mu.Unlock()
		return errors.New("Grok Build already has an active prompt")
	}
	sessionID := g.sessionID
	bootstrap := g.bootstrapPending
	g.mu.Unlock()

	text := prompt.Envelope(input)
	if bootstrap {
		text = collaborationPrompt(g.cfg) + "\n\n" + text
	}
	g.mu.Lock()
	promptImage := g.capabilities.promptImage
	g.mu.Unlock()
	content, err := grokContent(text, input.Attachments, promptImage)
	if err != nil {
		return err
	}
	requestID := g.nextRequestID.Add(1)
	reply := make(chan grokRPCReply, 1)
	turn := &grokTurn{
		turnID: model.NewID("grok-turn"), inputs: []model.AgentInput{input},
	}
	g.mu.Lock()
	g.pending[requestID] = reply
	g.turn = turn
	g.mu.Unlock()
	if err := g.send(map[string]any{
		"jsonrpc": "2.0", "id": requestID, "method": "session/prompt",
		"params": map[string]any{"sessionId": sessionID, "prompt": content, "_meta": map[string]any{"screenMode": "headless"}},
	}); err != nil {
		g.mu.Lock()
		delete(g.pending, requestID)
		if g.turn == turn {
			g.turn = nil
		}
		g.mu.Unlock()
		return err
	}
	// The prompt request has crossed the native ACP boundary. If this is a
	// newly allocated binding, retain its session ID across a later process
	// restart; before this point the ID was only an uncommitted session/new
	// allocation and may safely be discarded.
	g.mu.Lock()
	g.sessionEngaged = true
	g.mu.Unlock()
	if bootstrap {
		g.mu.Lock()
		g.bootstrapPending = false
		g.mu.Unlock()
	}

	g.setState(model.StateWorking, "")
	started := runtimeEvent(g.cfg.Actor, model.RuntimeTurnStarted)
	started.TurnID = turn.turnID
	started.CorrelationID = input.MessageID
	g.sink(started)
	processing := runtimeEvent(g.cfg.Actor, model.RuntimeInputProcessing)
	processing.TurnID = turn.turnID
	processing.CorrelationID = input.MessageID
	processing.Name = string(model.ProcessingWorking)
	processing.Text = "accepted by Grok ACP"
	g.sink(processing)
	go g.awaitPrompt(turn, reply)
	return nil
}

func grokContent(text string, attachments []model.AgentAttachment, allowImages bool) ([]map[string]any, error) {
	content := []map[string]any{{"type": "text", "text": text}}
	for _, attachment := range attachments {
		if attachment.Kind != "image" || !strings.HasPrefix(strings.ToLower(attachment.MediaType), "image/") {
			return nil, fmt.Errorf("attachment %q is not a Grok image", attachment.Name)
		}
		data, err := os.ReadFile(attachment.Path)
		if err != nil {
			return nil, fmt.Errorf("read Grok image %q: %w", attachment.Name, err)
		}
		if len(data) == 0 || (attachment.Size > 0 && int64(len(data)) != attachment.Size) {
			return nil, fmt.Errorf("Grok image %q changed after attachment validation", attachment.Name)
		}
		if !allowImages {
			continue
		}
		content = append(content, map[string]any{
			"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": attachment.MediaType,
		})
	}
	if !allowImages && len(attachments) > 0 {
		// The envelope retains the verified local paths. Do not send unsupported
		// binary blocks or reject the complete message because of optional media.
		content = append(content, map[string]any{"type": "text", "text": "[PairRoom attachment notice]\nGrok Build does not support image input over ACP. The images were not sent as visual content; their names and local paths are listed in the message. Use an available native image-reading tool if supported and permitted. If you cannot inspect them, state that limitation and ask @user for the needed details; do not infer image contents from filenames."})
	}
	return content, nil
}

func (g *GrokAdapter) Steer(ctx context.Context, input model.AgentInput) SteerOutcome {
	g.submitMu.Lock()
	defer g.submitMu.Unlock()
	g.mu.Lock()
	turn := g.turn
	sessionID := g.sessionID
	g.mu.Unlock()
	if turn == nil || sessionID == "" {
		return SteerOutcome{State: SteerUnavailable, Detail: "Grok Build has no active prompt"}
	}
	text := prompt.Envelope(input)
	g.mu.Lock()
	promptImage := g.capabilities.promptImage
	g.mu.Unlock()
	content, err := grokContent(text, input.Attachments, promptImage)
	if err != nil {
		return SteerOutcome{State: SteerRejected, Detail: err.Error()}
	}
	interjectParams := map[string]any{
		"sessionId": sessionID, "text": text, "interjectionId": input.MessageID, "content": content,
	}
	interjectMethod, acknowledgement, err := g.callGrokInterject(ctx, interjectParams)
	if err != nil {
		var rpcErr grokRPCError
		if errors.As(err, &rpcErr) {
			if rpcErr.Code == -32601 {
				return SteerOutcome{State: SteerUnavailable, Detail: "Grok Build does not expose an interject extension: " + err.Error()}
			}
			return SteerOutcome{State: SteerRejected, Detail: err.Error()}
		}
		return SteerOutcome{State: SteerUnknown, Detail: err.Error()}
	}
	ackState, ackDetail := classifyGrokInterjectAcknowledgement(acknowledgement)
	if ackState != SteerAccepted {
		return SteerOutcome{State: ackState, Detail: ackDetail}
	}
	g.mu.Lock()
	if g.turn != turn {
		g.mu.Unlock()
		// A successful ACP extension response is the native acceptance receipt;
		// the prompt may legitimately finish before this client observes the
		// acknowledgement. Do not downgrade that receipt to an automatic FIFO
		// retry, which could execute the same interjection twice.
		return SteerOutcome{State: SteerAccepted, Detail: "accepted by Grok " + interjectMethod + " before the prompt completed"}
	}
	turn.inputs = append(turn.inputs, input)
	g.mu.Unlock()
	processing := runtimeEvent(g.cfg.Actor, model.RuntimeInputProcessing)
	processing.TurnID = turn.turnID
	processing.CorrelationID = input.MessageID
	processing.Name = string(model.ProcessingWorking)
	processing.Text = "injected through Grok " + interjectMethod
	g.sink(processing)
	return SteerOutcome{State: SteerAccepted, Detail: "accepted by Grok " + interjectMethod}
}

// callGrokInterject handles the extension spelling transition in Grok Build.
// The private `_x.ai/interject` spelling is retained as the first attempt for
// the PairRoom v5 wire contract. A method-not-found response is the only case
// that permits trying the current public `x.ai/interject` spelling: all other
// failures are returned unchanged so an uncertain native write is never
// silently duplicated.
func (g *GrokAdapter) callGrokInterject(ctx context.Context, params map[string]any) (string, json.RawMessage, error) {
	methods := []string{"_x.ai/interject", "x.ai/interject"}
	var last error
	for index, method := range methods {
		result, err := g.call(ctx, method, params)
		if err == nil {
			return method, result, nil
		}
		last = err
		var rpcErr grokRPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 || index == len(methods)-1 {
			return method, nil, err
		}
	}
	return methods[len(methods)-1], nil, last
}

func classifyGrokInterjectAcknowledgement(raw json.RawMessage) (SteerState, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return SteerUnknown, "Grok interject returned no acknowledgement; explicit retry required"
	}
	var response struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return SteerUnknown, "decode Grok interject acknowledgement: " + err.Error()
	}
	status := strings.ToLower(strings.TrimSpace(response.Status))
	detail := firstNonEmpty(response.Reason, response.Error, status)
	switch status {
	case "queued", "accepted", "ok", "success", "pending":
		return SteerAccepted, "Grok interject acknowledged as " + status
	case "unsupported", "unavailable", "not_supported", "not-supported":
		return SteerUnavailable, "Grok interject is unavailable: " + detail
	case "rejected", "denied", "declined", "cancelled", "canceled", "failed", "error":
		return SteerRejected, "Grok interject was rejected: " + detail
	default:
		return SteerUnknown, "Grok interject returned unknown status " + detail + "; explicit retry required"
	}
}

func (g *GrokAdapter) awaitPrompt(turn *grokTurn, reply <-chan grokRPCReply) {
	result := <-reply

	// Serialize turn finalization with StartTurn/Steer. ACP may deliver the
	// session/prompt response and a queued interject acknowledgement back to
	// back; holding submitMu lets a steer that crossed the wire first append its
	// input before we snapshot the completed turn's correlation list.
	g.submitMu.Lock()
	g.mu.Lock()
	if g.turn != turn {
		g.mu.Unlock()
		g.submitMu.Unlock()
		return
	}
	g.turn = nil
	inputs := append([]model.AgentInput(nil), turn.inputs...)
	text := turn.final.String()
	sessionID := g.sessionID
	g.mu.Unlock()
	g.submitMu.Unlock()

	correlationID := ""
	if len(inputs) > 0 {
		correlationID = inputs[len(inputs)-1].MessageID
	}
	terminalKind := model.RuntimeInputCompleted
	terminalState := model.ProcessingCompleted
	var status string
	detail := "completed by Grok ACP"
	if result.err != nil {
		terminalKind = model.RuntimeInputFailed
		terminalState = model.ProcessingFailed
		status = "failed"
		detail = result.err.Error()
		errorEvent := runtimeEvent(g.cfg.Actor, model.RuntimeError)
		errorEvent.TurnID = turn.turnID
		errorEvent.CorrelationID = correlationID
		errorEvent.Text = detail
		g.sink(errorEvent)
		g.setState(model.StateError, detail)
	} else {
		var response struct {
			StopReason string `json:"stopReason"`
		}
		_ = json.Unmarshal(result.result, &response)
		status = strings.TrimSpace(response.StopReason)
		if status == "" {
			status = "completed"
		}
		if status == "cancelled" || status == "canceled" || status == "interrupted" {
			terminalKind = model.RuntimeInputCancelled
			terminalState = model.ProcessingCancelled
			detail = "Grok prompt was " + status
		}
		g.setState(model.StateIdle, "")
	}
	if result.err == nil && terminalKind == model.RuntimeInputCompleted && strings.TrimSpace(text) != "" {
		final := runtimeEvent(g.cfg.Actor, model.RuntimeFinal)
		final.TurnID = turn.turnID
		final.CorrelationID = correlationID
		final.Text = text
		g.sink(final)
	}
	for _, input := range inputs {
		event := runtimeEvent(g.cfg.Actor, terminalKind)
		event.TurnID = turn.turnID
		event.CorrelationID = input.MessageID
		event.Name = string(terminalState)
		event.Text = detail
		g.sink(event)
	}
	completed := runtimeEvent(g.cfg.Actor, model.RuntimeTurnCompleted)
	completed.TurnID = turn.turnID
	completed.CorrelationID = correlationID
	completed.SessionID = sessionID
	completed.Name = status
	g.sink(completed)
}

func (g *GrokAdapter) attachCurrentTurn(event *model.RuntimeEvent) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.turn == nil {
		return
	}
	event.TurnID = g.turn.turnID
	event.CorrelationID = g.turn.inputs[len(g.turn.inputs)-1].MessageID
}

func lastGrokInputID(inputs []model.AgentInput) string {
	if len(inputs) == 0 {
		return ""
	}
	return inputs[len(inputs)-1].MessageID
}
