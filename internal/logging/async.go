package logging

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const logQueueSize = 256

var errLogOutputBlocked = errors.New("log output did not drain before shutdown")

// Keep slow stdout collectors and files out of every request's critical path.
// One worker and a bounded queue cap the resources held by a blocked sink.
type asyncWriter struct {
	mu      sync.RWMutex
	closed  bool
	queue   chan []byte
	done    chan struct{}
	dropped atomic.Uint64
	err     error // Written by the worker, read only after done closes.
}

func newAsyncWriter(output io.Writer, closeOutput func() error) *asyncWriter {
	w := &asyncWriter{queue: make(chan []byte, logQueueSize), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		if closeOutput != nil {
			defer func() { w.err = errors.Join(w.err, closeOutput()) }()
		}
		for data := range w.queue {
			if _, err := output.Write(data); err != nil {
				w.err = err
			}
			if dropped := w.dropped.Swap(0); dropped > 0 {
				_, _ = fmt.Fprintf(output, "[WARN] Dropped %d log messages while output was congested\n", dropped)
			}
		}
	}()
	return w
}

func (w *asyncWriter) Write(data []byte) (int, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return len(data), nil
	}
	select {
	case w.queue <- append([]byte(nil), data...):
	default:
		w.dropped.Add(1)
	}
	return len(data), nil
}

func (w *asyncWriter) Close() error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	// The worker owns closing the file: closing it here can itself block on
	// an in-flight filesystem write. Leave at most that one worker to finish.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-w.done:
		return w.err
	case <-timer.C:
		return errLogOutputBlocked
	}
}
