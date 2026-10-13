package sqlstore

import (
	"context"
	"errors"
)

func resourceAdmissionFailure(s *Store, ctx context.Context, err error) (Result, error) {
	var k *KernelError
	if errors.As(err, &k) {
		return Result{Outcome: k.Outcome, Class: k.Class, Err: k}, k
	}
	return controlStatementFailure(s, ctx, err)
}
