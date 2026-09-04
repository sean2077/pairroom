package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

type MockAdapter struct {
	cfg       Config
	sink      EventSink
	lifecycle sync.Mutex
	mu        sync.Mutex
	state     model.AgentState
	queue     chan model.AgentInput
	cancel    context.CancelFunc
	turn      context.CancelFunc
	done      chan struct{}
}

func NewMock(cfg Config, sink EventSink) *MockAdapter {
	if cfg.MockDelay <= 0 {
		cfg.MockDelay = 650 * time.Millisecond
	}
	return &MockAdapter{cfg: cfg, sink: sink, state: model.StateStopped, queue: make(chan model.AgentInput, 64)}
}

func (m *MockAdapter) Actor() model.ActorID { return m.cfg.Actor }
func (m *MockAdapter) SessionID() string {
	if sessionID := strings.TrimSpace(m.cfg.SessionID); sessionID != "" {
		return sessionID
	}
	return "mock-" + string(m.cfg.Actor)
}

func (m *MockAdapter) State() model.AgentState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *MockAdapter) setState(state model.AgentState) {
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
	e := runtimeEvent(m.cfg.Actor, model.RuntimeState)
	e.State = state
	m.sink(e)
}

func (m *MockAdapter) Start(ctx context.Context) error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.mu.Lock()
	if m.state != model.StateStopped && m.state != model.StateError {
		m.mu.Unlock()
		return nil
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.cancel = cancel
	m.done = done
	m.state = model.StateStarting
	m.mu.Unlock()
	m.setState(model.StateIdle)
	info := model.RuntimeInfo{
		Available: true, Command: "mock", Protocol: "pairroom-mock", Version: version.Current,
		Model: "deterministic-mock", Capabilities: []string{"queued-input", "interrupt", "tool-events"}, ProbedAt: time.Now().UTC(),
	}
	emitRuntimeInfo(m.sink, m.cfg.Actor, info)
	sessionEvent := runtimeEvent(m.cfg.Actor, model.RuntimeSession)
	sessionEvent.SessionID = m.SessionID()
	m.sink(sessionEvent)
	go func() {
		defer close(done)
		m.loop(workerCtx)
	}()
	return nil
}

func (m *MockAdapter) StartTurn(ctx context.Context, input model.AgentInput) error {
	if err := m.Start(ctx); err != nil {
		return err
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if state := m.State(); state == model.StateWorking || state == model.StateWaiting || len(m.queue) > 0 {
		return errors.New("mock runtime already has an active turn")
	}
	select {
	case m.queue <- input:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *MockAdapter) Steer(context.Context, model.AgentInput) SteerOutcome {
	return SteerOutcome{State: SteerUnavailable, Detail: "mock runtime does not inject into an active turn"}
}

func (m *MockAdapter) loop(ctx context.Context) {

next:
	for {
		select {
		case <-ctx.Done():
			m.setState(model.StateStopped)
			return
		case input := <-m.queue:
			turnCtx, cancelTurn := context.WithCancel(ctx)
			m.mu.Lock()
			m.turn = cancelTurn
			m.mu.Unlock()
			m.setState(model.StateWorking)
			turnID := model.NewID("mock-turn")
			started := runtimeEvent(m.cfg.Actor, model.RuntimeTurnStarted)
			started.TurnID = turnID
			started.CorrelationID = input.MessageID
			m.sink(started)
			processing := runtimeEvent(m.cfg.Actor, model.RuntimeInputProcessing)
			processing.TurnID = turnID
			processing.CorrelationID = input.MessageID
			processing.Name = string(model.ProcessingWorking)
			processing.Text = "accepted by mock runtime"
			m.sink(processing)

			response := m.response(input)
			for _, chunk := range chunks(response, 18) {
				select {
				case <-turnCtx.Done():
					cancelTurn()
					m.clearTurn()
					if ctx.Err() != nil {
						m.emitInterrupted(turnID, input.MessageID, "mock runtime was stopped")
						m.setState(model.StateStopped)
						return
					}
					m.emitInterrupted(turnID, input.MessageID, "interrupted by user")
					m.setState(model.StateIdle)
					continue next
				case <-time.After(m.cfg.MockDelay / 8):
				}
				delta := runtimeEvent(m.cfg.Actor, model.RuntimeTextDelta)
				delta.TurnID = turnID
				delta.CorrelationID = input.MessageID
				delta.Text = chunk
				m.sink(delta)
			}
			select {
			case <-turnCtx.Done():
				cancelTurn()
				m.clearTurn()
				if ctx.Err() != nil {
					m.emitInterrupted(turnID, input.MessageID, "mock runtime was stopped")
					m.setState(model.StateStopped)
					return
				}
				m.emitInterrupted(turnID, input.MessageID, "interrupted by user")
				m.setState(model.StateIdle)
				continue next
			case <-time.After(m.cfg.MockDelay):
			}
			final := runtimeEvent(m.cfg.Actor, model.RuntimeFinal)
			final.TurnID = turnID
			final.CorrelationID = input.MessageID
			final.Text = response
			m.sink(final)
			completed := runtimeEvent(m.cfg.Actor, model.RuntimeTurnCompleted)
			completed.TurnID = turnID
			completed.CorrelationID = input.MessageID
			m.sink(completed)
			inputCompleted := runtimeEvent(m.cfg.Actor, model.RuntimeInputCompleted)
			inputCompleted.TurnID = turnID
			inputCompleted.CorrelationID = input.MessageID
			inputCompleted.Name = string(model.ProcessingCompleted)
			m.sink(inputCompleted)
			cancelTurn()
			m.clearTurn()
			m.setState(model.StateIdle)
		}
	}
}

func (m *MockAdapter) clearTurn() {
	m.mu.Lock()
	if m.turn != nil {
		// Function values cannot be compared. Clearing the current value is safe:
		// only the single mock worker installs turn cancellation functions.
		m.turn = nil
	}
	m.mu.Unlock()
}

func (m *MockAdapter) emitInterrupted(turnID, correlationID, detail string) {
	inputCancelled := runtimeEvent(m.cfg.Actor, model.RuntimeInputCancelled)
	inputCancelled.TurnID = turnID
	inputCancelled.CorrelationID = correlationID
	inputCancelled.Name = string(model.ProcessingCancelled)
	inputCancelled.Text = detail
	m.sink(inputCancelled)
	e := runtimeEvent(m.cfg.Actor, model.RuntimeTurnCompleted)
	e.TurnID = turnID
	e.CorrelationID = correlationID
	e.Name = "interrupted"
	m.sink(e)
}

func (m *MockAdapter) response(input model.AgentInput) string {
	body := strings.TrimSpace(input.Text)
	if len(body) > 140 {
		body = body[:140] + "…"
	}
	if m.cfg.Actor == model.ActorClaude {
		if input.From == model.ActorUser && mockShouldRelay(input.Text) {
			return fmt.Sprintf("我先从架构边界和风险入手。对“%s”，建议保持一个写入者、一个独立审查者，并把讨论消息与执行事件分层。%s 请重点检查并发控制、故障恢复和是否存在更简单的实现。", body, input.PeerHandle)
		}
		if input.From == model.ActorUser {
			return fmt.Sprintf("我先从架构边界和风险入手。对“%s”，建议保持一个写入者、一个独立审查者，并把讨论消息与执行事件分层。", body)
		}
		return "我接受其中关于状态竞态的提醒，但建议保留事件溯源，因为它能支撑断线重放和审计。下一步我会按用户指定的角色执行；当前没有必须继续追问对方的问题。"
	}
	if input.From == model.ActorUser && mockShouldRelay(input.Text) {
		return fmt.Sprintf("我会从可执行性和失败模式审查“%s”。初步意见：应优先实现稳定的消息投递、turn 关联和取消语义。%s 请说明你准备如何避免双写和无意义的互相唤醒。", body, input.PeerHandle)
	}
	if input.From == model.ActorUser {
		return fmt.Sprintf("我会从可执行性和失败模式审查“%s”。初步意见：应优先实现稳定的消息投递、turn 关联和取消语义。", body)
	}
	return "审查结论：采用单一房间事件日志、精确点名和每个 runtime 独立 session 是合理的。还需确保用户插话优先于待发送的 Agent 路由，并在进程重启后恢复 session ID。"
}

// mockShouldRelay keeps the deterministic fixture aligned with the v5
// convergence rule: a mock Agent only addresses its peer when the input is
// visibly collaborative (an explicit handle or a direct greeting request),
// rather than manufacturing an acknowledgement loop for every user message.
func mockShouldRelay(text string) bool {
	lower := strings.ToLower(text)
	for _, cue := range []string{
		"@", "introduce", "greet", "each other", "collaborat", "互相", "打个招呼", "介绍自己",
	} {
		if strings.Contains(lower, cue) {
			return true
		}
	}
	return false
}

func (m *MockAdapter) Interrupt(context.Context) error {
	m.mu.Lock()
	cancel := m.turn
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (m *MockAdapter) Stop(ctx context.Context) error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	turn := m.turn
	m.turn = nil
	done := m.done
	m.done = nil
	m.mu.Unlock()
	if turn != nil {
		turn()
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		select {
		case input := <-m.queue:
			cancelled := runtimeEvent(m.cfg.Actor, model.RuntimeInputCancelled)
			cancelled.CorrelationID = input.MessageID
			cancelled.Name = string(model.ProcessingCancelled)
			cancelled.Text = "mock runtime was stopped before processing began"
			m.sink(cancelled)
		default:
			m.setState(model.StateStopped)
			return nil
		}
	}
}

func (m *MockAdapter) ResolveApproval(context.Context, string, model.ApprovalResolution) error {
	return ErrApprovalUnsupported
}

func (m *MockAdapter) SetRole(context.Context, model.ParticipantRole) error { return nil }

func (m *MockAdapter) SetWorkspace(_ context.Context, workspace string) error {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return errors.New("mock workspace is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == model.StateWorking || m.state == model.StateWaiting || m.state == model.StateStarting {
		return errors.New("interrupt or stop mock agent before changing its workspace")
	}
	m.cfg.Repo = workspace
	return nil
}

func chunks(s string, n int) []string {
	if n <= 0 || len(s) <= n {
		return []string{s}
	}
	runes := []rune(s)
	out := make([]string, 0, (len(runes)+n-1)/n)
	for len(runes) > 0 {
		end := n
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[:end]))
		runes = runes[end:]
	}
	return out
}
