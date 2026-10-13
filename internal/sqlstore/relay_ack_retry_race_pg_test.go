package sqlstore

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Two independent work connections share the current admitted lease, as in
// the other SQL guard races. Neither Store semaphore can order the operations.
// PostgreSQL's actual room-row wait queue determines which operation commits.
func TestRelayOriginalAckOwnerRetrySameMessagePostgresOrdering(t *testing.T) {
	for _, firstKind := range []relay.EnterpriseOperationKind{relay.EnterpriseOperationAck, relay.EnterpriseOperationRetry} {
		t.Run(string(firstKind)+"-first", func(t *testing.T) {
			b, owner, member := recoveryPGPair(t)
			sent := relayPGCommit(t, b, relayPGCommand(t, b, owner, "race-original", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "original immutable content", ExpectedPeerGeneration: 1}))
			claimCommand := relayPGCommand(t, b, member, "original-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim})
			originalClaim := relayPGCommit(t, b, claimCommand)
			recoveryRestart(t, b)
			t.Cleanup(func() { _ = b.f.store.Close() })
			report, err := b.f.store.RecoverDeliveries(t.Context())
			if err != nil || !report.Complete || report.Reaped != 1 {
				t.Fatal("startup did not mark the original claim unknown", err)
			}
			original := ackRetryPGMessages(t, b)[sent.Relay.Message.ID]
			if original.State != "unknown" || original.Receipt != originalClaim.Relay.Claim.Receipt || original.FromRuntime != model.RuntimeClaude || original.FromGeneration != 1 || original.TargetGeneration != 1 {
				t.Fatal("fixture has no original unknown delivery and identity")
			}
			var revision uint64
			if err := b.f.admin.QueryRow(t.Context(), `SELECT revision FROM rooms WHERE room_id='binding-room'`).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			ack := relayPGCommand(t, b, member, "competing-original-ack", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: original.ID, Receipt: original.Receipt})
			retry := RelayCommand{Auth: b.owner.Auth, RoomID: "binding-room", ClientID: "competing-owner-retry", ExpectedRevision: revision, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationRetry, MessageID: original.ID}}
			commands := []RelayCommand{ack, retry}
			if firstKind == relay.EnterpriseOperationRetry {
				commands[0], commands[1] = retry, ack
			}
			work := schemaConn(t, b.f.dsn, b.f.schema)
			if err := configureSession(t.Context(), work, b.f.schema); err != nil {
				t.Fatal(err)
			}
			other := &Store{cfg: b.f.store.cfg, lease: b.f.store.lease, work: work, sem: make(chan struct{}, 1)}
			stores := []*Store{b.f.store, other}
			blocker := schemaConn(t, b.f.dsn, b.f.schema)
			tx, err := blocker.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(t.Context(), `SELECT room_id FROM rooms WHERE room_id='binding-room' FOR UPDATE`); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			var results [2]Result
			var errs [2]error
			var wg sync.WaitGroup
			defer func() { _ = tx.Rollback(context.Background()); cancel(); wg.Wait() }()
			launch := func(i int) {
				wg.Add(1)
				go func() { defer wg.Done(); results[i], errs[i] = stores[i].CommitRelayOperation(ctx, commands[i]) }()
			}
			launch(0)
			waitForControlLockWait(t, b.f.admin, stores[0].work.PgConn().PID(), blocker.PgConn().PID())
			launch(1)
			waitIdentityControlGuardChain(t, b.f, []uint32{stores[0].work.PgConn().PID(), stores[1].work.PgConn().PID()}, blocker.PgConn().PID())
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			if errs[0] != nil || results[0].Outcome != OutcomeCommitted || len(results[0].Facts) != 1 {
				t.Fatalf("first operation did not commit exactly once: outcome=%s error=%v", results[0].Outcome, errs[0])
			}
			if k := kernelErr(t, results[1], errs[1]); k.Class != ClassRevisionConflict || results[1].Revision != results[0].Revision || len(results[1].Facts) != 0 || results[1].Relay != nil {
				t.Fatalf("competing stale revision was not refused: class=%s", k.Class)
			}
			// A fresh explicit invocation must also respect the winning transition,
			// rather than merely relying on a stale browser revision to prevent it.
			loser := commands[1]
			loser.ExpectedRevision = results[0].Revision
			beforeRefusal := ackRetryPGCounts(t, b)
			refusedResult, err := b.f.store.CommitRelayOperation(t.Context(), loser)
			if err == nil {
				t.Fatalf("fresh losing %s unexpectedly committed after %s: outcome=%s", loser.Intent.Kind, firstKind, refusedResult.Outcome)
			}
			if k := kernelErr(t, refusedResult, err); k.Class != ClassInvalidControlState || len(refusedResult.Facts) != 0 || refusedResult.Relay != nil {
				t.Fatalf("fresh losing operation crossed semantic gate: class=%s", k.Class)
			}
			if ackRetryPGCounts(t, b) != beforeRefusal {
				t.Fatal("semantic refusal wrote facts, operation receipt, audit or revision")
			}
			resolved, err := b.f.store.ResolveRelayOperation(t.Context(), loser)
			if err != nil || resolved.Outcome != OutcomeNotCommitted || resolved.Relay != nil || len(resolved.Facts) != 0 {
				t.Fatal("refused loser left an accepted operation receipt")
			}
			messages := ackRetryPGMessages(t, b)
			if firstKind == relay.EnterpriseOperationAck {
				if len(messages) != 1 || messages[original.ID].State != "handed_off" || messages[original.ID].Receipt != original.Receipt || !results[0].Relay.HandedOff {
					t.Fatal("ACK-first created duplicate work or failed original settlement")
				}
				ackRetryPGAssertIdentity(t, original, messages[original.ID])
			} else {
				newAttempt := results[0].Relay.Message
				if newAttempt == nil || newAttempt.ID == original.ID || newAttempt.RetryOf != original.ID || len(messages) != 2 || !reflect.DeepEqual(messages[original.ID], original) {
					t.Fatal("Retry-first changed original receipt/history or did not create distinct work")
				}
				ackRetryPGAssertIdentity(t, original, messages[newAttempt.ID])
				claim := relayPGCommit(t, b, relayPGCommand(t, b, member, "claim-new-attempt", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
				if claim.Relay.Claim == nil || claim.Relay.Claim.ID != newAttempt.ID || claim.Relay.Claim.Receipt == original.Receipt {
					t.Fatal("new attempt did not obtain its own delivery receipt")
				}
				beforeOldProofs := ackRetryPGCounts(t, b)
				late := relayPGCommand(t, b, member, "late-original-ack", ack.Intent)
				r, e := b.f.store.CommitRelayOperation(t.Context(), late)
				if k := kernelErr(t, r, e); k.Class != ClassInvalidControlState {
					t.Fatal("late original ACK settled a delivering Retry")
				}
				wrong := relayPGCommand(t, b, member, "old-proof-new-attempt", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: newAttempt.ID, Receipt: original.Receipt})
				r, e = b.f.store.CommitRelayOperation(t.Context(), wrong)
				if k := kernelErr(t, r, e); k.Class != ClassAuthorization {
					t.Fatal("old receipt authorized new attempt")
				}
				if ackRetryPGCounts(t, b) != beforeOldProofs {
					t.Fatal("old ACK proofs wrote facts, operation receipt, audit or revision")
				}
				before := ackRetryPGMessages(t, b)
				if !reflect.DeepEqual(before[original.ID], original) || before[newAttempt.ID].State != "delivering" {
					t.Fatal("rejected old ACK mutated either attempt")
				}
				settled := relayPGCommit(t, b, relayPGCommand(t, b, member, "new-attempt-ack", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: newAttempt.ID, Receipt: claim.Relay.Claim.Receipt}))
				if !settled.Relay.HandedOff {
					t.Fatal("new receipt could not settle its own attempt")
				}
				after := ackRetryPGMessages(t, b)
				if !reflect.DeepEqual(after[original.ID], original) || after[newAttempt.ID].State != "handed_off" {
					t.Fatal("new ACK relabeled original unknown history")
				}
				ackRetryPGAssertIdentity(t, original, after[newAttempt.ID])
			}
			claimReplay, err := b.f.store.ResolveRelayOperation(t.Context(), claimCommand)
			if err != nil || claimReplay.Outcome != OutcomeReplayed || !reflect.DeepEqual(claimReplay.Relay, originalClaim.Relay) || !reflect.DeepEqual(claimReplay.Facts, originalClaim.Facts) || claimReplay.Seq != originalClaim.Seq || claimReplay.Revision != originalClaim.Revision {
				t.Fatal("original claim operation receipt changed after ACK/Retry competition")
			}
			accepted, err := b.f.store.ResolveRelayOperation(t.Context(), commands[0])
			if err != nil || accepted.Outcome != OutcomeReplayed || !reflect.DeepEqual(accepted.Relay, results[0].Relay) || !reflect.DeepEqual(accepted.Facts, results[0].Facts) || accepted.Seq != results[0].Seq || accepted.Revision != results[0].Revision {
				t.Fatal("winning original operation receipt drifted after competition")
			}
		})
	}
}

// Inspect the private stored delivery facts rather than a browser projection,
// which deliberately redacts the original receipt.
func ackRetryPGMessages(t *testing.T, b *bindingPGState) map[string]relay.Message {
	t.Helper()
	snapshot, err := b.f.store.ReadonlySnapshot(t.Context(), b.owner.Auth, "binding-room")
	if err != nil {
		t.Fatal(err)
	}
	messages := map[string]relay.Message{}
	for _, event := range snapshot.Events {
		if event.Kind != relay.EventMessage {
			continue
		}
		var fact struct {
			relay.Message
			Receipt string `json:"receipt"`
		}
		if err := json.Unmarshal(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		fact.Message.Receipt = fact.Receipt
		messages[fact.ID] = fact.Message
	}
	return messages
}

func ackRetryPGAssertIdentity(t *testing.T, original, current relay.Message) {
	t.Helper()
	if current.From != original.From || current.To != original.To || current.Text != original.Text || current.Author != original.Author || current.FromRuntime != original.FromRuntime || current.FromGeneration != original.FromGeneration || current.TargetGeneration != original.TargetGeneration {
		t.Fatal("delivery transition changed original author/runtime/generation/content")
	}
}

func ackRetryPGCounts(t *testing.T, b *bindingPGState) [5]int64 {
	t.Helper()
	var counts [5]int64
	if err := b.f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM room_events WHERE room_id='binding-room'),(SELECT count(*) FROM relay_operations WHERE room_id='binding-room'),(SELECT count(*) FROM audit WHERE room_id='binding-room'),current_seq,revision FROM rooms WHERE room_id='binding-room'`).Scan(&counts[0], &counts[1], &counts[2], &counts[3], &counts[4]); err != nil {
		t.Fatal(err)
	}
	return counts
}
