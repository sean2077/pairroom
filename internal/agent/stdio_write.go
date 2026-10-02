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

	// os.File pipes (including Windows pipes) and io.Pipe support Close during
	// Write. Close the captured endpoint, never a later replacement process's
	// stdin. Join a started callback before releasing the slot.
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
		if n > 0 {
			return fmt.Errorf("%w: %w", ErrSubmissionUnknown, err)
		}
		return err
	}
	return nil
}
