package lanclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/relay"
)

// Observing the real response body's first Read makes the body-cancellation
// barrier deterministic: the transport has already returned its HTTP headers.
type evidenceReadTransport struct {
	base    http.RoundTripper
	started chan struct{}
	once    sync.Once
}

func (t *evidenceReadTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err == nil && strings.HasSuffix(request.URL.Path, "/download") {
		response.Body = evidenceReadBody{ReadCloser: response.Body, started: func() {
			t.once.Do(func() { close(t.started) })
		}}
	}
	return response, err
}

func (t *evidenceReadTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

type evidenceReadBody struct {
	io.ReadCloser
	started func()
}

func (b evidenceReadBody) Read(data []byte) (int, error) {
	b.started()
	return b.ReadCloser.Read(data)
}

func TestEvidenceCancellationPreservesCauseAndWaitIsEmpty(t *testing.T) {
	for _, operation := range []string{"download", "wait"} {
		t.Run(operation, func(t *testing.T) {
			for _, phase := range []string{"headers", "body"} {
				t.Run(phase, func(t *testing.T) {
					ctx := context.Background()
					s, f := newStore(t), newRemote(t)
					c, auth, options := f.join(t, s)
					message, content, _ := incomingFile(t)
					original, err := c.withRecord(ctx, func(r *record) error {
						r.Deliveries = []delivery{{ID: "older-unknown", Receipt: "older-original-receipt", Generation: auth.Generation, State: "unknown"}}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					before, err := os.ReadFile(filepath.Join(c.dir, "client.json"))
					if err != nil {
						t.Fatal(err)
					}
					started, exited, unblock := make(chan struct{}), make(chan struct{}), make(chan struct{})
					release := sync.OnceFunc(func() { close(unblock) })
					t.Cleanup(release)
					if phase == "body" {
						c.mu.Lock()
						c.http.Transport = &evidenceReadTransport{base: c.http.Transport, started: started}
						c.mu.Unlock()
					}
					f.setHandler(func(w http.ResponseWriter, request *http.Request, action string, _ []byte) {
						switch action {
						case "head":
							reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: message, Digest: relay.Digest("accepted-manifest")}})
						case "download":
							defer close(exited)
							if phase == "headers" {
								close(started)
							} else {
								w.Header().Set("Content-Length", strconv.Itoa(len(content)))
								w.WriteHeader(http.StatusOK)
								_, _ = io.WriteString(w, content[:1])
								w.(http.Flusher).Flush()
							}
							select {
							case <-request.Context().Done():
							case <-unblock:
							}
						default:
							t.Errorf("cancellation attempted an unexpected operation: %s", action)
							w.WriteHeader(http.StatusNotFound)
						}
					})
					cancelled, cancel := context.WithCancel(ctx)
					defer cancel()
					type outcome struct {
						result collectResult
						bytes  []byte
						err    error
					}
					finished := make(chan outcome, 1)
					go func() {
						var got outcome
						if operation == "wait" {
							got.err = c.Relay(cancelled, auth, "wait", relayRequest{TimeoutSeconds: 30}, &got.result)
						} else {
							got.bytes, got.err = c.downloadEvidence(cancelled, original, message.Attachments[0])
						}
						finished <- got
					}()
					select {
					case <-started:
					case got := <-finished:
						t.Fatalf("download did not reach its cancellation barrier: %v", got.err)
					case <-time.After(5 * time.Second):
						t.Fatal("download did not start")
					}
					cancel()
					var got outcome
					select {
					case got = <-finished:
					case <-time.After(5 * time.Second):
						t.Fatal("cancelled evidence request did not return")
					}
					release()
					select {
					case <-exited:
					case <-time.After(5 * time.Second):
						t.Fatal("cancelled evidence handler did not finish")
					}
					if operation == "wait" {
						if got.err != nil || got.result.Claim != nil || got.result.ForegroundRequired {
							t.Fatalf("cancelled preparation was not an empty poll: %+v %v", got.result, got.err)
						}
					} else if !errors.Is(got.err, context.Canceled) || got.bytes != nil {
						t.Fatalf("evidence cancellation lost its cause or returned partial bytes: %v", got.err)
					}
					after, err := os.ReadFile(filepath.Join(c.dir, "client.json"))
					if err != nil || !bytes.Equal(before, after) {
						t.Fatal("cancelled evidence retrieval rewrote an original unknown receipt")
					}
					wantActions := "join,download"
					if operation == "wait" {
						wantActions = "join,head,download"
					}
					if got := strings.Join(f.actions(), ","); got != wantActions {
						t.Fatalf("cancelled preparation claimed, acknowledged or replayed work: %s", got)
					}
					if operation == "download" {
						return
					}
					media, err := attachment.Open(filepath.Join(c.dir, "evidence"), options.Workspace)
					if err != nil {
						t.Fatal(err)
					}
					if _, _, err := media.Resolve(message.Attachments[0].ID); !errors.Is(err, attachment.ErrUnknown) {
						t.Fatalf("cancelled body was committed as verified evidence: %v", err)
					}
					// A later process can collect the same still-queued head.
					// The previous unknown receipt is neither ACKed nor replayed.
					f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, data []byte) {
						switch action {
						case "head":
							reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: message, Digest: relay.Digest("accepted-manifest")}})
						case "download":
							_, _ = io.WriteString(w, content)
						case "claim":
							respondClaim(t, w, data, message, "new-original-receipt")
						default:
							t.Errorf("retry replayed an unrelated receipt: %s", action)
							w.WriteHeader(http.StatusNotFound)
						}
					})
					reopened, err := OpenAt(s.Root())
					if err != nil {
						t.Fatal(err)
					}
					defer reopened.Close()
					client, err := reopened.Get(ctx, c.id)
					if err != nil {
						t.Fatal(err)
					}
					var collected collectResult
					if err := client.Relay(ctx, auth, "wait", relayRequest{TimeoutSeconds: 30}, &collected); err != nil {
						t.Fatalf("fresh collection did not recover after cancellation: %v", err)
					}
					if collected.Claim == nil || collected.Claim.ID != message.ID || collected.Claim.Receipt != "new-original-receipt" {
						t.Fatalf("fresh collection lost its original claim identity: %+v", collected)
					}
					current, err := client.read(ctx)
					if err != nil || len(current.Deliveries) != 2 || current.Deliveries[0] != original.Deliveries[0] || current.Deliveries[1].State != "claimed" {
						t.Fatalf("fresh collection changed uncertain delivery evidence: %+v %v", current.Deliveries, err)
					}
					if got := strings.Join(f.actions(), ","); got != wantActions+",head,download,claim" {
						t.Fatalf("fresh collection replayed or acknowledged without stdout: %s", got)
					}
				})
			}
		})
	}
}
