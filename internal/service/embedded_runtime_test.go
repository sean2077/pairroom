package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/room"
)

func TestEmbeddedRuntimesIsolateRoomStateBindingsAndHTTPAuth(t *testing.T) {
	repo := testGitRepo(t)
	command := exec.Command("git", "-C", repo, "-c", "user.name=PairRoom Test", "-c", "user.email=pairroom@example.invalid", "commit", "--allow-empty", "-qm", "initial")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create test HEAD: %v: %s", err, output)
	}
	registry, project := testRegistry(t, repo)
	roomA, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Room A",
		Bindings:  specs(BindingNew, BindingNew, "room-a"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	roomB, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Room B",
		Bindings:  specs(BindingNew, BindingNew, "room-b"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}

	factory := EmbeddedRuntimeFactory(registry, EmbeddedRuntimeConfig{
		Mock:        true,
		RoutingMode: model.RoutingManual,
		Claude:      agent.Config{MockDelay: 10 * time.Millisecond},
		Codex:       agent.Config{MockDelay: 10 * time.Millisecond},
	})
	manager, err := NewRuntimeManager(registry, factory, RuntimeManagerConfig{
		Limit: 2, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Errorf("shutdown runtime manager: %v", err)
		}
	})

	// This context spans two runtime activations and the complete HTTP,
	// attachment, suspend, and restore flow below. Keep per-request client
	// timeouts tight, but allow slower Windows filesystem and process startup.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	valueA, _, err := manager.Activate(ctx, roomA.ID)
	if err != nil {
		t.Fatal(err)
	}
	valueB, _, err := manager.Activate(ctx, roomB.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeA, ok := valueA.(*embeddedRuntime)
	if !ok {
		t.Fatalf("Room A runtime type=%T", valueA)
	}
	runtimeB, ok := valueB.(*embeddedRuntime)
	if !ok {
		t.Fatalf("Room B runtime type=%T", valueB)
	}

	assertRoomBindings := func(t *testing.T, runtime *embeddedRuntime, durable Room) {
		t.Helper()
		snapshot := runtime.engine.Snapshot()
		if snapshot.Meta.ID != durable.ID {
			t.Fatalf("snapshot Room ID=%q, want %q", snapshot.Meta.ID, durable.ID)
		}
		for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
			if got, want := snapshot.Participants[actor].SessionID, durable.Bindings[actor].SessionID; got != want {
				t.Fatalf("%s session=%q, want durable binding %q", actor, got, want)
			}
		}
	}
	assertRoomBindings(t, runtimeA, roomA)
	assertRoomBindings(t, runtimeB, roomB)
	if roomA.Bindings[model.ActorClaude].SessionID == roomB.Bindings[model.ActorClaude].SessionID {
		t.Fatal("provisioned Rooms unexpectedly share a Claude binding")
	}

	if _, err := runtimeA.engine.Send(ctx, room.SendRequest{Text: "message-visible-only-in-room-a", To: []model.ActorID{model.ActorClaude}}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeB.engine.Send(ctx, room.SendRequest{Text: "message-visible-only-in-room-b", To: []model.ActorID{model.ActorCodex}}); err != nil {
		t.Fatal(err)
	}

	waitFor := func(t *testing.T, condition func() bool, description string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", description)
	}
	waitFor(t, func() bool { return !runtimeA.Busy() && !runtimeB.Busy() }, "mock turns to become idle")

	containsMessage := func(snapshot model.RoomSnapshot, marker string) bool {
		for _, message := range snapshot.Messages {
			if strings.Contains(message.Text, marker) {
				return true
			}
		}
		return false
	}
	snapshotA := runtimeA.engine.Snapshot()
	snapshotB := runtimeB.engine.Snapshot()
	if !containsMessage(snapshotA, "only-in-room-a") || containsMessage(snapshotA, "only-in-room-b") {
		t.Fatalf("Room A transcript leaked or lost data: %#v", snapshotA.Messages)
	}
	if !containsMessage(snapshotB, "only-in-room-b") || containsMessage(snapshotB, "only-in-room-a") {
		t.Fatalf("Room B transcript leaked or lost data: %#v", snapshotB.Messages)
	}
	if snapshotA.LatestSeq == 0 || snapshotB.LatestSeq == 0 {
		t.Fatalf("independent event sequences were not advanced: A=%d B=%d", snapshotA.LatestSeq, snapshotB.LatestSeq)
	}

	endpointA, tokenA := roomRuntimeEndpoint(t, runtimeA.URL(), "/api/v1/snapshot")
	endpointB, tokenB := roomRuntimeEndpoint(t, runtimeB.URL(), "/api/v1/snapshot")
	if tokenA == tokenB || tokenA == "" || tokenB == "" {
		t.Fatal("Room runtimes did not receive independent non-empty HTTP tokens")
	}
	// Role mutations refresh the reviewer Git worktree before committing the
	// event. That durable boundary can exceed two seconds on Windows while the
	// HTTP handler itself deliberately permits up to 45 seconds.
	client := &http.Client{Timeout: 30 * time.Second}
	readSnapshot := func(t *testing.T, endpoint, token string, wantStatus int) model.RoomSnapshot {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != wantStatus {
			t.Fatalf("GET %s status=%d, want %d", endpoint, response.StatusCode, wantStatus)
		}
		if wantStatus != http.StatusOK {
			return model.RoomSnapshot{}
		}
		var snapshot model.RoomSnapshot
		if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	if got := readSnapshot(t, endpointA, tokenA, http.StatusOK); got.Meta.ID != roomA.ID {
		t.Fatalf("Room A HTTP snapshot ID=%q", got.Meta.ID)
	}
	if got := readSnapshot(t, endpointB, tokenB, http.StatusOK); got.Meta.ID != roomB.ID {
		t.Fatalf("Room B HTTP snapshot ID=%q", got.Meta.ID)
	}
	_ = readSnapshot(t, endpointB, tokenA, http.StatusUnauthorized)
	_ = readSnapshot(t, endpointA, tokenB, http.StatusUnauthorized)

	// Participant roles are durable Room state. Changing Claude's role through
	// Room A's HTTP API must not mutate Room B even though both runtimes use the
	// same Project and loopback host.
	roleEndpointA, _ := roomRuntimeEndpoint(t, runtimeA.URL(), "/api/v1/participants/claude/role")
	roleRequest, err := http.NewRequestWithContext(ctx, http.MethodPut, roleEndpointA, strings.NewReader(`{"role":"peer"}`))
	if err != nil {
		t.Fatal(err)
	}
	roleRequest.Header.Set("Authorization", "Bearer "+tokenA)
	roleRequest.Header.Set("Content-Type", "application/json")
	roleResponse, err := client.Do(roleRequest)
	if err != nil {
		t.Fatal(err)
	}
	roleResponse.Body.Close()
	if roleResponse.StatusCode != http.StatusOK {
		t.Fatalf("change Room A Claude role status=%d, want %d", roleResponse.StatusCode, http.StatusOK)
	}
	if got := readSnapshot(t, endpointA, tokenA, http.StatusOK).Participants[model.ActorClaude].Role; got != model.RolePeer {
		t.Fatalf("Room A Claude role=%q, want %q", got, model.RolePeer)
	}
	if got := readSnapshot(t, endpointB, tokenB, http.StatusOK).Participants[model.ActorClaude].Role; got != model.RoleDriver {
		t.Fatalf("Room A role mutation leaked into Room B: Claude role=%q", got)
	}

	// Attachment stores are Room-scoped even when two runtimes serve the same
	// project on the same loopback host. An opaque attachment ID from Room A
	// must remain unknown to Room B.
	uploadA, _ := roomRuntimeEndpoint(t, runtimeA.URL(), "/api/v1/attachments")
	attachmentA := uploadRuntimeTestImage(t, ctx, client, uploadA, tokenA)
	readAttachment := func(t *testing.T, rawRuntimeURL, token, id string, wantStatus int) {
		t.Helper()
		endpoint, _ := roomRuntimeEndpoint(t, rawRuntimeURL, "/api/v1/attachments/"+url.PathEscape(id))
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != wantStatus {
			t.Fatalf("GET %s status=%d, want %d", endpoint, response.StatusCode, wantStatus)
		}
	}
	readAttachment(t, runtimeA.URL(), tokenA, attachmentA.ID, http.StatusOK)
	readAttachment(t, runtimeB.URL(), tokenB, attachmentA.ID, http.StatusNotFound)

	// SSE replay cursors and live subscriptions are also per-Room. Subscribe
	// after the existing tails, emit one durable event only in Room A, and
	// prove Room B's stream never carries Room A data.
	streamCtx, stopStreams := context.WithCancel(ctx)
	defer stopStreams()
	cursorA := runtimeA.engine.Snapshot().LatestSeq
	cursorB := runtimeB.engine.Snapshot().LatestSeq
	eventsA, streamErrA := subscribeRuntimeEvents(t, streamCtx, runtimeA.URL(), tokenA, cursorA)
	eventsB, streamErrB := subscribeRuntimeEvents(t, streamCtx, runtimeB.URL(), tokenB, cursorB)
	const sseMarker = "sse-visible-only-in-room-a"
	if _, err := runtimeA.engine.Send(ctx, room.SendRequest{Text: sseMarker, To: []model.ActorID{model.ActorClaude}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-eventsA:
			if event.RoomID != roomA.ID {
				t.Fatalf("Room A SSE emitted Room ID %q", event.RoomID)
			}
			if bytes.Contains(event.Data, []byte(sseMarker)) {
				goto observedRoomAEvent
			}
		case err := <-streamErrA:
			t.Fatalf("Room A SSE failed: %v", err)
		case <-deadline.C:
			t.Fatal("Room A SSE did not publish the marked Room event")
		}
	}

observedRoomAEvent:
	quiet := time.NewTimer(150 * time.Millisecond)
	defer quiet.Stop()
	for {
		select {
		case event := <-eventsB:
			// Room B may still emit its own asynchronous runtime state updates.
			// Those are not leakage; a foreign Room ID or Room A's message is.
			if event.RoomID != roomB.ID || bytes.Contains(event.Data, []byte(sseMarker)) {
				t.Fatalf("Room A event leaked into Room B SSE: %#v", event)
			}
		case err := <-streamErrB:
			t.Fatalf("Room B SSE failed while checking isolation: %v", err)
		case <-quiet.C:
			goto sseIsolationObserved
		}
	}

sseIsolationObserved:

	// Suspending and reactivating a Room must reopen the same durable Event Log
	// and native bindings rather than creating a fresh Claude Session or Codex
	// Thread. Stop the live SSE requests first and wait for the message used to
	// exercise SSE to settle so suspension tests the idle path deterministically.
	stopStreams()
	waitFor(t, func() bool { return !runtimeA.Busy() }, "Room A SSE turn to become idle")
	if err := manager.Suspend(ctx, roomA.ID); err != nil {
		t.Fatalf("suspend Room A: %v", err)
	}
	if status := manager.Status(roomA.ID); status.Phase != RuntimeSuspended {
		t.Fatalf("Room A status after suspend=%#v", status)
	}
	reopenedValue, _, err := manager.Activate(ctx, roomA.ID)
	if err != nil {
		t.Fatalf("reactivate Room A: %v", err)
	}
	reopened, ok := reopenedValue.(*embeddedRuntime)
	if !ok {
		t.Fatalf("reactivated Room A runtime type=%T", reopenedValue)
	}
	if reopened == runtimeA {
		t.Fatal("reactivation reused the closed runtime instead of rebuilding Room resources")
	}
	assertRoomBindings(t, reopened, roomA)
	reopenedSnapshot := reopened.engine.Snapshot()
	if got := reopenedSnapshot.Participants[model.ActorClaude].Role; got != model.RolePeer {
		t.Fatalf("reactivated Room Claude role=%q, want durable %q", got, model.RolePeer)
	}
	if !containsMessage(reopenedSnapshot, "only-in-room-a") || containsMessage(reopenedSnapshot, "only-in-room-b") {
		t.Fatalf("reactivated Room history leaked or was lost: %#v", reopenedSnapshot.Messages)
	}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		if got, want := reopenedSnapshot.Participants[actor].SessionID, roomA.Bindings[actor].SessionID; got != want {
			t.Fatalf("reactivated %s session=%q, want original binding %q", actor, got, want)
		}
	}
	reopenedAttachmentEndpoint, reopenedToken := roomRuntimeEndpoint(t, reopened.URL(), "/api/v1/attachments/"+url.PathEscape(attachmentA.ID))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, reopenedAttachmentEndpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+reopenedToken)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reactivated Room attachment status=%d, want %d", response.StatusCode, http.StatusOK)
	}
}

func uploadRuntimeTestImage(t *testing.T, ctx context.Context, client *http.Client, endpoint, token string) model.Attachment {
	t.Helper()
	var encoded bytes.Buffer
	canvas := image.NewRGBA(image.Rect(0, 0, 1, 1))
	canvas.Set(0, 0, color.RGBA{R: 0x42, G: 0x61, B: 0x7a, A: 0xff})
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "room-a.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(encoded.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s status=%d, want %d", endpoint, response.StatusCode, http.StatusCreated)
	}
	var value model.Attachment
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	if value.ID == "" {
		t.Fatal("upload returned an empty attachment ID")
	}
	return value
}

func subscribeRuntimeEvents(t *testing.T, ctx context.Context, rawRuntimeURL, token string, since uint64) (<-chan model.Event, <-chan error) {
	t.Helper()
	endpoint, _ := roomRuntimeEndpoint(t, rawRuntimeURL, "/api/v1/events")
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("since", fmt.Sprint(since))
	parsed.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("GET %s status=%d, want %d", parsed.String(), response.StatusCode, http.StatusOK)
	}
	events := make(chan model.Event, 16)
	errorsOut := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errorsOut)
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event model.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				errorsOut <- err
				return
			}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			errorsOut <- err
		}
	}()
	return events, errorsOut
}

func TestTranscriptBoundaryFilterDropsUncorrelatedVendorHistory(t *testing.T) {
	expectedSession := "durable-session"
	secret := "vendor-history-that-must-not-enter-the-room"
	for _, kind := range []string{
		model.RuntimeTextDelta,
		model.RuntimeFinal,
		model.RuntimeToolStarted,
		model.RuntimeCommandOutput,
		model.RuntimePlanUpdated,
		model.RuntimeDiffUpdated,
		model.RuntimeUsageUpdated,
		model.RuntimeApprovalRequested,
		model.RuntimeLog,
	} {
		if filtered, ok := filterTranscriptBoundaryEvent(expectedSession, model.RuntimeEvent{
			Agent: model.ActorClaude, Kind: kind, TurnID: "vendor-turn", Text: secret,
			Data: json.RawMessage(`{"transcript":"` + secret + `"}`),
		}); ok {
			t.Fatalf("uncorrelated %s event crossed transcript boundary: %#v", kind, filtered)
		}
	}

	if filtered, ok := filterTranscriptBoundaryEvent(expectedSession, model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeSession, SessionID: "wrong-session", Text: secret,
	}); ok {
		t.Fatalf("wrong native session crossed boundary: %#v", filtered)
	}
	filteredSession, ok := filterTranscriptBoundaryEvent(expectedSession, model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeSession, SessionID: expectedSession,
		Text: secret, Data: json.RawMessage(`{"transcript":"secret"}`),
	})
	if !ok || filteredSession.SessionID != expectedSession || filteredSession.Text != "" || len(filteredSession.Data) != 0 {
		t.Fatalf("safe session projection=%#v ok=%t", filteredSession, ok)
	}

	filteredInfo, ok := filterTranscriptBoundaryEvent(expectedSession, model.RuntimeEvent{
		Agent: model.ActorCodex, Kind: model.RuntimeInfoUpdated, Text: secret,
		Runtime: &model.RuntimeInfo{
			Available: true, Version: "1.2.3", Capabilities: []string{"resume"},
			Warnings: []string{secret}, Data: json.RawMessage(`{"raw":"secret"}`),
		},
		Data: json.RawMessage(`{"raw":"secret"}`),
	})
	if !ok || filteredInfo.Runtime == nil || filteredInfo.Runtime.Version != "1.2.3" || len(filteredInfo.Runtime.Warnings) != 0 || len(filteredInfo.Runtime.Data) != 0 || filteredInfo.Text != "" || len(filteredInfo.Data) != 0 {
		t.Fatalf("safe runtime-info projection=%#v ok=%t", filteredInfo, ok)
	}

	filteredError, ok := filterTranscriptBoundaryEvent(expectedSession, model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeError, Text: secret,
		Data: json.RawMessage(`{"stderr":"secret"}`),
	})
	if !ok || filteredError.Text != uncorrelatedRuntimeErrorNotice || strings.Contains(filteredError.Text, secret) || len(filteredError.Data) != 0 {
		t.Fatalf("sanitized runtime error=%#v ok=%t", filteredError, ok)
	}

	correlated := model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeFinal, CorrelationID: "room-message-1",
		TurnID: "vendor-turn", Text: "post-binding answer", Data: json.RawMessage(`{"post_binding":true}`),
	}
	filteredCorrelated, ok := filterTranscriptBoundaryEvent(expectedSession, correlated)
	if !ok || filteredCorrelated.CorrelationID != correlated.CorrelationID || filteredCorrelated.Text != correlated.Text || string(filteredCorrelated.Data) != string(correlated.Data) {
		t.Fatalf("correlated Room event was modified or dropped: %#v ok=%t", filteredCorrelated, ok)
	}
}

func roomRuntimeEndpoint(t *testing.T, rawURL, path string) (string, string) {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := url.ParseQuery(parsed.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	token := fragment.Get("token")
	parsed.Fragment = ""
	parsed.Path = path
	return parsed.String(), token
}

func TestRoomSessionCookieNameIsStableAndRoomScoped(t *testing.T) {
	first := roomSessionCookieName("room-a")
	if first == "" || first != roomSessionCookieName("room-a") {
		t.Fatalf("Room cookie name is not stable: %q", first)
	}
	if second := roomSessionCookieName("room-b"); second == first {
		t.Fatalf("different Rooms share cookie name %q", first)
	}
	if !strings.HasPrefix(first, "pairroom_session_") {
		t.Fatalf("unexpected Room cookie name %q", first)
	}
}

func TestDrainHandlerAllowsOnlySettlingControls(t *testing.T) {
	runtime := &embeddedRuntime{}
	runtime.SetDraining(true)
	reached := 0
	handler := runtime.drainHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		method string
		path   string
		allow  bool
	}{
		{http.MethodGet, "/api/v1/snapshot", true},
		{http.MethodPost, "/api/v1/session", true},
		{http.MethodPost, "/api/v1/approvals/approval-1", true},
		{http.MethodPost, "/api/v1/participants/claude/interrupt", true},
		{http.MethodPost, "/api/v1/messages/message-1/cancel", true},
		{http.MethodPost, "/api/v1/messages", false},
		{http.MethodPost, "/api/v1/messages/message-1/retry", false},
		{http.MethodPut, "/api/v1/settings", false},
		{http.MethodPost, "/api/v1/participants/claude/stop", false},
		{http.MethodPost, "/api/v1/approvals/approval-1/extra", false},
		{http.MethodDelete, "/api/v1/session", false},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			before := reached
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, nil)
			handler.ServeHTTP(recorder, request)
			if test.allow {
				if recorder.Code != http.StatusNoContent || reached != before+1 {
					t.Fatalf("settling control rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				return
			}
			if recorder.Code != http.StatusServiceUnavailable || reached != before {
				t.Fatalf("new mutation crossed drain gate: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestEmbeddedRuntimeCloseTimeoutIsRetryableAndDoesNotInterruptTurn(t *testing.T) {
	repo := testGitRepo(t)
	command := exec.Command("git", "-C", repo, "-c", "user.name=PairRoom Test", "-c", "user.email=pairroom@example.invalid", "commit", "--allow-empty", "-qm", "initial")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create test HEAD: %v: %s", err, output)
	}
	registry, project := testRegistry(t, repo)
	durable, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Drain Race Room",
		Bindings:  specs(BindingNew, BindingNew, "drain-race"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	value, err := startEmbeddedRuntime(context.Background(), nil, project, durable, EmbeddedRuntimeConfig{
		Mock:              true,
		RoutingMode:       model.RoutingManual,
		Claude:            agent.Config{MockDelay: 250 * time.Millisecond},
		Codex:             agent.Config{MockDelay: 250 * time.Millisecond},
		DrainPollInterval: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := value.(*embeddedRuntime)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = runtime.Close(ctx)
	})

	message, err := runtime.engine.Send(context.Background(), room.SendRequest{
		Text: "complete this turn without interruption",
		To:   []model.ActorID{model.ActorClaude},
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !runtime.Busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !runtime.Busy() {
		t.Fatal("mock turn never became busy")
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 15*time.Millisecond)
	err = runtime.Close(closeCtx)
	cancelClose()
	if !errors.Is(err, ErrRuntimeDrainAborted) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error=%v, want retryable drain timeout", err)
	}
	runtime.requestMu.Lock()
	stillOpen := !runtime.admissionClosed && !runtime.closeDraining
	runtime.requestMu.Unlock()
	if !stillOpen {
		t.Fatal("retryable Close crossed the irreversible admission boundary")
	}

	endpoint, token := roomRuntimeEndpoint(t, runtime.URL(), "/api/v1/snapshot")
	request := httptest.NewRequest(http.MethodGet, endpoint, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	runtime.http.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("Room View was not usable after retryable Close: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	deadline = time.Now().Add(10 * time.Second)
	for runtime.Busy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if runtime.Busy() {
		t.Fatal("mock turn did not finish naturally after Close timeout")
	}
	var persisted *model.Message
	for i := range runtime.engine.Snapshot().Messages {
		candidate := runtime.engine.Snapshot().Messages[i]
		if candidate.ID == message.ID {
			persisted = &candidate
			break
		}
	}
	if persisted == nil || persisted.Processing[model.ActorClaude] != model.ProcessingCompleted {
		t.Fatalf("Close timeout interrupted or lost the active turn: %#v", persisted)
	}

	finalCtx, cancelFinal := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelFinal()
	if err := runtime.Close(finalCtx); err != nil {
		t.Fatalf("retry Close after idle: %v", err)
	}
	if err := runtime.Close(finalCtx); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
}
