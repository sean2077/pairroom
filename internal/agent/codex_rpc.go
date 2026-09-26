package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

type rpcReply struct {
	result json.RawMessage
	err    error
}

type codexRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e codexRPCError) Error() string {
	if e.Code == 0 {
		return e.Message
	}
	return fmt.Sprintf("codex rpc error %d: %s", e.Code, e.Message)
}

func (c *CodexAdapter) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		result, err := c.callOnce(ctx, method, params)
		var rpcErr codexRPCError
		if err == nil || !errors.As(err, &rpcErr) || rpcErr.Code != -32001 || attempt >= 4 {
			return result, err
		}
		delay := time.Duration(100*(1<<attempt))*time.Millisecond + time.Duration(time.Now().UnixNano()%75)*time.Millisecond
		logEvent := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		logEvent.Name = "app-server.overloaded.retry"
		logEvent.Text = fmt.Sprintf("%s rejected as overloaded; retrying in %s (attempt %d/5)", method, delay, attempt+2)
		c.sink(logEvent)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *CodexAdapter) callOnce(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextRequestID.Add(1)
	ch := make(chan rpcReply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply.result, reply.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *CodexAdapter) notify(method string, params any) error {
	return c.send(map[string]any{"method": method, "params": params})
}

func (c *CodexAdapter) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode codex rpc message: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("codex stdin is not available")
	}
	if _, err := stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write codex rpc message: %w", err)
	}
	return nil
}

func (c *CodexAdapter) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		c.handleRPCLine(line)
	}
	if err := scanner.Err(); err != nil {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeError)
		e.Name = "adapter.stream_error"
		e.Text = "read Codex stream: " + err.Error()
		c.sink(e)
	}
}

func (c *CodexAdapter) readStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = "stderr"
		e.Text = text
		c.sink(e)
	}
}

func (c *CodexAdapter) handleRPCLine(line []byte) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *codexRPCError  `json:"error"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		e := runtimeEvent(c.cfg.Actor, model.RuntimeLog)
		e.Name = "stdout"
		e.Text = string(line)
		c.sink(e)
		return
	}
	if envelope.Method != "" {
		if len(envelope.ID) > 0 && string(envelope.ID) != "null" {
			c.handleServerRequest(envelope.ID, envelope.Method, envelope.Params)
			return
		}
		c.handleNotification(envelope.Method, envelope.Params)
		return
	}
	if len(envelope.ID) == 0 {
		return
	}
	var id int64
	if err := json.Unmarshal(envelope.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	hook := c.replyHooks[id]
	delete(c.replyHooks, id)
	_, abandoned := c.abandonedCalls[id]
	delete(c.abandonedCalls, id)
	c.mu.Unlock()
	if ch == nil {
		return
	}
	reply := rpcReply{result: envelope.Result}
	if envelope.Error != nil {
		reply = rpcReply{err: *envelope.Error}
	}
	if hook != nil {
		reply = hook(reply, abandoned)
	}
	ch <- reply
}

func (c *CodexAdapter) sendRawResponse(id json.RawMessage, result any, rpcErr *codexRPCError) error {
	message := struct {
		ID     json.RawMessage `json:"id"`
		Result any             `json:"result,omitempty"`
		Error  *codexRPCError  `json:"error,omitempty"`
	}{ID: id, Result: result, Error: rpcErr}
	return c.send(message)
}

func (c *CodexAdapter) failPendingRPCs(detail string) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan rpcReply)
	c.replyHooks = nil
	c.abandonedCalls = nil
	c.approvals = make(map[string]pendingApproval)
	c.mu.Unlock()

	err := errors.New(detail)
	for _, ch := range pending {
		select {
		case ch <- rpcReply{err: err}:
		default:
		}
	}
}

// ParseCodexRequestID is kept small and exported only for protocol tests.
func ParseCodexRequestID(raw json.RawMessage) (int64, error) {
	var id int64
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}
