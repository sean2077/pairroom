package lanclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestEvidenceQuotaIsActionableAndCannotStartAClaim(t *testing.T) {
	for _, budget := range []string{"committed", "temporary"} {
		t.Run(budget, func(t *testing.T) {
			ctx := context.Background()
			s, f := newStore(t), newRemote(t)
			c, auth, options := f.join(t, s)
			message, content, _ := incomingFile(t)
			dir := filepath.Join(c.dir, "evidence")
			if err := privatefile.Mkdir(dir); err != nil {
				t.Fatal(err)
			}
			media, err := attachment.Open(dir, options.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			name, size, expected := "retained.data", attachment.MaxRoomSharedBytes, attachment.ErrSharedQuota
			if budget == "temporary" {
				name, size, expected = ".staged-interrupted.tmp", attachment.MaxTemporaryBytes, attachment.ErrTemporaryQuota
			}
			file, err := os.Create(filepath.Join(media.Root(), name))
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(size); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			var claims atomic.Int32
			f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, _ []byte) {
				switch action {
				case "head":
					reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: message, Digest: relay.Digest("accepted-manifest")}})
				case "download":
					_, _ = io.WriteString(w, content)
				case "claim":
					claims.Add(1)
					reply(w, lanshare.ClaimResponse{})
				default:
					t.Errorf("unexpected evidence operation %s", action)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			var result collectResult
			err = c.Relay(ctx, auth, "wait", nil, &result)
			if !errors.Is(err, expected) || strings.Contains(err.Error(), dir) {
				t.Fatalf("capacity failure lost its safe actionable reason: %v", err)
			}
			if budget == "committed" && (!strings.Contains(err.Error(), "verified-evidence cache") || strings.Contains(err.Error(), "new Room")) {
				t.Fatalf("guest cache exhaustion did not name this machine's cache instead of a host-only remedy: %v", err)
			}
			if budget == "committed" {
				for _, boundary := range []string{"stop collection", "optional observer", "complete evidence subdirectory", "content and manifests together", "backup", "parent client record and journals intact", "authorized host access"} {
					if !strings.Contains(err.Error(), boundary) {
						t.Fatalf("guest quota recovery omitted %q: %v", boundary, err)
					}
				}
			}
			if claims.Load() != 0 || result.Claim != nil {
				t.Fatal("quota failure crossed the delivery claim boundary")
			}
			record, err := c.read(ctx)
			if err != nil || len(record.Deliveries) != 0 {
				t.Fatal("quota failure manufactured a delivery receipt")
			}
			entries, err := os.ReadDir(media.Root())
			if err != nil || len(entries) != 1 || entries[0].Name() != name {
				t.Fatal("quota failure evicted evidence or leaked an import")
			}
		})
	}
}

func TestMovingCompleteEvidenceCachePreservesUncertainReceiptAndRefetchesVerifiedBytes(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, options := f.join(t, s)
	message, content, _ := incomingFile(t)
	expected := message.Attachments[0]
	var downloads, claims atomic.Int32
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, data []byte) {
		switch action {
		case "attachment":
			reply(w, expected)
		case "head":
			reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: message, Digest: relay.Digest("accepted-manifest")}})
		case "download":
			downloads.Add(1)
			_, _ = io.WriteString(w, content)
		case "claim":
			claims.Add(1)
			respondClaim(t, w, data, message, "original-receipt")
		default:
			t.Errorf("unexpected cache recovery operation %s", action)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	var collected collectResult
	if err := c.Relay(ctx, auth, "wait", nil, &collected); err != nil || collected.Claim == nil {
		t.Fatalf("initial evidence-backed claim: %+v %v", collected, err)
	}
	// No stdout completion is recorded: clearing the cache cannot acknowledge
	// this uncertain delivery or discard the private original receipt.
	before, err := os.ReadFile(filepath.Join(c.dir, "client.json"))
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(c.dir, "evidence")
	backupDir := filepath.Join(localDir(t), "evidence-backup")
	if err := os.Rename(cacheDir, backupDir); err != nil {
		t.Fatal(err)
	}
	metadata, path, err := c.Download(ctx, expected.ID)
	if err != nil || metadata != expected {
		t.Fatalf("complete cache move prevented authorized re-fetch: %+v %v", metadata, err)
	}
	data, err := os.ReadFile(path)
	expectedPath := filepath.Join(cacheDir, "attachments", expected.ID+".data")
	if err != nil || string(data) != content || relay.Digest(string(data)) != expected.SHA256 || path != expectedPath {
		t.Fatalf("fresh cache did not contain the same verified artifact at %q: path=%q err=%v", expectedPath, path, err)
	}
	backup, err := attachment.Open(backupDir, options.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if metadata, _, err := backup.Resolve(expected.ID); err != nil || metadata != expected {
		t.Fatalf("operator backup lost original content or manifest: %+v %v", metadata, err)
	}
	after, err := os.ReadFile(filepath.Join(c.dir, "client.json"))
	if err != nil || string(after) != string(before) || downloads.Load() != 2 || claims.Load() != 1 {
		t.Fatal("cache recovery changed identity, original delivery receipt, or claim/ACK state")
	}
}
