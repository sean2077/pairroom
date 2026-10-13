package enterpriseservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

// The real Store commits first; only its return to the production HTTP handler
// is paused. No authorization, transaction result or envelope is fabricated.
type releaseFenceStore struct {
	*sqlstore.Store
	committed chan sqlstore.Result
	resume    chan struct{}
}

func (s *releaseFenceStore) CommitRelayOperation(ctx context.Context, command sqlstore.RelayCommand) (sqlstore.Result, error) {
	result, err := s.Store.CommitRelayOperation(ctx, command)
	if err == nil && result.Outcome == sqlstore.OutcomeCommitted && command.Intent.Kind == relay.EnterpriseOperationClaim && result.Relay != nil && result.Relay.Claim != nil {
		s.committed <- result
		select {
		case <-s.resume:
		case <-ctx.Done():
			return sqlstore.Result{}, ctx.Err()
		}
	}
	return result, err
}

func TestHTTPPGLeaseLostAfterActualClaimCommitReleasesNoPrivateEnvelope(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(fmt.Sprintf("successor=%t", successor), func(t *testing.T) {
			f := newHTTPPGFixture(t)
			// The fixture also retains a schema-bootstrap connection. Tag only the
			// Store connections so observing a surviving PID cannot select it.
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			workTag := "release-fence-" + f.schema
			if strings.HasPrefix(f.cfg.DSN, "postgres://") || strings.HasPrefix(f.cfg.DSN, "postgresql://") {
				connectionURL, err := url.Parse(f.cfg.DSN)
				if err != nil {
					t.Fatal("private test connection URI invalid")
				}
				query := connectionURL.Query()
				query.Set("application_name", workTag)
				connectionURL.RawQuery = query.Encode()
				f.cfg.DSN = connectionURL.String()
			} else {
				f.cfg.DSN += " application_name='" + workTag + "'"
			}
			store, err := sqlstore.Open(t.Context(), f.cfg)
			if err != nil {
				t.Fatal("tagged test Store unavailable")
			}
			f.store = store
			t.Cleanup(func() { _ = store.Close() })
			gate := &releaseFenceStore{Store: f.store, committed: make(chan sqlstore.Result, 1), resume: make(chan struct{})}
			released := false
			defer func() {
				if !released {
					close(gate.resume)
				}
			}()
			transactions, err := enterpriseauth.NewMemoryStore(16)
			if err != nil {
				t.Fatal(err)
			}
			handler, err := New(Config{DeploymentID: f.cfg.DeploymentID, PublicOrigin: f.origin}, f.provider, transactions, gate)
			if err != nil {
				t.Fatal(err)
			}
			f.handler.Store(handler)
			owner := f.browser("member-one")
			f.login(owner)
			session := f.session(owner)
			const room = "release-fence-room"
			const marker = "LEASE_RELEASE_PRIVATE_BODY_847cc7"
			f.createRoom(owner, session.CSRF, room)
			native := grantsPGNative(t, f)
			device := grantsPGEnroll(t, f, native, owner, session.CSRF, "release-fence")
			one := grantsPGBind(t, f, native, owner, session.CSRF, room, device, model.ActorSlot1, "release-one")
			two := grantsPGBind(t, f, native, owner, session.CSRF, room, device, model.ActorSlot2, "release-two")
			status, _ := grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+room+"/relay", one.token, grantsPGRelayRequest{ClientID: "release-source", BindingID: one.bindingID, Generation: 1, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: marker, ExpectedPeerGeneration: 1}})
			if status != 200 {
				t.Fatal("actual publication failed")
			}
			var leasePID, workPID int32
			var epoch int64
			if err := f.admin.QueryRow(t.Context(), `SELECT lease_pid,epoch FROM ownership`).Scan(&leasePID, &epoch); err != nil {
				t.Fatal(err)
			}
			var workCount int
			if err := f.admin.QueryRow(t.Context(), `SELECT count(*),min(pid) FROM pg_catalog.pg_stat_activity WHERE datname=current_database() AND backend_type='client backend' AND application_name=$1 AND pid<>$2`, workTag, leasePID).Scan(&workCount, &workPID); err != nil || workCount != 1 {
				t.Fatal("tagged original work backend not uniquely observable")
			}
			type reply struct {
				status int
				body   []byte
			}
			done := make(chan reply, 1)
			go func() {
				code, body := grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+room+"/relay", two.token, grantsPGRelayRequest{ClientID: "held-release-claim", BindingID: two.bindingID, Generation: 1, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}})
				done <- reply{code, body}
			}()
			var committed sqlstore.Result
			select {
			case committed = <-gate.committed:
			case <-time.After(5 * time.Second):
				t.Fatal("claim did not reach actual committed return barrier")
			}
			if committed.Relay.Claim.Envelope == "" || !bytes.Contains([]byte(committed.Relay.Claim.Envelope), []byte(marker)) || committed.Relay.Claim.Receipt == "" {
				t.Fatal("barrier lacks real private claim")
			}
			var claimCount int
			if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM relay_operations WHERE room_id=$1 AND identifier='held-release-claim'`, room).Scan(&claimCount); err != nil || claimCount != 1 {
				t.Fatal("claim is not durable before lease termination")
			}
			counts := func() [3]int64 {
				var value [3]int64
				if err := f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM room_events),(SELECT count(*) FROM relay_operations),(SELECT count(*) FROM audit)`).Scan(&value[0], &value[1], &value[2]); err != nil {
					t.Fatal(err)
				}
				return value
			}
			beforeRefusals := counts()
			var terminated bool
			if err := f.admin.QueryRow(t.Context(), `SELECT pg_catalog.pg_terminate_backend($1)`, leasePID).Scan(&terminated); err != nil || !terminated {
				t.Fatal("test-owned lease termination failed")
			}
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				var exists bool
				if err := f.admin.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid=$1)`, leasePID).Scan(&exists); err != nil {
					t.Fatal(err)
				}
				if !exists {
					break
				}
				select {
				case <-ticker.C:
				case <-deadline.C:
					t.Fatal("terminated lease backend did not leave")
				}
			}
			if successor {
				next, err := sqlstore.Open(t.Context(), f.cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = next.Close() })
			}
			var workAlive bool
			var currentEpoch int64
			if err := f.admin.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid=$1),(SELECT epoch FROM ownership)`, workPID).Scan(&workAlive, &currentEpoch); err != nil || !workAlive || currentEpoch != epoch+boolInt64(successor) {
				t.Fatal("old work backend must remain alive at the exact ownership boundary")
			}
			close(gate.resume)
			released = true
			select {
			case response := <-done:
				if response.status != 503 || bytes.Contains(response.body, []byte(marker)) || bytes.Contains(response.body, []byte(committed.Relay.Claim.Receipt)) || bytes.Contains(response.body, []byte(committed.Relay.Claim.ID)) {
					t.Fatal("lease-lost collector released private claim identity or envelope")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("held HTTP request did not terminate")
			}
			for _, path := range []string{"/enterprise/api/v1/session", "/enterprise/api/v1/rooms/" + room} {
				code, body := f.get(owner, path)
				if code != 503 || bytes.Contains(body, []byte(marker)) || bytes.Contains(body, []byte(session.CSRF)) || bytes.Contains(body, []byte(session.UserID)) {
					t.Fatal("lease-lost private browser route disclosed original scope")
				}
			}
			code, body := f.post(owner, "/enterprise/api/v1/rooms/"+room+"/messages", map[string]string{"client_id": "old-work-write", "to": "slot1", "text": "must not persist"}, session.CSRF)
			if code != 503 || bytes.Contains(body, []byte(marker)) || counts() != beforeRefusals {
				t.Fatal("old work connection authorized a write or private refusal produced business effects")
			}
			if err := f.admin.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid=$1)`, workPID).Scan(&workAlive); err != nil || !workAlive {
				t.Fatal("test accidentally closed the old work connection instead of fencing it")
			}
			var resultEncoding []byte
			if err := f.admin.QueryRow(t.Context(), `SELECT result_encoding FROM relay_operations WHERE room_id=$1 AND identifier='held-release-claim'`, room).Scan(&resultEncoding); err != nil {
				t.Fatal(err)
			}
			var original relay.EnterpriseOperationResult
			if json.Unmarshal(resultEncoding, &original) != nil || original.Claim == nil || original.Claim.Receipt != committed.Relay.Claim.Receipt || original.Claim.ID != committed.Relay.Claim.ID {
				t.Fatal("release refusal changed original accepted receipt")
			}
		})
	}
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}
