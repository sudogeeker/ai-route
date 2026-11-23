package relay

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

func (p *Proxy) refresh(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	network := "ip"
	switch p.cfg.IPFamily {
	case "ipv4":
		network = "ip4"
	case "ipv6":
		network = "ip6"
	}
	ips, err := p.lookup(ctx, network, p.target.Hostname())
	if err != nil {
		return err
	}

	destinations := p.filterIPs(ips)
	if len(destinations) == 0 {
		return fmt.Errorf("no %s addresses for %q", p.cfg.IPFamily, p.target.Hostname())
	}

	p.mu.Lock()
	p.destinations = destinations
	p.mu.Unlock()
	p.logger.Infof("Resolved %d %s address(es) for %s", len(destinations), p.cfg.IPFamily, p.target.Hostname())
	return nil
}

func (p *Proxy) filterIPs(ips []netip.Addr) []netip.Addr {
	seen := make(map[netip.Addr]bool, len(ips))
	destinations := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		ip = ip.Unmap()
		if !p.cfg.AllowsIP(ip) || seen[ip] {
			continue
		}
		seen[ip] = true
		destinations = append(destinations, ip)
	}
	return destinations
}

func (p *Proxy) snapshot() []netip.Addr {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.destinations
}

func (p *Proxy) Run(ctx context.Context) {
	if p.static {
		return
	}
	update := func() {
		if err := p.refresh(ctx); err != nil && ctx.Err() == nil {
			p.logger.Warnf("DNS refresh failed for %s: %v", p.target.Hostname(), err)
		}
	}
	update()
	ticker := time.NewTicker(p.cfg.UpdateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			update()
		}
	}
}
