package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// writeHelperPID lets a test find the real vendor stand-in process even when
// it runs behind a launcher shim.
func writeHelperPID() {
	if path := os.Getenv("PAIRROOM_HELPER_PID_FILE"); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
}

// holdAfterEOF models a vendor CLI that ignores a closed stdin, so only a kill
// can end it.
func holdAfterEOF() {
	if os.Getenv("PAIRROOM_HELPER_IGNORE_EOF") == "1" {
		time.Sleep(60 * time.Second)
	}
}

// runClaudeScriptHelper is a minimal stream-json Claude Code stand-in. It
// completes the control handshake and reacts to the first user message
// according to mode.
func runClaudeScriptHelper(mode string, args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("2.1.283 (Claude Code)")
		return 0
	}
	if len(args) == 1 && args[0] == "--help" {
		help := os.Getenv("PAIRROOM_CLAUDE_SCRIPT_HELP")
		if help == "" {
			help = "--input-format --output-format --session-id --resume --verbose"
		}
		fmt.Println(help)
		return 0
	}
	writeHelperPID()
	if path := os.Getenv("PAIRROOM_HELPER_ARGS_FILE"); path != "" {
		raw, _ := json.Marshal(args)
		_ = os.WriteFile(path, raw, 0o600)
	}
	defer holdAfterEOF()
	out := bufio.NewWriter(os.Stdout)
	emit := func(line string) {
		_, _ = out.WriteString(line + "\n")
		_ = out.Flush()
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		var request map[string]any
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		switch request["type"] {
		case "control_request":
			raw, _ := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": request["request_id"], "response": map[string]any{}}})
			emit(string(raw))
		case "user":
			session := fmt.Sprint(request["session_id"])
			switch mode {
			case "hang":
			case "oversized":
				emit(oversizedStdoutRecord())
			default:
				emit(`{"type":"system","subtype":"init","session_id":"` + session + `"}`)
				emit(`{"type":"result","subtype":"success","result":"ok","session_id":"` + session + `"}`)
			}
		}
	}
	return 0
}

// oversizedStdoutRecord exceeds every adapter's stdout record limit.
func oversizedStdoutRecord() string {
	return `{"type":"assistant","padding":"` + strings.Repeat("x", 17<<20) + `"}`
}
