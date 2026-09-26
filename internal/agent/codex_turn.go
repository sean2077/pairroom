package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

func (c *CodexAdapter) StartTurn(ctx context.Context, input model.AgentInput) error {
	// A Codex thread accepts one active turn. PairRoom reserves the Room owner;
	// this lock closes the smaller native start/steer observation race.
	c.submitMu.Lock()
	defer c.submitMu.Unlock()

	if err := c.Start(ctx); err != nil {
		return err
	}

	text := prompt.Envelope(input)
	c.mu.Lock()
	threadID := c.threadID
	turnID := c.currentTurn
	active := turnID != "" && (c.state == model.StateWorking || c.state == model.StateWaiting)
	c.mu.Unlock()

	if active {
		return errors.New("Codex already has an active turn")
	}

	params := c.turnStartParams(threadID, text, input)
	// turn/started can arrive before the turn/start response. Keep the input in
	// a temporary slot so either ordering receives the correct correlation ID.
	starting := input
	c.mu.Lock()
	c.startingInput = &starting
	c.startingTurnID = ""
	c.mu.Unlock()
	c.stageWireInput(input)
	if err := c.callTurnStart(ctx, input, params); err != nil {
		c.mu.Lock()
		c.startingInput = nil
		c.startingTurnID = ""
		c.pendingCompletions = make(map[string]json.RawMessage)
		c.mu.Unlock()
		c.unstageWireInput(input.MessageID)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// The app-server may have accepted turn/start after the deadline;
			// the orphaned native turn is not reachable through Interrupt once
			// local state was cleared, so the failure must tell the operator to
			// inspect before retrying.
			return fmt.Errorf("%w; the native turn/start may still have been accepted — inspect the Codex thread before retrying this input", err)
		}
		return err
	}
	return nil
}

// callTurnStart sends turn/start and applies a successful response on the
// stdout reader, in wire order, before the reader handles the turn's later
// notifications. Applying it on the waiting goroutine instead would race the
// turn's own turn/completed and could settle it without its correlation.
func (c *CodexAdapter) callTurnStart(ctx context.Context, input model.AgentInput, params map[string]any) error {
	for attempt := 0; ; attempt++ {
		id := c.nextRequestID.Add(1)
		ch := make(chan rpcReply, 1)
		c.mu.Lock()
		c.pending[id] = ch
		if c.replyHooks == nil {
			c.replyHooks = make(map[int64]codexReplyHook)
		}
		c.replyHooks[id] = func(reply rpcReply) rpcReply {
			if reply.err == nil {
				if err := c.acceptTurnStart(input, reply.result); err != nil {
					reply.err = err
				}
			}
			return reply
		}
		c.mu.Unlock()
		if err := c.send(map[string]any{"id": id, "method": "turn/start", "params": params}); err != nil {
			c.mu.Lock()
			delete(c.pending, id)
			delete(c.replyHooks, id)
			c.mu.Unlock()
			return err
		}
		var reply rpcReply
		select {
		case reply = <-ch:
		case <-ctx.Done():
			c.mu.Lock()
			_, waiting := c.pending[id]
			delete(c.pending, id)
			delete(c.replyHooks, id)
			c.mu.Unlock()
			if waiting {
				return ctx.Err()
			}
			// The reader already took the response and is applying it.
			reply = <-ch
		}
		var rpcErr codexRPCError
		if reply.err == nil || !errors.As(reply.err, &rpcErr) || rpcErr.Code != -32001 || attempt >= 4 {
			return reply.err
		}
		delay := time.Duration(100*(1<<attempt))*time.Millisecond + time.Duration(time.Now().UnixNano()%75)*time.Millisecond
		logEvent := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		logEvent.Name = "app-server.overloaded.retry"
		logEvent.Text = fmt.Sprintf("turn/start rejected as overloaded; retrying in %s (attempt %d/5)", delay, attempt+2)
		c.sink(logEvent)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// acceptTurnStart applies a successful turn/start response on the reader.
func (c *CodexAdapter) acceptTurnStart(input model.AgentInput, result json.RawMessage) error {
	c.unstageWireInput(input.MessageID)
	starting := input
	var turnResult struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &turnResult); err != nil || turnResult.Turn.ID == "" {
		c.mu.Lock()
		c.startingInput = nil
		c.startingTurnID = ""
		c.pendingCompletions = make(map[string]json.RawMessage)
		c.mu.Unlock()
		if err == nil {
			err = errors.New("missing turn id")
		}
		return fmt.Errorf("decode codex turn: %w", err)
	}
	c.mu.Lock()
	if _, completed := c.terminalTurns[turnResult.Turn.ID]; completed {
		c.startingInput = nil
		c.startingTurnID = ""
		c.mu.Unlock()
		// The native completion already emitted input terminal events and the
		// Room boundary. Do not recreate currentTurn or emit a second start.
		return nil
	}
	pendingCompletion := append(json.RawMessage(nil), c.pendingCompletions[turnResult.Turn.ID]...)
	// There is only one turn/start request in flight under submitMu. Any other
	// completion held while its opaque ID was unknown is therefore stale (or
	// belongs to a native turn PairRoom never owned); discard it at this exact
	// response boundary instead of retaining unbounded connection-local state.
	c.pendingCompletions = make(map[string]json.RawMessage)
	c.currentTurn = turnResult.Turn.ID
	c.threadEngaged = true
	_, startedNotificationSeen := c.startedTurns[turnResult.Turn.ID]
	if !startedNotificationSeen {
		c.startedTurns[turnResult.Turn.ID] = struct{}{}
	}
	if c.turnBuffers[turnResult.Turn.ID] == nil {
		c.turnBuffers[turnResult.Turn.ID] = &strings.Builder{}
	}
	c.mu.Unlock()
	if len(pendingCompletion) > 0 {
		// The terminal notification won the wire race. Let the normal completion
		// path settle the staged input and emit exactly one boundary. If no native
		// turn/started notification arrived, synthesize the lifecycle start before
		// the terminal event so observers never see a completion without a start.
		if !startedNotificationSeen {
			c.setState(model.StateWorking, "")
			started := runtimeEvent(c.cfg.Actor, model.RuntimeTurnStarted)
			started.TurnID = turnResult.Turn.ID
			started.CorrelationID = input.MessageID
			c.sink(started)
		}
		c.mu.Lock()
		// Preserve the staged input for handleTurnCompleted; call() already
		// removed its wire correlation before decoding the turn/start response.
		c.startingInput = &starting
		c.startingTurnID = turnResult.Turn.ID
		c.mu.Unlock()
		c.handleTurnCompleted(pendingCompletion)
		return nil
	}
	c.mu.Lock()
	c.startingInput = nil
	c.startingTurnID = ""
	c.mu.Unlock()
	if !startedNotificationSeen {
		started := runtimeEvent(c.cfg.Actor, model.RuntimeTurnStarted)
		started.TurnID = turnResult.Turn.ID
		started.CorrelationID = input.MessageID
		c.sink(started)
	}
	if c.bindTurnInput(turnResult.Turn.ID, input) {
		c.emitInputProcessing(turnResult.Turn.ID, input, "started Codex turn")
	}
	c.setState(model.StateWorking, "")
	return nil
}

func (c *CodexAdapter) Steer(ctx context.Context, input model.AgentInput) SteerOutcome {
	c.submitMu.Lock()
	defer c.submitMu.Unlock()

	c.mu.Lock()
	threadID := c.threadID
	turnID := c.currentTurn
	active := turnID != "" && (c.state == model.StateWorking || c.state == model.StateWaiting)
	c.mu.Unlock()
	if !active {
		return SteerOutcome{State: SteerUnavailable, Detail: "Codex has no active turn"}
	}

	text := prompt.Envelope(input)
	c.stageWireInput(input)
	defer c.unstageWireInput(input.MessageID)
	result, err := c.call(ctx, "turn/steer", codexTurnSteerParams(threadID, turnID, text, input))
	if err != nil {
		var rpcErr codexRPCError
		if errors.As(err, &rpcErr) {
			if rpcErr.Code == -32601 {
				return SteerOutcome{State: SteerUnavailable, Detail: err.Error()}
			}
			return SteerOutcome{State: SteerRejected, Detail: err.Error()}
		}
		return SteerOutcome{State: SteerUnknown, Detail: err.Error()}
	}
	var response struct {
		TurnID string `json:"turnId"`
	}
	if err := json.Unmarshal(result, &response); err != nil || response.TurnID != turnID {
		if err == nil {
			err = fmt.Errorf("returned turn %q instead of %q", response.TurnID, turnID)
		}
		return SteerOutcome{State: SteerUnknown, Detail: "decode Codex turn/steer acknowledgement: " + err.Error()}
	}
	c.mu.Lock()
	terminal, completed := c.terminalTurns[turnID]
	current := c.currentTurn
	c.mu.Unlock()
	if completed {
		if _, accepted := terminal.inputIDs[input.MessageID]; accepted {
			// The completion path included this staged input and already emitted
			// its terminal event. The RPC response arrived late; report accepted
			// without resurrecting the turn or duplicating lifecycle events.
			return SteerOutcome{State: SteerAccepted, Detail: "accepted by Codex turn/steer (turn completed before acknowledgement)"}
		}
		return SteerOutcome{State: SteerUnknown, Detail: "Codex turn completed before the steered input was correlated; explicit retry required"}
	}
	if current != turnID {
		return SteerOutcome{State: SteerUnknown, Detail: "Codex active turn ended before steer acknowledgement; explicit retry required"}
	}
	if c.bindTurnInput(turnID, input) {
		c.emitInputProcessing(turnID, input, "injected into active Codex turn")
	}
	return SteerOutcome{State: SteerAccepted, Detail: "accepted by Codex turn/steer"}
}

func codexTurnSteerParams(threadID, turnID, text string, input model.AgentInput) map[string]any {
	params := map[string]any{
		"threadId":       threadID,
		"expectedTurnId": turnID,
		"input":          codexInputItems(text, input.Attachments),
	}
	if input.MessageID != "" {
		params["clientUserMessageId"] = input.MessageID
	}
	return params
}

func (c *CodexAdapter) developerInstructions() string {
	return collaborationPrompt(c.cfg)
}

func (c *CodexAdapter) threadResumeParams(threadID string) map[string]any {
	return map[string]any{
		"threadId":              threadID,
		"cwd":                   c.cfg.Repo,
		"developerInstructions": c.developerInstructions(),
	}
}

func (c *CodexAdapter) threadStartParams() map[string]any {
	params := map[string]any{
		"cwd":                   c.cfg.Repo,
		"serviceName":           "pairroom",
		"developerInstructions": c.developerInstructions(),
	}
	if c.cfg.ApprovalPolicy != "" {
		params["approvalPolicy"] = c.cfg.ApprovalPolicy
	}
	if c.cfg.Sandbox != "" {
		params["sandbox"] = c.threadSandbox()
	}
	if c.cfg.Model != "" {
		params["model"] = c.cfg.Model
	}
	return params
}

func (c *CodexAdapter) turnStartParams(threadID, text string, input model.AgentInput) map[string]any {
	params := map[string]any{
		"threadId": threadID,
		"input":    codexInputItems(text, input.Attachments),
		"cwd":      c.cfg.Repo,
	}
	if c.cfg.ApprovalPolicy != "" {
		params["approvalPolicy"] = c.cfg.ApprovalPolicy
	}
	if input.Access == model.NativeAccessReadOnly {
		params["sandboxPolicy"] = map[string]any{"type": "readOnly"}
	} else if c.cfg.Sandbox != "" {
		params["sandboxPolicy"] = c.sandboxPolicy(input.Access)
	}
	if input.MessageID != "" {
		params["clientUserMessageId"] = input.MessageID
	}
	if c.cfg.Model != "" {
		params["model"] = c.cfg.Model
	}
	if c.cfg.Effort != "" {
		params["effort"] = c.cfg.Effort
	}
	return params
}

func codexInputItems(text string, attachments []model.AgentAttachment) []any {
	items := make([]any, 0, 1+len(attachments))
	items = append(items, map[string]any{"type": "text", "text": text})
	for _, value := range attachments {
		if value.Path == "" || !strings.HasPrefix(strings.ToLower(value.MediaType), "image/") {
			continue
		}
		items = append(items, map[string]any{"type": "localImage", "path": value.Path})
	}
	return items
}

func (c *CodexAdapter) stageWireInput(input model.AgentInput) {
	if input.MessageID == "" {
		return
	}
	c.mu.Lock()
	if _, exists := c.wireInputs[input.MessageID]; !exists {
		c.wireInputOrder = append(c.wireInputOrder, input.MessageID)
	}
	c.wireInputs[input.MessageID] = input
	c.mu.Unlock()
}

func (c *CodexAdapter) unstageWireInput(messageID string) {
	if messageID == "" {
		return
	}
	c.mu.Lock()
	delete(c.wireInputs, messageID)
	for index, value := range c.wireInputOrder {
		if value != messageID {
			continue
		}
		copy(c.wireInputOrder[index:], c.wireInputOrder[index+1:])
		c.wireInputOrder = c.wireInputOrder[:len(c.wireInputOrder)-1]
		break
	}
	c.mu.Unlock()
}

func (c *CodexAdapter) bindTurnInput(turnID string, input model.AgentInput) bool {
	if turnID == "" || input.MessageID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, existing := range c.turnInputs[turnID] {
		if existing.MessageID == input.MessageID {
			return false
		}
	}
	c.turnInputs[turnID] = append(c.turnInputs[turnID], input)
	return true
}

func (c *CodexAdapter) latestTurnInputLocked(turnID string) model.AgentInput {
	inputs := c.turnInputs[turnID]
	if len(inputs) == 0 {
		return model.AgentInput{}
	}
	return inputs[len(inputs)-1]
}

// knownTurnLocked is the transcript boundary for turn-scoped App Server
// notifications. A resumed Codex thread can replay events for native work
// that PairRoom did not submit; those events must remain diagnostics and must
// never become the current Room turn or acquire a message correlation.
// c.mu must be held by the caller.
func (c *CodexAdapter) knownTurnLocked(turnID string) bool {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return false
	}
	if c.currentTurn == turnID || c.startingTurnID == turnID {
		return true
	}
	if _, ok := c.turnInputs[turnID]; ok {
		return true
	}
	if _, ok := c.startedTurns[turnID]; ok {
		return true
	}
	return false
}

func (c *CodexAdapter) emitInputProcessing(turnID string, input model.AgentInput, detail string) {
	e := runtimeEvent(c.cfg.Actor, model.RuntimeInputProcessing)
	e.TurnID = turnID
	e.CorrelationID = input.MessageID
	e.Name = string(model.ProcessingWorking)
	e.Text = detail
	c.sink(e)
}

func (c *CodexAdapter) emitInputTerminal(turnID string, input model.AgentInput, kind, detail string) {
	e := runtimeEvent(c.cfg.Actor, kind)
	e.TurnID = turnID
	e.CorrelationID = input.MessageID
	e.Text = detail
	c.sink(e)
}

type codexOutstanding struct {
	inputs        []model.AgentInput
	inputTurns    map[string]string
	turnID        string
	correlationID string
}

func (c *CodexAdapter) takeOutstanding() codexOutstanding {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := codexOutstanding{turnID: c.currentTurn, inputTurns: make(map[string]string)}
	if input := c.latestTurnInputLocked(c.currentTurn); input.MessageID != "" {
		result.correlationID = input.MessageID
	} else if c.startingInput != nil {
		result.correlationID = c.startingInput.MessageID
	}
	seen := make(map[string]struct{})
	add := func(input model.AgentInput, turnID string) {
		if input.MessageID == "" {
			return
		}
		if _, exists := seen[input.MessageID]; exists {
			if result.inputTurns[input.MessageID] == "" && turnID != "" {
				result.inputTurns[input.MessageID] = turnID
			}
			return
		}
		seen[input.MessageID] = struct{}{}
		result.inputs = append(result.inputs, input)
		result.inputTurns[input.MessageID] = turnID
		if result.correlationID == "" {
			result.correlationID = input.MessageID
		}
	}
	for turnID, values := range c.turnInputs {
		for _, input := range values {
			add(input, turnID)
		}
	}
	if c.startingInput != nil {
		add(*c.startingInput, c.currentTurn)
	}
	for _, messageID := range c.wireInputOrder {
		if input, ok := c.wireInputs[messageID]; ok {
			add(input, c.currentTurn)
		}
	}
	c.turnInputs = make(map[string][]model.AgentInput)
	c.wireInputs = make(map[string]model.AgentInput)
	c.wireInputOrder = nil
	c.terminalTurns = make(map[string]codexTurnTerminal)
	c.startedTurns = make(map[string]struct{})
	c.pendingCompletions = make(map[string]json.RawMessage)
	c.startingInput = nil
	c.startingTurnID = ""
	c.turnBuffers = make(map[string]*strings.Builder)
	c.turnFinal = make(map[string]string)
	c.currentTurn = ""
	// thread/start creates a Codex thread in memory, but Codex only persists a
	// rollout once a turn is accepted. If the app-server process exits before
	// the first turn starts on a thread/start-only ID, that ID has no durable
	// rollout; strict-resuming it across a process restart hard-fails forever
	// ("no rollout found"). A pending new binding (no durable cfg.SessionID)
	// whose thread was never engaged has no identity to honor, so drop the
	// ephemeral ID and let the next Start create a fresh thread that the first
	// accepted turn atomically materializes. An existing/materialized binding
	// keeps its ID so the next Start resumes exactly; a missing rollout there
	// is a real binding inconsistency that must surface, not be replaced.
	if c.cfg.SessionID == "" && !c.threadEngaged {
		c.threadID = ""
	}
	return result
}

func (c *CodexAdapter) takeOutstandingInputs() []model.AgentInput {
	return c.takeOutstanding().inputs
}
