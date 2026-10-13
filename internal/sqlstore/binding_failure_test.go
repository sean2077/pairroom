package sqlstore

import (
	"context"
	"errors"
	"testing"
)

func TestBindingPrecommitReadFailureAndIncompleteResolutionHaveDistinctOutcomes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	readFailure := incompleteError(ctx, context.Canceled)
	store := &Store{}
	result, err := bindingOperationFailure(store, ctx, Result{}, readFailure, false)
	var kernel *KernelError
	if result.Outcome != OutcomeNotCommitted || result.Class != ClassCanceled || !errors.As(err, &kernel) || kernel.Outcome != OutcomeNotCommitted {
		t.Fatalf("pre-COMMIT read failure became uncertain: %+v %v", result, err)
	}
	result, err = bindingOperationFailure(store, ctx, Result{}, readFailure, true)
	if result.Outcome != OutcomeUnknown || result.Class != ClassCanceled || !errors.As(err, &kernel) || kernel.Outcome != OutcomeUnknown {
		t.Fatalf("incomplete verification claimed absence: %+v %v", result, err)
	}
}
