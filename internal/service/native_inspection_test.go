package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func inspectionRequest(t *testing.T, f *nativeFixture, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+f.native.token)
	res := httptest.NewRecorder()
	f.native.http.Handler.ServeHTTP(res, req)
	return res
}
func TestNativeInspectionHistoryPendingReceiptAndDiagnosticHTTP(t *testing.T) {
	f := nativeHTTP(t)
	a := f.bind(t, "slot1")
	f.bind(t, "slot2")
	first, err := f.native.engine.Send(a, relay.SendRequest{ID: "old", Text: "private long task"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 301; i++ {
		if _, err = f.native.engine.Send(a, relay.SendRequest{ID: fmt.Sprint(i), Text: "human report", To: model.ActorUser}); err != nil {
			t.Fatal(err)
		}
	}
	seq := f.native.engine.Sequence()
	for _, path := range []string{"/api/v1/pending?limit=1", "/api/v1/history?id=" + first.ID} {
		res := inspectionRequest(t, f, path)
		var page relay.HistoryPage
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil || len(page.Messages) != 1 || page.Messages[0].ID != first.ID {
			t.Fatalf("%s: %d %s", path, res.Code, res.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/history?limit=101", "/api/v1/history?limit=1&limit=2", "/api/v1/pending?cursor=history:3", "/api/v1/history?since=bad"} {
		if res := inspectionRequest(t, f, path); res.Code != 409 {
			t.Fatalf("accepted %s: %d", path, res.Code)
		}
	}
	res := inspectionRequest(t, f, "/api/v1/sends/old")
	if !strings.Contains(res.Body.String(), `"found":false`) {
		t.Fatal("leaked peer client ID")
	}
	user, err := f.native.engine.SendUser(relay.SendRequest{ID: "user-id", Text: "user task", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	seq = f.native.engine.Sequence()
	res = inspectionRequest(t, f, "/api/v1/sends/user-id")
	if res.Code != 200 || !strings.Contains(res.Body.String(), user.ID) {
		t.Fatal(res.Body.String())
	}
	res = inspectionRequest(t, f, "/api/v1/diagnostics")
	if res.Code != 200 {
		t.Fatal(res.Code)
	}
	for _, secret := range []string{a.SessionID, a.Secret, f.native.token, first.Text, f.project.Root} {
		if secret != "" && strings.Contains(res.Body.String(), secret) {
			t.Fatalf("diagnostic leak: %s", secret)
		}
	}
	if !strings.Contains(res.Body.String(), `"hook_approval":"unknown"`) {
		t.Fatal("invented approvals")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pending", nil)
	res = httptest.NewRecorder()
	f.native.http.Handler.ServeHTTP(res, req)
	if res.Code != 401 {
		t.Fatal("missing auth accepted")
	}
	if f.native.engine.Sequence() != seq {
		t.Fatal("inspection changed log")
	}
}
func TestNativeHistoryDoctorAndReviewCLI(t *testing.T) {
	f := nativeHTTP(t)
	cmd := exec.Command("git", "-C", f.project.Root, "-c", "user.name=Fixture", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "fixture")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %s %v", output, err)
	}
	f.bind(t, "slot1")
	f.bind(t, "slot2")
	raw, err := f.run(t, []string{"send", "--room", f.room.ID, "--slot", "1", "--id", "review", "--text", "review only", "--review"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Published string `json:"published"`
	}
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		t.Fatal(err)
	}
	seq := f.native.engine.Sequence()
	page, err := f.run(t, []string{"history", "--room", f.room.ID, "--slot", "1", "--id", receipt.Published}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), `"dirty_sha256"`) || !strings.Contains(string(page), `"state":"queued"`) {
		t.Fatal(page)
	}
	check, err := f.run(t, []string{"review", "--room", f.room.ID, "--slot", "1", "--id", receipt.Published}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(check), `"status":"unchanged_observation"`) {
		t.Fatal(check)
	}
	report, err := f.run(t, []string{"doctor", "--room", f.room.ID, "--slot", "1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), `"hook_installation":"installed"`) || !strings.Contains(string(report), `"model_acceptance":"unknown"`) {
		t.Fatal(report)
	}
	if f.native.engine.Sequence() != seq {
		t.Fatal("read CLI changed durable log")
	}
}
func TestNativeInspectionGatewayAndManagementMode(t *testing.T) {
	f := nativeHTTP(t)
	f.bind(t, "slot1")
	body := bytes.NewBufferString(`{"mode":"native","room_id":"` + f.room.ID + `"}`)
	req, err := http.NewRequest(http.MethodPost, f.server.URL+diagnosticsPath, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer management-secret")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	for _, path := range []string{"/api/v1/history", "/api/v1/pending", "/api/v1/diagnostics", "/api/v1/review", "/api/v1/sends/original", "/_pairroom/richtext.js", "/_pairroom/native-outbox.js"} {
		if !allowedSurfaceRequest("GET", path) || allowedSurfaceRequest("POST", path) {
			t.Fatal("incorrect gateway boundary", path)
		}
	}

}
