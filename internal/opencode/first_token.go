package opencode

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"
)

const (
	minFirstTokenGrace = 10 * time.Second
	maxFirstTokenGrace = 30 * time.Second
)

var errFirstTokenTimeout = errors.New("OpenCode first token grace expired")

func (p *Proxy) firstTokenTimeout() time.Duration {
	if p.firstTokenGrace != nil {
		return p.firstTokenGrace()
	}
	return minFirstTokenGrace + time.Duration(rand.Int63n(int64(maxFirstTokenGrace-minFirstTokenGrace)+1))
}

// This clock includes connection setup and headers. Reading heartbeat bytes or
// role metadata never extends it. It is stopped when the first output arrives.
type firstTokenDeadline struct {
	mu      sync.Mutex
	timer   *time.Timer
	expires time.Time
	cancel  context.CancelCauseFunc
	stopped bool
	expired bool
}

func newFirstTokenDeadline(timeout time.Duration, cancel context.CancelCauseFunc) *firstTokenDeadline {
	d := &firstTokenDeadline{expires: time.Now().Add(timeout), cancel: cancel}
	d.mu.Lock()
	d.timer = time.AfterFunc(timeout, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if !d.stopped {
			d.expired = true
			d.cancel(errFirstTokenTimeout)
		}
	})
	d.mu.Unlock()
	return d
}

func (d *firstTokenDeadline) stop() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Enforce the deadline even if the timer callback has not been scheduled yet.
	if !d.stopped && !time.Now().Before(d.expires) {
		d.expired = true
		d.cancel(errFirstTokenTimeout)
	}
	d.stopped = true
	d.timer.Stop()
	return d.expired
}
