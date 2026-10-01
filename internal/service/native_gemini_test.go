package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

// Real Service/CLI transport, publication and durable receipts with official
// Gemini-shaped hook fixtures. No authenticated vendor process is involved.
func geminiNativeHTTP(t *testing.T) *nativeFixture {
	t.Helper()
	f := nativeHTTP(t)
	noSpawn := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
		t.Error("Gemini Native spawned an adapter")
		return Binding{}, nil, errors.New("must not spawn")
	})
	room, err := f.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: f.project.ID, Name: "Gemini pair", HostMode: model.HostNative,
		Agents: map[model.ActorID]model.AgentSelection{model.ActorSlot1: {Runtime: model.RuntimeGemini}, model.ActorSlot2: {Runtime: model.RuntimeGemini}},
	}, noSpawn)
	if err != nil {
		t.Fatal(err)
	}
	rt, _, err := f.manager.Activate(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.room, f.native = room, rt.(*nativeHostRuntime)
	return f
}

func bindGeminiFixture(t *testing.T, f *nativeFixture, slot model.ActorID) relay.Auth {
	t.Helper()
	// Caller discovery/create/bind is independently exercised in relayclient.
	// Provision exact, private local state here to isolate the hook HTTP protocol.
	a := relay.Auth{Slot: slot, BindID: "gemini-bind-" + string(slot), Secret: "gemini-private-" + string(slot), SessionID: "gemini-session-" + string(slot)}
	b, err := f.native.engine.Bind(slot, relay.BindRequest{BindID: a.BindID, CredentialHash: relay.Digest(a.Secret), SessionID: a.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	a.Generation = b.Generation
	if err := f.native.engine.Park(slot, false); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.project.Root, ".pairroom", "rooms", f.room.ID, "slots", string(slot))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state := relayclient.State{Schema: 2, Room: f.room.ID, Slot: slot, Runtime: model.RuntimeGemini, Workspace: f.project.Root, EndpointPath: f.endpoint, BindID: a.BindID, Generation: a.Generation, SessionID: a.SessionID}
	if err := relay.AtomicJSON(filepath.Join(dir, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	if err := relay.AtomicJSON(filepath.Join(dir, "credentials"), map[string]string{"bind_id": a.BindID, "secret": a.Secret}); err != nil {
		t.Fatal(err)
	}
	return a
}

func geminiHook(t *testing.T, f *nativeFixture, a relay.Auth, text string) ([]byte, error) {
	t.Helper()
	return f.run(t, []string{"hook", "--runtime", "gemini"}, map[string]any{"hook_event_name": "AfterAgent", "session_id": a.SessionID, "cwd": f.project.Root, "prompt_response": text, "stop_hook_active": false})
}

func TestGeminiNativePairFullReplyDeliveryAndReceipt(t *testing.T) {
	f := geminiNativeHTTP(t)
	first := bindGeminiFixture(t, f, model.ActorSlot1)
	second := bindGeminiFixture(t, f, model.ActorSlot2)
	body := "@gemini1\n  " + strings.Repeat("完整响应 🌟\n", 300)
	output, err := geminiHook(t, f, first, body)
	if err != nil {
		t.Fatalf("publish: %s %v", output, err)
	}
	messages := f.native.engine.Snapshot().Messages
	if len(messages) != 1 || messages[0].Text != body {
		t.Fatalf("full reply lost: %+v", messages)
	}
	if err := f.native.engine.Park(second.Slot, true); err != nil {
		t.Fatal(err)
	}
	output, err = geminiHook(t, f, second, "ready")
	if err != nil || !strings.Contains(string(output), `"decision":"block"`) || !strings.Contains(string(output), "完整响应") {
		t.Fatalf("delivery: %s %v", output, err)
	}
	for _, secret := range []string{first.Secret, second.Secret, "management-secret"} {
		if strings.Contains(string(output), secret) {
			t.Fatal("hook output leaked credentials")
		}
	}
	var delivered bool
	for _, m := range f.native.engine.Snapshot().Messages {
		if m.ID == messages[0].ID {
			delivered = m.State == "handed_off"
		}
	}
	if !delivered {
		t.Fatal("stdout delivery did not acknowledge exact inbox message")
	}
	if err := f.native.engine.Park(second.Slot, false); err != nil {
		t.Fatal(err)
	}
	if _, err = geminiHook(t, f, second, "@gemini0 return message"); err != nil {
		t.Fatal(err)
	}
	messages = f.native.engine.Snapshot().Messages
	if len(messages) != 2 || messages[1].Text != "@gemini0 return message" {
		t.Fatalf("reverse same-runtime routing lost: %+v", messages)
	}
	// A different official session is inert, not silently associated with this slot.
	wrong := first
	wrong.SessionID = "other-session"
	if _, err = geminiHook(t, f, wrong, "@gemini1 forged"); err != nil {
		t.Fatal(err)
	}
	if len(f.native.engine.Snapshot().Messages) != 2 {
		t.Fatal("wrong session published into the Room")
	}
	// Exact sessions survive runtime restart; no vendor process is spawned.
	if err := f.manager.Suspend(context.Background(), f.room.ID); err != nil {
		t.Fatal(err)
	}
	rt, _, err := f.manager.Activate(context.Background(), f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.native = rt.(*nativeHostRuntime)
	if binding, err := f.native.engine.Inspect(first); err != nil || binding.SessionID != first.SessionID {
		t.Fatalf("lost durable Gemini identity: %+v %v", binding, err)
	}
}

func TestGeminiNativeContinuationPublishesOnlyNewResponse(t *testing.T) {
	f := geminiNativeHTTP(t)
	first := bindGeminiFixture(t, f, model.ActorSlot1)
	second := bindGeminiFixture(t, f, model.ActorSlot2)
	if _, err := geminiHook(t, f, second, "@gemini0 queued input"); err != nil {
		t.Fatal(err)
	}
	if err := f.native.engine.Park(first.Slot, true); err != nil {
		t.Fatal(err)
	}
	addressed := "@gemini1 full response 🌟"
	output, err := geminiHook(t, f, first, addressed)
	if err != nil || !strings.Contains(string(output), `"decision":"block"`) {
		t.Fatalf("initial continuation: %s %v", output, err)
	}
	if err := f.native.engine.Park(first.Slot, false); err != nil {
		t.Fatal(err)
	}
	continued := func(text string) error {
		_, err := f.run(t, []string{"hook", "--runtime", "gemini"}, map[string]any{
			"hook_event_name": "AfterAgent", "session_id": first.SessionID, "cwd": f.project.Root,
			"prompt_response": text, "stop_hook_active": true,
		})
		return err
	}
	// Upstream v0.62.0 retains the original response when it invokes the hook
	// after a block. Its old handle must not route the new unaddressed reply.
	cumulative := addressed + "\nprivate follow-up"
	if err := continued(cumulative); err != nil {
		t.Fatal(err)
	}
	if messages := f.native.engine.Snapshot().Messages; len(messages) != 2 {
		t.Fatalf("old handle routed private continuation: %+v", messages)
	}
	cumulative += "\n" + addressed
	if err := continued(cumulative); err != nil {
		t.Fatal(err)
	}
	// Identical addressed text in a fresh turn must also publish again.
	if _, err := geminiHook(t, f, first, addressed); err != nil {
		t.Fatal(err)
	}
	messages := f.native.engine.Snapshot().Messages
	if len(messages) != 4 || messages[2].Text != addressed || messages[3].Text != addressed {
		t.Fatalf("repeated legitimate responses lost or cumulative text leaked: %+v", messages)
	}
	output, err = f.run(t, []string{"hook", "--runtime", "gemini"}, map[string]any{
		"hook_event_name": "AfterAgent", "session_id": first.SessionID, "cwd": f.project.Root,
		"prompt_response": "@gemini1 mismatching cumulative history", "stop_hook_active": true,
	})
	if err != nil || !strings.Contains(string(output), `"continue":false`) {
		t.Fatalf("unverifiable continuation did not stop the native turn: %s %v", output, err)
	}
	if len(f.native.engine.Snapshot().Messages) != 4 {
		t.Fatal("failed continuation changed Room messages")
	}
}

func TestGeminiNativeAmbiguousEmptyMarkerNeverCollects(t *testing.T) {
	f := geminiNativeHTTP(t)
	first := bindGeminiFixture(t, f, model.ActorSlot1)
	second := bindGeminiFixture(t, f, model.ActorSlot2)
	if _, err := geminiHook(t, f, second, "@gemini0 pending input"); err != nil {
		t.Fatal(err)
	}
	if err := f.native.engine.Park(first.Slot, true); err != nil {
		t.Fatal(err)
	}
	// Official synthetic fallback and an identical literal model reply are
	// indistinguishable. Both retain an ordinary unaddressed receipt, while
	// refusing to claim input that could lead to ambiguous prefix removal.
	for i := 0; i < 2; i++ {
		output, err := geminiHook(t, f, first, "[no response text]")
		if err != nil || !strings.Contains(string(output), `"continue":false`) {
			t.Fatalf("ambiguous boundary did not stop: %s %v", output, err)
		}
	}
	output, err := f.run(t, []string{"hook", "--runtime", "gemini"}, map[string]any{
		"hook_event_name": "AfterAgent", "session_id": first.SessionID, "cwd": f.project.Root,
		"prompt_response": "[no response text]\n@gemini1 legitimate literal-prefixed response", "stop_hook_active": true,
	})
	if err != nil || !strings.Contains(string(output), `"continue":false`) {
		t.Fatalf("ambiguous continuation was guessed: %s %v", output, err)
	}
	messages := f.native.engine.Snapshot().Messages
	if len(messages) != 1 || messages[0].State != "queued" {
		t.Fatalf("ambiguous hook consumed or published text: %+v", messages)
	}
	path := filepath.Join(f.project.Root, ".pairroom", "rooms", f.room.ID, "slots", string(first.Slot), "state.json")
	raw, err := os.ReadFile(path)
	var state relayclient.State
	if err != nil || json.Unmarshal(raw, &state) != nil || state.LastSeq != 2 || state.LastConfirmedSeq != 2 {
		t.Fatalf("unaddressed marker receipts were not retained: %+v %v", state, err)
	}
}

func TestGeminiNativeOversizedReplyStopsBeforeCollection(t *testing.T) {
	f := geminiNativeHTTP(t)
	first := bindGeminiFixture(t, f, model.ActorSlot1)
	second := bindGeminiFixture(t, f, model.ActorSlot2)
	if _, err := geminiHook(t, f, second, "@gemini0 pending input"); err != nil {
		t.Fatal(err)
	}
	if err := f.native.engine.Park(first.Slot, true); err != nil {
		t.Fatal(err)
	}
	output, err := geminiHook(t, f, first, "@gemini1 "+strings.Repeat("x", relay.MaxBodyBytes))
	if err != nil || !strings.Contains(string(output), `"continue":false`) {
		t.Fatalf("oversized boundary did not stop: %s %v", output, err)
	}
	messages := f.native.engine.Snapshot().Messages
	if len(messages) != 1 || messages[0].State != "queued" {
		t.Fatalf("oversized reply consumed or published input: %+v", messages)
	}
}
