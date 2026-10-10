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
			if err := c.Relay(ctx, auth, "wait", nil, &result); !errors.Is(err, expected) || strings.Contains(err.Error(), dir) {
				t.Fatalf("capacity failure lost its safe actionable reason: %v", err)
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
