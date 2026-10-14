package enterpriseservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestSoakHTTPPGThirtyMinutes(t *testing.T) {
	if os.Getenv("PAIRROOM_SOAK_RUN") != "1" {
		t.Skip("opt-in measured workload; run with isolated PostgreSQL and controlled report path")
	}
	output := os.Getenv("PAIRROOM_SOAK_REPORT")
	if output == "" {
		t.Fatal("capacity report path required")
	}
	steadyDuration := 30 * time.Minute
	probe := os.Getenv("PAIRROOM_SOAK_SHORT_PROBE") == "1"
	if probe {
		steadyDuration = 5 * time.Second
	}
	started := time.Now()
	f := newObjectHTTPFixture(t)
	if _, err := f.store.RecoverDeliveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	report := capacityReport{Version: 2, AvailabilityComplete: true, Go: runtime.Version(), OS: runtime.GOOS, Architecture: runtime.GOARCH, LogicalCPUs: runtime.NumCPU(), Users: 100, PersistedRooms: 500, ShortHistoryMessages: 2, LongHistoryRooms: 8, LongHistoryMessages: 320, ShortMessageBytes: 256, LongMessageBytes: 4096, Objects: 20}
	if probe {
		report.Users, report.PersistedRooms, report.LongHistoryRooms, report.LongHistoryMessages, report.Objects = 4, 8, 2, 4, 4
	}
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
	actors := make([]capacityActor, report.Users)
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
	t.Logf("%d users admitted through signed synthetic OIDC and real TLS/SQL", len(actors))
	rooms := make([]capacityRoom, report.PersistedRooms)
	for i := range rooms {
		rooms[i] = capacityRoom{id: fmt.Sprintf("capacity-room-%03d", i), owner: i % len(actors)}
		f.createRoom(actors[i%len(actors)].client, actors[i%len(actors)].session.CSRF, rooms[i].id)
	}
	t.Logf("%d Rooms persisted through authenticated HTTP creation", len(rooms))
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
	var timeline []map[string]any
	runStage := func(name string, active, rounds int, extended, pollInterval time.Duration) {
		t.Logf("measuring %s: %d active Rooms", name, active)
		stage := capacityStage{Name: name, ActiveRooms: active, PollIntervalMS: pollInterval.Milliseconds(), FailureClasses: map[string]int{}, PendingVerification: map[string]int{}, ObjectStates: map[string]int{}}
		stageStart := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), steadyDuration+2*time.Minute)
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
					}
					mu.Unlock()
					if err != nil {
						return
					}
					if strings.HasPrefix(name, "reconnect-storm") {
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
		nextSample := time.Duration(0)
		ticker := time.NewTicker(100 * time.Millisecond)
		for pending := true; pending; {
			select {
			case <-waitDone:
				pending = false
			case <-ticker.C:
				stage.PeakRSS = max(stage.PeakRSS, capacityRSS())
				if elapsed := time.Since(stageStart); elapsed >= nextSample {
					var ms runtime.MemStats
					runtime.ReadMemStats(&ms)
					connections, waiters, storage, sampleErr := capacityDatabaseSample(ctx, f)
					if sampleErr != nil {
						t.Fatal(sampleErr)
					}
					timeline = append(timeline, map[string]any{"stage": name, "elapsed_seconds": elapsed.Seconds(), "rss_bytes": capacityRSS(), "heap_bytes": ms.HeapAlloc, "heap_objects": ms.HeapObjects, "gc_count": ms.NumGC, "sql_connections": connections, "sampled_lock_waiters": waiters, "sql_storage_bytes": storage})
					t.Logf("soak progress %s elapsed=%.0fs rss=%d heap=%d gc=%d", name, elapsed.Seconds(), capacityRSS(), ms.HeapAlloc, ms.NumGC)
					nextSample += 30 * time.Second
				}
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
		if extended > 0 && time.Since(stageStart) < extended {
			t.Errorf("steady stage ended before requested duration: %.2fs / %.2fs", stage.ElapsedSeconds, extended.Seconds())
		}
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
	active := min(50, len(rooms))
	runStage("reconnect-storm", min(100, len(rooms)), 4, 0, 0)
	runStage("steady-50-soak", active, 4, steadyDuration, time.Second)
	runStage("reconnect-storm-after-soak", min(100, len(rooms)), 4, 0, 0)
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
	if err != nil || !recovery.Complete || recovery.Rooms != uint64(report.PersistedRooms) {
		t.Fatal("registered-Room ownership recovery failed")
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
	for i := range actors {
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
	data, err := json.MarshalIndent(map[string]any{"run_kind": map[bool]string{true: "short_fixture_probe", false: "thirty_minute_soak"}[probe], "requested_steady_seconds": steadyDuration.Seconds(), "report": report, "timeline": timeline}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(data, '\n'), 0600); err != nil {
		t.Fatal("cannot write controlled capacity report")
	}
	t.Logf("measured capacity report saved; %d stages, correctness=%t", len(report.Stages), report.Correctness)
}
