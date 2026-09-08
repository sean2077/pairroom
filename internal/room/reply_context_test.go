package room

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

func recordDetailedQuoteSource(t *testing.T, engine *Engine) model.Message {
	t.Helper()
	ancestor := model.Message{ID: model.NewID("msg"), From: model.ActorUser, Text: "unrelated ancestor must not be replayed", ThreadID: model.NewID("thread")}
	if _, err := engine.record(EventMessageCreated, ancestor.From, ancestor); err != nil {
		t.Fatal(err)
	}
	source := model.Message{
		ID: model.NewID("msg"), From: model.ActorCodex, ReplyTo: ancestor.ID, ThreadID: ancestor.ThreadID,
		Text: "@codex 引用内容\n```go\nanswer := 42\n```\n" + strings.Repeat("完整上下文 ", 400) + "\nfinal line",
	}
	event, err := engine.record(EventMessageCreated, source.From, source)
	if err != nil {
		t.Fatal(err)
	}
	source.Seq = event.Seq
	return source
}

func waitQuoteDelivery(t *testing.T, engine *Engine, id string, target model.ActorID, delivery model.DeliveryState, processing model.ProcessingState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, message := range engine.Snapshot().Messages {
			if message.ID == id && message.Delivery[target] == delivery && message.Processing[target] == processing {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("message %s did not reach %s/%s for %s", id, delivery, processing, target)
}

func assertNativeQuote(t *testing.T, input model.AgentInput, source, message model.Message) {
	t.Helper()
	if input.Text != message.Text || input.Quote == nil || input.Quote.Text != source.Text || input.Quote.FromHandle != "@codex" {
		t.Fatalf("native input omitted or changed the quote: %+v", input)
	}
	if input.ReplyTo != source.ID || input.MessageID != message.ID || input.ThreadID != source.ThreadID {
		t.Fatalf("quote changed transport correlation: %+v", input)
	}
	envelope := prompt.Envelope(input)
	if !strings.Contains(envelope, fmt.Sprintf("  text: %q\n", source.Text)) || strings.Count(envelope, "quoted_message:\n") != 1 || strings.Contains(envelope, source.ID) || strings.Contains(envelope, source.ReplyTo) || strings.Contains(envelope, "unrelated ancestor") {
		t.Fatal("model envelope lost quoted content or replayed transport IDs / recursive history")
	}
}

func TestUserQuoteReachesNativeInputs(t *testing.T) {
	for _, mode := range []string{"start", "steer", "queue", "steer-fallback", "restore-queue"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			engine, adapters := newTestEngine(t, dir)
			source := recordDetailedQuoteSource(t, engine)
			var seed model.Message
			if mode != "start" {
				var err error
				seed, err = engine.Send(context.Background(), SendRequest{Text: "active work", To: []model.ActorID{model.ActorClaude}})
				if err != nil {
					t.Fatal(err)
				}
				receiveInput(t, adapters[model.ActorClaude])
				waitQuoteDelivery(t, engine, seed.ID, model.ActorClaude, model.DeliveryStarted, model.ProcessingWorking)
			}
			if mode == "steer-fallback" {
				adapters[model.ActorClaude].mu.Lock()
				adapters[model.ActorClaude].steerOutcome = agent.SteerOutcome{State: agent.SteerUnavailable}
				adapters[model.ActorClaude].mu.Unlock()
			}
			intent := model.IntentSteer
			if mode == "queue" || mode == "restore-queue" {
				intent = model.IntentQueue
			}
			body := "Please check this quoted proposal."
			message, err := engine.Send(context.Background(), SendRequest{
				Text: body, To: []model.ActorID{model.ActorClaude}, ReplyTo: source.ID, Intent: intent,
			})
			if err != nil {
				t.Fatal(err)
			}
			if message.Text != body || len(message.To) != 1 || message.To[0] != model.ActorClaude {
				t.Fatal("quote changed the durable user body or routed a quoted mention")
			}
			if mode == "queue" || mode == "steer-fallback" || mode == "restore-queue" {
				waitQuoteDelivery(t, engine, message.ID, model.ActorClaude, model.DeliveryQueued, model.ProcessingWaiting)
				select {
				case <-adapters[model.ActorClaude].submissions:
					t.Fatal("queued quote bypassed the native turn boundary")
				default:
				}
				if mode == "restore-queue" {
					if err := engine.Close(); err != nil {
						t.Fatal(err)
					}
					engine, adapters = newTestEngine(t, dir)
				} else {
					engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorClaude, Kind: model.RuntimeTurnCompleted, CorrelationID: seed.ID})
				}
			}
			input := receiveInput(t, adapters[model.ActorClaude])
			assertNativeQuote(t, input, source, message)
			delivery := model.DeliveryStarted
			if mode == "steer" {
				delivery = model.DeliveryInjected
			}
			waitQuoteDelivery(t, engine, message.ID, model.ActorClaude, delivery, model.ProcessingWorking)
			for _, stored := range engine.Snapshot().Messages {
				if stored.ID == message.ID && stored.Text != body {
					t.Fatal("expanded native quote was written back into the Event Log projection")
				}
			}
		})
	}
}

func TestUserQuoteRetryAfterRestart(t *testing.T) {
	dir := t.TempDir()
	engine, adapters := newTestEngine(t, dir)
	source := recordDetailedQuoteSource(t, engine)
	message, err := engine.Send(context.Background(), SendRequest{Text: "Review it", To: []model.ActorID{model.ActorClaude}, ReplyTo: source.ID})
	if err != nil {
		t.Fatal(err)
	}
	first := receiveInput(t, adapters[model.ActorClaude])
	waitQuoteDelivery(t, engine, message.ID, model.ActorClaude, model.DeliveryStarted, model.ProcessingWorking)
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	engine, adapters = newTestEngine(t, dir)
	retry, err := engine.Retry(context.Background(), message.ID, RetryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	input := receiveInput(t, adapters[model.ActorClaude])
	assertNativeQuote(t, input, source, retry)
	if input.Text != first.Text || retry.RetryOf != message.ID || retry.Text != message.Text {
		t.Fatal("retry dropped, duplicated, or persisted the expanded quote")
	}
	waitQuoteDelivery(t, engine, retry.ID, model.ActorClaude, model.DeliveryStarted, model.ProcessingWorking)
}

func TestUserQuoteUsesFullRoomHistoryAndRuntimeIdentity(t *testing.T) {
	engine, _ := newTestEngine(t, "")
	source := recordDetailedQuoteSource(t, engine)
	if window := engine.WindowedSnapshot(1); len(window.Messages) != 1 {
		t.Fatal("invalid history fixture")
	}
	// One newer message puts the quote outside the browser's requested window.
	if _, err := engine.record(EventMessageCreated, model.ActorUser, model.Message{ID: model.NewID("msg"), From: model.ActorUser, Text: "newer"}); err != nil {
		t.Fatal(err)
	}
	if engine.WindowedSnapshot(1).Messages[0].ID == source.ID {
		t.Fatal("quote still appears in the browser window")
	}
	quote, _, err := engine.deliveryQuote(model.Message{From: model.ActorUser, ReplyTo: source.ID, Text: "review"})
	if err != nil || quote == nil || !strings.Contains(quote.Text, "final line") {
		t.Fatalf("older quoted source was omitted: %v", err)
	}

	for _, tc := range []struct {
		name   string
		first  model.RuntimeKind
		second model.RuntimeKind
		handle string
	}{
		{"duplicate Codex", model.RuntimeCodex, model.RuntimeCodex, "@codex1"},
		{"Grok in second slot", model.RuntimeClaude, model.RuntimeGrok, "@grok"},
		{"Claude in second slot", model.RuntimeGrok, model.RuntimeClaude, "@claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{cfg: Config{ClaudeConfig: agent.Config{Runtime: tc.first}, CodexConfig: agent.Config{Runtime: tc.second}}, snapshot: model.RoomSnapshot{Messages: []model.Message{source}}}
			quote, _, err := e.deliveryQuote(model.Message{From: model.ActorUser, ReplyTo: source.ID, Text: "review"})
			if err != nil || quote == nil || quote.FromHandle != tc.handle {
				t.Fatalf("quote used slot/vendor identity instead of runtime handle: %+v, %v", quote, err)
			}
		})
	}
}

func TestDeliveryQuoteDoesNotExpandOrdinaryInputOrAgentCorrelation(t *testing.T) {
	engine := &Engine{}
	for _, message := range []model.Message{
		{From: model.ActorUser, Text: "ordinary input"},
		{From: model.ActorClaude, ReplyTo: "transport-correlation", Text: "@codex full visible response"},
		{From: model.ActorCodex, ReplyTo: "transport-correlation", Text: "@claude another response"},
	} {
		quote, _, err := engine.deliveryQuote(message)
		if err != nil || quote != nil {
			t.Fatalf("ordinary input or Agent relay acquired quote history: %+v, %v", quote, err)
		}
	}
}

func TestMissingUserQuoteFailsBeforeNativeSubmission(t *testing.T) {
	engine, adapters := newTestEngine(t, "")
	other, _ := newTestEngine(t, "")
	foreign := recordDetailedQuoteSource(t, other)
	for _, id := range []string{"missing-message", foreign.ID} {
		_, err := engine.Send(context.Background(), SendRequest{Text: "Review it", To: []model.ActorID{model.ActorClaude}, ReplyTo: id})
		if err == nil {
			t.Fatal("missing or cross-Room quote was accepted")
		}
		if _, _, err := engine.deliveryQuote(model.Message{From: model.ActorUser, ReplyTo: id}); err == nil {
			t.Fatal("restored input with a missing or cross-Room quote was accepted")
		}
		select {
		case <-adapters[model.ActorClaude].submissions:
			t.Fatal("missing or cross-Room quote silently submitted a context-free input")
		default:
		}
	}
}

func TestUserQuoteAttachmentsKeepContentIdentity(t *testing.T) {
	image := model.Attachment{ID: "quoted-image", Name: "diagram.png", Kind: "image", MediaType: "image/png", SHA256: strings.Repeat("a", 64)}
	own := model.Attachment{ID: "own-image", Name: "comparison.png", Kind: "image", MediaType: "image/png", SHA256: strings.Repeat("b", 64)}
	media := &fakeAttachmentStore{metadata: map[string]model.Attachment{image.ID: image, own.ID: own}, paths: map[string]string{image.ID: "/private/diagram.png", own.ID: "/private/comparison.png"}}
	source := model.Message{ID: "source", From: model.ActorCodex, Attachments: []model.Attachment{image}}
	engine := &Engine{cfg: Config{Attachments: media}, snapshot: model.RoomSnapshot{Messages: []model.Message{source}}}
	message := model.Message{From: model.ActorUser, ReplyTo: source.ID, Text: "Compare these", Attachments: []model.Attachment{own, image}}
	quote, metadata, err := engine.deliveryQuote(message)
	if err != nil || quote == nil || quote.Text != "" || quote.FromHandle != "@codex" || len(metadata) != 2 || metadata[0].ID != own.ID || metadata[1].ID != image.ID {
		t.Fatalf("image-only quote was lost or duplicated: %+v, %+v, %v", quote, metadata, err)
	}
	// Also exercise an image that exists only on the quoted source: its
	// integrity must be checked even when the current message does not attach it.
	quoteOnly := message
	quoteOnly.Attachments = []model.Attachment{own}
	_, metadata, err = engine.deliveryQuote(quoteOnly)
	if err != nil || len(metadata) != 2 || metadata[1].ID != image.ID {
		t.Fatalf("quote-only image was omitted: %+v, %v", metadata, err)
	}
	native, err := engine.agentAttachments(metadata)
	if err != nil || len(native) != 2 || native[1].Path != media.paths[image.ID] {
		t.Fatalf("quoted image did not reach the native attachment boundary: %+v, %v", native, err)
	}
	serialized, err := json.Marshal(model.AgentInput{Text: message.Text, Quote: quote, Attachments: native})
	if err != nil || strings.Contains(string(serialized), "/private/") {
		t.Fatal("quoted image exposed a host-local path in JSON")
	}
	if len(source.Attachments) != 1 || len(message.Attachments) != 2 || message.Attachments[0].ID != own.ID {
		t.Fatal("native projection mutated durable attachments")
	}
	replaced := image
	replaced.SHA256 = strings.Repeat("c", 64)
	media.metadata[image.ID] = replaced
	if _, err := engine.agentAttachments(metadata); err == nil {
		t.Fatal("quoted image bypassed accepted-content integrity validation")
	}
}
