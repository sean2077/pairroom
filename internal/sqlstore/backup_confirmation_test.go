package sqlstore

import (
	"errors"
	"testing"
)

func TestBackupExportNeverReportsNilErrorForUnconfirmedOutcome(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeUnknown, OutcomeNotCommitted, OutcomeRefused, OutcomeConflict, OutcomeReplayed, ""} {
		e := confirmedBackupExport(Result{Outcome: outcome}, nil)
		if e == nil {
			t.Fatalf("unconfirmed %s reported success", outcome)
		}
		var k *KernelError
		if !errors.As(e, &k) {
			t.Fatal("lost typed backup outcome")
		}
		if outcome == OutcomeUnknown && k.Outcome != OutcomeUnknown {
			t.Fatal("unknown export relabeled definite")
		}
	}
	if e := confirmedBackupExport(Result{Outcome: OutcomeCommitted}, nil); e != nil {
		t.Fatal(e)
	}
	original := errors.New("original failure")
	if e := confirmedBackupExport(Result{Outcome: OutcomeUnknown}, original); e != original {
		t.Fatal("backup discarded original failure")
	}
}
