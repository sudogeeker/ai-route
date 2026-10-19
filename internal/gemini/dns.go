package gemini

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"time"
)

func (p *Proxy) Refresh(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip6", p.cfg.TargetHost)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(ips))
	tempIPs := make([]string, 0, len(ips))
	for _, ip := range ips {
		// Ensure IPv6 only
		if ip == nil || ip.To16() == nil || ip.To4() != nil {
			continue
		}
		s := ip.String()
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		tempIPs = append(tempIPs, s)
	}

	if len(tempIPs) == 0 {
		return fmt.Errorf("empty AAAA result for host %q", p.cfg.TargetHost)
	}

	p.mu.Lock()
	p.destinations = tempIPs
	count := len(p.destinations)
	p.mu.Unlock()

	p.logger.Infof("Resolved %d AAAA record(s) for %s", count, p.cfg.TargetHost)
	return nil
}

func (p *Proxy) pickRandomDestIP() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.destinations) == 0 {
		return ""
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(p.destinations))))
	if err != nil {
		return p.destinations[0]
	}
	return p.destinations[n.Int64()]
}

// Run refreshes Gemini AAAA records until the owning application stops.
func (p *Proxy) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.UpdateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.Refresh(ctx); err != nil {
				p.logger.Warnf("AAAA update failed: %v", err)
			} else {
				p.logger.Debugf("AAAA cache updated")
			}
		}
	}
}
