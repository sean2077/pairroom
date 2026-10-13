// Package codexwake reads Codex's persisted native input queue through a
// short-lived app-server. It never starts or resumes a thread, submits input,
// or changes that queue.
package codexwake

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"time"

	"github.com/sean2077/pairroom/internal/execx"
)

const (
	probeTimeout  = 5 * time.Second
	maxLineBytes  = 1 << 20
	maxTotalBytes = 4 << 20
	maxPages      = 16
)

// Pending reports whether the existing native queue contains a submission with
// exactly one text input equal to exactNudge. Errors mean the queue could not be established,
// never that it is empty. All errors are deliberately independent of queue
// contents, thread identity, executable paths, and native diagnostic output.
func Pending(ctx context.Context, executable, sessionID, exactNudge string) (bool, error) {
	if executable == "" || sessionID == "" || exactNudge == "" {
		return false, errors.New("codex wake queue: invalid probe configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return false, errors.New("codex wake queue: probe canceled")
	}
	// Preserve the environment, including CODEX_HOME, to read the same native
	// queue as the configured CLI. The only requests are initialize and queue/list.
	cmd := exec.Command(executable, "app-server", "--stdio")
	execx.NoConsole(cmd)
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false, errors.New("codex wake queue: transport unavailable")
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, errors.New("codex wake queue: transport unavailable")
	}
	defer stdout.Close()
	// stderr remains the null device: native diagnostics may contain private data.
	tree, err := execx.StartTree(cmd)
	if err != nil {
		return false, errors.New("codex wake queue: app-server unavailable")
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = stdin.Close()
			_ = stdout.Close()
			_ = tree.Kill()
		case <-stop:
		}
	}()
	defer func() {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = tree.Kill()
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			tree.Release()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()
	return inspect(ctx, stdin, stdout, sessionID, exactNudge)
}

type response struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func inspect(ctx context.Context, input io.Writer, output io.Reader, sessionID, nudge string) (bool, error) {
	scanner := bufio.NewScanner(io.LimitReader(output, maxTotalBytes+1))
	scanner.Buffer(make([]byte, 4096), maxLineBytes)
	encoder := json.NewEncoder(input)
	used := 0
	call := func(id int, method string, params any) (json.RawMessage, error) {
		if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return nil, errors.New("codex wake queue: request failed")
		}
		for scanner.Scan() {
			used += len(scanner.Bytes()) + 1
			if used > maxTotalBytes || ctx.Err() != nil {
				return nil, errors.New("codex wake queue: probe limit exceeded")
			}
			var reply response
			if err := json.Unmarshal(scanner.Bytes(), &reply); err != nil {
				return nil, errors.New("codex wake queue: invalid response")
			}
			if len(reply.ID) == 0 && reply.Method != "" {
				continue // Unrelated app-server notifications are never retained.
			}
			var replyID int
			if json.Unmarshal(reply.ID, &replyID) != nil || replyID != id || reply.Method != "" {
				return nil, errors.New("codex wake queue: unexpected response")
			}
			if len(reply.Error) != 0 && !bytes.Equal(reply.Error, []byte("null")) {
				return nil, errors.New("codex wake queue: request unavailable")
			}
			if len(reply.Result) == 0 || reply.Result[0] != '{' {
				return nil, errors.New("codex wake queue: invalid result")
			}
			return reply.Result, nil
		}
		return nil, errors.New("codex wake queue: incomplete response")
	}
	if _, err := call(1, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "pairroom_wake", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}); err != nil {
		return false, err
	}
	if err := encoder.Encode(map[string]string{"method": "initialized"}); err != nil {
		return false, errors.New("codex wake queue: initialization failed")
	}
	cursor := ""
	seen := make(map[string]bool)
	for page := range maxPages {
		params := map[string]any{"threadId": sessionID, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := call(page+2, "thread/queue/list", params)
		if err != nil {
			return false, err
		}
		pending, next, err := inspectPage(raw, nudge)
		if err != nil {
			return false, err
		}
		if pending {
			return true, nil
		}
		if next == "" {
			return false, nil
		}
		if seen[next] {
			return false, errors.New("codex wake queue: repeated cursor")
		}
		seen[next], cursor = true, next
	}
	return false, errors.New("codex wake queue: page limit exceeded")
}

func inspectPage(raw json.RawMessage, nudge string) (bool, string, error) {
	var page struct {
		Data []struct {
			ID                  *string `json:"id"`
			ClientUserMessageID *string `json:"clientUserMessageId"`
			Input               []struct {
				Type         string            `json:"type"`
				Text         *string           `json:"text"`
				TextElements []json.RawMessage `json:"text_elements"`
				URL          *string           `json:"url"`
				FileID       *string           `json:"fileId"`
				Path         *string           `json:"path"`
				Name         *string           `json:"name"`
			} `json:"input"`
		} `json:"data"`
		NextCursor json.RawMessage `json:"nextCursor"`
	}
	invalid := errors.New("codex wake queue: invalid queue page")
	if json.Unmarshal(raw, &page) != nil || page.Data == nil || len(page.NextCursor) == 0 {
		return false, "", invalid
	}
	var nextCursor *string
	if json.Unmarshal(page.NextCursor, &nextCursor) != nil || (nextCursor != nil && *nextCursor == "") {
		return false, "", invalid
	}
	found := false
	for _, item := range page.Data {
		if item.ID == nil || item.ClientUserMessageID == nil || len(item.Input) == 0 {
			return false, "", invalid
		}
		for _, input := range item.Input {
			valid := false
			switch input.Type {
			case "text":
				valid = input.Text != nil
				if valid && len(item.Input) == 1 && len(input.TextElements) == 0 && *input.Text == nudge {
					found = true
				}
			case "image":
				valid = input.URL != nil || input.FileID != nil
			case "localImage", "localAudio":
				valid = input.Path != nil
			case "audio":
				valid = input.URL != nil
			case "skill", "mention":
				valid = input.Name != nil && input.Path != nil
			}
			if !valid {
				return false, "", invalid
			}
		}
	}
	if nextCursor == nil {
		return found, "", nil
	}
	return found, *nextCursor, nil
}
