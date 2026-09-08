package room

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

func recordQuoteSource(t *testing.T, engine *Engine, source model.Message) model.Message {
	t.Helper()
	source.ID = model.NewID("msg")
	source.ThreadID = model.NewID("thread")
	source.CreatedAt = time.Now().UTC()
	event, err := engine.record(EventMessageCreated, source.From, source)
	if err != nil {
		t.Fatal(err)
	}
	source.Seq = event.Seq
	return source
}

func TestUserQuoteReachesNativeInput(t *testing.T) {
	for _, test := range []struct {
		name  string
		owner model.ActorID
	}{
		{name: "new turn"},
		{name: "same target steer", owner: model.ActorCodex},
		{name: "Room FIFO", owner: model.ActorClaude},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine, adapters := newTestEngine(t, "")
			source := recordQuoteSource(t, engine, model.Message{
				From: model.ActorClaude,
				Text: strings.Repeat("完整原文 @claude\n", 512),
			})
			// Reserve a pre-existing owner to select the relevant scheduling path.
			engine.turnMu.Lock()
			engine.turnOwner = test.owner
			engine.turnMu.Unlock()
			sent, err := engine.Send(context.Background(), SendRequest{
				Text: "Review this", To: []model.ActorID{model.ActorCodex}, ReplyTo: source.ID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.owner == model.ActorClaude {
				select {
				case <-adapters[model.ActorCodex].submissions:
					t.Fatal("quoted message bypassed the Room FIFO")
				default:
				}
				engine.finishTurn(model.ActorClaude)
			}
			input := receiveInput(t, adapters[model.ActorCodex])
			if input.Quote == nil || input.Quote.Text != source.Text || input.Quote.FromHandle != "@claude" {
				t.Fatalf("quoted source did not reach native input: %+v", input.Quote)
			}
			if input.Text != sent.Text || input.To != model.ActorCodex || input.ReplyTo != source.ID {
				t.Fatalf("quote changed current text/target/correlation: %+v", input)
			}
			if !strings.Contains(prompt.Envelope(input), fmt.Sprintf("  text: %q\n", source.Text)) {
				t.Fatal("native envelope omitted the source text")
			}
			engine.mu.RLock()
			stored, found := engine.findMessageLocked(sent.ID)
			storedText := stored.Text
			engine.mu.RUnlock()
			if !found || storedText != "Review this" {
				t.Fatal("quote must not be baked into the durable user message")
			}
		})
	}
}

func TestRetryPreservesQuotedMessage(t *testing.T) {
	engine, adapters := newTestEngine(t, "")
	source := recordQuoteSource(t, engine, model.Message{From: model.ActorClaude, Text: "Original proposal"})
	original, err := engine.Send(context.Background(), SendRequest{
		Text: "Review this", To: []model.ActorID{model.ActorCodex}, ReplyTo: source.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorCodex])
	engine.processing(original.ID, model.ActorCodex, model.ProcessingFailed, "test failure", "")
	retried, err := engine.Retry(context.Background(), original.ID, RetryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	input := receiveInput(t, adapters[model.ActorCodex])
	if retried.ID == original.ID || retried.RetryOf != original.ID || input.ReplyTo != source.ID ||
		input.Quote == nil || input.Quote.Text != source.Text || input.Text != original.Text {
		t.Fatalf("retry lost quote or message identity: %+v", input)
	}
}

func TestMissingQuoteIsRejectedBeforePersistence(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			engine, adapters := newTestEngine(t, "")
			req := SendRequest{Text: "Review this", ReplyTo: "not-in-this-Room"}
			if explicit {
				req.To = []model.ActorID{model.ActorCodex}
			}
			if _, err := engine.Send(context.Background(), req); err == nil {
				t.Fatal("missing quote silently accepted")
			}
			engine.mu.RLock()
			count := len(engine.snapshot.Messages)
			engine.mu.RUnlock()
			if count != 0 {
				t.Fatal("invalid quoted input was persisted")
			}
			for _, adapter := range adapters {
				select {
				case <-adapter.submissions:
					t.Fatal("invalid quoted input reached a native adapter")
				default:
				}
			}
		})
	}
}

func TestDeliveryQuoteUsesRuntimeIdentityWithoutExpandingReplyChains(t *testing.T) {
	engine := &Engine{
		cfg: Config{
			ClaudeConfig: agent.Config{Runtime: model.RuntimeGrok},
			CodexConfig:  agent.Config{Runtime: model.RuntimeGrok},
		},
		snapshot: model.RoomSnapshot{Messages: []model.Message{{
			ID: "source", From: model.ActorClaude, Text: "Immediate source only", ReplyTo: "older-message",
		}}},
	}
	quote, _, err := engine.deliveryQuote(model.Message{From: model.ActorUser, ReplyTo: "source"})
	if err != nil || quote == nil || quote.FromHandle != "@grok0" || quote.Text != "Immediate source only" {
		t.Fatalf("wrong quote attribution or recursive expansion: %+v, %v", quote, err)
	}
	for _, from := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		quote, _, err := engine.deliveryQuote(model.Message{From: from, ReplyTo: "missing-correlation"})
		if err != nil || quote != nil {
			t.Fatalf("Agent relay correlation was treated as an explicit quote: %+v, %v", quote, err)
		}
	}
}

func TestDeliveryQuoteMergesMediaWithoutMutatingTranscript(t *testing.T) {
	engine := &Engine{snapshot: model.RoomSnapshot{Messages: []model.Message{{
		ID: "image-source", From: model.ActorUser,
		Attachments: []model.Attachment{{ID: "shared", Name: "shared.png"}, {ID: "quoted", Name: "quote.png"}},
	}}}}
	message := model.Message{
		From: model.ActorUser, ReplyTo: "image-source",
		Attachments: []model.Attachment{{ID: "shared", Name: "shared.png"}, {ID: "current", Name: "current.png"}},
	}
	quote, media, err := engine.deliveryQuote(message)
	if err != nil || quote == nil || quote.FromHandle != "@user" || quote.Text != "" {
		t.Fatalf("image-only quote missing: %+v, %v", quote, err)
	}
	if len(media) != 3 || media[0].ID != "shared" || media[1].ID != "current" || media[2].ID != "quoted" {
		t.Fatalf("quoted images missing or duplicated: %+v", media)
	}
	media[0].Name = "changed"
	media[2].Name = "changed"
	if message.Attachments[0].Name != "shared.png" || engine.snapshot.Messages[0].Attachments[1].Name != "quote.png" {
		t.Fatal("delivery media aliases durable message data")
	}
	if _, _, err := engine.deliveryQuote(model.Message{From: model.ActorUser, ReplyTo: "missing"}); err == nil {
		t.Fatal("restored input with an unavailable quote must fail, not silently lose context")
	}
}
