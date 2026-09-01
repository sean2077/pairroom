package room

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

type workflowDispatch struct {
	Active     bool
	New        bool
	Actor      model.ActorID
	Mode       model.WorkflowMode
	WorkflowID string
	StageIndex int
}

var workflowPairPattern = regexp.MustCompile(`(?i)(claude(?:\s+code)?|cc|codex)\s*(?:来|负责|进行|先|再|然后|[,，:：;；\s\-–—>→])*\s*(planning|plan|规划|计划|方案|review|审查|评审|audit|复核|验收|execute|implement|执行|实现|开发|discussion|discuss|讨论|探讨|协商)`)

var workflowApprovalPattern = regexp.MustCompile(`(?i)^\s*(?:please\s+|请)?(?:approve|approved|go\s+ahead|proceed|批准执行当前计划|批准|同意执行|开始执行|按计划执行)\s*[.!。！]?\s*$`)
var workflowRejectPattern = regexp.MustCompile(`(?i)^\s*(?:please\s+|请)?(?:(?:reject|cancel)(?:\s+(?:it|this|workflow|execution|plan))?|do\s+not\s+(?:execute|proceed)|don'?t\s+(?:execute|proceed)|not\s+approved|(?:拒绝|不批准|取消流程|不要执行|停止执行)(?:[，,；;\s]+(?:拒绝|不批准|取消流程|不要执行|停止执行))*)\s*[.!。！]?\s*$`)
var workflowNoGatePattern = regexp.MustCompile(`(?i)(without\s+approval|no\s+approval|直接执行|无需审批|不需批准|自动执行)`)
var workflowRequireGatePattern = regexp.MustCompile(`(?i)((?:do\s+not|don'?t|never)\s+(?:execute|proceed|run)\s+without\s+(?:approval|permission)|(?:must|requires?|needs?)\s+(?:approval|permission)|(?:批准|审批)后(?:再)?执行|不要直接执行|不得直接执行|禁止直接执行|执行(?:前|之前|需|需要|必须).{0,8}(?:审批|批准))`)

func compileWorkflow(text string) (*model.WorkflowState, bool) {
	matches := workflowPairPattern.FindAllStringSubmatch(text, -1)
	if len(matches) < 2 {
		return nil, false
	}
	if len(matches) > 12 {
		matches = matches[:12]
	}
	now := time.Now().UTC()
	workflow := &model.WorkflowState{
		ID: model.NewID("workflow"), Goal: strings.TrimSpace(text),
		Status: model.WorkflowStatusRunning, CurrentStage: 0,
		CreatedAt: now, UpdatedAt: now,
	}
	workflow.Stages = make([]model.WorkflowStage, 0, len(matches))
	seenPreExecution := false
	for index, match := range matches {
		actor := workflowActor(match[1])
		mode := workflowMode(match[2])
		if !actor.ValidParticipant() || !mode.Valid() {
			continue
		}
		stage := model.WorkflowStage{
			ID: fmt.Sprintf("%s-%d", workflow.ID, index+1), Index: index,
			Actor: actor, Mode: mode, Label: workflowStageLabel(actor, mode),
			Status: model.WorkflowStagePending,
		}
		if mode == model.WorkflowExecute && seenPreExecution {
			workflow.RequiresApproval = true
		}
		if mode == model.WorkflowPlan || mode == model.WorkflowReview || mode == model.WorkflowAudit {
			seenPreExecution = true
		}
		workflow.Stages = append(workflow.Stages, stage)
	}
	if len(workflow.Stages) < 2 {
		return nil, false
	}
	for index := range workflow.Stages {
		workflow.Stages[index].Index = index
		workflow.Stages[index].ID = fmt.Sprintf("%s-%d", workflow.ID, index+1)
	}
	workflow.Stages[0].Status = model.WorkflowStageRunning
	workflow.Stages[0].StartedAt = &now
	if workflowExplicitNoGate(text) {
		workflow.RequiresApproval = false
	}
	return workflow, true
}

func workflowExplicitNoGate(text string) bool {
	return workflowNoGatePattern.MatchString(text) && !workflowRequireGatePattern.MatchString(text)
}

func workflowActor(value string) model.ActorID {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "claude", "claude code", "cc":
		return model.ActorClaude
	case "codex":
		return model.ActorCodex
	default:
		return ""
	}
}

func workflowMode(value string) model.WorkflowMode {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "planning", "plan", "规划", "计划", "方案":
		return model.WorkflowPlan
	case "review", "审查", "评审":
		return model.WorkflowReview
	case "audit", "复核", "验收":
		return model.WorkflowAudit
	case "execute", "implement", "执行", "实现", "开发":
		return model.WorkflowExecute
	case "discussion", "discuss", "讨论", "探讨", "协商":
		return model.WorkflowDiscuss
	default:
		return ""
	}
}

func workflowStageLabel(actor model.ActorID, mode model.WorkflowMode) string {
	return fmt.Sprintf("%s · %s", actor.DisplayName(), strings.Title(string(mode))) //nolint:staticcheck -- stable UI label
}

func cloneWorkflow(in *model.WorkflowState) *model.WorkflowState {
	if in == nil {
		return nil
	}
	out := *in
	out.Stages = append([]model.WorkflowStage(nil), in.Stages...)
	return &out
}

func workflowActive(status string) bool {
	switch status {
	case model.WorkflowStatusRunning, model.WorkflowStatusWaitingHuman, model.WorkflowStatusAwaitingApproval:
		return true
	default:
		return false
	}
}

// workflowDispatchForUser compiles a new sequence or associates a human
// intervention with the currently active stage. Caller holds routingMu.
func (e *Engine) workflowDispatchForUser(text string, req SendRequest) (workflowDispatch, error) {
	if workflow, ok := compileWorkflow(text); ok {
		e.mu.RLock()
		existing := cloneWorkflow(e.snapshot.Workflow)
		e.mu.RUnlock()
		if existing != nil && workflowActive(existing.Status) {
			existing.Status = model.WorkflowStatusSuperseded
			existing.UpdatedAt = time.Now().UTC()
			if _, err := e.record(EventWorkflowUpdated, model.ActorUser, *existing); err != nil {
				return workflowDispatch{}, err
			}
		}
		if _, err := e.record(EventWorkflowUpdated, model.ActorUser, *workflow); err != nil {
			return workflowDispatch{}, err
		}
		first := workflow.Stages[0]
		return workflowDispatch{Active: true, New: true, Actor: first.Actor, Mode: first.Mode, WorkflowID: workflow.ID, StageIndex: 0}, nil
	}

	e.mu.RLock()
	workflow := cloneWorkflow(e.snapshot.Workflow)
	e.mu.RUnlock()
	if workflow == nil || !workflowActive(workflow.Status) || workflow.CurrentStage < 0 || workflow.CurrentStage >= len(workflow.Stages) {
		return workflowDispatch{}, nil
	}
	stage := workflow.Stages[workflow.CurrentStage]

	switch workflow.Status {
	case model.WorkflowStatusAwaitingApproval:
		if workflowRejectPattern.MatchString(text) {
			now := time.Now().UTC()
			workflow.Status = model.WorkflowStatusCancelled
			workflow.Stages[workflow.CurrentStage].Status = model.WorkflowStageCancelled
			workflow.UpdatedAt, workflow.CompletedAt = now, &now
			if _, err := e.record(EventWorkflowUpdated, model.ActorUser, *workflow); err != nil {
				return workflowDispatch{}, err
			}
			e.notice("info", "The pending workflow execution was cancelled by the user.")
			return workflowDispatch{}, nil
		}
		if !workflowApprovalPattern.MatchString(text) {
			return workflowDispatch{}, nil
		}
		now := time.Now().UTC()
		workflow.ApprovedRevision = workflow.Revision
		workflow.Status = model.WorkflowStatusRunning
		workflow.Stages[workflow.CurrentStage].Status = model.WorkflowStageRunning
		workflow.Stages[workflow.CurrentStage].StartedAt = &now
		workflow.UpdatedAt = now
		if _, err := e.record(EventWorkflowUpdated, model.ActorUser, *workflow); err != nil {
			return workflowDispatch{}, err
		}
		return workflowDispatch{Active: true, Actor: stage.Actor, Mode: stage.Mode, WorkflowID: workflow.ID, StageIndex: workflow.CurrentStage}, nil

	case model.WorkflowStatusWaitingHuman:
		now := time.Now().UTC()
		workflow.Status = model.WorkflowStatusRunning
		workflow.Stages[workflow.CurrentStage].Status = model.WorkflowStageRunning
		if workflow.Stages[workflow.CurrentStage].StartedAt == nil {
			workflow.Stages[workflow.CurrentStage].StartedAt = &now
		}
		workflow.UpdatedAt = now
		if _, err := e.record(EventWorkflowUpdated, model.ActorUser, *workflow); err != nil {
			return workflowDispatch{}, err
		}
		return workflowDispatch{Active: true, Actor: stage.Actor, Mode: stage.Mode, WorkflowID: workflow.ID, StageIndex: workflow.CurrentStage}, nil

	case model.WorkflowStatusRunning:
		if !e.workflowRequestTargetsStage(text, req, stage.Actor) {
			return workflowDispatch{}, nil
		}
		return workflowDispatch{Active: true, Actor: stage.Actor, Mode: stage.Mode, WorkflowID: workflow.ID, StageIndex: workflow.CurrentStage}, nil
	}
	return workflowDispatch{}, nil
}

// workflowRequestTargetsStage reports whether a message without a newly
// compiled workflow belongs to the active stage. Explicit recipient, role,
// mention, and reply targets retain ordinary routing semantics.
func (e *Engine) workflowRequestTargetsStage(text string, req SendRequest, stageActor model.ActorID) bool {
	targets := model.NormalizeActors(req.To)
	if req.TargetRole != "" {
		if len(targets) > 0 || (req.TargetRole != model.RoleDriver && req.TargetRole != model.RoleReviewer) {
			return false
		}
		e.mu.RLock()
		for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
			if e.snapshot.Participants[actor].Role == req.TargetRole {
				targets = append(targets, actor)
			}
		}
		e.mu.RUnlock()
		return len(targets) == 1 && targets[0] == stageActor
	}
	if len(targets) > 0 {
		return len(targets) == 1 && targets[0] == stageActor
	}
	if targets = prompt.Mentions(text, model.ActorUser); len(targets) > 0 {
		return len(targets) == 1 && targets[0] == stageActor
	}
	if req.ReplyTo != "" {
		e.mu.RLock()
		replied, found := e.findMessageLocked(req.ReplyTo)
		e.mu.RUnlock()
		if found && replied.From.ValidParticipant() {
			return replied.From == stageActor
		}
	}
	return true
}

func (e *Engine) workflowAttachMessage(dispatch workflowDispatch, messageID string) {
	if !dispatch.Active || dispatch.WorkflowID == "" || messageID == "" {
		return
	}
	e.mu.RLock()
	workflow := cloneWorkflow(e.snapshot.Workflow)
	e.mu.RUnlock()
	if workflow == nil || workflow.ID != dispatch.WorkflowID {
		return
	}
	if workflow.SourceMessageID == "" {
		workflow.SourceMessageID = messageID
	}
	workflow.LastMessageID = messageID
	workflow.UpdatedAt = time.Now().UTC()
	_, _ = e.record(EventWorkflowUpdated, model.ActorUser, *workflow)
}

func (e *Engine) prepareWorkflowStage(ctx context.Context, message model.Message, target model.ActorID) error {
	if message.WorkflowID == "" || !message.WorkflowMode.Valid() {
		return nil
	}
	desiredDriver := target
	switch message.WorkflowMode {
	case model.WorkflowReview, model.WorkflowAudit:
		desiredDriver = model.OtherParticipant(target)
	case model.WorkflowDiscuss:
		return nil
	}
	if !desiredDriver.ValidParticipant() {
		return errors.New("workflow stage has no valid Driver")
	}
	e.mu.RLock()
	currentDriver := model.ActorID("")
	currentReviewer := model.ActorID("")
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		switch e.snapshot.Participants[actor].Role {
		case model.RoleDriver:
			currentDriver = actor
		case model.RoleReviewer:
			currentReviewer = actor
		}
	}
	e.mu.RUnlock()
	if currentDriver == desiredDriver && currentReviewer == model.OtherParticipant(desiredDriver) {
		return nil
	}
	return e.SwitchDriver(ctx, desiredDriver)
}

func (e *Engine) deliverRouted(ctx context.Context, message model.Message, target model.ActorID) {
	if message.WorkflowID != "" {
		prepareCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := e.prepareWorkflowStage(prepareCtx, message, target)
		cancel()
		if err != nil {
			detail := "prepare workflow stage: " + err.Error()
			e.delivery(message.ID, target, model.DeliveryFailed, detail)
			e.processing(message.ID, target, model.ProcessingFailed, detail, "")
			e.notice("error", fmt.Sprintf("Workflow stage %d could not start for %s: %v", message.WorkflowStage+1, target.DisplayName(), err))
			return
		}
	}
	e.deliver(ctx, message, target)
}

func (e *Engine) workflowOwnsFinal(incoming model.Message, actor model.ActorID) bool {
	if incoming.WorkflowID == "" || !actor.ValidParticipant() {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	workflow := e.snapshot.Workflow
	if workflow == nil || workflow.Status != model.WorkflowStatusRunning || workflow.ID != incoming.WorkflowID ||
		workflow.CurrentStage != incoming.WorkflowStage || workflow.CurrentStage < 0 || workflow.CurrentStage >= len(workflow.Stages) {
		return false
	}
	return workflow.Stages[workflow.CurrentStage].Actor == actor
}

// workflowOnFinal captures a stage result. The actual transition waits for
// turn.completed so changing Driver/workspace can never race an active turn.
// Caller holds routingMu.
func (e *Engine) workflowOnFinal(incoming, output model.Message, control string) {
	e.mu.RLock()
	workflow := cloneWorkflow(e.snapshot.Workflow)
	e.mu.RUnlock()
	if workflow == nil || workflow.Status != model.WorkflowStatusRunning || incoming.WorkflowID == "" || workflow.ID != incoming.WorkflowID ||
		workflow.CurrentStage != incoming.WorkflowStage || workflow.CurrentStage < 0 || workflow.CurrentStage >= len(workflow.Stages) {
		return
	}
	now := time.Now().UTC()
	workflow.LastMessageID = output.ID
	workflow.LastTurnID = output.TurnID
	workflow.LastResult = output.Text
	workflow.LastSignal = control
	workflow.UpdatedAt = now
	stage := &workflow.Stages[workflow.CurrentStage]

	wait := control == "WAIT" || control == "BLOCKED" || prompt.MentionsHuman(output.Text)
	if wait {
		workflow.Status = model.WorkflowStatusWaitingHuman
		stage.Status = model.WorkflowStageWaitingHuman
	}
	_, _ = e.record(EventWorkflowUpdated, output.From, *workflow)
}

func (e *Engine) advanceWorkflow(runtimeEvent model.RuntimeEvent) {
	if runtimeEvent.TurnID == "" || !runtimeEvent.Agent.ValidParticipant() {
		return
	}
	e.routingMu.Lock()
	defer e.routingMu.Unlock()

	e.mu.RLock()
	workflow := cloneWorkflow(e.snapshot.Workflow)
	e.mu.RUnlock()
	if workflow == nil || workflow.Status != model.WorkflowStatusRunning || workflow.LastTurnID != runtimeEvent.TurnID ||
		workflow.CurrentStage < 0 || workflow.CurrentStage >= len(workflow.Stages) {
		return
	}
	stage := &workflow.Stages[workflow.CurrentStage]
	if stage.Actor != runtimeEvent.Agent {
		return
	}
	if e.workflowStageHasInFlightInput(workflow.ID, workflow.CurrentStage, stage.Actor, runtimeEvent.CorrelationID) {
		e.notice("info", fmt.Sprintf("Workflow stage %d remains active while queued guidance for %s is processed.", workflow.CurrentStage+1, stage.Actor.DisplayName()))
		return
	}
	now := time.Now().UTC()
	if !workflowTurnSucceeded(runtimeEvent.Name) {
		stage.Status = model.WorkflowStageFailed
		stage.CompletedAt = &now
		workflow.Status = model.WorkflowStatusFailed
		workflow.CompletedAt = &now
		workflow.UpdatedAt = now
		_, _ = e.record(EventWorkflowUpdated, model.ActorSystem, *workflow)
		return
	}
	stage.Status = model.WorkflowStageCompleted
	stage.CompletedAt = &now
	if stage.Mode == model.WorkflowPlan {
		workflow.Revision++
	}
	next := workflow.CurrentStage + 1
	if next >= len(workflow.Stages) {
		workflow.Status = model.WorkflowStatusCompleted
		workflow.CompletedAt = &now
		workflow.UpdatedAt = now
		_, _ = e.record(EventWorkflowUpdated, model.ActorSystem, *workflow)
		e.notice("info", "The requested natural-language workflow completed all stages.")
		return
	}

	workflow.CurrentStage = next
	workflow.UpdatedAt = now
	nextStage := &workflow.Stages[next]
	if nextStage.Mode == model.WorkflowExecute && workflow.RequiresApproval {
		if workflow.Revision == 0 {
			workflow.Revision = 1
		}
		if workflow.ApprovedRevision < workflow.Revision {
			workflow.Status = model.WorkflowStatusAwaitingApproval
			nextStage.Status = model.WorkflowStagePending
			if _, err := e.record(EventWorkflowUpdated, model.ActorSystem, *workflow); err == nil {
				e.notice("warning", fmt.Sprintf("Workflow plan revision %d is ready. Send “批准执行当前计划” / “approve” to start %s's execution stage.", workflow.Revision, nextStage.Actor.DisplayName()))
			}
			return
		}
	}

	workflow.Status = model.WorkflowStatusRunning
	nextStage.Status = model.WorkflowStageRunning
	nextStage.StartedAt = &now
	if _, err := e.record(EventWorkflowUpdated, model.ActorSystem, *workflow); err != nil {
		return
	}
	message, err := e.workflowHandoffMessage(*workflow, *stage, *nextStage)
	if err != nil {
		e.notice("error", "Create workflow handoff: "+err.Error())
		return
	}
	e.scheduleDelivery(e.runtimeContext(context.Background()), message, nextStage.Actor)
}

func (e *Engine) workflowStageHasInFlightInput(workflowID string, stageIndex int, actor model.ActorID, completedMessageID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, message := range e.snapshot.Messages {
		if message.ID == completedMessageID || message.WorkflowID != workflowID || message.WorkflowStage != stageIndex {
			continue
		}
		targeted := false
		for _, target := range message.To {
			if target == actor {
				targeted = true
				break
			}
		}
		if !targeted {
			continue
		}
		state := message.Processing[actor]
		if state == model.ProcessingWaiting || state == model.ProcessingWorking {
			return true
		}
	}
	return false
}

// prepareWorkflowRetry validates that a failed delivery still belongs to the
// current stage and reopens a failed workflow before the auditable retry is
// recorded. Caller holds routingMu.
func (e *Engine) prepareWorkflowRetry(original model.Message, targets []model.ActorID, now time.Time) error {
	if original.WorkflowID == "" {
		return nil
	}
	e.mu.RLock()
	workflow := cloneWorkflow(e.snapshot.Workflow)
	inFlight := false
	for _, candidate := range e.snapshot.Messages {
		if candidate.ID == original.ID || candidate.WorkflowID != original.WorkflowID || candidate.WorkflowStage != original.WorkflowStage {
			continue
		}
		for _, target := range targets {
			state := candidate.Processing[target]
			if state == model.ProcessingWaiting || state == model.ProcessingWorking {
				inFlight = true
			}
		}
	}
	e.mu.RUnlock()
	if workflow == nil || workflow.ID != original.WorkflowID || workflow.CurrentStage != original.WorkflowStage || workflow.CurrentStage < 0 || workflow.CurrentStage >= len(workflow.Stages) {
		return errors.New("workflow stage is no longer current")
	}
	stage := &workflow.Stages[workflow.CurrentStage]
	if len(targets) != 1 || targets[0] != stage.Actor {
		return errors.New("workflow retry must target the current stage actor")
	}
	if inFlight {
		return errors.New("workflow stage already has an in-flight retry")
	}
	switch workflow.Status {
	case model.WorkflowStatusRunning:
		return nil
	case model.WorkflowStatusFailed:
		workflow.Status = model.WorkflowStatusRunning
		workflow.CompletedAt = nil
		workflow.LastTurnID = ""
		workflow.LastResult = ""
		workflow.LastSignal = ""
		workflow.UpdatedAt = now
		stage.Status = model.WorkflowStageRunning
		stage.StartedAt = &now
		stage.CompletedAt = nil
		_, err := e.record(EventWorkflowUpdated, model.ActorUser, *workflow)
		return err
	default:
		return fmt.Errorf("workflow status %q cannot be retried; reply to or restart the workflow instead", workflow.Status)
	}
}

func workflowTurnSucceeded(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "" || value == "completed" || value == "success"
}

func (e *Engine) workflowHandoffMessage(workflow model.WorkflowState, previous, next model.WorkflowStage) (model.Message, error) {
	e.mu.RLock()
	prior, found := model.Message{}, false
	if workflow.LastTurnID != "" {
		for index := len(e.snapshot.Messages) - 1; index >= 0; index-- {
			candidate := e.snapshot.Messages[index]
			if candidate.WorkflowID == workflow.ID && candidate.WorkflowStage == previous.Index &&
				candidate.TurnID == workflow.LastTurnID && candidate.From == previous.Actor {
				prior, found = candidate, true
				break
			}
		}
	}
	if !found {
		prior, found = e.findMessageLocked(workflow.LastMessageID)
	}
	e.mu.RUnlock()
	if !found {
		return model.Message{}, errors.New("previous workflow result is missing")
	}
	result := strings.TrimSpace(workflow.LastResult)
	if result == "" {
		result = prior.Text
	}
	now := time.Now().UTC()
	handoff := compactHandoff(workflowStagePrompt(workflow, previous, next, result))
	message := model.Message{
		ID: model.NewID("msg"), From: previous.Actor,
		To:      []model.ActorID{model.ActorUser, next.Actor},
		Text:    fmt.Sprintf("Workflow stage %d/%d completed; continuing with %s.", previous.Index+1, len(workflow.Stages), next.Label),
		Handoff: handoff, ReplyTo: prior.ID, ThreadID: prior.ThreadID,
		Hop: prior.Hop + 1, Intent: model.IntentNextTurn,
		WorkflowID: workflow.ID, WorkflowStage: next.Index, WorkflowMode: next.Mode,
		CreatedAt:               now,
		Delivery:                map[model.ActorID]model.DeliveryState{next.Actor: model.DeliveryPending},
		DeliveryDetail:          make(map[model.ActorID]string, 1),
		Processing:              map[model.ActorID]model.ProcessingState{next.Actor: model.ProcessingWaiting},
		ProcessingDetail:        make(map[model.ActorID]string, 1),
		ProcessingTurn:          make(map[model.ActorID]string, 1),
		ProcessingLastUpdatedAt: map[model.ActorID]time.Time{next.Actor: now},
	}
	event, err := e.record(EventMessageCreated, previous.Actor, message)
	if err != nil {
		return model.Message{}, err
	}
	message.Seq = event.Seq
	e.workflowAttachMessage(workflowDispatch{Active: true, WorkflowID: workflow.ID, StageIndex: next.Index}, message.ID)
	return message, nil
}

func workflowStagePrompt(workflow model.WorkflowState, previous, next model.WorkflowStage, result string) string {
	approval := ""
	if next.Mode == model.WorkflowExecute && workflow.RequiresApproval {
		approval = fmt.Sprintf("\nThe human approved plan revision %d. Implement only the approved scope.", workflow.ApprovedRevision)
	}
	return fmt.Sprintf(`Human-requested PairRoom workflow
Goal: %s
Current stage: %d/%d — %s
Previous stage: %s%s

Complete only the current stage. Preserve material disagreements and evidence. If a human choice is required, ask visibly with @human and end [PAIRROOM:WAIT]; never wait on a hidden terminal prompt.

Previous result:
%s`, workflow.Goal, next.Index+1, len(workflow.Stages), next.Label, previous.Label, approval, result)
}

func (e *Engine) workflowExpectedWaitLocked(actor model.ActorID) bool {
	workflow := e.snapshot.Workflow
	if workflow == nil || workflow.CurrentStage < 0 || workflow.CurrentStage >= len(workflow.Stages) || workflow.Stages[workflow.CurrentStage].Actor != actor {
		return false
	}
	return workflow.Status == model.WorkflowStatusWaitingHuman || workflow.Status == model.WorkflowStatusAwaitingApproval
}

func (e *Engine) actorHasPendingApprovalLocked(actor model.ActorID) bool {
	for _, approval := range e.snapshot.Approvals {
		if approval.Agent == actor && approval.Status == "pending" {
			return true
		}
	}
	return false
}
