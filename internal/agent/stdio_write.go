package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// This bounds transport backpressure, not model execution or the RPC response.
// Reader-generated responses have no request context, so they need a ceiling too.
const nativeStdinWriteTimeout = 30 * time.Second

// nativeStdinWriter serializes frames without making a queued caller wait past
// cancellation. Its zero value is ready for use. No adapter state lock is held
// across I/O; source is evaluated only after acquiring the write slot.
type nativeStdinWriter struct {
	once sync.Once
	slot chan struct{}
}

func (w *nativeStdinWriter) write(parent context.Context, source func() io.WriteCloser, frame []byte) error {
	ctx, cancel := context.WithTimeout(parent, nativeStdinWriteTimeout)
	defer cancel()
	w.once.Do(func() { w.slot = make(chan struct{}, 1) })
	select {
	case w.slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-w.slot }()
	if err := ctx.Err(); err != nil {
		return err
	}
	stdin := source()
	if stdin == nil {
		return errors.New("native stdin is not available")
	}

	// Owned stdin is pollable (an overlapped parent handle on Windows), and
	// io.Pipe supports Close during Write. Synchronous Windows os.Pipe handles
	// do not satisfy this boundary. Close only the captured endpoint and join
	// the callback before releasing its write slot.
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = stdin.Close()
		close(closed)
	})
	n, err := stdin.Write(frame)
	if !stop() {
		<-closed
	}
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	if err != nil {
		_ = stdin.Close() // An incomplete JSON frame cannot be followed by another.
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		// Once I/O began, cancellation can race a peer consuming bytes even
		// when the OS reports zero transferred. Only pre-I/O cancellation is
		// a definite no-send outcome.
		if n > 0 || ctx.Err() != nil {
			return fmt.Errorf("%w: %w", ErrSubmissionUnknown, err)
		}
		return err
	}
	return nil
}
