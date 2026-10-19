package opencode

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net"
	"sync"
	"time"
)

// This cache has no dependency on Gemini's target, AAAA cache or updater.
type dnsCache struct {
	host       string
	lookup     func(context.Context, string, string) ([]net.IP, error)
	mu         sync.Mutex
	ips        []net.IP
	err        error
	expires    time.Time
	refreshing chan struct{}
}

func (d *dnsCache) pick(ctx context.Context) (net.IP, error) {
	if ip := net.ParseIP(d.host); ip != nil {
		if ip.To4() != nil {
			return nil, errors.New("OpenCode requires an IPv6 upstream")
		}
		return ip, nil
	}
	for {
		d.mu.Lock()
		if time.Now().Before(d.expires) {
			ips, err := d.ips, d.err
			d.mu.Unlock()
			if err != nil {
				return nil, err
			}
			index, err := rand.Int(rand.Reader, big.NewInt(int64(len(ips))))
			if err != nil {
				return nil, err
			}
			return ips[index.Int64()], nil
		}
		if pending := d.refreshing; pending != nil {
			d.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-pending:
				continue
			}
		}
		pending := make(chan struct{})
		d.refreshing = pending
		d.mu.Unlock()
		resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ips, err := d.lookup(resolveCtx, "ip6", d.host)
		cancel()
		var valid []net.IP
		for _, ip := range ips {
			if ip.To16() != nil && ip.To4() == nil {
				valid = append(valid, ip)
			}
		}
		if err == nil && len(valid) == 0 {
			err = errors.New("OpenCode upstream has no AAAA records")
		}
		d.mu.Lock()
		d.ips, d.err = valid, err
		d.expires = time.Now().Add(10 * time.Minute)
		if err != nil {
			d.expires = time.Now().Add(time.Second)
		}
		d.refreshing = nil
		close(pending)
		d.mu.Unlock()
	}
}
