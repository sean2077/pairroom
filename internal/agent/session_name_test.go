package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// Records and responds to the real adapter's JSON-RPC transport, without a
// vendor process, storage write, or model call.
type nameRPCWriter struct {
	requests []map[string]any
	receive  func([]byte)
	respond  func(map[string]any) map[string]any
}

func (w *nameRPCWriter) Close() error { return nil }
func (w *nameRPCWriter) Write(data []byte) (int, error) {
	var req map[string]any
	if err := json.Unmarshal(data, &req); err != nil {
		return 0, err
	}
	w.requests = append(w.requests, req)
	reply := w.respond(req)
	reply["id"] = req["id"]
	raw, err := json.Marshal(reply)
	if err != nil {
		return 0, err
	}
	w.receive(raw)
	return len(data), nil
}
func nameInfo() model.RuntimeInfo {
	return model.RuntimeInfo{SessionName: "Name $(not-a-command) · @codex · 123456789abc", SessionNameStatus: "pending"}
}

func TestNativeSessionNameSyncIsOptionalBoundedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		err        error
	}{
		{"success", "synced", nil}, {"unsupported Codex", "unsupported", codexRPCError{Code: -32601}},
		{"unsupported Grok", "unsupported", grokRPCError{Code: -32601}}, {"invalid", "failed", errors.New("vendor diagnostic")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := syncSessionName(context.Background(), nameInfo(), func(ctx context.Context, name string) error {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Second {
					t.Fatal("metadata request not bounded")
				}
				if name != nameInfo().SessionName {
					t.Fatal("name changed")
				}
				return tc.err
			})
			if info.SessionNameStatus != tc.want || info.SessionName != nameInfo().SessionName || len(info.Warnings) != 0 {
				t.Fatalf("info=%+v", info)
			}
		})
	}
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		info := nameInfo()
		if cancelled {
			cancel()
		} else {
			info.SessionName = ""
		}
		syncSessionName(ctx, info, func(context.Context, string) error { t.Fatal("unneeded/cancelled metadata request"); return nil })
	}
	cfg := Config{Actor: model.ActorClaude, RoomName: "validation"}
	if got := configuredSessionName(cfg); got != "" {
		t.Fatalf("renamed before binding commit: %q", got)
	}
}

func TestCodexSessionNameUsesExactThreadMetadataRPC(t *testing.T) {
	for _, unsupported := range []bool{false, true} {
		a := NewCodex(Config{Actor: model.ActorCodex}, func(model.RuntimeEvent) {})
		a.threadID = "same-thread"
		w := &nameRPCWriter{receive: a.handleRPCLine, respond: func(map[string]any) map[string]any {
			if unsupported {
				return map[string]any{"error": map[string]any{"code": -32601, "message": "old CLI"}}
			}
			return map[string]any{"result": map[string]any{}}
		}}
		a.stdin = w
		info := a.syncSessionName(context.Background(), nameInfo(), a.threadID)
		want := "synced"
		if unsupported {
			want = "unsupported"
		}
		if info.SessionNameStatus != want || a.threadID != "same-thread" || len(w.requests) != 1 {
			t.Fatalf("sync=%+v requests=%v", info, w.requests)
		}
		req := w.requests[0]
		if req["method"] != "thread/name/set" || !reflect.DeepEqual(req["params"], map[string]any{"threadId": "same-thread", "name": nameInfo().SessionName}) {
			t.Fatalf("wrong naming transport: %#v", req)
		}
	}
}

func TestGrokSessionNameFallbackOnlyForMissingMethod(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		receipt bool
		calls   int
		status  string
	}{
		{"public", 0, true, 1, "synced"}, {"legacy", -32601, true, 2, "synced"},
		{"invalid", -32602, true, 1, "failed"}, {"negative receipt", 0, false, 1, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewGrok(Config{Actor: model.ActorClaude, Repo: "/actual workspace"}, func(model.RuntimeEvent) {})
			a.sessionID = "same-session"
			w := &nameRPCWriter{receive: a.handleRPCLine, respond: func(req map[string]any) map[string]any {
				if tc.code != 0 && req["method"] == "x.ai/session/rename" {
					return map[string]any{"error": map[string]any{"code": tc.code, "message": "rejected"}}
				}
				return map[string]any{"result": map[string]any{"success": tc.receipt}}
			}}
			a.stdin = w
			info := a.syncSessionName(context.Background(), nameInfo(), a.sessionID)
			if info.SessionNameStatus != tc.status || len(w.requests) != tc.calls || a.sessionID != "same-session" {
				t.Fatalf("info=%+v requests=%v", info, w.requests)
			}
			for i, req := range w.requests {
				method := "x.ai/session/rename"
				if i == 1 {
					method = "_x.ai/session/rename"
				}
				if req["method"] != method || !reflect.DeepEqual(req["params"], map[string]any{"sessionId": "same-session", "title": nameInfo().SessionName, "cwd": "/actual workspace"}) {
					t.Fatalf("wrong naming transport: %#v", req)
				}
			}
		})
	}
}

// This subprocess is a protocol fixture, not the official Claude executable.
func runClaudeNameHelper(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("claude 2.1.231")
		return 0
	}
	if len(args) == 1 && args[0] == "--help" {
		flags := "--input-format --output-format --session-id --resume --verbose"
		if os.Getenv("PAIRROOM_NAME_SUPPORTED") == "1" {
			flags += " --name"
		}
		fmt.Println(flags)
		return 0
	}
	raw, _ := json.Marshal(args)
	if os.WriteFile(os.Getenv("PAIRROOM_NAME_ARGS"), raw, 0600) != nil {
		return 94
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req map[string]any
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req["type"] != "control_request" {
			return 95
		}
		_ = encoder.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": req["request_id"], "response": map[string]any{}}})
	}
	return 0
}

func TestClaudeNameIsAnOptionalSingleArgOnTheSameSession(t *testing.T) {
	for _, tc := range []struct {
		supported, owned bool
		status           string
	}{
		{true, true, "configured"}, {false, true, "unsupported"}, {true, false, ""},
	} {
		t.Run(fmt.Sprint(tc.supported, tc.owned), func(t *testing.T) {
			t.Setenv("PAIRROOM_NAMES_HELPER", "claude")
			supported := "0"
			if tc.supported {
				supported = "1"
			}
			t.Setenv("PAIRROOM_NAME_SUPPORTED", supported)
			path := filepath.Join(t.TempDir(), "argv.json")
			t.Setenv("PAIRROOM_NAME_ARGS", path)
			cfg := Config{Actor: model.ActorClaude, Runtime: model.RuntimeClaude, PeerRuntime: model.RuntimeCodex, RoomName: "--help '名' $(touch never)", Command: os.Args[0], Repo: t.TempDir(), DataDir: t.TempDir(), SessionID: "same-native-session", RequireExactSession: true}
			if tc.owned {
				cfg.RoomID = "room-0123456789abcdef01234567"
			}
			var mu sync.Mutex
			var info model.RuntimeInfo
			a := NewClaude(cfg, func(event model.RuntimeEvent) {
				if event.Kind == model.RuntimeInfoUpdated {
					mu.Lock()
					defer mu.Unlock()
					_ = json.Unmarshal(event.Data, &info)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := a.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer a.Stop(context.Background())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var args []string
			if err = json.Unmarshal(raw, &args); err != nil {
				t.Fatal(err)
			}
			hasName := false
			hasResume := false
			for i, arg := range args {
				if strings.HasPrefix(arg, "--name=") {
					hasName = true
					if arg != "--name="+configuredSessionName(cfg) {
						t.Fatalf("name was parsed as command flags: %v", args)
					}
				}
				if arg == "--resume="+cfg.SessionID || (arg == "--resume" && i+1 < len(args) && args[i+1] == cfg.SessionID) {
					hasResume = true
				}
			}
			if hasName != (tc.supported && tc.owned) || !hasResume || a.SessionID() != cfg.SessionID {
				t.Fatalf("unexpected argv or session: %v", args)
			}
			mu.Lock()
			defer mu.Unlock()
			if info.SessionNameStatus != tc.status {
				t.Fatalf("name status=%q want=%q", info.SessionNameStatus, tc.status)
			}
		})
	}
}

func TestDisplayNamesDoNotEnterInstructions(t *testing.T) {
	cfg := Config{Actor: model.ActorClaude, RoomID: "room-123456789abc", RoomName: "Before"}
	before := collaborationPrompt(cfg)
	cfg.RoomName = "Different user label"
	if collaborationPrompt(cfg) != before {
		t.Fatal("display-name changes polluted stable instructions")
	}
}
