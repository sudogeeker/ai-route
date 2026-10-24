package opencode

import (
	"context"
	"errors"
	"sync"
	"time"

	"aizasy.pages.dev/ai-route/internal/config"
)

var errFirstTokenTimeout = errors.New("OpenCode first token grace expired")

func (p *Proxy) firstTokenTimeout() time.Duration {
	if p.firstTokenGrace != nil {
		return p.firstTokenGrace()
	}
	if p.cfg.FirstTokenTimeout > 0 {
		return p.cfg.FirstTokenTimeout
	}
	return config.DefaultOpenCodeFirstTokenGrace
}

// This clock starts only after the request body has been fully written to the
// transport. Connection setup and a slow upload are guarded by the overall race
// budget instead. Reading heartbeat bytes or role metadata never extends it.
type firstTokenDeadline struct {
	mu      sync.Mutex
	timer   *time.Timer
	expires time.Time
	cancel  context.CancelCauseFunc
	started bool
	stopped bool
	expired bool
}

func newFirstTokenDeadline(cancel context.CancelCauseFunc) *firstTokenDeadline {
	return &firstTokenDeadline{cancel: cancel}
}

func (d *firstTokenDeadline) start(timeout time.Duration) {
	d.mu.Lock()
	if d.started || d.stopped {
		d.mu.Unlock()
		return
	}
	d.started = true
	d.expires = time.Now().Add(timeout)
	d.timer = time.AfterFunc(timeout, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.started && !d.stopped {
			d.expired = true
			d.cancel(errFirstTokenTimeout)
		}
	})
	d.mu.Unlock()
}

func (d *firstTokenDeadline) stop() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.started {
		d.stopped = true
		return false
	}
	// Enforce the deadline even if the timer callback has not been scheduled yet.
	if !d.stopped && !time.Now().Before(d.expires) {
		d.expired = true
		d.cancel(errFirstTokenTimeout)
	}
	d.stopped = true
	if d.timer != nil {
		d.timer.Stop()
	}
	return d.expired
}
