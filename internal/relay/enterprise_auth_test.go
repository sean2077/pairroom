package relay

import (
	"context"
	"reflect"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

// One Engine serves commands from different live sessions. A conflict rebuild
// must use the command's authenticated session, not the session that opened
// the Engine and not a newly constructed user-only context.
func TestEnterpriseSessionAuthSurvivesSendAndConflictRebuild(t *testing.T) {
	opening := EnterpriseAuth{Deployment: "dep-test", UserID: "u-owner", SessionID: "browser-open"}
	command := EnterpriseAuth{Deployment: "dep-test", UserID: "u-member", SessionID: "browser-command"}
	var snapshotAuths []EnterpriseAuth
	port := &fakePort{snapshot: emptySnapshot(t)}
	port.snapFn = func(auth EnterpriseAuth) (EnterpriseRoomSnapshot, error) {
		snapshotAuths = append(snapshotAuths, auth)
		return port.snapshot, nil
	}
	attempts := 0
	port.commitFn = func(cmd EnterpriseSendCommand) (EnterpriseSendResult, error) {
		attempts++
		if attempts == 1 {
			return EnterpriseSendResult{}, &EnterpriseCommandError{Outcome: EnterpriseOutcomeRefused, Class: EnterpriseClassRevisionConflict}
		}
		return committedEcho(cmd), nil
	}
	engine, err := OpenEnterprise(context.Background(), Config{RoomID: "room-1"}, port, opening)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.ExecuteHumanSend(context.Background(), command, SendRequest{ID: "session-send", Text: "body", To: model.ActorSlot2}); err != nil {
		t.Fatal(err)
	}
	if got := port.receivedAuths(); !reflect.DeepEqual(got, []EnterpriseAuth{command, command}) {
		t.Fatalf("send/replan dropped or replaced session auth: %+v", got)
	}
	if !reflect.DeepEqual(snapshotAuths, []EnterpriseAuth{opening, command}) {
		t.Fatalf("constructor/rebuild used wrong session auth: %+v", snapshotAuths)
	}
	if engine.Snapshot().Sequence != 2 || port.commitCalls() != 2 {
		t.Fatal("the conflict/rebuild path did not execute")
	}
}
