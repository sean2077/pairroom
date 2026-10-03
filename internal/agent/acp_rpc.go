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

type acpRPCReply struct {
	result json.RawMessage
	err    error
}

type acpRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e acpRPCError) Error() string {
	if e.Code == 0 {
		return e.Message
	}
	return fmt.Sprintf("ACP error %d: %s", e.Code, e.Message)
}

func (g *ACPAdapter) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := g.nextRequestID.Add(1)
	reply := make(chan acpRPCReply, 1)
	g.mu.Lock()
	g.pending[id] = reply
	g.mu.Unlock()
	if err := g.sendContext(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
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

func (g *ACPAdapter) redactError(err error) error {
	if err == nil {
		return nil
	}
	var rpcErr acpRPCError
	if errors.As(err, &rpcErr) {
		rpcErr.Message = redactRuntimeSecrets(rpcErr.Message, g.cfg.Env)
		return rpcErr
	}
	return errors.New(redactRuntimeSecrets(err.Error(), g.cfg.Env))
}

func (g *ACPAdapter) redactRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(redactRuntimeSecrets(string(raw), g.cfg.Env))
}

func (g *ACPAdapter) sendRawResponse(id json.RawMessage, result any, rpcErr *acpRPCError) error {
	return g.sendRawResponseContext(context.Background(), id, result, rpcErr)
}

func (g *ACPAdapter) sendRawResponseContext(ctx context.Context, id json.RawMessage, result any, rpcErr *acpRPCError) error {
	return g.sendContext(ctx, struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result,omitempty"`
		Error   *acpRPCError    `json:"error,omitempty"`
	}{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
}

func (g *ACPAdapter) send(value any) error {
	return g.sendContext(context.Background(), value)
}

func (g *ACPAdapter) sendContext(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode ACP message: %w", err)
	}
	if err := g.writer.write(ctx, func() io.WriteCloser {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.stdin
	}, append(data, '\n')); err != nil {
		return fmt.Errorf("write ACP message: %w", err)
	}
	return nil
}

func (g *ACPAdapter) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), grokMaxStdoutLine)
	for scanner.Scan() {
		g.handleRPCLine(append([]byte(nil), scanner.Bytes()...))
	}
	if err := scanner.Err(); err != nil {
		// A dropped ACP response would strand its call, including the
		// session/prompt result that ends the Turn.
		g.failStream(streamFailureReason("ACP", grokMaxStdoutLine, err))
	}
}

func (g *ACPAdapter) readStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = string(g.cfg.Runtime) + ".stderr"
		e.Text = redactRuntimeSecrets(text, g.cfg.Env)
		g.sink(e)
	}
	// Abandoning stderr can leave the child blocked on a full pipe forever.
	if err := scanner.Err(); err != nil {
		g.failStream(streamFailureReason("ACP stderr", 1024*1024, err))
	}
}

func (g *ACPAdapter) handleRPCLine(line []byte) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *acpRPCError    `json:"error"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		e := runtimeEvent(g.cfg.Actor, model.RuntimeLog)
		e.Name = string(g.cfg.Runtime) + ".stdout"
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
		reply <- acpRPCReply{err: *envelope.Error}
	} else {
		reply <- acpRPCReply{result: append(json.RawMessage(nil), envelope.Result...)}
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
