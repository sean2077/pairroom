package agent

import (
	"encoding/json"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

func (c *CodexAdapter) handleNotification(method string, params json.RawMessage) {
	switch method {
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.Turn.ID) == "" {
			return
		}
		newlyBound := false
		emitStarted := false
		c.mu.Lock()
		if _, completed := c.terminalTurns[p.Turn.ID]; completed {
			// A late notification for a turn whose completion already won the
			// race must not resurrect currentTurn or reopen its input lifecycle.
			c.mu.Unlock()
			return
		}
		// A notification for an unrelated/resumed native turn must never take
		// ownership of the Room. Only the already-recorded current turn, or the
		// turn that the staged turn/start input was bound to by its exact
		// userMessage.clientId echo, is admissible. While turn/start is in
		// flight and no echo has bound a turn, an arbitrary turn/started may be
		// stale; the turn/start response (or the echo) supplies the start event.
		if c.currentTurn != p.Turn.ID && c.startingTurnID != p.Turn.ID {
			c.mu.Unlock()
			return
		}
		if _, already := c.startedTurns[p.Turn.ID]; !already {
			c.startedTurns[p.Turn.ID] = struct{}{}
			emitStarted = true
		}
		c.currentTurn = p.Turn.ID
		c.threadEngaged = true
		if c.turnBuffers[p.Turn.ID] == nil {
			c.turnBuffers[p.Turn.ID] = &strings.Builder{}
		}
		if len(c.turnInputs[p.Turn.ID]) == 0 && c.startingInput != nil && c.startingTurnID == p.Turn.ID {
			// The staged input is already correlated to this exact turn; keep the
			// Inspector and final events on the same room-message correlation.
			c.turnInputs[p.Turn.ID] = append(c.turnInputs[p.Turn.ID], *c.startingInput)
			newlyBound = true
		}
		input := c.latestTurnInputLocked(p.Turn.ID)
		c.mu.Unlock()
		c.setState(model.StateWorking, "")
		if emitStarted {
			e := runtimeEvent(c.cfg.Actor, model.RuntimeTurnStarted)
			e.TurnID = p.Turn.ID
			e.CorrelationID = input.MessageID
			c.sink(e)
		}
		if newlyBound {
			c.emitInputProcessing(p.Turn.ID, input, "started Codex turn")
		}

	case "item/agentMessage/delta":
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.TurnID) == "" {
			return
		}
		c.mu.Lock()
		if !c.knownTurnLocked(p.TurnID) {
			c.mu.Unlock()
			return
		}
		builder := c.turnBuffers[p.TurnID]
		if builder == nil {
			builder = &strings.Builder{}
			c.turnBuffers[p.TurnID] = builder
		}
		builder.WriteString(p.Delta)
		input := c.latestTurnInputLocked(p.TurnID)
		c.mu.Unlock()
		e := runtimeEvent(c.cfg.Actor, model.RuntimeTextDelta)
		e.TurnID = p.TurnID
		e.ItemID = p.ItemID
		e.CorrelationID = input.MessageID
		e.Text = p.Delta
		c.sink(e)

	case "item/started", "item/completed":
		c.handleItem(method, params)

	case "item/commandExecution/outputDelta":
		var p struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.TurnID) == "" {
			return
		}
		c.mu.Lock()
		known := c.knownTurnLocked(p.TurnID)
		correlationID := c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		if !known {
			return
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimeCommandOutput)
		e.TurnID, e.ItemID, e.Text = p.TurnID, p.ItemID, p.Delta
		e.CorrelationID = correlationID
		c.sink(e)

	case "turn/diff/updated":
		var p struct {
			TurnID string `json:"turnId"`
			Diff   string `json:"diff"`
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.TurnID) == "" {
			return
		}
		c.mu.Lock()
		known := c.knownTurnLocked(p.TurnID)
		correlationID := c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		if !known {
			return
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimeDiffUpdated)
		e.TurnID, e.Text = p.TurnID, p.Diff
		e.CorrelationID = correlationID
		c.sink(e)

	case "item/plan/delta":
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.TurnID) == "" {
			return
		}
		c.mu.Lock()
		known := c.knownTurnLocked(p.TurnID)
		correlationID := c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		if !known {
			return
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimePlanUpdated)
		e.TurnID, e.ItemID, e.Text = p.TurnID, p.ItemID, p.Delta
		e.CorrelationID = correlationID
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)

	case "turn/plan/updated":
		// Older app-server releases emitted a whole-plan notification. Keep this
		// compatibility path while preferring the current item/plan/delta stream.
		var p struct {
			TurnID string `json:"turnId"`
		}
		_ = json.Unmarshal(params, &p)
		if strings.TrimSpace(p.TurnID) == "" {
			return
		}
		c.mu.Lock()
		known := c.knownTurnLocked(p.TurnID)
		correlationID := c.latestTurnInputLocked(p.TurnID).MessageID
		c.mu.Unlock()
		if !known {
			return
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimePlanUpdated)
		e.TurnID = p.TurnID
		e.CorrelationID = correlationID
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)

	case "thread/tokenUsage/updated":
		e := runtimeEvent(c.cfg.Actor, model.RuntimeUsageUpdated)
		c.mu.Lock()
		e.TurnID = c.currentTurn
		e.CorrelationID = c.latestTurnInputLocked(c.currentTurn).MessageID
		if e.CorrelationID == "" && c.startingInput != nil {
			e.CorrelationID = c.startingInput.MessageID
		}
		c.mu.Unlock()
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)

	case "turn/completed":
		c.handleTurnCompleted(params)

	case "serverRequest/resolved":
		c.handleServerRequestResolved(params)

	case "error", "warning", "configWarning":
		// App Server `error` notifications are diagnostics and may arrive while
		// the native Turn continues. Preserve their error classification for the
		// inspector, but Room scheduling must not treat RuntimeError as a terminal
		// boundary; only turn/completed, confirmed process exit, or explicit abort
		// and stop signals may release ownership.
		kind := model.RuntimeLog
		if method == "error" {
			kind = model.RuntimeError
		}
		e := runtimeEvent(c.cfg.Actor, kind)
		e.Name = method
		e.Data = append(json.RawMessage(nil), params...)
		var p struct {
			TurnID  string `json:"turnId"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(params, &p)
		c.mu.Lock()
		turnID := p.TurnID
		if strings.TrimSpace(turnID) != "" && !c.knownTurnLocked(turnID) {
			c.mu.Unlock()
			return
		}
		if turnID == "" && method == "error" {
			turnID = c.currentTurn
		}
		e.TurnID = turnID
		e.CorrelationID = c.latestTurnInputLocked(turnID).MessageID
		c.mu.Unlock()
		e.Text = p.Message
		if e.Text == "" {
			e.Text = p.Error.Message
		}
		c.sink(e)

	default:
		if strings.HasPrefix(method, "thread/") || strings.HasPrefix(method, "serverRequest/") {
			return
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = method
		e.Data = append(json.RawMessage(nil), params...)
		c.sink(e)
	}
}

func (c *CodexAdapter) handleItem(method string, params json.RawMessage) {
	var p struct {
		TurnID string `json:"turnId"`
		Item   struct {
			ID               string          `json:"id"`
			ClientID         string          `json:"clientId"`
			Type             string          `json:"type"`
			Phase            string          `json:"phase"`
			Text             string          `json:"text"`
			Command          json.RawMessage `json:"command"`
			Cwd              string          `json:"cwd"`
			Status           string          `json:"status"`
			AggregatedOutput string          `json:"aggregatedOutput"`
			Changes          json.RawMessage `json:"changes"`
		} `json:"item"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if strings.TrimSpace(p.TurnID) == "" {
		return
	}
	if p.Item.Type == "userMessage" {
		// App-server echoes turn/start or turn/steer's optional
		// clientUserMessageId as userMessage.clientId. Use that documented
		// correlation surface to bind notifications that can race the RPC reply.
		if p.Item.ClientID != "" {
			c.mu.Lock()
			if _, completed := c.terminalTurns[p.TurnID]; completed {
				c.mu.Unlock()
				return
			}
			input, ok := c.wireInputs[p.Item.ClientID]
			// The echo of the staged turn/start input is the exact evidence that
			// binds an opaque turn ID before the turn/start response arrives.
			adopt := ok && c.startingInput != nil && c.startingInput.MessageID == p.Item.ClientID &&
				c.startingTurnID == "" && c.currentTurn == ""
			emitStarted := false
			if adopt {
				c.startingTurnID = p.TurnID
				c.currentTurn = p.TurnID
				c.threadEngaged = true
				if c.turnBuffers[p.TurnID] == nil {
					c.turnBuffers[p.TurnID] = &strings.Builder{}
				}
				if _, already := c.startedTurns[p.TurnID]; !already {
					c.startedTurns[p.TurnID] = struct{}{}
					emitStarted = true
				}
			}
			c.mu.Unlock()
			if emitStarted {
				c.setState(model.StateWorking, "")
				started := runtimeEvent(c.cfg.Actor, model.RuntimeTurnStarted)
				started.TurnID = p.TurnID
				started.CorrelationID = input.MessageID
				c.sink(started)
			}
			if ok && c.bindTurnInput(p.TurnID, input) {
				c.emitInputProcessing(p.TurnID, input, "acknowledged by Codex app-server")
			}
		}
		// A userMessage is transport activity, not a tool invocation.
		return
	}
	c.mu.Lock()
	known := c.knownTurnLocked(p.TurnID)
	correlationID := c.latestTurnInputLocked(p.TurnID).MessageID
	c.mu.Unlock()
	if !known {
		return
	}
	kind := model.RuntimeToolStarted
	if method == "item/completed" {
		kind = model.RuntimeToolCompleted
	}
	if p.Item.Type == "agentMessage" && method == "item/completed" {
		c.mu.Lock()
		// Prefer the authoritative final_answer item. Older app-server versions
		// may omit phase, in which case the latest completed message is retained.
		if p.Item.Phase == "final_answer" || p.Item.Phase == "" || c.turnFinal[p.TurnID] == "" {
			c.turnFinal[p.TurnID] = p.Item.Text
		}
		c.mu.Unlock()
		return
	}
	e := runtimeEvent(c.cfg.Actor, kind)
	e.TurnID = p.TurnID
	e.ItemID = p.Item.ID
	e.Name = p.Item.Type
	e.CorrelationID = correlationID
	e.Data = append(json.RawMessage(nil), params...)
	c.sink(e)
}

func (c *CodexAdapter) handleTurnCompleted(params json.RawMessage) {
	var p struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
			Items []struct {
				Type   string `json:"type"`
				Phase  string `json:"phase"`
				Text   string `json:"text"`
				Review string `json:"review"`
			} `json:"items"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if strings.TrimSpace(p.Turn.ID) == "" {
		return
	}
	terminalKind := model.RuntimeInputFailed
	detail := p.Turn.Status
	switch strings.ToLower(p.Turn.Status) {
	case "completed", "success":
		terminalKind = model.RuntimeInputCompleted
	case "interrupted", "cancelled", "canceled", "aborted":
		terminalKind = model.RuntimeInputCancelled
	}
	if p.Turn.Error != nil && p.Turn.Error.Message != "" {
		detail = p.Turn.Error.Message
	}
	c.mu.Lock()
	wasCurrent := c.currentTurn == p.Turn.ID
	inputs := append([]model.AgentInput(nil), c.turnInputs[p.Turn.ID]...)
	if _, completed := c.terminalTurns[p.Turn.ID]; completed {
		c.mu.Unlock()
		return
	}
	// While turn/start is in flight, only its exact response or clientId echo
	// can bind an opaque turn ID. A completion for any other turn may be stale
	// and must never settle the staged input or relay its final text.
	startingTurn := c.startingInput != nil && c.startingTurnID == p.Turn.ID
	knownTurn := wasCurrent || len(inputs) > 0 || startingTurn
	if !knownTurn {
		if c.startingInput != nil && c.startingTurnID == "" {
			// We cannot correlate an arbitrary completion to the in-flight
			// turn/start until its response reveals the ID. Hold it and let
			// StartTurn consume only an exact ID match.
			if c.pendingCompletions == nil {
				c.pendingCompletions = make(map[string]json.RawMessage)
			}
			if len(c.pendingCompletions) < codexPendingCompletionLimit {
				c.pendingCompletions[p.Turn.ID] = append(json.RawMessage(nil), params...)
			}
		}
		// A connection can receive a late notification for a turn that this
		// adapter never owned (for example, a stale subscription event). Do not
		// manufacture a Room boundary or release another active owner for it.
		c.mu.Unlock()
		return
	}
	seenInputs := make(map[string]struct{}, len(inputs))
	for _, value := range inputs {
		if value.MessageID != "" {
			seenInputs[value.MessageID] = struct{}{}
		}
	}
	addInput := func(value model.AgentInput) {
		if value.MessageID == "" {
			return
		}
		if _, exists := seenInputs[value.MessageID]; exists {
			return
		}
		seenInputs[value.MessageID] = struct{}{}
		inputs = append(inputs, value)
	}
	// A completion can overtake the turn/start response. Include the input that
	// is still staged for that request so it is settled exactly once. A
	// turn/steer input whose userMessage echo has not arrived is not yet
	// accepted: Steer settles it from the RPC response (accepted inputs get
	// this turn's terminal event; rejected inputs get none and fall back).
	if wasCurrent || startingTurn {
		if c.startingInput != nil {
			addInput(*c.startingInput)
		}
		for _, messageID := range c.wireInputOrder {
			if messageID == c.steeringInput {
				continue
			}
			if value, ok := c.wireInputs[messageID]; ok {
				addInput(value)
			}
		}
	}
	input := model.AgentInput{}
	if len(inputs) > 0 {
		input = inputs[len(inputs)-1]
	}
	text := c.turnFinal[p.Turn.ID]
	if text == "" {
		// Current App Server v2 includes the final agentMessage in the completed
		// turn as a summary fallback when item notifications were suppressed or
		// raced the terminal event. Prefer that authoritative item over a partial
		// delta buffer; exitedReviewMode is the equivalent final text for an
		// inline review turn.
		for _, item := range p.Turn.Items {
			if item.Type == "agentMessage" && strings.TrimSpace(item.Text) != "" {
				if item.Phase == "final_answer" || text == "" {
					text = item.Text
				}
			}
			if item.Type == "exitedReviewMode" && strings.TrimSpace(item.Review) != "" {
				text = item.Review
			}
		}
	}
	if text == "" && c.turnBuffers[p.Turn.ID] != nil {
		text = c.turnBuffers[p.Turn.ID].String()
	}
	delete(c.turnInputs, p.Turn.ID)
	delete(c.turnFinal, p.Turn.ID)
	delete(c.turnBuffers, p.Turn.ID)
	delete(c.startedTurns, p.Turn.ID)
	if wasCurrent {
		c.currentTurn = ""
	}
	if _, duplicate := c.terminalTurns[p.Turn.ID]; duplicate {
		c.mu.Unlock()
		return
	}
	inputIDs := make(map[string]struct{}, len(inputs))
	for _, value := range inputs {
		if value.MessageID != "" {
			inputIDs[value.MessageID] = struct{}{}
		}
	}
	c.terminalTurns[p.Turn.ID] = codexTurnTerminal{status: p.Turn.Status, kind: terminalKind, detail: detail, inputIDs: inputIDs}
	if len(c.terminalTurns) > 256 {
		// Turn IDs are opaque and unique. Evicting an arbitrary old tombstone
		// only bounds memory; late responses are expected within the current
		// request lifetime and are consumed before this limit is reached.
		for turnID := range c.terminalTurns {
			if turnID != p.Turn.ID {
				delete(c.terminalTurns, turnID)
				break
			}
		}
	}
	if wasCurrent || startingTurn {
		c.startingInput = nil
		c.startingTurnID = ""
		steering, steeringStaged := c.wireInputs[c.steeringInput]
		c.wireInputs = make(map[string]model.AgentInput)
		c.wireInputOrder = nil
		if steeringStaged {
			// Keep the unsettled steer staged so a process exit before its
			// response still reports it as an outstanding input.
			c.wireInputs[c.steeringInput] = steering
			c.wireInputOrder = []string{c.steeringInput}
		}
	}
	staleApprovals := make([]pendingApproval, 0, len(c.approvals))
	for id, pending := range c.approvals {
		if pending.turnID == p.Turn.ID {
			staleApprovals = append(staleApprovals, pending)
			delete(c.approvals, id)
		}
	}
	c.mu.Unlock()
	c.clearStaleApprovals(staleApprovals)
	for _, item := range inputs {
		c.emitInputTerminal(p.Turn.ID, item, terminalKind, detail)
	}
	if terminalKind == model.RuntimeInputCompleted && strings.TrimSpace(text) != "" {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeFinal)
		e.TurnID = p.Turn.ID
		e.CorrelationID = input.MessageID
		e.Text = text
		c.sink(e)
	}
	completed := runtimeEvent(c.cfg.Actor, model.RuntimeTurnCompleted)
	completed.TurnID = p.Turn.ID
	completed.CorrelationID = input.MessageID
	completed.Name = p.Turn.Status
	completed.Data = append(json.RawMessage(nil), params...)
	c.sink(completed)
	if p.Turn.Error != nil && p.Turn.Error.Message != "" {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeError)
		e.TurnID = p.Turn.ID
		e.CorrelationID = input.MessageID
		e.Text = p.Turn.Error.Message
		c.sink(e)
		c.setState(model.StateError, p.Turn.Error.Message)
		return
	}
	if terminalKind == model.RuntimeInputFailed {
		c.setState(model.StateError, detail)
		return
	} else {
		c.setState(model.StateIdle, "")
	}
}
