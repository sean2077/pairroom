package sqlstore

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRecoverySubprocessEntry(t *testing.T) {
	schema := os.Getenv("PAIRROOM_RECOVERY_CHILD_SCHEMA")
	if schema == "" {
		return
	}
	s, e := Open(context.Background(), Config{DSN: os.Getenv("PAIRROOM_SQLSTORE_TEST_DSN"), Schema: schema, DeploymentID: "dep-test", RequireSessionAuth: true})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	fmt.Printf("RECOVERY_READY %d %d %d\n", s.work.PgConn().PID(), s.lease.pid, s.lease.epoch)
	if _, e = s.RecoverDeliveries(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestRecoveryKilledProcessContinuesInNewEpochWithoutRepeatingCommittedRoom(t *testing.T) {
	b, owner, member := recoveryPGPair(t)
	ctx := context.Background()
	relayPGCommit(t, b, relayPGCommand(t, b, owner, "process-send", recoverySendIntent("old process input")))
	relayPGCommit(t, b, relayPGCommand(t, b, member, "process-claim", recoveryClaimIntent()))
	if e := b.f.store.Close(); e != nil {
		t.Fatal(e)
	}
	blocker := schemaConn(t, b.f.dsn, b.f.schema)
	tx, e := blocker.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT room_id FROM rooms WHERE room_id='room-1' FOR UPDATE`); e != nil {
		t.Fatal(e)
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	childCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(childCtx, exe, "-test.run=^TestRecoverySubprocessEntry$", "-test.count=1")
	command.Env = append(os.Environ(), "PAIRROOM_RECOVERY_CHILD_SCHEMA="+b.f.schema)
	stdout, e := command.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = command.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}()
	line, e := bufio.NewReader(stdout).ReadString('\n')
	if e != nil {
		t.Fatal(e)
	}
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0] != "RECOVERY_READY" {
		t.Fatalf("missing child startup metadata: %q", line)
	}
	workPID, _ := strconv.ParseUint(fields[1], 10, 32)
	leasePID, _ := strconv.ParseUint(fields[2], 10, 32)
	epoch, _ := strconv.ParseInt(fields[3], 10, 64)
	waitForControlLockWait(t, b.f.admin, uint32(workPID), blocker.PgConn().PID())
	var committed int
	if e = b.f.admin.QueryRow(ctx, `SELECT count(*) FROM startup_room_recoveries WHERE ownership_epoch=$1 AND room_id='binding-room' AND reaped_count=1`, epoch).Scan(&committed); e != nil || committed != 1 {
		t.Fatalf("child did not commit earlier Room before barrier: %d %v", committed, e)
	}
	if e = command.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = command.Wait()
	if e = tx.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var gone bool
		if e = b.f.admin.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid=$1)`, int32(leasePID)).Scan(&gone); e != nil {
			t.Fatal(e)
		}
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("killed child lease backend did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := recoveryCounts(t, b)
	s, e := Open(ctx, b.f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	b.f.store = s
	report, e := s.RecoverDeliveries(ctx)
	if e != nil || !report.Complete || report.Epoch <= epoch || report.Reaped != 0 || report.Rooms != 2 {
		t.Fatalf("successor recovery: %+v %v", report, e)
	}
	if after := recoveryCounts(t, b); after[0] != before[0] {
		t.Fatal("successor repeated already committed recovery fact")
	}
}
