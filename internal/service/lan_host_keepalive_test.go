package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

type lanKeepAliveResponse struct {
	http.ResponseWriter
	ctx        context.Context
	interrupts *atomic.Int32
}

func (w lanKeepAliveResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w lanKeepAliveResponse) SetReadDeadline(deadline time.Time) error {
	err := http.NewResponseController(w.ResponseWriter).SetReadDeadline(deadline)
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		w.interrupts.Add(1)
		// Make an accidental successful-request interruption deterministic:
		// net/http's background read must observe the expired deadline before
		// a later reset can conceal this connection-cancelling race.
		if err == nil {
			select {
			case <-w.ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
	return err
}

func TestLANSuccessfulDownloadPreservesKeepAliveForConditionalClaim(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	var interrupts, claims atomic.Int32
	downloadHandled := make(chan struct{})
	controlled := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/download") {
			defer close(downloadHandled)
		}
		if strings.HasSuffix(r.URL.Path, "/claim") {
			claims.Add(1)
		}
		f.management.lanHost.serve(lanKeepAliveResponse{ResponseWriter: w, ctx: r.Context(), interrupts: &interrupts}, r)
	}))
	var err error
	controlled.TLS, err = lanshare.ServerTLS(f.management.lanHost.config.Identity)
	if err != nil {
		t.Fatal(err)
	}
	controlled.StartTLS()
	t.Cleanup(controlled.Close)
	// Keep the exact host certificate and Room authority; this second local
	// endpoint only makes a deadline interruption observable in the fixture.
	f.invite.Endpoint = controlled.URL
	client, pending, _ := f.join(t)
	admitted := f.accept(t, client, pending)
	content := "original reproduction evidence\n"
	object, err := f.native.media.SaveEvidence("keepalive.log", strings.NewReader(content), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	message, err := f.native.engine.Send(f.owner, relay.SendRequest{ID: "keepalive-evidence", Text: "inspect this evidence", AttachmentIDs: []string{object.ID}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var prepared struct {
		Head *relay.Prepared `json:"head"`
	}
	if err := lanshare.Call(ctx, client, f.invite, "head", map[string]int{"timeout_seconds": 1}, &prepared); err != nil || prepared.Head == nil || prepared.Head.Message.ID != message.ID {
		t.Fatalf("prepare evidence: %+v, %v", prepared.Head, err)
	}
	var connectionMu sync.Mutex
	var downloadConnections, claimConnections []httptrace.GotConnInfo
	trace := func(target *[]httptrace.GotConnInfo) *httptrace.ClientTrace {
		return &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			connectionMu.Lock()
			*target = append(*target, info)
			connectionMu.Unlock()
		}}
	}
	body, err := json.Marshal(map[string]string{"id": object.ID})
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace(&downloadConnections)), http.MethodPost, f.invite.Endpoint+"/lan/v1/rooms/"+f.room.ID+"/download", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(data) != content {
		t.Fatalf("download failed: HTTP%d %v", response.StatusCode, err)
	}
	select {
	case <-downloadHandled:
	case <-ctx.Done():
		t.Fatal("successful download cleanup did not finish")
	}
	if interrupts.Load() != 0 {
		t.Fatal("successful download expired the keepalive connection's read deadline")
	}
	var claimed struct {
		Claim *relay.StructuredClaim `json:"claim"`
	}
	claimCtx := httptrace.WithClientTrace(ctx, trace(&claimConnections))
	if err := lanshare.Call(claimCtx, client, f.invite, "claim", map[string]any{"id": prepared.Head.Message.ID, "digest": prepared.Head.Digest, "generation": admitted.Room.Generation}, &claimed); err != nil || claimed.Claim == nil || claimed.Claim.ID != message.ID {
		t.Fatalf("first conditional claim after download failed: %+v, %v", claimed.Claim, err)
	}
	connectionMu.Lock()
	defer connectionMu.Unlock()
	if len(downloadConnections) != 1 || len(claimConnections) != 1 || !claimConnections[0].Reused || downloadConnections[0].Conn != claimConnections[0].Conn || claims.Load() != 1 {
		t.Fatal("download required connection replacement or repeated conditional claim")
	}
	page, err := f.native.engine.History(relay.HistoryQuery{ID: message.ID})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].State != "delivering" {
		t.Fatal("claim was retried or acknowledged without original stdout confirmation")
	}
}
