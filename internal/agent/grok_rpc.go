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

	"github.com/sean2077/pairroom/internal/model"
)

type grokRPCReply struct {
	result json.RawMessage
	err    error
}

type grokRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e grokRPCError) Error() string {
	if e.Code == 0 {
		return e.Message
	}
	return fmt.Sprintf("grok ACP error %d: %s", e.Code, e.Message)
}

func (g *GrokAdapter) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := g.nextRequestID.Add(1)
	reply := make(chan grokRPCReply, 1)
	g.mu.Lock()
	g.pending[id] = reply
	g.mu.Unlock()
	if err := g.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		g.mu.Lock()
		delete(g.pending, id)
		g.mu.Unlock()
		return nil, err
	}
	select {
	case response := <-reply:
		return response.result, g.redactError(response.err)
	case <-ctx.Done():
		g.mu.Lock()
		delete(g.pending, id)
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (g *GrokAdapter) redactError(err error) error {
	if err == nil {
		return nil
	}
	var rpcErr grokRPCError
	if errors.As(err, &rpcErr) {
		rpcErr.Message = redactRuntimeSecrets(rpcErr.Message, g.cfg.Env)
		return rpcErr
	}
	return errors.New(redactRuntimeSecrets(err.Error(), g.cfg.Env))
}

func (g *GrokAdapter) redactRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(redactRuntimeSecrets(string(raw), g.cfg.Env))
}

func (g *GrokAdapter) sendNotification(method string, params any) error {
	return g.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (g *GrokAdapter) sendRawResponse(id json.RawMessage, result any, rpcErr *grokRPCError) error {
	return g.send(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result,omitempty"`
		Error   *grokRPCError   `json:"error,omitempty"`
	}{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
}

func (g *GrokAdapter) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode Grok ACP message: %w", err)
	}
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	g.mu.Lock()
	stdin := g.stdin
	g.mu.Unlock()
	if stdin == nil {
		return errors.New("Grok ACP stdin is not available")
	}
	if _, err := stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write Grok ACP message: %w", err)
	}
	return nil
}

func (g *GrokAdapter) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), grokMaxStdoutLine)
	for scanner.Scan() {
		g.handleRPCLine(append([]byte(nil), scanner.Bytes()...))
	}
	if err := scanner.Err(); err != nil {
		// A dropped ACP response would strand its call, including the
		// session/prompt result that ends the Turn.
		g.failStream(streamFailureReason("Grok ACP", grokMaxStdoutLine, err))
	}
}

func (g *GrokAdapter) readStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = "grok.stderr"
		e.Text = redactRuntimeSecrets(text, g.cfg.Env)
		g.sink(e)
	}
}

func (g *GrokAdapter) handleRPCLine(line []byte) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *grokRPCError   `json:"error"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = "grok.stdout"
		e.Text = redactRuntimeSecrets(string(line), g.cfg.Env)
		g.sink(e)
		return
	}
	if envelope.Method != "" {
		if len(envelope.ID) > 0 && string(envelope.ID) != "null" {
			g.handleServerRequest(envelope.ID, envelope.Method, envelope.Params)
			return
		}
		g.handleNotification(envelope.Method, envelope.Params)
		return
	}
	id, err := strconv.ParseInt(strings.Trim(string(envelope.ID), `"`), 10, 64)
	if err != nil {
		return
	}
	g.mu.Lock()
	reply := g.pending[id]
	delete(g.pending, id)
	g.mu.Unlock()
	if reply == nil {
		return
	}
	if envelope.Error != nil {
		reply <- grokRPCReply{err: *envelope.Error}
	} else {
		reply <- grokRPCReply{result: append(json.RawMessage(nil), envelope.Result...)}
	}
}

func unwrapGrokExtParams(raw json.RawMessage) json.RawMessage {
	var wrapped struct {
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped.Params) > 0 {
		return append(json.RawMessage(nil), wrapped.Params...)
	}
	return append(json.RawMessage(nil), raw...)
}
