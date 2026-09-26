package agent

import (
	"bufio"
	"errors"
	"fmt"
	"time"

	"github.com/sean2077/pairroom/internal/execx"
)

// Maximum stdout record sizes. A vendor line beyond the limit cannot be
// parsed, and dropping it could strand a JSON-RPC call or a Turn terminal, so
// the adapter treats it as a fatal stream failure (see streamFailureReason).
const (
	codexMaxStdoutLine  = 16 << 20
	claudeMaxStdoutLine = 8 << 20
	grokMaxStdoutLine   = 16 << 20
)

// streamFailureReason describes why the adapter stopped a vendor process whose
// stdout could no longer be read. Once the reader stops, the vendor would block
// writing and never report a terminal, so the adapter kills the process tree
// and its normal exit path settles outstanding input with this reason.
func streamFailureReason(runtime string, limit int, err error) string {
	if errors.Is(err, bufio.ErrTooLong) {
		return fmt.Sprintf("%s wrote a stdout record larger than %d MiB; PairRoom stopped the process because the rest of the stream cannot be parsed reliably", runtime, limit>>20)
	}
	return fmt.Sprintf("read %s stream: %v; PairRoom stopped the process", runtime, err)
}

// processTreeExitTimeout bounds how long a stop or interrupt waits for a
// killed vendor process tree to release its output pipes.
const processTreeExitTimeout = 5 * time.Second

// stopProcessTree kills the vendor process tree and reports success only once
// the adapter's reader and wait goroutines finished (procDone closed). A tree
// that is still holding the output pipes after the bound is reported as an
// uncertain stop, so callers keep the capacity claim instead of treating the
// runtime as gone.
func stopProcessTree(tree *execx.Tree, procDone <-chan struct{}, runtime string) error {
	if err := tree.Kill(); err != nil {
		return fmt.Errorf("kill %s: %w", runtime, err)
	}
	if procDone == nil {
		return nil
	}
	timer := time.NewTimer(processTreeExitTimeout)
	defer timer.Stop()
	select {
	case <-procDone:
		return nil
	case <-timer.C:
		return fmt.Errorf("%s was killed but its process tree has not exited (a descendant may still hold its output pipes); stop state is uncertain", runtime)
	}
}
