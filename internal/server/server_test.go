package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/room"
	"github.com/sean2077/pairroom/internal/store"
)

func newTestServer(t *testing.T, token string) (*Server, *room.Engine) {
	t.Helper()
	repo := t.TempDir()
	dataDir := t.TempDir()
	eventStore, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	media, err := attachment.Open(dataDir, repo)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := room.New(room.Config{
		Name: "test room", Repo: repo, Store: eventStore,
		Settings:      model.RoomSettings{RoutingMode: model.RoutingManual, MaxHops: 3},
		ClaudeFactory: agent.MockFactory, CodexFactory: agent.MockFactory,
		ClaudeConfig: agent.Config{MockDelay: 5 * time.Millisecond},
		CodexConfig:  agent.Config{MockDelay: 5 * time.Millisecond},
		Attachments:  media,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	server, err := New(Config{Engine: engine, Repo: repo, Token: token, Attachments: media})
	if err != nil {
		t.Fatal(err)
	}
	return server, engine
}

func localRequest(method, target string, body *bytes.Buffer) *http.Request {
	var reader any
	if body != nil {
		reader = body
	}
	request := httptest.NewRequest(method, "http://127.0.0.1"+target, nil)
	if reader != nil {
		request = httptest.NewRequest(method, "http://127.0.0.1"+target, body)
	}
	return request
}

func TestHealthSnapshotAndMessageAPI(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "")

	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, localRequest(http.MethodGet, "/api/v1/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d: %s", health.Code, health.Body.String())
	}

	body := bytes.NewBufferString(`{"text":"Review the design","to":["claude","codex"]}`)
	send := httptest.NewRecorder()
	request := localRequest(http.MethodPost, "/api/v1/messages", body)
	request.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(send, request)
	if send.Code != http.StatusAccepted {
		t.Fatalf("message status = %d: %s", send.Code, send.Body.String())
	}
	var created model.Message
	if err := json.Unmarshal(send.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Seq == 0 || created.From != model.ActorUser {
		t.Fatalf("unexpected message: %#v", created)
	}

	snapshot := httptest.NewRecorder()
	server.Handler().ServeHTTP(snapshot, localRequest(http.MethodGet, "/api/v1/snapshot", nil))
	if snapshot.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d: %s", snapshot.Code, snapshot.Body.String())
	}
	var state model.RoomSnapshot
	if err := json.Unmarshal(snapshot.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) == 0 || state.Messages[0].ID != created.ID {
		t.Fatalf("message was not projected into snapshot: %#v", state.Messages)
	}
}

func TestRichConversationAssetsAreEmbedded(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "")

	index := httptest.NewRecorder()
	server.Handler().ServeHTTP(index, localRequest(http.MethodGet, "/", nil))
	if index.Code != http.StatusOK {
		t.Fatalf("index status = %d", index.Code)
	}
	for _, marker := range []string{"timeline-scope", "attachment-input", "image-lightbox", "/richtext.js"} {
		if !strings.Contains(index.Body.String(), marker) {
			t.Fatalf("index omitted rich-conversation marker %q", marker)
		}
	}

	rich := httptest.NewRecorder()
	server.Handler().ServeHTTP(rich, localRequest(http.MethodGet, "/richtext.js", nil))
	if rich.Code != http.StatusOK || !strings.Contains(rich.Body.String(), "createImage") || !strings.Contains(rich.Body.String(), "code-copy") {
		t.Fatalf("richtext asset is incomplete: status=%d", rich.Code)
	}

	app := httptest.NewRecorder()
	server.Handler().ServeHTTP(app, localRequest(http.MethodGet, "/app.js", nil))
	for _, marker := range []string{"threadFilter", "uploadPendingAttachment", "openLightbox", "renderClaudeQuestions", "initializeSession", "X-PairRoom-CSRF"} {
		if app.Code != http.StatusOK || !strings.Contains(app.Body.String(), marker) {
			t.Fatalf("app asset omitted %q: status=%d", marker, app.Code)
		}
	}
	if strings.Contains(app.Body.String(), "pairroom.token") || strings.Contains(app.Body.String(), "sessionStorage.setItem") {
		t.Fatal("browser asset must not persist the bootstrap token in Web Storage")
	}
}

func TestBearerTokenProtectsOnlyAPI(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "secret")

	asset := httptest.NewRecorder()
	server.Handler().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/", nil))
	if asset.Code != http.StatusOK {
		t.Fatalf("static asset should remain reachable: %d", asset.Code)
	}

	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", unauthorized.Code)
	}

	authorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	request.Header.Set("Authorization", "Bearer secret")
	server.Handler().ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK {
		t.Fatalf("valid token status = %d: %s", authorized.Code, authorized.Body.String())
	}
}

func TestQueryTokenNeverAuthorizesAPI(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "secret")

	for _, target := range []string{"/api/v1/snapshot?token=secret", "/api/v1/events?token=secret"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, target, nil)
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("query token must not authorize %s: status=%d body=%s", target, recorder.Code, recorder.Body.String())
		}
	}
}

func TestBrowserSessionAndCSRF(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "secret")

	bootstrap := httptest.NewRecorder()
	bootstrapRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/session", nil)
	bootstrapRequest.Header.Set("Authorization", "Bearer secret")
	server.Handler().ServeHTTP(bootstrap, bootstrapRequest)
	if bootstrap.Code != http.StatusCreated {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.CSRF == "" {
		t.Fatal("browser session omitted CSRF token")
	}
	cookies := bootstrap.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("bootstrap cookies=%d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != browserSessionCookie || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected session cookie: %#v", cookie)
	}

	snapshot := httptest.NewRecorder()
	snapshotRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/snapshot", nil)
	snapshotRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(snapshot, snapshotRequest)
	if snapshot.Code != http.StatusOK {
		t.Fatalf("session snapshot status=%d body=%s", snapshot.Code, snapshot.Body.String())
	}

	missingCSRF := httptest.NewRecorder()
	missingRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/messages", strings.NewReader(`{"text":"blocked","to":["claude"]}`))
	missingRequest.Header.Set("Content-Type", "application/json")
	missingRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(missingCSRF, missingRequest)
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s", missingCSRF.Code, missingCSRF.Body.String())
	}

	accepted := httptest.NewRecorder()
	acceptedRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/messages", strings.NewReader(`{"text":"accepted","to":["claude"]}`))
	acceptedRequest.Header.Set("Content-Type", "application/json")
	acceptedRequest.Header.Set(csrfHeaderName, session.CSRF)
	acceptedRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(accepted, acceptedRequest)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("valid CSRF status=%d body=%s", accepted.Code, accepted.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events := httptest.NewRecorder()
	eventsRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/events", nil).WithContext(ctx)
	eventsRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(events, eventsRequest)
	if events.Code != http.StatusOK || events.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("session SSE status=%d content-type=%q body=%s", events.Code, events.Header().Get("Content-Type"), events.Body.String())
	}

	logout := httptest.NewRecorder()
	logoutRequest := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1/api/v1/session", nil)
	logoutRequest.Header.Set(csrfHeaderName, session.CSRF)
	logoutRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(logout, logoutRequest)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout status=%d body=%s", logout.Code, logout.Body.String())
	}

	after := httptest.NewRecorder()
	afterRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/snapshot", nil)
	afterRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(after, afterRequest)
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status=%d body=%s", after.Code, after.Body.String())
	}
}

func TestCrossOriginRequestsAreRejected(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://pairroom.local/api/v1/snapshot", nil)
	request.Host = "pairroom.local"
	request.Header.Set("Origin", "https://attacker.example")
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", recorder.Code)
	}
}

func TestTokenlessServerRejectsNonLoopbackHost(t *testing.T) {
	t.Parallel()
	server, _ := newTestServer(t, "")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://attacker.example/api/v1/snapshot", nil)
	request.Host = "attacker.example"
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-loopback Host status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestLoopbackHostRecognition(t *testing.T) {
	for _, value := range []string{"localhost", "localhost:7332", "127.0.0.1", "127.0.0.1:7332", "[::1]", "[::1]:7332"} {
		if !isLoopbackRequestHost(value) {
			t.Errorf("expected loopback host %q", value)
		}
	}
	for _, value := range []string{"", "0.0.0.0:7332", "pairroom.local", "192.168.1.8:7332"} {
		if isLoopbackRequestHost(value) {
			t.Errorf("unexpected loopback host %q", value)
		}
	}
}

func TestSSECursorKeepsTransientEventsLive(t *testing.T) {
	last := uint64(12)
	write, next := advanceSSECursor(model.Event{Seq: 0}, last)
	if !write || next != last {
		t.Fatalf("transient event must be written without moving cursor: write=%v next=%d", write, next)
	}
	write, next = advanceSSECursor(model.Event{Seq: 12}, last)
	if write || next != last {
		t.Fatalf("durable duplicate must be filtered: write=%v next=%d", write, next)
	}
	write, next = advanceSSECursor(model.Event{Seq: 13}, last)
	if !write || next != 13 {
		t.Fatalf("new durable event must advance cursor: write=%v next=%d", write, next)
	}

	var output bytes.Buffer
	if err := writeSSE(&output, model.Event{Seq: 0, Kind: "runtime.event"}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("id:")) {
		t.Fatalf("transient event must not set an SSE replay id: %q", output.String())
	}
}

func TestRetryAndExportAPI(t *testing.T) {
	server, engine := newTestServer(t, "")

	send := httptest.NewRecorder()
	request := localRequest(http.MethodPost, "/api/v1/messages", bytes.NewBufferString(`{"text":"Inspect failure","to":["codex"]}`))
	request.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(send, request)
	if send.Code != http.StatusAccepted {
		t.Fatalf("message status = %d: %s", send.Code, send.Body.String())
	}
	var original model.Message
	if err := json.Unmarshal(send.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var accepted bool
		for _, message := range engine.Snapshot().Messages {
			if message.ID == original.ID && message.Delivery[model.ActorCodex] != model.DeliveryPending {
				accepted = true
				break
			}
		}
		if accepted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorCodex, Kind: model.RuntimeInputFailed,
		CorrelationID: original.ID, TurnID: "turn-test", Text: "synthetic failure",
		CreatedAt: time.Now().UTC(),
	})

	retry := httptest.NewRecorder()
	retryRequest := localRequest(http.MethodPost, "/api/v1/messages/"+original.ID+"/retry", bytes.NewBufferString(`{"to":["codex"]}`))
	retryRequest.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(retry, retryRequest)
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d: %s", retry.Code, retry.Body.String())
	}
	var retried model.Message
	if err := json.Unmarshal(retry.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.RetryOf != original.ID || retried.ID == original.ID {
		t.Fatalf("unexpected retry message: %#v", retried)
	}

	markdown := httptest.NewRecorder()
	server.Handler().ServeHTTP(markdown, localRequest(http.MethodGet, "/api/v1/export?format=markdown", nil))
	if markdown.Code != http.StatusOK || !strings.Contains(markdown.Body.String(), "# test room") || !strings.Contains(markdown.Body.String(), "Inspect failure") {
		t.Fatalf("unexpected markdown export: status=%d body=%q", markdown.Code, markdown.Body.String())
	}
	if disposition := markdown.Header().Get("Content-Disposition"); !strings.Contains(disposition, "test-room.md") {
		t.Fatalf("unexpected markdown filename: %q", disposition)
	}

	jsonExport := httptest.NewRecorder()
	server.Handler().ServeHTTP(jsonExport, localRequest(http.MethodGet, "/api/v1/export?format=json", nil))
	if jsonExport.Code != http.StatusOK {
		t.Fatalf("json export status = %d: %s", jsonExport.Code, jsonExport.Body.String())
	}
	var snapshot model.RoomSnapshot
	if err := json.Unmarshal(jsonExport.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) < 2 {
		t.Fatalf("json export omitted retry history: %#v", snapshot.Messages)
	}
	if len(snapshot.Events) != 0 {
		t.Fatalf("normal JSON export must omit Inspector event noise: %d events", len(snapshot.Events))
	}

	forensic := httptest.NewRecorder()
	server.Handler().ServeHTTP(forensic, localRequest(http.MethodGet, "/api/v1/export?format=json&include_events=1", nil))
	if forensic.Code != http.StatusOK {
		t.Fatalf("forensic export status = %d: %s", forensic.Code, forensic.Body.String())
	}
	var forensicSnapshot model.RoomSnapshot
	if err := json.Unmarshal(forensic.Body.Bytes(), &forensicSnapshot); err != nil {
		t.Fatal(err)
	}
	if len(forensicSnapshot.Events) == 0 {
		t.Fatal("forensic JSON export should retain the Inspector event tail")
	}
}

const serverTestPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func uploadImageRequest(t *testing.T, target, name string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := localRequest(http.MethodPost, target, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func testPNGBytes(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(serverTestPNG)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAttachmentUploadServeDeleteAndTranscriptReference(t *testing.T) {
	server, _ := newTestServer(t, "")

	upload := httptest.NewRecorder()
	server.Handler().ServeHTTP(upload, uploadImageRequest(t, "/api/v1/attachments", "diagram.png", testPNGBytes(t)))
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload status = %d: %s", upload.Code, upload.Body.String())
	}
	var image model.Attachment
	if err := json.Unmarshal(upload.Body.Bytes(), &image); err != nil {
		t.Fatal(err)
	}
	if image.ID == "" || image.MediaType != "image/png" || image.Width != 1 || image.Height != 1 || image.Size <= 0 {
		t.Fatalf("unexpected attachment metadata: %#v", image)
	}

	get := httptest.NewRecorder()
	server.Handler().ServeHTTP(get, localRequest(http.MethodGet, "/api/v1/attachments/"+image.ID, nil))
	if get.Code != http.StatusOK || get.Header().Get("Content-Type") != "image/png" || !bytes.Equal(get.Body.Bytes(), testPNGBytes(t)) {
		t.Fatalf("unexpected image response: status=%d type=%q bytes=%d", get.Code, get.Header().Get("Content-Type"), get.Body.Len())
	}
	etag := get.Header().Get("ETag")
	if etag == "" {
		t.Fatal("attachment response omitted ETag")
	}
	cached := httptest.NewRecorder()
	cachedRequest := localRequest(http.MethodGet, "/api/v1/attachments/"+image.ID, nil)
	cachedRequest.Header.Set("If-None-Match", etag)
	server.Handler().ServeHTTP(cached, cachedRequest)
	if cached.Code != http.StatusNotModified {
		t.Fatalf("conditional attachment status = %d", cached.Code)
	}

	payload, err := json.Marshal(map[string]any{
		"text": "Review this diagram", "to": []string{"claude"}, "attachments": []model.Attachment{image},
	})
	if err != nil {
		t.Fatal(err)
	}
	send := httptest.NewRecorder()
	request := localRequest(http.MethodPost, "/api/v1/messages", bytes.NewBuffer(payload))
	request.Header.Set("Content-Type", "application/json")
	server.Handler().ServeHTTP(send, request)
	if send.Code != http.StatusAccepted {
		t.Fatalf("send image message status = %d: %s", send.Code, send.Body.String())
	}
	var message model.Message
	if err := json.Unmarshal(send.Body.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	if len(message.Attachments) != 1 || message.Attachments[0].ID != image.ID {
		t.Fatalf("message omitted canonical image: %#v", message.Attachments)
	}

	deleteReferenced := httptest.NewRecorder()
	server.Handler().ServeHTTP(deleteReferenced, localRequest(http.MethodDelete, "/api/v1/attachments/"+image.ID, nil))
	if deleteReferenced.Code != http.StatusConflict {
		t.Fatalf("referenced image delete status = %d: %s", deleteReferenced.Code, deleteReferenced.Body.String())
	}

	markdown := httptest.NewRecorder()
	server.Handler().ServeHTTP(markdown, localRequest(http.MethodGet, "/api/v1/export?format=markdown", nil))
	if markdown.Code != http.StatusOK || !strings.Contains(markdown.Body.String(), "diagram.png") || !strings.Contains(markdown.Body.String(), image.ID) {
		t.Fatalf("markdown export omitted image metadata: %d %q", markdown.Code, markdown.Body.String())
	}

	uploadUnused := httptest.NewRecorder()
	server.Handler().ServeHTTP(uploadUnused, uploadImageRequest(t, "/api/v1/attachments", "unused.png", testPNGBytes(t)))
	if uploadUnused.Code != http.StatusCreated {
		t.Fatalf("unused upload status = %d: %s", uploadUnused.Code, uploadUnused.Body.String())
	}
	var unused model.Attachment
	if err := json.Unmarshal(uploadUnused.Body.Bytes(), &unused); err != nil {
		t.Fatal(err)
	}
	deleted := httptest.NewRecorder()
	server.Handler().ServeHTTP(deleted, localRequest(http.MethodDelete, "/api/v1/attachments/"+unused.ID, nil))
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("unused image delete status = %d: %s", deleted.Code, deleted.Body.String())
	}
	missing := httptest.NewRecorder()
	server.Handler().ServeHTTP(missing, localRequest(http.MethodGet, "/api/v1/attachments/"+unused.ID, nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("deleted image get status = %d", missing.Code)
	}
}

func TestAttachmentUploadRejectsNonImageAndSecurityHeadersAllowBlobPreview(t *testing.T) {
	server, _ := newTestServer(t, "")

	rejected := httptest.NewRecorder()
	server.Handler().ServeHTTP(rejected, uploadImageRequest(t, "/api/v1/attachments", "payload.txt", []byte("not an image")))
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("non-image upload status = %d: %s", rejected.Code, rejected.Body.String())
	}

	index := httptest.NewRecorder()
	server.Handler().ServeHTTP(index, localRequest(http.MethodGet, "/", nil))
	csp := index.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' data: blob:") {
		t.Fatalf("CSP does not allow authenticated blob previews: %q", csp)
	}

	traversal := httptest.NewRecorder()
	server.Handler().ServeHTTP(traversal, localRequest(http.MethodGet, "/api/v1/attachments/../../etc/passwd", nil))
	if traversal.Code == http.StatusOK {
		t.Fatal("path traversal unexpectedly served content")
	}
}

func TestWindowedSnapshotAndMessagePaginationAPI(t *testing.T) {
	t.Parallel()
	server, engine := newTestServer(t, "")
	for i := 0; i < 9; i++ {
		if _, err := engine.Send(context.Background(), room.SendRequest{
			Text: fmt.Sprintf("history-%02d", i), To: []model.ActorID{model.ActorClaude},
		}); err != nil {
			t.Fatal(err)
		}
	}

	windowRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(windowRecorder, localRequest(http.MethodGet, "/api/v1/snapshot?message_limit=3", nil))
	if windowRecorder.Code != http.StatusOK {
		t.Fatalf("window snapshot status = %d: %s", windowRecorder.Code, windowRecorder.Body.String())
	}
	var window model.RoomSnapshot
	if err := json.Unmarshal(windowRecorder.Body.Bytes(), &window); err != nil {
		t.Fatal(err)
	}
	if len(window.Messages) != 3 || window.MessageWindow == nil || window.MessageWindow.Total != 9 || !window.MessageWindow.HasMore {
		t.Fatalf("unexpected window snapshot: messages=%d window=%#v", len(window.Messages), window.MessageWindow)
	}

	pageRecorder := httptest.NewRecorder()
	target := fmt.Sprintf("/api/v1/messages?before_seq=%d&limit=4", window.Messages[0].Seq)
	server.Handler().ServeHTTP(pageRecorder, localRequest(http.MethodGet, target, nil))
	if pageRecorder.Code != http.StatusOK {
		t.Fatalf("message page status = %d: %s", pageRecorder.Code, pageRecorder.Body.String())
	}
	var page model.MessagePage
	if err := json.Unmarshal(pageRecorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 4 || page.Total != 9 || !page.HasMore {
		t.Fatalf("unexpected message page: %#v", page)
	}

	bad := httptest.NewRecorder()
	server.Handler().ServeHTTP(bad, localRequest(http.MethodGet, "/api/v1/messages?before_seq=nope", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor status = %d", bad.Code)
	}
}
