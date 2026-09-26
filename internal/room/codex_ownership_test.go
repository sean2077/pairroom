package room

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/bus"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

// These tests run the real Embedded Codex adapter inside a Room against a
// scripted app-server subprocess (this test binary). The fixture reproduces
// legal JSON-RPC orderings; it does not certify a vendor release.

const fakeCodexHelperEnv = "PAIRROOM_ROOM_FAKE_CODEX"
const fakeCodexScenarioEnv = "PAIRROOM_ROOM_FAKE_CODEX_SCENARIO"

func TestMain(m *testing.M) {
	if os.Getenv(fakeCodexHelperEnv) == "1" {
		os.Exit(runFakeCodexAppServer(os.Args[1:], os.Getenv(fakeCodexScenarioEnv)))
	}
	os.Exit(m.Run())
}

func runFakeCodexAppServer(args []string, scenario string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex-cli 1.2.3")
		return 0
	}
	if len(args) >= 2 && args[0] == "app-server" && args[1] == "--help" {
		fmt.Println("Codex app-server")
		return 0
	}
	if len(args) == 0 || args[0] != "app-server" {
		return 97
	}
	encoder := json.NewEncoder(os.Stdout)
	notify := func(method string, params any) {
		_ = encoder.Encode(map[string]any{"method": method, "params": params})
	}
	turn := func(id, status, text string) map[string]any {
		value := map[string]any{"id": id, "status": status}
		if text != "" {
			value["items"] = []any{map[string]any{"type": "agentMessage", "phase": "final_answer", "text": text}}
		}
		return map[string]any{"turn": value}
	}
	turns := 0
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil || len(request.ID) == 0 || string(request.ID) == "null" {
			continue
		}
		var id any
		_ = json.Unmarshal(request.ID, &id)
		respond := func(result any) { _ = encoder.Encode(map[string]any{"id": id, "result": result}) }
		fail := func(code int, message string) {
			_ = encoder.Encode(map[string]any{"id": id, "error": map[string]any{"code": code, "message": message}})
		}
		switch request.Method {
		case "initialize":
			respond(map[string]any{})
		case "thread/start":
			respond(map[string]any{"thread": map[string]any{"id": "thread-fake"}})
		case "turn/start":
			turns++
			turnID := fmt.Sprintf("turn-%d", turns)
			if scenario == "stale-start" && turns == 1 {
				// A stale native turn finishes while turn/start is in flight.
				notify("turn/started", turn("turn-stale", "inProgress", ""))
				notify("turn/completed", turn("turn-stale", "completed", "stale answer"))
			}
			respond(map[string]any{"turn": map[string]any{"id": turnID}})
			notify("turn/started", turn(turnID, "inProgress", ""))
			if scenario == "steer-race" && turns == 1 {
				continue // stays active until the steer arrives
			}
			notify("turn/completed", turn(turnID, "completed", fmt.Sprintf("fresh answer %d", turns)))
		case "turn/steer":
			// The active turn finishes before Codex processes the steer, which
			// Codex then rejects because no turn is active.
			notify("turn/completed", turn(fmt.Sprintf("turn-%d", turns), "completed", fmt.Sprintf("fresh answer %d", turns)))
			fail(-32600, "no active turn")
		default:
			fail(-32601, "unsupported "+request.Method)
		}
	}
	return 0
}

func newFakeCodexEngine(t *testing.T, scenario string) *Engine {
	t.Helper()
	t.Setenv(fakeCodexHelperEnv, "1")
	eventStore, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{
		Name: "codex-ownership", Repo: t.TempDir(), Store: eventStore, Hub: bus.New(256),
		Settings:    model.RoomSettings{StallWarningSeconds: 300},
		Slot1Config: agent.Config{Runtime: model.RuntimeClaude},
		Slot2Config: agent.Config{
			Runtime: model.RuntimeCodex, Command: os.Args[0],
			Env: map[string]string{fakeCodexScenarioEnv: scenario},
		},
		Slot1Factory: func(cfg agent.Config, sink agent.EventSink) agent.Adapter {
			return &fakeAdapter{actor: cfg.Actor, sink: sink, state: model.StateStopped, submissions: make(chan model.AgentInput, 16)}
		},
		Slot2Factory: agent.CodexFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}

// waitForMessage polls the projection with a budget that covers launching the
// scripted app-server subprocess.
func waitForMessage(t *testing.T, engine *Engine, what string, match func(model.Message) bool) model.Message {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, message := range engine.Snapshot().Messages {
			if match(message) {
				return message
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	var lines []string
	for _, message := range engine.Snapshot().Messages {
		lines = append(lines, fmt.Sprintf("%s from=%s text=%q delivery=%v processing=%v turn=%v detail=%v", message.ID, message.From, message.Text, message.Delivery, message.Processing, message.ProcessingTurn, message.ProcessingDetail))
	}
	t.Fatalf("timed out waiting for %s; messages: %s", what, strings.Join(lines, " | "))
	return model.Message{}
}

func slot2Replies(engine *Engine, messageID string) []string {
	var texts []string
	for _, message := range engine.Snapshot().Messages {
		if message.ReplyTo == messageID && message.From == model.ActorSlot2 {
			texts = append(texts, message.Text)
		}
	}
	return texts
}

func TestCodexStaleTurnDuringStartDoesNotAnswerTheRoomMessage(t *testing.T) {
	engine := newFakeCodexEngine(t, "stale-start")
	first, err := engine.Send(context.Background(), SendRequest{Text: "first", To: []model.ActorID{model.ActorSlot2}})
	if err != nil {
		t.Fatal(err)
	}
	waitForMessage(t, engine, "first answer", func(m model.Message) bool {
		return m.ReplyTo == first.ID && m.Text == "fresh answer 1"
	})
	settled := waitForMessage(t, engine, "first settled", func(m model.Message) bool {
		return m.ID == first.ID && m.Processing[model.ActorSlot2].Terminal()
	})
	if settled.Processing[model.ActorSlot2] != model.ProcessingCompleted || settled.ProcessingTurn[model.ActorSlot2] != "turn-1" {
		t.Fatalf("first message = %s by turn %q", settled.Processing[model.ActorSlot2], settled.ProcessingTurn[model.ActorSlot2])
	}
	if got := slot2Replies(engine, first.ID); len(got) != 1 {
		t.Fatalf("replies to the first message = %q; a stale turn's answer was relayed", got)
	}

	second, err := engine.Send(context.Background(), SendRequest{Text: "second", To: []model.ActorID{model.ActorSlot2}})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForMessage(t, engine, "second settled", func(m model.Message) bool {
		return m.ID == second.ID && m.Processing[model.ActorSlot2].Terminal()
	})
	if done.Processing[model.ActorSlot2] != model.ProcessingCompleted {
		t.Fatalf("next FIFO item was not admitted: %s (%s)", done.Processing[model.ActorSlot2], done.ProcessingDetail[model.ActorSlot2])
	}
	waitForMessage(t, engine, "second answer", func(m model.Message) bool {
		return m.ReplyTo == second.ID && m.Text == "fresh answer 2"
	})
}
