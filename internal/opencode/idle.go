package opencode

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var errUpstreamIdle = errors.New("OpenCode upstream stopped sending data before completing the response")

// An idle timeout limits silence, not the total duration of a healthy stream.
// Cancelling the HTTP request interrupts an already-blocked Body.Read.
type idleResponseBody struct {
	io.ReadCloser
	mu      sync.Mutex
	timer   *time.Timer
	timeout time.Duration
	expires time.Time
	expired bool
	closed  bool
}

func newIdleResponseBody(body io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *idleResponseBody {
	b := &idleResponseBody{ReadCloser: body, timeout: timeout, expires: time.Now().Add(timeout)}
	b.mu.Lock()
	b.timer = time.AfterFunc(timeout, func() {
		b.mu.Lock()
		if b.closed || b.expired {
			b.mu.Unlock()
			return
		}
		// A callback already scheduled before Reset can run after a fresh
		// read. Recheck the deadline instead of cancelling an active stream.
		if remaining := time.Until(b.expires); remaining > 0 {
			b.timer.Reset(remaining)
			b.mu.Unlock()
			return
		}
		b.expired = true
		b.mu.Unlock()
		cancel()
	})
	b.mu.Unlock()
	return b
}

func (b *idleResponseBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.expired {
		return n, errUpstreamIdle
	}
	if n > 0 && !b.closed {
		b.expires = time.Now().Add(b.timeout)
		b.timer.Reset(b.timeout)
	}
	return n, err
}

func (b *idleResponseBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.timer.Stop()
	b.mu.Unlock()
	return b.ReadCloser.Close()
}
