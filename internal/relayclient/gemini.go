package relayclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

// Gemini v0.62.0 keeps prompt_response cumulative when an AfterAgent hook
// requests continuation (core/client.ts, sendMessageStream/processTurn). Keep
// only a digest and byte offset of the already-retained prefix, never another
// copy of a private response. This cursor shares the publication's atomic WAL.
type GeminiResponseCursor struct {
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

const geminiEmptyResponse = "[no response text]"

func geminiResponse(hook HookInput, previous *GeminiResponseCursor) (string, *GeminiResponseCursor, error) {
	if hook.LastAssistantMessage == nil {
		return "", nil, errors.New("Gemini AfterAgent payload has no prompt_response")
	}
	full := *hook.LastAssistantMessage
	// Upstream synthesizes this marker when its cumulative buffer is empty,
	// but a model can also emit the same literal. There is no official field
	// that distinguishes them. Do not deliver a continuation whose prefix
	// would require guessing which happened; foreground wait remains usable.
	if hook.StopHookActive && ambiguousGeminiResponse(previous) {
		return "", nil, errors.New("Gemini empty-response marker is ambiguous; start a fresh user turn and use foreground relay wait")
	}
	text := full
	if hook.StopHookActive {
		if previous == nil || previous.Bytes < 0 || previous.Bytes > len(full) || geminiResponseDigest(full[:previous.Bytes]) != previous.SHA256 {
			return "", nil, errors.New("cannot isolate Gemini continuation from its retained response; publish the complete new reply explicitly with relay send, then start a fresh user turn")
		}
		text = full[previous.Bytes:]
		if text != "" && previous.Bytes > 0 {
			if !strings.HasPrefix(text, "\n") {
				return "", nil, errors.New("Gemini continuation is missing its response separator; no publication attempted")
			}
			text = text[1:]
		}
	}
	return text, &GeminiResponseCursor{Bytes: len(full), SHA256: geminiResponseDigest(full)}, nil
}

func ambiguousGeminiResponse(cursor *GeminiResponseCursor) bool {
	return cursor != nil && cursor.Bytes == len(geminiEmptyResponse) && cursor.SHA256 == geminiResponseDigest(geminiEmptyResponse)
}

func geminiResponseDigest(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// Gemini exposes session_id to hooks, not run_shell_command's environment.
// Its approved BeforeTool hook records only that official identity, scoped to
// the live host process instance. No transcript, model-visible nonce, command
// contents, credentials or cwd-based guessing is used for bind discovery.
// This is caller metadata, not authorization: existing private Binding and
// Service generation/session checks still apply, and AfterAgent confirms it.
type geminiCallerRecord struct {
	Session string    `json:"session_id"`
	Birth   string    `json:"process_birth"`
	Seen    time.Time `json:"seen_at"`
}

var geminiCallerNow = time.Now
var geminiProcessBirth = platformProcessBirth

const geminiCallerTTL = 2 * time.Minute

func validGeminiSession(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "/\\") && !strings.ContainsFunc(id, unicode.IsControl)
}

func geminiCallerPath(pid int, create bool) (string, error) {
	if pid <= 0 {
		return "", errors.New("invalid Gemini host process")
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	var dir string
	parts := []string{"pairroom", "relay-callers", "gemini"}
	if create {
		if err := os.MkdirAll(base, 0700); err != nil {
			return "", err
		}
		dir, err = secureDir(base, parts...)
	} else {
		dir, err = existingDir(base, parts...)
	}
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, strconv.Itoa(pid)+".json"), nil
}

func geminiHostName(name string) bool {
	switch strings.TrimSuffix(strings.ToLower(name), ".exe") {
	case "gemini", "node", "nodejs":
		return true
	default:
		return false
	}
}

// Keep a live, already-observed Gemini host recognizable after its identity
// expires, so inherited metadata from an outer harness cannot take over.
func geminiRecordForProcess(pid int, name string) (geminiCallerRecord, bool) {
	var record geminiCallerRecord
	if !geminiHostName(name) {
		return record, false
	}
	path, err := geminiCallerPath(pid, false)
	if err != nil || readPrivate(path, &record) != nil || !validGeminiSession(record.Session) {
		return record, false
	}
	birth, err := geminiProcessBirth(pid)
	return record, err == nil && birth != "" && birth == record.Birth
}

func geminiCallerForProcess(pid int, name string) string {
	record, ok := geminiRecordForProcess(pid, name)
	if !ok {
		return ""
	}
	age := geminiCallerNow().Sub(record.Seen)
	if age < 0 || age > geminiCallerTTL {
		return ""
	}
	return record.Session
}

func recordGeminiCaller(session string) error {
	if !validGeminiSession(session) {
		return errors.New("invalid Gemini hook session_id")
	}
	table, err := processTable()
	if err != nil {
		return err
	}
	pid := os.Getpid()
	for range 16 {
		row, ok := table[pid]
		if !ok {
			break
		}
		if geminiHostName(row.name) {
			birth, err := geminiProcessBirth(pid)
			if err != nil || birth == "" {
				return errors.New("cannot identify the live Gemini host instance")
			}
			path, err := geminiCallerPath(pid, true)
			if err != nil {
				return err
			}
			data, err := json.Marshal(geminiCallerRecord{Session: session, Birth: birth, Seen: geminiCallerNow()})
			if err != nil {
				return err
			}
			return atomicText(path, string(data)+"\n", 0600)
		}
		if _, other := harnessRuntimes[strings.TrimSuffix(strings.ToLower(row.name), ".exe")]; other {
			break
		}
		if row.ppid <= 0 || row.ppid == pid {
			break
		}
		pid = row.ppid
	}
	return errors.New("cannot locate the Gemini host; run PairRoom as a Gemini run_shell_command tool call")
}

// BeforeTool does not modify the tool input or native permission decision.
// Unrelated shell calls are inert. A failed identity observation is reported
// on stderr; bind then fails closed with setup guidance instead of guessing.
func geminiBeforeTool(data []byte, out, diagnostic io.Writer) (bool, error) {
	var input struct {
		Event   string `json:"hook_event_name"`
		Session string `json:"session_id"`
		Tool    string `json:"tool_name"`
		Input   struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if !utf8.Valid(data) || json.Unmarshal(data, &input) != nil || input.Event != "BeforeTool" {
		return false, nil
	}
	command := strings.ToLower(input.Input.Command)
	if input.Tool == "run_shell_command" && strings.Contains(command, "pairroom") && strings.Contains(command, "relay") {
		if err := recordGeminiCaller(input.Session); err != nil {
			fmt.Fprintln(diagnostic, "PairRoom Gemini caller metadata:", err)
		}
	}
	return true, writeJSON(out, map[string]any{})
}

func decodeGeminiHook(data []byte) (HookInput, error) {
	if !utf8.Valid(data) {
		return HookInput{}, errors.New("invalid UTF-8 in Gemini hook input")
	}
	var wire struct {
		HookInput
		Response *string `json:"prompt_response"`
	}
	if json.Unmarshal(data, &wire) != nil {
		return HookInput{}, errors.New("invalid Gemini hook input")
	}
	if wire.Event != "AfterAgent" {
		return HookInput{}, nil
	}
	if !validGeminiSession(wire.SessionID) {
		return HookInput{}, errors.New("official Gemini session_id is required")
	}
	if wire.LastAssistantMessage != nil {
		return HookInput{}, errors.New("unexpected non-Gemini reply field")
	}
	wire.Event = "Stop" // normalized response boundary, never a vendor Stop hook
	wire.LastAssistantMessage = wire.Response
	return wire.HookInput, nil
}

func hasGeminiBeforeTool(hooks map[string]any) bool {
	groups, _ := hooks["BeforeTool"].([]any)
	for _, raw := range groups {
		group, _ := raw.(map[string]any)
		matcher, _ := group["matcher"].(string)
		if matcher != "run_shell_command" {
			continue
		}
		entries, _ := group["hooks"].([]any)
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			async, _ := entry["async"].(bool)
			timeout, _ := entry["timeout"].(float64)
			if entry["type"] == "command" && entry["command"] == hookCommand(model.RuntimeGemini) && !async && timeout >= 45000 {
				return true
			}
		}
	}
	return false
}
