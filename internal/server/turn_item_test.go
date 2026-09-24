package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestTurnItemEvidenceEndpoint(t *testing.T) {
	server, engine := newTestServer(t, "")
	started := time.Now().UTC()
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot2, Kind: model.RuntimeTurnStarted, TurnID: "http-turn", CreatedAt: started})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot2, Kind: model.RuntimeToolCompleted, TurnID: "http-turn", ItemID: "call:1",
		Name: "Read", Text: "README.md contents", Data: json.RawMessage(`{"path":"README.md"}`), CreatedAt: started.Add(time.Second)})

	path := "/api/v1/turns/" + url.PathEscape("slot2:http-turn") + "/items/" + url.PathEscape("call:1")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, localRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Evidence []model.TurnItemEvidence `json:"evidence"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Evidence) != 1 || response.Evidence[0].Text != "README.md contents" || string(response.Evidence[0].Data) != `{"path":"README.md"}` {
		t.Fatalf("unexpected evidence: %s", recorder.Body.String())
	}

	missing := httptest.NewRecorder()
	server.Handler().ServeHTTP(missing, localRequest(http.MethodGet, "/api/v1/turns/slot2:http-turn/items/unknown", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown item status = %d", missing.Code)
	}
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		wrong := httptest.NewRecorder()
		server.Handler().ServeHTTP(wrong, localRequest(method, path, nil))
		if wrong.Code == http.StatusOK {
			t.Fatalf("%s was accepted on a read-only route", method)
		}
	}
}
