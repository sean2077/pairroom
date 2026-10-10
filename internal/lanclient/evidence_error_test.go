package lanclient

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestEvidenceHTTPErrorPreservesHostUnavailableWithoutChangingMembership(t *testing.T) {
	for _, operation := range []string{"summary", "upload", "download", "wait"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			s, f := newStore(t), newRemote(t)
			c, auth, _ := f.join(t, s)
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
			const privateDiagnostic = "/private/host/room/events.jsonl: storage failed"
			f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, _ []byte) {
				switch action {
				case "head":
					reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: message, Digest: relay.Digest("accepted-manifest")}})
				case "attachment":
					reply(w, message.Attachments[0])
				case "summary", "upload", "download":
					w.WriteHeader(http.StatusServiceUnavailable)
					reply(w, map[string]string{"error": privateDiagnostic, "code": lanshare.HostUnavailableCode})
				default:
					t.Errorf("unavailable host caused an unexpected retry or effect: %s", action)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			var operationErr error
			var metadata model.Attachment
			var path string
			var collected collectResult
			wantActions := "join," + operation
			switch operation {
			case "summary":
				operationErr = c.Relay(ctx, auth, "summary", nil, nil)
			case "upload":
				metadata, operationErr = c.Upload(ctx, auth, "file", "repro.sh", strings.NewReader(content))
			case "download":
				metadata, path, operationErr = c.Download(ctx, message.Attachments[0].ID)
				wantActions = "join,attachment,download"
			case "wait":
				operationErr = c.Relay(ctx, auth, "wait", nil, &collected)
				wantActions = "join,head,download"
			}
			var failure *Error
			if !errors.As(operationErr, &failure) || failure.Status != http.StatusServiceUnavailable || failure.Code != lanshare.HostUnavailableCode {
				t.Fatalf("HTTP error lost its status/code at the real %s boundary: %v", operation, operationErr)
			}
			if errors.Is(operationErr, relay.ErrAuth) || errors.Is(operationErr, ErrTransportUnavailable) || strings.Contains(operationErr.Error(), privateDiagnostic) || !strings.Contains(operationErr.Error(), "membership stays valid") {
				t.Fatalf("host availability became auth/transport failure or leaked remote text: %v", operationErr)
			}
			if metadata != (model.Attachment{}) || path != "" || collected.Claim != nil {
				t.Fatal("failed evidence operation returned an upload, artifact or delivery receipt")
			}
			after, err := os.ReadFile(filepath.Join(c.dir, "client.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("host availability refusal changed identity or unknown-delivery evidence")
			}
			if err := s.identities.Check(ctx, reservation(original)); err != nil {
				t.Fatalf("host availability refusal invalidated local session ownership: %v", err)
			}
			snapshot, err := c.Snapshot(ctx)
			if err != nil || snapshot.Status != "accepted" || !snapshot.Connected || snapshot.LastSeen.IsZero() {
				t.Fatalf("host contact was conflated with membership refusal: %+v %v", snapshot, err)
			}
			if got := strings.Join(f.actions(), ","); got != wantActions {
				t.Fatalf("definite HTTP response caused replay, claim or ACK: %s", got)
			}
		})
	}
}
