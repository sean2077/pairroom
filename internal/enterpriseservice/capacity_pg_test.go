package enterpriseservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type capacityActor struct {
	client  *http.Client
	session httpPGSession
	auth    sqlstore.AuthContext
}
type capacityRoom struct {
	id          string
	owner       int
	objectID    string
	objectBytes []byte
}
type capacityStage struct {
	Name                string         `json:"name"`
	ActiveRooms         int            `json:"active_rooms"`
	Samples             int            `json:"samples"`
	Completed           int            `json:"completed"`
	AdmissionRetries    int            `json:"admission_retries"`
	Failures            int            `json:"failures"`
	P50MS               float64        `json:"p50_ms"`
	P95MS               float64        `json:"p95_ms"`
	P99MS               float64        `json:"p99_ms"`
	ElapsedSeconds      float64        `json:"elapsed_seconds"`
	RSSBytes            uint64         `json:"rss_bytes"`
	HeapBytes           uint64         `json:"heap_bytes"`
	HeapObjects         uint64         `json:"heap_objects"`
	SQLConnections      int            `json:"sql_connections"`
	SQLLockWaiters      int            `json:"sql_lock_waiters"`
	SQLLockWaitSamples  int            `json:"sql_lock_wait_samples"`
	SQLStorageBytes     int64          `json:"sql_storage_bytes"`
	ObjectStorageBytes  int64          `json:"object_storage_bytes"`
	Uploads             int            `json:"uploads"`
	PollIntervalMS      int64          `json:"poll_interval_ms"`
	SemanticFailures    int            `json:"semantic_failures"`
	OverloadFailures    int            `json:"overload_failures"`
	FailureClasses      map[string]int `json:"failure_classes"`
	StatusClasses       map[string]int `json:"status_classes"`
	PendingVerification map[string]int `json:"pending_verification"`
	ObjectStates        map[string]int `json:"object_states"`
	RequestsRejected    uint64         `json:"requests_rejected"`
	ReplayRejected      uint64         `json:"replay_rejected"`
	PeakRSS             uint64         `json:"peak_rss_bytes"`
}
type capacityReport struct {
	Version               int             `json:"version"`
	Go                    string          `json:"go"`
	OS                    string          `json:"os"`
	Architecture          string          `json:"architecture"`
	LogicalCPUs           int             `json:"logical_cpus"`
	PostgreSQL            string          `json:"postgresql"`
	Users                 int             `json:"users"`
	PersistedRooms        int             `json:"persisted_rooms"`
	ShortHistoryMessages  int             `json:"short_history_messages"`
	LongHistoryRooms      int             `json:"long_history_rooms"`
	LongHistoryMessages   int             `json:"long_history_messages"`
	ShortMessageBytes     int             `json:"short_message_bytes"`
	LongMessageBytes      int             `json:"long_message_bytes"`
	Objects               int             `json:"objects"`
	SeededObjects         int             `json:"seeded_objects"`
	ObjectBytes           int             `json:"object_bytes"`
	SQLRTTMS              float64         `json:"sql_rtt_ms"`
	HTTPSRTTMS            float64         `json:"https_rtt_ms"`
	RecoveryMS            float64         `json:"recovery_ms"`
	Stages                []capacityStage `json:"stages"`
	BaselineRSS           uint64          `json:"baseline_rss_bytes"`
	FinalRSS              uint64          `json:"final_idle_rss_bytes"`
	FinalHeap             uint64          `json:"final_idle_heap_bytes"`
	FinalConnections      int             `json:"final_idle_sql_connections"`
	FinalEvents           uint64          `json:"final_room_events"`
	FinalPublications     uint64          `json:"final_publications"`
	Correctness           bool            `json:"correctness"`
	SemanticCorrectness   bool            `json:"semantic_correctness"`
	AvailabilityComplete  bool            `json:"availability_complete"`
	RemainingQueued       int             `json:"remaining_queued_messages"`
	TotalOverloadFailures int             `json:"total_overload_failures"`
	SetupSeconds          float64         `json:"setup_seconds"`
	ElapsedSeconds        float64         `json:"elapsed_seconds"`
}

func capacityCall(ctx context.Context, f *httpPGFixture, actor capacityActor, method, path string, value any) (int, []byte, int, error) {
	var payload []byte
	if raw, ok := value.([]byte); method == http.MethodPut && ok {
		payload = raw
	} else if value != nil {
		var err error
		payload, err = json.Marshal(value)
		if err != nil {
			return 0, nil, 0, err
		}
	}
	seed := sha256.Sum256(append([]byte(method+"\x00"+path+"\x00"), payload...))
	rng := rand.New(rand.NewPCG(binary.LittleEndian.Uint64(seed[:8]), binary.LittleEndian.Uint64(seed[8:16])))
	lastCapacityCode := "admission_exhausted"
	for attempt := 0; attempt < 40; attempt++ {
		r, err := http.NewRequestWithContext(ctx, method, f.origin+path, bytes.NewReader(payload))
		if err != nil {
			return 0, nil, attempt, err
		}
		if value != nil {
			r.Header.Set("Content-Type", "application/json")
			if method == http.MethodPut {
				r.Header.Set("Content-Type", "application/octet-stream")
			}
			r.Header.Set("Origin", f.origin)
			r.Header.Set(CSRFHeader, actor.session.CSRF)
		}
		response, err := actor.client.Do(r)
		if err != nil {
			return 0, nil, attempt, err
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 3<<20))
		_ = response.Body.Close()
		if observer, ok := ctx.Value(capacityObservationKey{}).(*capacityObservation); ok {
			observer.record(response.StatusCode, data)
		}
		if err != nil {
			return response.StatusCode, nil, attempt, err
		}
		if response.StatusCode != 429 {
			return response.StatusCode, data, attempt, nil
		}
		var refused struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &refused)
		switch refused.Error {
		case "request_capacity", "replay_capacity", "object_capacity":
			lastCapacityCode = refused.Error + "_exhausted"
		}
		// Explicit bounded admission backoff retains the same frozen operation
		// identity; transport/unknown failures are never automatically submitted.
		limit := min(time.Second, 20*time.Millisecond*time.Duration(1<<min(attempt, 6)))
		timer := time.NewTimer(time.Duration(rng.Int64N(int64(limit) + 1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, nil, attempt, ctx.Err()
		case <-timer.C:
		}
	}
	return 429, nil, 40, &capacityFailure{Code: lastCapacityCode, Status: 429}
}

func capacityMessage(ctx context.Context, f *httpPGFixture, actor capacityActor, room capacityRoom, key, text string, attachments ...model.EnterpriseAttachmentIDs) (int, error) {
	input := map[string]any{"client_id": key, "to": "slot1", "text": text}
	intent := sqlstore.HumanSendIntent{To: "slot1", Text: text}
	prefix := ""
	if strings.HasSuffix(key, "-reference") {
		prefix = "reference_"
	}
	if len(attachments) > 0 {
		input["attachment_ids"] = attachments[0]
		intent.AttachmentIDs = attachments[0]
	}
	pending := &capacityPendingSend{actor: actor, room: room, key: key, intent: intent}
	status, data, retries, err := capacityCall(ctx, f, actor, "POST", "/enterprise/api/v1/rooms/"+room.id+"/messages", input)
	if err != nil || status != 200 {
		return retries, capacityFailureAt(prefix+"send", status, data, err, pending)
	}
	var accepted struct {
		Revision uint64         `json:"revision"`
		Message  *relay.Message `json:"message"`
	}
	if json.Unmarshal(data, &accepted) != nil || accepted.Message == nil {
		failure := capacitySemanticFailure(prefix+"send", "malformed_response").(*capacityFailure)
		failure.Pending = pending
		return retries, failure
	}
	if err := capacityCheckPublicMessages([]relay.Message{*accepted.Message}, actor); err != nil {
		return retries, err
	}
	status, data, more, err := capacityCall(ctx, f, actor, "GET", catalogPrefix+"room?room_id="+room.id, nil)
	if err != nil || status != 200 {
		return retries + more, capacityFailureAt(prefix+"catalog", status, data, err, pending)
	}
	var catalog sqlstore.CatalogPage
	if json.Unmarshal(data, &catalog) != nil || len(catalog.Rooms) != 1 {
		failure := capacitySemanticFailure(prefix+"catalog", "malformed_response").(*capacityFailure)
		failure.Pending = pending
		return retries + more, failure
	}
	if catalog.Rooms[0].RoomID != room.id || catalog.Rooms[0].OwnerUserID != actor.auth.UserID {
		return retries + more, capacitySemanticFailure(prefix+"catalog", "room_scope_mismatch")
	}
	accepted.Revision = catalog.Rooms[0].Revision
	retries += more
	// Cancellation is an explicit typed owner operation. Long history fixtures
	// do not strand >256 pending messages to bypass the admission contract.
	cancelIntent := relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationCancel, MessageID: accepted.Message.ID}
	pending.cancel = &sqlstore.RelayCommand{Auth: actor.auth, RoomID: room.id, ClientID: "cancel-" + key, ExpectedRevision: accepted.Revision, Intent: cancelIntent}
	status, data, more, err = capacityCall(ctx, f, actor, "POST", "/enterprise/api/v1/rooms/"+room.id+"/queue", map[string]any{"client_id": "cancel-" + key, "expected_revision": accepted.Revision, "intent": cancelIntent})
	if err != nil || status != 200 {
		return retries + more, capacityFailureAt(prefix+"cancel", status, data, err, pending)
	}
	return retries + more, nil
}

// capacitySeedMessage uses the actual fenced typed SQL boundary with the exact
// live browser Session obtained after signed OIDC. It avoids repeated HTTP
// metadata round trips during setup; measured stages always use TLS HTTP.
func capacitySeedMessage(ctx context.Context, f *httpPGFixture, actor capacityActor, room capacityRoom, revision uint64, key, text string) (uint64, error) {
	intent := sqlstore.HumanSendIntent{To: "slot1", Text: text}
	result, err := f.store.CommitHumanMessage(ctx, sqlstore.HumanMessageCommand{Auth: actor.auth, RoomID: room.id, ClientID: key, Intent: intent, ExpectedRevision: revision})
	if err != nil || result.Outcome != sqlstore.OutcomeCommitted {
		return revision, fmt.Errorf("typed seed send refused: %s", result.Class)
	}
	message, err := relay.EnterpriseHumanSendMessage(result.Fact, actor.auth.UserID, key, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: model.ActorSlot1, Text: text})
	if err != nil {
		return revision, err
	}
	cancel, err := f.store.CommitRelayOperation(ctx, sqlstore.RelayCommand{Auth: actor.auth, RoomID: room.id, ClientID: "cancel-" + key, ExpectedRevision: result.Revision, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationCancel, MessageID: message.ID}})
	if err != nil || cancel.Outcome != sqlstore.OutcomeCommitted {
		return result.Revision, fmt.Errorf("typed seed cancel refused: %s", cancel.Class)
	}
	return cancel.Revision, nil
}

func capacityUpload(ctx context.Context, f *httpPGFixture, actor capacityActor, room capacityRoom, key string, data []byte) (int, error) {
	sum := sha256.Sum256(data)
	base := "/enterprise/api/v1/rooms/" + room.id + "/attachments"
	status, body, retries, err := capacityCall(ctx, f, actor, "POST", base, map[string]any{"client_id": key, "spec": map[string]any{"name": "capacity.dat", "kind": "file", "size": len(data), "sha256": hex.EncodeToString(sum[:])}})
	if err != nil || status != 200 {
		return retries, capacityFailureAt("object_reserve", status, body, err, nil)
	}
	var response struct {
		Receipt sqlstore.AttachmentReceipt `json:"receipt"`
	}
	if json.Unmarshal(body, &response) != nil || !model.ValidEnterpriseAttachmentID(response.Receipt.ObjectID) {
		return retries, capacitySemanticFailure("object_reserve", "malformed_response")
	}
	base += "/" + response.Receipt.ObjectID
	status, body, more, err := capacityCall(ctx, f, actor, "PUT", base+"/content", data)
	retries += more
	if err != nil || status != 200 {
		return retries, capacityFailureAt("object_upload", status, body, err, nil)
	}
	status, body, more, err = capacityCall(ctx, f, actor, "POST", base+"/verify", map[string]string{"client_id": key + "-verify"})
	retries += more
	if err != nil || status != 200 {
		return retries, capacityFailureAt("object_verify", status, body, err, nil)
	}
	if json.Unmarshal(body, &response) != nil || response.Receipt.State != "verified" {
		return retries, capacitySemanticFailure("object_verify", "malformed_response")
	}
	ids, err := model.NewEnterpriseAttachmentIDs([]string{response.Receipt.ObjectID})
	if err != nil {
		return retries, err
	}
	more, err = capacityMessage(ctx, f, actor, room, key+"-reference", "measured immutable attachment", ids)
	return retries + more, err
}

func capacityDatabaseSample(ctx context.Context, f *httpPGFixture) (connections, lockWaiters int, storage int64, err error) {
	err = f.admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname=current_database()),(SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'),COALESCE((SELECT sum(pg_catalog.pg_total_relation_size(c.oid)) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind='r'),0)::bigint`, f.schema).Scan(&connections, &lockWaiters, &storage)
	return
}

func TestCapacityFixtureRepresentativeUploadsAndTypedSeed(t *testing.T) {
	f := newObjectHTTPFixture(t)
	client := f.browser("member-one")
	f.login(client)
	session := f.session(client)
	authority, err := f.store.AuthenticateSession(t.Context(), tokenHash(f.cookie(client).Value))
	if err != nil {
		t.Fatal(err)
	}
	actor := capacityActor{client: client, session: session, auth: authority.Auth}
	room := capacityRoom{id: "capacity-preflight", owner: 0}
	f.createRoom(client, session.CSRF, room.id)
	if _, err := capacitySeedMessage(t.Context(), f, actor, room, 1, "preflight-seed", strings.Repeat("x", 4096)); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{8 << 10, 256 << 10} {
		if _, err := capacityUpload(t.Context(), f, actor, room, fmt.Sprintf("preflight-upload-%d", size), bytes.Repeat([]byte("a"), size)); err != nil {
			t.Fatal(err)
		}
	}
	var referenced int
	if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM attachments WHERE state='referenced'`).Scan(&referenced); err != nil || referenced != 2 {
		t.Fatal("representative uploaded bytes were not referenced")
	}
	status, body, _, err := capacityCall(t.Context(), f, actor, "POST", "/enterprise/api/v1/rooms/"+room.id+"/history", relay.HistoryQuery{Limit: 100})
	var page relay.HistoryPage
	if err != nil || status != 200 || json.Unmarshal(body, &page) != nil || page.Total != 3 {
		t.Fatal("typed setup/history producer mismatch")
	}
	for _, message := range page.Messages {
		for _, attachment := range message.Attachments {
			status, body, _, err := capacityCall(t.Context(), f, actor, "GET", "/enterprise/api/v1/rooms/"+room.id+"/attachments/"+attachment.ID+"/content", nil)
			if err != nil || status != 200 || len(body) != int(attachment.Size) || !bytes.Equal(body, bytes.Repeat([]byte("a"), len(body))) {
				t.Fatal("representative full download differed")
			}
		}
	}
	snapshot, err := f.store.ReadonlySnapshot(t.Context(), actor.auth, room.id)
	if err != nil {
		t.Fatal(err)
	}
	queuedIntent := sqlstore.HumanSendIntent{To: "slot1", Text: "known accepted pending effect"}
	if result, err := f.store.CommitHumanMessage(t.Context(), sqlstore.HumanMessageCommand{Auth: actor.auth, RoomID: room.id, ClientID: "preflight-queued", Intent: queuedIntent, ExpectedRevision: snapshot.Revision}); err != nil || result.Outcome != sqlstore.OutcomeCommitted {
		t.Fatal("pending effect fixture refused")
	}
	counts, semantic := capacityVerifyPending(t.Context(), f, []capacityPendingSend{{actor: actor, room: room, key: "preflight-seed", intent: sqlstore.HumanSendIntent{To: "slot1", Text: strings.Repeat("x", 4096)}}, {actor: actor, room: room, key: "preflight-queued", intent: queuedIntent}})
	if semantic != 0 || counts["accepted_cancelled"] != 1 || counts["accepted_queued"] != 1 {
		t.Fatal("read-only intent verification concealed accepted queued work")
	}
}

func TestCapacityHTTPPGMeasuredWorkload(t *testing.T) {
	if os.Getenv("PAIRROOM_CAPACITY_RUN") != "1" {
		t.Skip("opt-in measured workload; run with isolated PostgreSQL and controlled report path")
	}
	output := os.Getenv("PAIRROOM_CAPACITY_REPORT")
	if output == "" {
		t.Fatal("capacity report path required")
	}
	started := time.Now()
	f := newObjectHTTPFixture(t)
	if _, err := f.store.RecoverDeliveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	report := capacityReport{Version: 2, AvailabilityComplete: true, Go: runtime.Version(), OS: runtime.GOOS, Architecture: runtime.GOARCH, LogicalCPUs: runtime.NumCPU(), Users: 100, PersistedRooms: 500, ShortHistoryMessages: 2, LongHistoryRooms: 8, LongHistoryMessages: 320, ShortMessageBytes: 256, LongMessageBytes: 4096, Objects: 20}
	if err := f.admin.QueryRow(t.Context(), `SELECT version()`).Scan(&report.PostgreSQL); err != nil {
		t.Fatal(err)
	}
	rtt := time.Now()
	for i := 0; i < 20; i++ {
		var one int
		if err := f.admin.QueryRow(t.Context(), `SELECT 1`).Scan(&one); err != nil {
			t.Fatal(err)
		}
	}
	report.SQLRTTMS = float64(time.Since(rtt).Microseconds()) / 20000
	admin := f.browser("operator")
	f.login(admin)
	adminSession := f.session(admin)
	actors := make([]capacityActor, 100)
	for i := range actors {
		subject := fmt.Sprintf("capacity-user-%03d", i)
		status, _ := f.post(admin, administrationPrefix+"commands", map[string]any{"client_id": "admit-" + subject, "intent": map[string]any{"kind": "identity.admission", "identity": map[string]string{"connection_id": "fixture-connection", "issuer": f.idp.server.URL, "subject": subject}, "enabled": true}}, adminSession.CSRF)
		if status != 200 {
			t.Fatalf("synthetic admission status %d", status)
		}
		client := f.browser(subject)
		client.Timeout = 60 * time.Second
		f.login(client)
		session := f.session(client)
		browserAuthority, err := f.store.AuthenticateSession(t.Context(), tokenHash(f.cookie(client).Value))
		if err != nil || browserAuthority.Auth.UserID != session.UserID {
			t.Fatal("synthetic browser authority not confirmed")
		}
		actors[i] = capacityActor{client: client, session: session, auth: browserAuthority.Auth}
	}
	t.Log("100 users admitted through signed synthetic OIDC and real TLS/SQL")
	rooms := make([]capacityRoom, 500)
	for i := range rooms {
		rooms[i] = capacityRoom{id: fmt.Sprintf("capacity-room-%03d", i), owner: i % 100}
		f.createRoom(actors[i%100].client, actors[i%100].session.CSRF, rooms[i].id)
	}
	t.Log("500 Rooms persisted through authenticated HTTP creation")
	for i, room := range rooms {
		count, size := report.ShortHistoryMessages, report.ShortMessageBytes
		if i < report.LongHistoryRooms {
			count, size = report.LongHistoryMessages, report.LongMessageBytes
		}
		revision := uint64(1)
		for n := 0; n < count; n++ {
			var err error
			revision, err = capacitySeedMessage(t.Context(), f, actors[room.owner], room, revision, fmt.Sprintf("seed-%d-%d", i, n), strings.Repeat("x", size))
			if err != nil {
				t.Fatal(err)
			}
		}
		if i%100 == 99 {
			t.Logf("history fixtures persisted for %d Rooms", i+1)
		}
	}
	for i := 0; i < report.Objects; i++ {
		size := 8 << 10
		if i%2 == 1 {
			size = 256 << 10
		}
		data := bytes.Repeat([]byte{byte('a' + i%26)}, size)
		actor := actors[rooms[i].owner]
		receipt := objectHTTPReserve(t, f, actor.client, rooms[i].id, fmt.Sprintf("capacity-object-%d", i), data, "", actor.session.CSRF)
		meta := objectHTTPFinish(t, f, actor.client, rooms[i].id, fmt.Sprintf("capacity-verify-%d", i), receipt, data, "", actor.session.CSRF)
		rooms[i].objectID, rooms[i].objectBytes = meta.ID, data
		report.ObjectBytes += len(data)
		ids, _ := model.NewEnterpriseAttachmentIDs([]string{meta.ID})
		if _, err := capacityMessage(t.Context(), f, actor, rooms[i], fmt.Sprintf("seed-reference-%d", i), "seeded immutable attachment", ids); err != nil {
			t.Fatal(err)
		}
	}
	report.SeededObjects = report.Objects
	httpsBegin := time.Now()
	for i := 0; i < 20; i++ {
		status, _, _, err := capacityCall(t.Context(), f, actors[0], "GET", "/enterprise/health/live", nil)
		if err != nil || status != 200 {
			t.Fatal("HTTPS RTT health read failed")
		}
	}
	report.HTTPSRTTMS = float64(time.Since(httpsBegin).Microseconds()) / 20000
	runtime.GC()
	report.BaselineRSS = capacityRSS()
	report.SetupSeconds = time.Since(started).Seconds()
	runStage := func(name string, active, rounds int, extended, pollInterval time.Duration) {
		t.Logf("measuring %s: %d active Rooms", name, active)
		stage := capacityStage{Name: name, ActiveRooms: active, PollIntervalMS: pollInterval.Milliseconds(), FailureClasses: map[string]int{}, PendingVerification: map[string]int{}, ObjectStates: map[string]int{}}
		stageStart := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		observer := &capacityObservation{status: map[string]int{}, admission: map[string]int{}}
		ctx = context.WithValue(ctx, capacityObservationKey{}, observer)
		var mu sync.Mutex
		var latencies []float64
		var pending []capacityPendingSend
		var wg sync.WaitGroup
		for i := 0; i < active; i++ {
			i := i
			wg.Go(func() {
				room := rooms[i]
				actor := actors[room.owner]
				for round := 0; round < rounds || extended > 0 && time.Since(stageStart) < extended; round++ {
					begin := time.Now()
					var status, retries int
					var err error
					switch round % 4 {
					case 0:
						var body []byte
						status, body, retries, err = capacityCall(ctx, f, actor, "GET", "/enterprise/api/v1/rooms/"+room.id, nil)
						if err != nil || status != 200 {
							err = capacityFailureAt("tail", status, body, err, nil)
						} else {
							err = capacityCheckBody("tail", body, room, actor, relay.SnapshotMessageLimit)
						}
					case 1:
						var body []byte
						status, body, retries, err = capacityCall(ctx, f, actor, "POST", "/enterprise/api/v1/rooms/"+room.id+"/history", relay.HistoryQuery{Limit: 100})
						if err != nil || status != 200 {
							err = capacityFailureAt("history", status, body, err, nil)
						} else {
							err = capacityCheckBody("history", body, room, actor, 100)
						}
					case 2:
						retries, err = capacityMessage(ctx, f, actor, room, fmt.Sprintf("load-%s-%d-%d", name, i, round), strings.Repeat("m", 1024))
						status = 200
					case 3:
						if room.objectID != "" {
							var body []byte
							status, body, retries, err = capacityCall(ctx, f, actor, "GET", "/enterprise/api/v1/rooms/"+room.id+"/attachments/"+room.objectID+"/content", nil)
							if err != nil || status != 200 {
								err = capacityFailureAt("object_download", status, body, err, nil)
							} else if !bytes.Equal(body, room.objectBytes) {
								err = capacitySemanticFailure("object_download", "wrong_object_bytes")
							}
							if err == nil && status == 200 {
								var more int
								more, err = capacityUpload(ctx, f, actor, room, fmt.Sprintf("upload-%s-%d-%d", name, i, round), room.objectBytes)
								retries += more
							}
						} else {
							var body []byte
							status, body, retries, err = capacityCall(ctx, f, actor, "POST", "/enterprise/api/v1/rooms/"+room.id+"/history", relay.HistoryQuery{Limit: 25})
							if err != nil || status != 200 {
								err = capacityFailureAt("history", status, body, err, nil)
							} else {
								err = capacityCheckBody("history", body, room, actor, 25)
							}
						}
					}
					mu.Lock()
					latencies = append(latencies, float64(time.Since(begin).Microseconds())/1000)
					stage.Samples++
					stage.AdmissionRetries += retries
					if err != nil || status != 200 {
						stage.Failures++
						var failure *capacityFailure
						if !errors.As(err, &failure) {
							failure = &capacityFailure{Step: "other", Code: "unclassified", Status: status, Semantic: true}
						}
						stage.FailureClasses[failure.Error()]++
						if failure.Semantic {
							stage.SemanticFailures++
						}
						if failure.Status == 429 {
							stage.OverloadFailures++
						}
						if failure.Pending != nil {
							pending = append(pending, *failure.Pending)
						}
					} else {
						stage.Completed++
						if round%4 == 3 && room.objectID != "" {
							stage.Uploads++
						}
					}
					mu.Unlock()
					if err != nil {
						return
					}
					if name == "reconnect-storm" {
						if transport, ok := actor.client.Transport.(*http.Transport); ok {
							transport.CloseIdleConnections()
						}
					}
					if pollInterval > 0 {
						timer := time.NewTimer(pollInterval)
						select {
						case <-ctx.Done():
							timer.Stop()
							return
						case <-timer.C:
						}
					}
				}
			})
		}
		waitDone := make(chan struct{})
		go func() { wg.Wait(); close(waitDone) }()
		ticker := time.NewTicker(100 * time.Millisecond)
		for pending := true; pending; {
			select {
			case <-waitDone:
				pending = false
			case <-ticker.C:
				stage.PeakRSS = max(stage.PeakRSS, capacityRSS())
				var n int
				if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n > stage.SQLLockWaiters {
					stage.SQLLockWaiters = n
				}
				stage.SQLLockWaitSamples += n
			}
		}
		ticker.Stop()
		sort.Float64s(latencies)
		if len(latencies) > 0 {
			quantile := func(q float64) float64 { return latencies[min(len(latencies)-1, int(float64(len(latencies)-1)*q))] }
			stage.P50MS = quantile(.50)
			stage.P95MS = quantile(.95)
			stage.P99MS = quantile(.99)
		}
		stage.ElapsedSeconds = time.Since(stageStart).Seconds()
		stage.RSSBytes = capacityRSS()
		stage.PeakRSS = max(stage.PeakRSS, stage.RSSBytes)
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		stage.HeapBytes, stage.HeapObjects = memory.HeapAlloc, memory.HeapObjects
		var err error
		var currentWaiters int
		stage.SQLConnections, currentWaiters, stage.SQLStorageBytes, err = capacityDatabaseSample(t.Context(), f)
		if err != nil {
			t.Fatal(err)
		}
		stage.SQLLockWaiters = max(stage.SQLLockWaiters, currentWaiters)
		if err := f.admin.QueryRow(t.Context(), `SELECT COALESCE(sum(size),0)::bigint FROM attachments WHERE deleted_at IS NULL AND state IN ('verified','referenced')`).Scan(&stage.ObjectStorageBytes); err != nil {
			t.Fatal(err)
		}
		stage.StatusClasses = observer.status
		stage.RequestsRejected = uint64(observer.admission["request_capacity"] + observer.admission["replay_capacity"] + observer.admission["object_capacity"] + observer.admission["other"])
		stage.ReplayRejected = uint64(observer.admission["replay_capacity"])
		var verificationSemantic int
		stage.PendingVerification, verificationSemantic = capacityVerifyPending(t.Context(), f, pending)
		stage.SemanticFailures += verificationSemantic
		for _, state := range []string{"reserved", "verified", "referenced", "abandoned", "deletion_pending"} {
			var count int
			if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM attachments WHERE state=$1`, state).Scan(&count); err != nil {
				t.Fatal(err)
			}
			stage.ObjectStates[state] = count
		}
		report.TotalOverloadFailures += stage.OverloadFailures
		report.Stages = append(report.Stages, stage)
		if stage.SemanticFailures > 0 {
			t.Errorf("%s semantic failures=%d", name, stage.SemanticFailures)
		}
		if stage.Failures > 0 {
			report.AvailabilityComplete = false
			t.Logf("%s operation failures=%d overload=%d semantic=%d (admission retries=%d)", name, stage.Failures, stage.OverloadFailures, stage.SemanticFailures, stage.AdmissionRetries)
		}
	}
	for _, active := range []int{50, 100, 250, 500} {
		runStage(fmt.Sprintf("active-%d", active), active, 4, 0, 0)
	}
	runStage("reconnect-storm", 100, 4, 0, 0)
	runStage("steady-50-paced", 50, 4, 60*time.Second, time.Second)
	runStage("steady-100-paced", 100, 4, 60*time.Second, time.Second)
	// Prove cross-user denial on a real object and verify restart preserves all
	// accepted history, sessions and the two-connection ownership model.
	status, _, _, err := capacityCall(t.Context(), f, actors[1], "GET", "/enterprise/api/v1/rooms/"+rooms[0].id+"/attachments/"+rooms[0].objectID+"/content", nil)
	if err != nil || status != 403 {
		t.Fatal("capacity object cross-user denial failed")
	}
	var before uint64
	if err := f.admin.QueryRow(t.Context(), `SELECT sum(current_seq)::bigint FROM rooms`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	restart := time.Now()
	store, err := sqlstore.Open(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recovery, err := store.RecoverDeliveries(t.Context())
	if err != nil || !recovery.Complete || recovery.Rooms != 500 {
		t.Fatal("500-Room ownership recovery failed")
	}
	report.RecoveryMS = float64(time.Since(restart).Microseconds()) / 1000
	f.store = store
	old := f.handler.Load()
	handler, err := NewWithObjectStore(Config{DeploymentID: f.cfg.DeploymentID, PublicOrigin: f.origin}, old.provider, old.transactions, store, old.objects)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.Store(handler)
	var after uint64
	if err := f.admin.QueryRow(t.Context(), `SELECT sum(current_seq)::bigint FROM rooms`).Scan(&after); err != nil || before != after {
		t.Fatal("idle recovery changed history or replayed accepted effects")
	}
	for i := 0; i < 100; i++ {
		status, _, _, err := capacityCall(t.Context(), f, actors[i], "GET", "/enterprise/api/v1/session", nil)
		if err != nil || status != 200 {
			t.Fatal("restart invalidated a live synthetic session")
		}
	}
	runtime.GC()
	report.FinalRSS = capacityRSS()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	report.FinalHeap = memory.HeapAlloc
	report.FinalConnections, _, _, err = capacityDatabaseSample(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	report.Correctness = !t.Failed()
	report.SemanticCorrectness = report.Correctness
	for _, room := range rooms {
		snapshot, err := f.store.ReadonlySnapshot(t.Context(), actors[room.owner].auth, room.id)
		if err != nil {
			t.Fatal("final authenticated history verification failed")
		}
		page, err := relay.ProjectEnterpriseHistory(relay.EnterpriseRoomSnapshot{RoomID: snapshot.RoomID, DeploymentID: snapshot.DeploymentID, OrgID: snapshot.OrgID, CreationUserID: snapshot.CreationUserID, HostMode: snapshot.HostMode, Events: snapshot.Events, Seq: snapshot.Seq, Revision: snapshot.Revision}, relay.HistoryQuery{Pending: true, Limit: 100})
		if err != nil {
			t.Fatal("final queue verification failed")
		}
		report.RemainingQueued += page.Total
	}
	if err := f.admin.QueryRow(t.Context(), `SELECT COALESCE(sum(current_seq),0)::bigint,(SELECT count(*) FROM operations WHERE op_kind='human_send') FROM rooms`).Scan(&report.FinalEvents, &report.FinalPublications); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.QueryRow(t.Context(), `SELECT count(*),COALESCE(sum(size),0)::bigint FROM attachments WHERE state='referenced'`).Scan(&report.Objects, &report.ObjectBytes); err != nil {
		t.Fatal(err)
	}
	report.ElapsedSeconds = time.Since(started).Seconds()
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(data, '\n'), 0600); err != nil {
		t.Fatal("cannot write controlled capacity report")
	}
	t.Logf("measured capacity report saved; %d stages, correctness=%t", len(report.Stages), report.Correctness)
}
