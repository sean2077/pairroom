package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEventStreamHEADDoesNotReplayOrWait(t *testing.T) {
	server, _ := newTestServer(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Even a closed probe must not replay a GET body for HEAD.
	req := httptest.NewRequest(http.MethodHead, "/api/v1/events", nil).WithContext(ctx)
	result := httptest.NewRecorder()
	server.events(result, req)
	if result.Code != http.StatusOK || result.Body.Len() != 0 {
		t.Fatalf("HEAD must return headers only: status=%d body=%s", result.Code, result.Body.String())
	}
	if result.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal("missing SSE headers")
	}
}
