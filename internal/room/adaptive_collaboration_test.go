package room

import (
	"context"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func TestDefaultCollaborationEitherMentionedAgentCanFinishDirectly(t *testing.T) {
	for _, actor := range model.SlotActors() {
		t.Run(string(actor), func(t *testing.T) {
			e, _, _ := newCollaborationEngine(t, "")
			current, _ := e.adapter(actor)
			other, _ := e.adapter(model.OtherParticipant(actor))
			target := current.(*fakeAdapter)
			peer := other.(*fakeAdapter)
			handle := e.Snapshot().Participants[actor].MentionHandle
			incoming, err := e.Send(context.Background(), SendRequest{Text: handle + " Fix this typo and verify the change."})
			if err != nil {
				t.Fatal(err)
			}
			input := receiveInput(t, target)
			if len(incoming.To) != 1 || incoming.To[0] != actor || input.From != model.ActorUser || input.To != actor {
				t.Fatalf("mention did not target the requested Agent: message=%+v input=%+v", incoming, input)
			}
			waitForDeliveryState(t, e, incoming.ID, actor, model.DeliveryStarted)
			const answer = "Corrected the typo; the focused check passed."
			e.HandleRuntimeEvent(model.RuntimeEvent{Agent: actor, Kind: model.RuntimeFinal, TurnID: "direct", CorrelationID: incoming.ID, Text: answer, CreatedAt: time.Now().UTC()})
			e.HandleRuntimeEvent(model.RuntimeEvent{Agent: actor, Kind: model.RuntimeTurnCompleted, TurnID: "direct", CorrelationID: incoming.ID, Name: "completed", CreatedAt: time.Now().UTC()})
			select {
			case got := <-peer.submissions:
				t.Fatalf("direct answer triggered delegation or ceremonial review: %+v", got)
			case <-time.After(150 * time.Millisecond):
			}
			snapshot := e.Snapshot()
			last := snapshot.Messages[len(snapshot.Messages)-1]
			if last.From != actor || last.Text != answer || len(last.To) != 1 || last.To[0] != model.ActorUser {
				t.Fatalf("direct result was not the final Room answer: %+v", last)
			}
			e.turnMu.Lock()
			owner, queued := e.turnOwner, len(e.turnQueue)
			e.turnMu.Unlock()
			if owner != "" || queued != 0 {
				t.Fatalf("direct completion left work behind: owner=%s queued=%d", owner, queued)
			}
		})
	}
}

func TestVersionOneCollaborationSurvivesReopenWithNewDefaults(t *testing.T) {
	for _, mode := range []string{model.CollaborationDefault, model.CollaborationCustom} {
		t.Run(mode, func(t *testing.T) {
			old := model.Collaboration{Version: 1, Mode: mode}
			if mode == model.CollaborationCustom {
				old.Instructions = "Agent 2 plans; Agent 1 reviews. 保留旧规则。"
			}
			old, err := old.ForCreation()
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			st, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			captures := &configurationCapture{}
			cfg := Config{Repo: t.TempDir(), Store: st, Collaboration: &old, ClaudeFactory: captures.factory, CodexFactory: captures.factory}
			original, err := New(cfg)
			if err != nil {
				_ = st.Close()
				t.Fatal(err)
			}
			if err := original.Close(); err != nil {
				t.Fatal(err)
			}
			current, err := (model.Collaboration{}).ForCreation()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Collaboration = &current
			cfg.Store, err = store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := New(cfg)
			if err != nil {
				_ = cfg.Store.Close()
				t.Fatal(err)
			}
			defer restored.Close()
			if err := restored.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := restored.Snapshot().Meta.Collaboration; got == nil || *got != old {
				t.Fatalf("reopen rewrote version-1 policy: %+v", got)
			}
			for _, actor := range model.SlotActors() {
				if got := captures.latest(actor).Collaboration; got == nil || *got != old {
					t.Fatalf("%s native projection lost version-1 policy: %+v", actor, got)
				}
			}
		})
	}
}
