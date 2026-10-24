package warp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

const DefaultPoolSize = 2

const egressCooldown = 10 * time.Minute

type logger interface {
	Debugf(string, ...interface{})
	Infof(string, ...interface{})
	Warnf(string, ...interface{})
	Errorf(string, ...interface{})
}

type Pool struct {
	ctx     context.Context
	cancel  context.CancelFunc
	logger  logger
	size    int
	newExit func(context.Context) (*exit, error)

	mu            sync.Mutex
	active        []*exit
	standby       []*exit
	activeTarget  int
	standbyTarget int
	next          int
	reserved      map[string]bool
	cooling       map[string]time.Time
	changed       chan struct{}
	closed        bool
	refilling     bool
	wait          sync.WaitGroup
	createMu      sync.Mutex
}

type Lease struct {
	exit *exit
	once sync.Once
}

func New(ctx context.Context, log logger, size int) (*Pool, error) {
	return NewWithStack(ctx, log, size, DefaultStack)
}

// NewWithStack creates a WARP pool whose exits use the selected IP stack.
func NewWithStack(ctx context.Context, log logger, size int, stack Stack) (*Pool, error) {
	normalized, err := ParseStack(string(stack))
	if err != nil {
		return nil, err
	}
	if size <= 0 {
		size = DefaultPoolSize
	}
	r := newRegistrar()
	return newPool(ctx, log, size, func(ctx context.Context) (*exit, error) {
		reg, err := r.register(ctx)
		if err != nil {
			return nil, err
		}
		return newExitWithStack(ctx, reg, normalized)
	})
}

func newPool(ctx context.Context, log logger, size int, create func(context.Context) (*exit, error)) (*Pool, error) {
	if size <= 0 {
		size = DefaultPoolSize
	}
	poolCtx, cancel := context.WithCancel(ctx)
	activeTarget, standbyTarget := poolTargets(size)
	p := &Pool{
		ctx:           poolCtx,
		cancel:        cancel,
		logger:        log,
		size:          size,
		activeTarget:  activeTarget,
		standbyTarget: standbyTarget,
		newExit:       create,
		reserved:      make(map[string]bool),
		cooling:       make(map[string]time.Time),
		changed:       make(chan struct{}),
	}
	first, err := p.createExit(poolCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	p.active = []*exit{first}
	if log != nil {
		log.Infof("OpenCode WARP/%s egress %s ready at %s (active target %d, standby target %d)", first.stack, first.id, first.egress, activeTarget, standbyTarget)
	}
	p.startRefill()
	return p, nil
}

func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("WARP pool is closed")
		}
		for attempts := 0; attempts < len(p.active); attempts++ {
			index := p.next % len(p.active)
			p.next = (p.next + 1) % len(p.active)
			active := p.active[index]
			if active.addRef() {
				p.mu.Unlock()
				return &Lease{exit: active}, nil
			}
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.ctx.Done():
			return nil, errors.New("WARP pool is closed")
		case <-changed:
		}
	}
}

func (p *Pool) Report(lease *Lease, status int, body []byte) {
	if lease == nil || lease.exit == nil {
		return
	}
	rotate, reason := lease.exit.observe(status, body)
	if !rotate {
		return
	}
	p.penalize(lease.exit, reason)
}

// Fail drops an exit that cannot carry traffic even though the upstream never
// produced an HTTP response, such as a broken WARP handshake or a stalled first
// token. Retrying the same user-space tunnel would only repeat the stall.
func (p *Pool) Fail(lease *Lease, reason string) {
	if lease == nil || lease.exit == nil || reason == "" {
		return
	}
	p.penalize(lease.exit, reason)
}

func (p *Pool) Size() int { return p.size }

func (p *Pool) penalize(bad *exit, reason string) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	index := p.activeIndexLocked(bad)
	if index < 0 {
		p.mu.Unlock()
		return
	}
	if !bad.markPenalized() {
		p.mu.Unlock()
		return
	}
	if key := bad.egressKey(); key != "" {
		p.cooling[key] = time.Now().Add(egressCooldown)
		delete(p.reserved, key)
	}
	p.active = append(p.active[:index], p.active[index+1:]...)
	if p.next >= len(p.active) {
		p.next = 0
	}
	var replacement *exit
	if len(p.standby) > 0 && len(p.active) < p.activeTarget {
		replacement = p.standby[0]
		p.standby = p.standby[1:]
		p.active = append(p.active, replacement)
	}
	if replacement != nil {
		p.wait.Add(1)
	}
	p.wait.Add(1)
	p.notifyLocked()
	p.mu.Unlock()

	if p.logger != nil {
		p.logger.Warnf("Rotating OpenCode WARP egress %s (%s)", bad.id, reason)
	}
	if replacement != nil {
		go func() {
			defer p.wait.Done()
			bad.retire()
		}()
		if p.logger != nil {
			p.logger.Infof("OpenCode WARP exit switched to standby %s at %s", replacement.id, replacement.egress)
		}
	}
	go func() {
		defer p.wait.Done()
		p.replace(bad, reason)
	}()
}

func (p *Pool) replace(old *exit, reason string) {
	replacement, err := p.createExit(p.ctx)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		if replacement != nil {
			p.releaseReservation(replacement)
			replacement.close()
		}
		old.retire()
		return
	}
	if err != nil {
		restored := len(p.active) == 0
		if restored {
			old.clearPenalty()
			if key := old.egressKey(); key != "" {
				p.reserved[key] = true
				delete(p.cooling, key)
			}
			p.active = append(p.active, old)
			p.notifyLocked()
		}
		p.mu.Unlock()
		if !restored {
			old.retire()
		}
		if p.logger != nil {
			p.logger.Errorf("Cannot replace OpenCode WARP egress %s: %v", old.id, err)
		}
		p.startRefill()
		return
	}
	if p.activeIndexLocked(old) >= 0 {
		p.mu.Unlock()
		p.releaseReservation(replacement)
		replacement.close()
		old.retire()
		return
	}
	placed := false
	if len(p.active) < p.activeTarget {
		p.active = append(p.active, replacement)
		placed = true
	} else if len(p.standby) < p.standbyTarget {
		p.standby = append(p.standby, replacement)
		placed = true
	}
	p.notifyLocked()
	p.mu.Unlock()
	if !placed {
		p.releaseReservation(replacement)
		replacement.close()
		old.retire()
		return
	}
	if p.logger != nil {
		p.logger.Infof("OpenCode WARP egress %s replaced by %s at %s (%s), active=%d standby=%d", old.id, replacement.id, replacement.egress, reason, len(p.active), len(p.standby))
	}
	old.retire()
	p.startRefill()
}

func (p *Pool) refill() {
	p.mu.Lock()
	if p.closed || p.refilling || len(p.active) == 0 || !p.needsRefillLocked() {
		p.mu.Unlock()
		return
	}
	p.refilling = true
	p.mu.Unlock()

	replacement, err := p.createExit(p.ctx)
	p.mu.Lock()
	p.refilling = false
	if err == nil && !p.closed {
		if len(p.active) < p.activeTarget {
			p.active = append(p.active, replacement)
			replacement = nil
			p.notifyLocked()
		} else if len(p.standby) < p.standbyTarget {
			p.standby = append(p.standby, replacement)
			replacement = nil
			p.notifyLocked()
		}
	}
	if p.closed {
		p.notifyLocked()
	}
	p.mu.Unlock()

	if replacement != nil {
		p.releaseReservation(replacement)
		replacement.close()
	}
	if err != nil && p.ctx.Err() == nil && p.logger != nil {
		p.logger.Warnf("Cannot pre-register OpenCode WARP egress: %v", err)
	}
	p.startRefill()
}

func (p *Pool) startRefill() {
	p.mu.Lock()
	if p.closed || p.refilling || len(p.active) == 0 || !p.needsRefillLocked() {
		p.mu.Unlock()
		return
	}
	p.wait.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wait.Done()
		p.refill()
	}()
}

func (p *Pool) createExit(ctx context.Context) (*exit, error) {
	p.createMu.Lock()
	defer p.createMu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return nil, errors.New("WARP pool is closed")
		}
		candidate, err := p.newExit(ctx)
		if err != nil {
			return nil, err
		}
		key := candidate.egressKey()
		now := time.Now()
		p.mu.Lock()
		for subnet, until := range p.cooling {
			if !until.After(now) {
				delete(p.cooling, subnet)
			}
		}
		usable := key != "" && !p.reserved[key] && !p.cooling[key].After(now)
		if usable {
			p.reserved[key] = true
		}
		p.mu.Unlock()
		if usable {
			return candidate, nil
		}
		lastErr = fmt.Errorf("WARP egress %s is already active or cooling down", key)
		if p.logger != nil {
			p.logger.Warnf("Discarding duplicate WARP exit %s: %v", candidate.id, lastErr)
		}
		candidate.close()
	}
	return nil, lastErr
}

func (p *Pool) releaseReservation(e *exit) {
	if e == nil {
		return
	}
	key := e.egressKey()
	if key == "" {
		return
	}
	p.mu.Lock()
	delete(p.reserved, key)
	p.mu.Unlock()
}

func (p *Pool) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *Pool) activeIndexLocked(target *exit) int {
	for index, candidate := range p.active {
		if candidate == target {
			return index
		}
	}
	return -1
}

func (p *Pool) needsRefillLocked() bool {
	return len(p.active) < p.activeTarget || len(p.standby) < p.standbyTarget
}

func poolTargets(size int) (active, standby int) {
	if size <= 1 {
		return 1, 0
	}
	return size - 1, 1
}

func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	active := p.active
	p.active = nil
	standby := p.standby
	p.standby = nil
	p.notifyLocked()
	p.mu.Unlock()

	p.cancel()
	for _, item := range active {
		p.releaseReservation(item)
		item.retire()
	}
	for _, item := range standby {
		p.releaseReservation(item)
		item.retire()
	}
	p.wait.Wait()
}

func (l *Lease) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if l == nil || l.exit == nil {
		return nil, errors.New("WARP lease is closed")
	}
	return l.exit.dialContext(ctx, network, address)
}

func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.exit != nil {
			l.exit.release()
		}
	})
}

func (l *Lease) EgressKey() string {
	if l == nil || l.exit == nil {
		return ""
	}
	return l.exit.egressKey()
}

// DialNetwork is the explicit TCP address family for this lease. Callers should
// pass it to DialContext instead of assuming IPv6.
func (l *Lease) DialNetwork() string {
	if l == nil || l.exit == nil {
		return ""
	}
	return l.exit.dialNetwork()
}

func (l *Lease) String() string {
	if l == nil || l.exit == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s/%s", l.exit.id, l.exit.egress)
}
