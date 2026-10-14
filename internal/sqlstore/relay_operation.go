package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

const (
	namespaceBinding         = "binding"
	relayResultVersion       = 1
	relayEncodingBudget      = 2 << 20
	maxRelayBatchFacts       = 16
	ClassPeerChanged         = "peer_changed"
	ClassPreparedHeadChanged = "prepared_head_changed"
)

// RelayCommand carries a closed intent, never arbitrary facts or a callback.
// BindingID/Generation select the expected authority and are checked against
// the live credential under transaction locks. They do not authorize a caller.
type RelayCommand struct {
	Auth                         AuthContext
	RoomID, ClientID, BindingID  string
	Generation, ExpectedRevision uint64
	Intent                       relay.EnterpriseOperationIntent
}

type relayMutation struct {
	intent relay.EnterpriseOperationIntent
}

func (s *Store) relayPlan(cmd RelayCommand) (opPlan, string) {
	if cmd.Auth.Deployment != s.cfg.DeploymentID || !validRoomCreateIdentity(cmd.Auth.UserID) || !validRoomCreateIdentity(cmd.RoomID) || !validRoomCreateIdentity(cmd.ClientID) || cmd.ExpectedRevision > math.MaxInt64 || relay.ValidateEnterpriseOperationIntent(cmd.Intent) != nil {
		return opPlan{}, ClassInvalidRequest
	}
	key := OpKey{Namespace: namespaceHuman, Deployment: cmd.Auth.Deployment, RoomID: cmd.RoomID, OpKind: string(cmd.Intent.Kind), Identifier: cmd.ClientID, Principal: cmd.Auth.UserID}
	if cmd.Intent.Kind == relay.EnterpriseOperationReport {
		// Native Stop publication identity is the Binding's report sequence,
		// independent of a transport request ID or a reconnecting process.
		key.Identifier = "report-" + strconv.FormatUint(cmd.Intent.ReportSeq, 10)
	}
	if cmd.Auth.BindingGrantID != "" {
		if cmd.Intent.Kind == relay.EnterpriseOperationRetry || cmd.Intent.Kind == relay.EnterpriseOperationCancel {
			return opPlan{}, ClassAuthorization
		}
		if cmd.Auth.SessionID != "" || cmd.Auth.DeviceGrantID != "" || !validRoomCreateIdentity(cmd.BindingID) || cmd.Generation == 0 || cmd.Generation > math.MaxInt64 {
			return opPlan{}, ClassAuthorization
		}
		key.Namespace, key.BindingID, key.Generation = namespaceBinding, cmd.BindingID, cmd.Generation
	} else if cmd.Auth.DeviceGrantID != "" || cmd.BindingID != "" || cmd.Generation != 0 || (cmd.Intent.Kind != relay.EnterpriseOperationRetry && cmd.Intent.Kind != relay.EnterpriseOperationCancel) {
		return opPlan{}, ClassAuthorization
	}
	return opPlan{auth: cmd.Auth, userID: cmd.Auth.UserID, key: key, hash: relayIntentHash(key, cmd.Intent), expectedRevision: cmd.ExpectedRevision, relay: &relayMutation{intent: cmd.Intent}}, ""
}

// CommitRelayOperation invokes the shared transaction runner. The SQL side
// obtains a trusted isolated snapshot only after live authorization, original
// receipt recovery and CAS; the existing Relay planner owns every transition.
func (s *Store) CommitRelayOperation(ctx context.Context, cmd RelayCommand) (Result, error) {
	plan, class := s.relayPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}

func (s *Store) ResolveRelayOperation(ctx context.Context, cmd RelayCommand) (Result, error) {
	plan, class := s.relayPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.resolveWithFallback(ctx, plan)
}

func relayIntentHash(key OpKey, intent relay.EnterpriseOperationIntent) [32]byte {
	h := sha256.New()
	for _, value := range []string{"sqlstore.relay.v1", key.Namespace, key.Deployment, key.RoomID, key.BindingID, key.OpKind, key.Identifier, key.Principal} {
		writePart(h, []byte(value))
	}
	var generation [8]byte
	binary.BigEndian.PutUint64(generation[:], key.Generation)
	writePart(h, generation[:])
	encoding, _ := json.Marshal(intent) // A closed value with no fallible fields.
	writePart(h, encoding)
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func relayReadAllowed(plan opPlan, d dependencyState) bool {
	if !d.readAllowed() {
		return false
	}
	if plan.key.Namespace == namespaceBinding {
		return d.credential.kind == CredentialBinding && d.credential.roomID == plan.key.RoomID && d.credential.bindingID == plan.key.BindingID && d.credential.generation == plan.key.Generation && d.credential.slot.ValidParticipant()
	}
	return plan.auth.DeviceGrantID == "" && plan.auth.BindingGrantID == ""
}

func (s *Store) lookupRelayOperation(ctx context.Context, tx pgx.Tx, plan opPlan) (Result, bool, error) {
	var intentHash, resultHash, encoding []byte
	var version int
	var revision, seq uint64
	var first, last *int64
	err := tx.QueryRow(ctx, `SELECT intent_hash,result_hash,result_version,result_encoding,result_revision,result_seq,fact_first,fact_last FROM `+s.table("relay_operations")+` WHERE op_namespace=$1 AND deployment_id=$2 AND room_id=$3 AND binding_id=$4 AND generation=$5 AND op_kind=$6 AND identifier=$7 AND principal=$8`, plan.key.Namespace, plan.key.Deployment, plan.key.RoomID, plan.key.BindingID, int64(plan.key.Generation), plan.key.OpKind, plan.key.Identifier, plan.key.Principal).Scan(&intentHash, &resultHash, &version, &encoding, &revision, &seq, &first, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if !bytes.Equal(intentHash, plan.hash[:]) {
		result, err := controlConflict()
		return result, true, err
	}
	var original relay.EnterpriseOperationResult
	digest := sha256.Sum256(encoding)
	decoder := json.NewDecoder(bytes.NewReader(encoding))
	decoder.DisallowUnknownFields()
	if version != relayResultVersion || len(encoding) > relayEncodingBudget || !bytes.Equal(digest[:], resultHash) || decoder.Decode(&original) != nil || original.Kind != plan.relay.intent.Kind || relay.ValidateEnterpriseOperationResult(original) != nil {
		return Result{}, true, &storedFactCorruption{err: errors.New("invalid stored Relay operation result")}
	}
	canonical, err := json.Marshal(original)
	if err != nil || !bytes.Equal(canonical, encoding) || (first == nil) != (last == nil) || first != nil && (*first < 1 || *last < *first || *last-*first >= maxRelayBatchFacts || uint64(*last) != seq) {
		return Result{}, true, &storedFactCorruption{err: errors.New("noncanonical stored Relay operation result")}
	}
	result := Result{Outcome: OutcomeReplayed, Seq: seq, Revision: revision, Relay: &original}
	if first != nil {
		for offset := int64(0); offset <= *last-*first; offset++ {
			fact, err := s.loadFact(ctx, tx, plan.key.RoomID, uint64(*first+offset))
			if err != nil {
				return Result{}, true, err
			}
			result.Facts = append(result.Facts, fact)
		}
		result.Seq = uint64(*last)
	}
	if original.Kind == relay.EnterpriseOperationSend {
		if relay.MatchEnterpriseSendEvidence(plan.relay.intent, *original.Message) != nil {
			return Result{}, true, &storedFactCorruption{err: errors.New("relay message evidence disagrees with frozen intent")}
		}
		if plan.relay.intent.QuoteID != "" {
			if first == nil {
				return Result{}, true, &storedFactCorruption{err: errors.New("quoted Relay result has no stored publication")}
			}
			quotePlan := plan
			quotePlan.human = &HumanSendIntent{AttachmentIDs: plan.relay.intent.AttachmentIDs, QuoteID: plan.relay.intent.QuoteID, Review: plan.relay.intent.Review}
			message := original.Message
			if err := s.validateHistoricalHumanQuote(ctx, tx, quotePlan, model.Event{Seq: uint64(*first)}, humanSendPayload{Attachments: message.Attachments, Quote: message.Quote, Review: message.Review}); err != nil {
				return Result{}, true, err
			}
		}
	}
	return result, true, nil
}

func (s *Store) resolveRelayPlan(ctx context.Context, tx pgx.Tx, plan opPlan, locked dependencyState) (Result, error) {
	if locked.roomMissing || !relayReadAllowed(plan, locked) {
		return refused(ClassAuthorization, nil)
	}
	result, found, err := s.lookupRelayOperation(ctx, tx, plan)
	if err != nil {
		if isStoredFactCorruption(err) {
			return refused(ClassStoredFactUnreadable, err)
		}
		var kernel *KernelError
		if errors.As(err, &kernel) {
			return result, err
		}
		return resolveIncomplete(ctx, err)
	}
	if !found {
		return Result{Outcome: OutcomeNotCommitted}, nil
	}
	return result, nil
}

func (s *Store) applyRelayPlan(ctx context.Context, tx pgx.Tx, plan opPlan, locked dependencyState) (Result, error) {
	if locked.roomMissing || !relayReadAllowed(plan, locked) {
		return refused(ClassAuthorization, nil)
	}
	result, found, err := s.lookupRelayOperation(ctx, tx, plan)
	if err != nil {
		if isStoredFactCorruption(err) {
			return refused(ClassStoredFactUnreadable, err)
		}
		var kernel *KernelError
		if errors.As(err, &kernel) {
			return result, err
		}
		return controlStatementFailure(s, ctx, err)
	}
	if found {
		return result, nil
	}
	// New operation policy follows recovery of an authorized accepted result.
	if !locked.writeAllowed() || plan.key.Namespace == namespaceHuman && (locked.memberRole != "owner" || locked.ownerUserID != plan.userID) {
		return refused(ClassAuthorization, nil)
	}
	if locked.archived && plan.relay.intent.Kind != relay.EnterpriseOperationAck {
		return refused(ClassInvalidControlState, nil)
	}
	if locked.revision != plan.expectedRevision {
		result, err := refused(ClassRevisionConflict, nil)
		result.Revision = locked.revision
		return result, err
	}
	if locked.revision >= math.MaxInt64 {
		return refused(ClassRangeExceeded, nil)
	}
	snapshot, err := s.readRoomEvents(ctx, tx, RoomSnapshot{RoomID: plan.key.RoomID, DeploymentID: plan.key.Deployment, OrgID: locked.orgID, HostMode: locked.hostMode, Seq: locked.currentSeq, Revision: locked.revision})
	if err != nil {
		var kernel *KernelError
		if errors.As(err, &kernel) && kernel.Outcome == OutcomeRefused {
			return refused(kernel.Class, kernel.Err)
		}
		return controlStatementFailure(s, ctx, err)
	}
	caller := relay.EnterpriseOperationCaller{Actor: model.ActorUser, UserID: plan.userID, ClientID: plan.key.Identifier}
	if plan.key.Namespace == namespaceBinding {
		caller.Actor, caller.BindingID, caller.Generation = locked.credential.slot, plan.key.BindingID, plan.key.Generation
	}
	var ids relay.EnterpriseOperationIDs
	switch plan.relay.intent.Kind {
	case relay.EnterpriseOperationSend, relay.EnterpriseOperationReport, relay.EnterpriseOperationRetry:
		ids.MessageID, err = controlID("relay")
	case relay.EnterpriseOperationClaim:
		ids.Receipt, err = controlID("receipt")
	}
	if err != nil {
		return refused(ClassStateInvalid, err)
	}
	media, err := attachmentReferenceMetadata(plan, locked)
	if err != nil {
		return refused(ClassAuthorization, err)
	}
	prepare, err := relay.PrepareEnterpriseOperationWithAttachments(toRelaySnapshot(snapshot), caller, plan.relay.intent, ids, media)
	if err != nil {
		return refused(ClassInvalidRequest, err)
	}
	batch, err := prepare(time.Now().UTC())
	if err != nil {
		if errors.Is(err, relay.ErrPeerChanged) {
			return refused(ClassPeerChanged, nil)
		}
		if errors.Is(err, relay.ErrPreparedClaimChanged) {
			return refused(ClassPreparedHeadChanged, nil)
		}
		if errors.Is(err, relay.ErrEnterpriseQueueLimit) {
			return refused(ClassResourceLimit, nil)
		}
		if errors.Is(err, relay.ErrAuth) {
			return refused(ClassAuthorization, nil)
		}
		return refused(ClassInvalidControlState, err)
	}
	if len(batch.Facts) > maxRelayBatchFacts || uint64(len(batch.Facts)) > math.MaxInt64-locked.currentSeq || batch.Result.Kind != plan.relay.intent.Kind || relay.ValidateEnterpriseOperationResult(batch.Result) != nil {
		return refused(ClassInvalidFact, nil)
	}
	if len(batch.Facts) > 0 {
		switch plan.relay.intent.Kind {
		case relay.EnterpriseOperationSend, relay.EnterpriseOperationReport, relay.EnterpriseOperationRetry, relay.EnterpriseOperationPark:
			if err := s.checkRoomFactAdmission(ctx, tx, plan, batch.Facts, snapshot); err != nil {
				return resourceAdmissionFailure(s, ctx, err)
			}
		}
	}
	encoding, err := json.Marshal(batch.Result)
	if err != nil || len(encoding) > relayEncodingBudget {
		return refused(ClassInvalidFact, err)
	}
	resultHash := sha256.Sum256(encoding)
	var first, last *int64
	for i := range batch.Facts {
		fact := &batch.Facts[i]
		if fact.RoomID != plan.key.RoomID || fact.Seq != 0 || fact.CreatedAt.IsZero() || !json.Valid(fact.Data) {
			return refused(ClassInvalidFact, nil)
		}
		if fact.ID == "" {
			fact.ID, err = controlID("event")
			if err != nil {
				return refused(ClassStateInvalid, err)
			}
		}
		fact.Seq = locked.currentSeq + uint64(i) + 1
		factEncoding, err := json.Marshal(fact)
		if err != nil || len(factEncoding) > relayEncodingBudget {
			return refused(ClassInvalidFact, err)
		}
		origin := userFactOrigin(plan.userID)
		if fact.Kind == relay.EventMessage && fact.Actor == model.ActorSystem {
			origin = operatorFactOrigin()
		}
		if err := s.writeStoredRoomEvent(ctx, tx, *fact, origin); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		if origin.kind == "operator" {
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("audit")+`(deployment_id,room_id,user_id,actor_kind,kind,outcome,at) VALUES($1,$2,NULL,'operator','relay.expiry','accepted',$3)`, plan.key.Deployment, plan.key.RoomID, fact.CreatedAt); err != nil {
				return controlStatementFailure(s, ctx, err)
			}
		}

	}
	seq := locked.currentSeq + uint64(len(batch.Facts))
	if plan.relay.intent.Kind == relay.EnterpriseOperationSend && batch.Result.Message != nil {
		if err := s.writeAttachmentReferences(ctx, tx, plan.key.RoomID, batch.Result.Message.ID, seq, locked.attachmentRefs); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		if plan.relay.intent.QuoteID != "" {
			if err := s.copyQuotedReferences(ctx, tx, plan.key.RoomID, plan.relay.intent.QuoteID, batch.Result.Message.ID, seq, batch.Result.Message.Attachments); err != nil {
				return controlStatementFailure(s, ctx, err)
			}
		}
	}
	if plan.relay.intent.Kind == relay.EnterpriseOperationRetry && batch.Result.Message != nil {
		message := batch.Result.Message
		if err := s.copyAttachmentReferences(ctx, tx, plan.key.RoomID, message.RetryOf, message.ID, seq, message.Attachments); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
	}
	if len(batch.Facts) > 0 {
		start, end := int64(locked.currentSeq+1), int64(seq)
		first, last = &start, &end
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("relay_operations")+`(op_namespace,deployment_id,room_id,binding_id,generation,op_kind,identifier,principal,intent_hash,result_hash,expected_revision,result_revision,result_seq,result_version,result_encoding,fact_first,fact_last,committed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, plan.key.Namespace, plan.key.Deployment, plan.key.RoomID, plan.key.BindingID, int64(plan.key.Generation), plan.key.OpKind, plan.key.Identifier, plan.key.Principal, plan.hash[:], resultHash[:], int64(plan.expectedRevision), int64(locked.revision+1), int64(seq), relayResultVersion, encoding, first, last, now); err != nil {
		return controlStatementFailure(s, ctx, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("audit")+`(deployment_id,room_id,user_id,kind,outcome,at) VALUES($1,$2,$3,$4,'accepted',$5)`, plan.key.Deployment, plan.key.RoomID, plan.userID, "relay."+plan.key.OpKind, now); err != nil {
		return controlStatementFailure(s, ctx, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.table("rooms")+` SET current_seq=$2,revision=$3 WHERE room_id=$1`, plan.key.RoomID, int64(seq), int64(locked.revision+1)); err != nil {
		return controlStatementFailure(s, ctx, err)
	}
	return Result{Outcome: OutcomeCommitted, Seq: seq, Revision: locked.revision + 1, Facts: batch.Facts, Relay: &batch.Result}, nil
}

func toRelaySnapshot(snapshot RoomSnapshot) relay.EnterpriseRoomSnapshot {
	return relay.EnterpriseRoomSnapshot{RoomID: snapshot.RoomID, DeploymentID: snapshot.DeploymentID, OrgID: snapshot.OrgID, CreationUserID: snapshot.CreationUserID, HostMode: snapshot.HostMode, Events: snapshot.Events, Seq: snapshot.Seq, Revision: snapshot.Revision}
}
