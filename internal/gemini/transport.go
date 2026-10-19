package gemini

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"time"

	"aizasy.pages.dev/ai-route/internal/logging"
)

// newTransport creates a high-performance transport with IPv6 rotation
func (p *Proxy) newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2000,
		MaxIdleConnsPerHost:   1000,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: p.cfg.TargetHost,
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Only intercept traffic to the target host
			if strings.Contains(addr, p.cfg.TargetHost) {
				return p.dialCustom(ctx)
			}
			// Fallback for other hosts
			d := net.Dialer{Timeout: 30 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
	}
}

// dialCustom handles the core IPv6 rotation logic (Src & Dest)
func (p *Proxy) dialCustom(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	// Bind random source IPv6
	if srcIP := p.source(); srcIP != nil {
		dialer.LocalAddr = &net.TCPAddr{IP: srcIP}
	}

	// Pick random destination IPv6
	destIP := p.pickRandomDestIP()
	if destIP == "" {
		// Cache may be empty/stale; do an on-demand AAAA resolve using local DNS.
		if err := p.Refresh(ctx); err == nil {
			destIP = p.pickRandomDestIP()
		} else {
			p.logger.Warnf("AAAA resolve failed: %v", err)
		}
	}
	if destIP == "" {
		// Final fallback: system resolver (still prefer tcp6 to force IPv6)
		return dialer.DialContext(ctx, "tcp6", net.JoinHostPort(p.cfg.TargetHost, "443"))
	}

	if p.logger.Enabled(logging.LevelDebug) {
		src := "System"
		if dialer.LocalAddr != nil {
			src = dialer.LocalAddr.String()
		}
		p.logger.Debugf("Dial: %s -> %s", src, destIP)
	}

	// Force IPv6 connection via IP to bypass DNS
	conn, err := dialer.DialContext(ctx, "tcp6", net.JoinHostPort(destIP, "443"))
	if err != nil {
		p.logger.Warnf("Dial failed to %s: %v", destIP, err)
		return nil, err
	}
	return conn, nil
}
